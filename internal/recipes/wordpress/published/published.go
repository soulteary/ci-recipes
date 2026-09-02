// Package published verifies that an immutable release tag has the same
// manifest digest on Docker Hub and GHCR. It replaces
// verify-published-release.sh.
package published

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/soulteary/ci-recipes/internal/cli"
)

const (
	usage                 = "usage: verify-published-release YYYY.MM.DD-rN"
	defaultImage          = "soulteary/sqlite-wordpress"
	defaultAttempts       = 4 // curl --retry 3 plus the initial request
	defaultRequestTimeout = 60 * time.Second
	maxResponseBytes      = 1 << 20
)

var (
	releasePattern = regexp.MustCompile(`^[0-9]{4}\.[0-9]{2}\.[0-9]{2}-r[1-9][0-9]*$`)
	digestPattern  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// SleepFunc is injectable for deterministic retry tests.
type SleepFunc func(context.Context, time.Duration) error

// Options controls endpoints, HTTP behavior, and retries. Endpoint overrides
// exist for tests and private mirrors; defaults match the shell script.
type Options struct {
	Client           *http.Client
	DockerHubBaseURL string
	GHCRBaseURL      string
	GHCRTokenURL     string
	Image            string
	Attempts         int
	RequestTimeout   time.Duration
	RetryDelay       func(retry int) time.Duration
	Sleep            SleepFunc
	Now              func() time.Time
	MaxRetryAfter    time.Duration
}

// Execute verifies the public soulteary/sqlite-wordpress release using secure
// production HTTP defaults.
func Execute(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return ExecuteWithOptions(ctx, args, stdin, stdout, stderr, Options{})
}

// ExecuteWithOptions is Execute with injectable HTTP and retry dependencies.
func ExecuteWithOptions(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, options Options) error {
	_ = stdin
	_ = stderr
	if ctx == nil {
		return cli.Exit(1, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return cli.Exit(1, "published release verification canceled: %v", err)
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if len(args) != 1 || !releasePattern.MatchString(args[0]) {
		return cli.Exit(2, usage)
	}
	releaseVersion := args[0]

	configuration := withDefaults(options)
	if configuration.ownedClient {
		defer configuration.Client.CloseIdleConnections()
	}
	dockerHubURL, err := dockerHubTagURL(configuration.DockerHubBaseURL, configuration.Image, releaseVersion)
	if err != nil {
		return cli.Exit(1, "unable to construct Docker Hub request: %v", err)
	}
	dockerHubResponse, err := requestWithRetry(ctx, configuration, dockerHubURL, nil)
	if err != nil {
		return cli.Exit(requestExitCode(err), "unable to query Docker Hub: %v", err)
	}
	var dockerHubDocument struct {
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal(dockerHubResponse.Body, &dockerHubDocument); err != nil || !digestPattern.MatchString(dockerHubDocument.Digest) {
		return cli.Exit(4, "Docker Hub did not return a valid manifest digest for %s", releaseVersion)
	}

	tokenURL, err := ghcrTokenURL(configuration.GHCRTokenURL, configuration.Image)
	if err != nil {
		return cli.Exit(1, "unable to construct GHCR token request: %v", err)
	}
	tokenResponse, err := requestWithRetry(ctx, configuration, tokenURL, nil)
	if err != nil {
		return cli.Exit(requestExitCode(err), "unable to request a GHCR pull token: %v", err)
	}
	var tokenDocument struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(tokenResponse.Body, &tokenDocument); err != nil || tokenDocument.Token == "" {
		return cli.Exit(4, "GHCR did not return a valid pull token")
	}

	manifestURL, err := ghcrManifestURL(configuration.GHCRBaseURL, configuration.Image, releaseVersion)
	if err != nil {
		return cli.Exit(1, "unable to construct GHCR manifest request: %v", err)
	}
	headers := http.Header{
		"Authorization": []string{"Bearer " + tokenDocument.Token},
		"Accept": []string{
			"application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json",
		},
		// Preserve the exact registry bytes used to compute Docker-Content-Digest.
		// Go's transport otherwise transparently requests and decodes gzip.
		"Accept-Encoding": []string{"identity"},
	}
	manifestResponse, err := requestWithRetry(ctx, configuration, manifestURL, headers)
	if err != nil {
		// The bearer token is held only in the request header and is never
		// interpolated into this error or a child-process argument.
		return cli.Exit(requestExitCode(err), "unable to query the GHCR manifest: %v", err)
	}
	digestHeaders := manifestResponse.Header.Values("Docker-Content-Digest")
	if len(digestHeaders) != 1 {
		return cli.Exit(1, "GHCR did not return a valid manifest digest for %s", releaseVersion)
	}
	ghcrDigest := strings.TrimSpace(digestHeaders[0])
	if !digestPattern.MatchString(ghcrDigest) {
		return cli.Exit(1, "GHCR did not return a valid manifest digest for %s", releaseVersion)
	}
	if err := validateManifestBody(manifestResponse.Body); err != nil {
		return cli.Exit(1, "GHCR returned an invalid manifest for %s: %v", releaseVersion, err)
	}
	calculatedDigest := contentDigest(manifestResponse.Body)
	if ghcrDigest != calculatedDigest {
		return cli.Exit(1, "GHCR manifest digest does not match its response body for %s\nHeader:     %s\nCalculated: %s", releaseVersion, ghcrDigest, calculatedDigest)
	}
	if dockerHubDocument.Digest != ghcrDigest {
		return cli.Exit(1, "registry digest mismatch for %s\nDocker Hub: %s\nGHCR:       %s", releaseVersion, dockerHubDocument.Digest, ghcrDigest)
	}

	if _, err := fmt.Fprintf(stdout, "published_release=%s\nmanifest_digest=%s\n", releaseVersion, dockerHubDocument.Digest); err != nil {
		return cli.Exit(1, "unable to write published release metadata: %v", err)
	}
	return nil
}

func validateManifestBody(body []byte) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return fmt.Errorf("response body is empty")
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(body, &document); err != nil {
		return fmt.Errorf("response body is not valid JSON: %w", err)
	}
	if document == nil {
		return fmt.Errorf("response body is not a JSON object")
	}
	return nil
}

func contentDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return fmt.Sprintf("sha256:%x", sum)
}

type configuration struct {
	Client           *http.Client
	DockerHubBaseURL string
	GHCRBaseURL      string
	GHCRTokenURL     string
	Image            string
	Attempts         int
	RequestTimeout   time.Duration
	RetryDelay       func(int) time.Duration
	Sleep            SleepFunc
	Now              func() time.Time
	MaxRetryAfter    time.Duration
	ownedClient      bool
}

func withDefaults(options Options) configuration {
	client := options.Client
	ownedClient := false
	if client == nil {
		client = secureHTTPClient()
		ownedClient = true
	}
	dockerHubBaseURL := options.DockerHubBaseURL
	if dockerHubBaseURL == "" {
		dockerHubBaseURL = "https://hub.docker.com"
	}
	ghcrBaseURL := options.GHCRBaseURL
	if ghcrBaseURL == "" {
		ghcrBaseURL = "https://ghcr.io"
	}
	ghcrTokenEndpoint := options.GHCRTokenURL
	if ghcrTokenEndpoint == "" {
		ghcrTokenEndpoint = "https://ghcr.io/token"
	}
	image := options.Image
	if image == "" {
		image = defaultImage
	}
	attempts := options.Attempts
	if attempts <= 0 {
		attempts = defaultAttempts
	}
	requestTimeout := options.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = defaultRequestTimeout
	}
	retryDelay := options.RetryDelay
	if retryDelay == nil {
		retryDelay = func(retry int) time.Duration {
			return time.Duration(1<<min(retry-1, 5)) * time.Second
		}
	}
	sleep := options.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	return configuration{
		Client:           client,
		DockerHubBaseURL: dockerHubBaseURL,
		GHCRBaseURL:      ghcrBaseURL,
		GHCRTokenURL:     ghcrTokenEndpoint,
		Image:            image,
		Attempts:         attempts,
		RequestTimeout:   requestTimeout,
		RetryDelay:       retryDelay,
		Sleep:            sleep,
		Now:              valueOrDefault(options.Now, time.Now),
		MaxRetryAfter:    durationOrDefault(options.MaxRetryAfter, 60*time.Second),
		ownedClient:      ownedClient,
	}
}

type responseSnapshot struct {
	Header http.Header
	Body   []byte
}

func requestWithRetry(ctx context.Context, configuration configuration, requestURL string, headers http.Header) (responseSnapshot, error) {
	var lastErr error
	for attempt := 1; attempt <= configuration.Attempts; attempt++ {
		requestCtx, cancel := context.WithTimeout(ctx, configuration.RequestTimeout)
		req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, requestURL, nil)
		if err == nil {
			for name, values := range headers {
				for _, value := range values {
					req.Header.Add(name, value)
				}
			}
			var response *http.Response
			response, err = configuration.Client.Do(req)
			if err == nil {
				if response.StatusCode >= http.StatusBadRequest {
					drainAndClose(response.Body)
					err = &httpStatusError{statusCode: response.StatusCode, retryAfter: response.Header.Get("Retry-After")}
				} else {
					body, readErr := readAndClose(response.Body)
					if readErr != nil {
						err = readErr
					} else {
						cancel()
						return responseSnapshot{Header: response.Header.Clone(), Body: body}, nil
					}
				}
			}
		}
		cancel()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return responseSnapshot{}, ctxErr
		}
		lastErr = err
		if attempt == configuration.Attempts {
			break
		}
		delay := configuration.RetryDelay(attempt)
		var statusErr *httpStatusError
		if errors.As(err, &statusErr) {
			if retryAfter, ok := parseRetryAfter(statusErr.retryAfter, configuration.Now(), configuration.MaxRetryAfter); ok {
				delay = retryAfter
			}
		}
		if err := configuration.Sleep(ctx, delay); err != nil {
			return responseSnapshot{}, err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("request failed")
	}
	return responseSnapshot{}, lastErr
}

type httpStatusError struct {
	statusCode int
	retryAfter string
}

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP %d", e.statusCode) }

func parseRetryAfter(value string, now time.Time, maximum time.Duration) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		delay := time.Duration(seconds) * time.Second
		if seconds > int64(^uint64(0)>>1)/int64(time.Second) {
			delay = maximum
		}
		if maximum > 0 && delay > maximum {
			delay = maximum
		}
		return delay, true
	}
	date, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := date.Sub(now)
	if delay < 0 {
		delay = 0
	}
	if maximum > 0 && delay > maximum {
		delay = maximum
	}
	return delay, true
}

func requestExitCode(err error) int {
	if errors.Is(err, context.DeadlineExceeded) {
		return 28
	}
	var statusErr *httpStatusError
	if errors.As(err, &statusErr) {
		return 22
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return 6
	}
	var networkErr *net.OpError
	if errors.As(err, &networkErr) {
		return 7
	}
	return 1
}

func readAndClose(body io.ReadCloser) ([]byte, error) {
	defer body.Close()
	payload, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > maxResponseBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", maxResponseBytes)
	}
	return payload, nil
}

func drainAndClose(body io.ReadCloser) {
	defer body.Close()
	_, _ = io.CopyN(io.Discard, body, maxResponseBytes)
}

func valueOrDefault[T any](value func() T, fallback func() T) func() T {
	if value != nil {
		return value
	}
	return fallback
}

func durationOrDefault(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

func dockerHubTagURL(baseURL, image, releaseVersion string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	parsed.Path = path.Join(parsed.Path, "v2/repositories", image, "tags", releaseVersion)
	return parsed.String(), nil
}

func ghcrTokenURL(endpoint, image string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Set("service", "ghcr.io")
	query.Set("scope", "repository:"+image+":pull")
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func ghcrManifestURL(baseURL, image, releaseVersion string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	parsed.Path = path.Join(parsed.Path, "v2", image, "manifests", releaseVersion)
	return parsed.String(), nil
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func secureHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
