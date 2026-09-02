// Package release validates the coordinated release metadata used by the
// docker-sqlite-wordpress project. It replaces validate-release.sh.
package release

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/soulteary/ci-recipes/internal/cli"
)

const (
	usage                 = "usage: validate-release RELEASE_VERSION [--verify-upstream]"
	defaultRequestTimeout = 60 * time.Second
	maxTokenResponse      = 1 << 20
	maxManifestResponse   = 16 << 20
)

var (
	releasePattern      = regexp.MustCompile(`^[0-9]{4}\.(0[1-9]|1[0-2])\.(0[1-9]|[12][0-9]|3[01])-r[1-9][0-9]*$`)
	stableVersion       = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	fullCommit          = regexp.MustCompile(`^[0-9a-f]{40}$`)
	pinnedOfficialImage = regexp.MustCompile(`^wordpress:[^@\s]+@sha256:[0-9a-f]{64}$`)
	pinnedDigest        = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	composeBuild        = regexp.MustCompile(`(?m)^[ \t]+build:$`)
)

// DigestResolver resolves an image tag to its registry manifest digest.
type DigestResolver interface {
	Resolve(context.Context, string) (string, error)
}

// ResolveFunc adapts a function into a DigestResolver.
type ResolveFunc func(context.Context, string) (string, error)

func (f ResolveFunc) Resolve(ctx context.Context, image string) (string, error) {
	return f(ctx, image)
}

// SleepFunc is injectable so retry behavior can be tested without wall-clock
// delays.
type SleepFunc func(context.Context, time.Duration) error

// Options controls repository IO and upstream digest resolution.
type Options struct {
	FS                fs.FS
	Root              string
	Resolver          DigestResolver
	HTTPClient        *http.Client
	RegistryBaseURL   string
	RegistryTokenURL  string
	RegistryService   string
	ResolutionTimeout time.Duration
	Attempts          int
	RetryDelay        func(attempt int) time.Duration
	Sleep             SleepFunc
}

// Execute validates using the current working directory and production
// network defaults when --verify-upstream is supplied.
func Execute(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return ExecuteWithOptions(ctx, args, stdin, stdout, stderr, Options{})
}

// ExecuteWithOptions is Execute with injectable filesystem, resolver, clock,
// and HTTP dependencies.
func ExecuteWithOptions(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, options Options) error {
	_ = stdin
	_ = stderr
	if ctx == nil {
		return cli.Exit(1, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return cli.Exit(1, "release validation canceled: %v", err)
	}
	if stdout == nil {
		stdout = io.Discard
	}

	releaseVersion := ""
	if len(args) > 0 {
		releaseVersion = args[0]
	}
	if !releasePattern.MatchString(releaseVersion) {
		return cli.Exit(1, "release version must use YYYY.MM.DD-rN CalVer (for example, 2026.09.01-r1)")
	}
	releaseDate := releaseVersion[:10]
	parsedDate, err := time.Parse("2006.01.02", releaseDate)
	if err != nil || parsedDate.UTC().Format("2006.01.02") != releaseDate {
		return cli.Exit(1, "release version contains an invalid calendar date: %s", releaseDate)
	}

	verifyUpstream := false
	if len(args) > 1 {
		switch args[1] {
		case "":
		case "--verify-upstream":
			verifyUpstream = true
		default:
			return cli.Exit(1, usage)
		}
	}
	if len(args) > 2 {
		return cli.Exit(1, usage)
	}

	repositoryFS := options.FS
	if repositoryFS == nil {
		root := options.Root
		if root == "" {
			root = "."
		}
		repositoryFS = os.DirFS(root)
	}
	read := func(name string) ([]byte, error) {
		data, readErr := fs.ReadFile(repositoryFS, name)
		if readErr != nil {
			return nil, cli.Exit(1, "unable to read %s: %v", name, readErr)
		}
		return data, nil
	}

	dockerfileBytes, err := read("Dockerfile")
	if err != nil {
		return err
	}
	dockerfile := string(dockerfileBytes)
	wordpressImage := dockerArg(dockerfile, "WORDPRESS_IMAGE")
	if wordpressImage == "" {
		return cli.Exit(1, "WORDPRESS_IMAGE must be defined")
	}
	if got := len(regexp.MustCompile(`(?m)^FROM \$\{WORDPRESS_IMAGE\}.*$`).FindAllString(dockerfile, -1)); got != 2 {
		return cli.Exit(1, "expected exactly two stages based on WORDPRESS_IMAGE")
	}
	if !pinnedOfficialImage.MatchString(wordpressImage) {
		return cli.Exit(1, "WORDPRESS_IMAGE must pin an official image tag and sha256 digest")
	}

	separator := strings.LastIndex(wordpressImage, "@sha256:")
	baseImage := wordpressImage[:separator]
	wordpressVersion := dockerArg(dockerfile, "WORDPRESS_VERSION")
	if !stableVersion.MatchString(wordpressVersion) {
		return cli.Exit(1, "WORDPRESS_VERSION must be an exact stable version")
	}
	basePattern := regexp.MustCompile(`^wordpress:` + regexp.QuoteMeta(wordpressVersion) + `-php[0-9]+\.[0-9]+-apache$`)
	if !basePattern.MatchString(baseImage) {
		return cli.Exit(1, "base image %s does not match WordPress %s", baseImage, wordpressVersion)
	}

	imageVersion := dockerArg(dockerfile, "IMAGE_VERSION")
	if imageVersion != releaseVersion {
		if imageVersion == "" {
			imageVersion = "<missing>"
		}
		return cli.Exit(1, "Dockerfile IMAGE_VERSION %s does not match release %s", imageVersion, releaseVersion)
	}
	pinned := wordpressImage[separator+1:]

	resolvedDigest := ""
	if verifyUpstream {
		resolvedDigest, err = resolveWithRetry(ctx, baseImage, options)
		if err != nil {
			return err
		}
		if !pinnedDigest.MatchString(resolvedDigest) {
			return cli.Exit(1, "unable to read the registry digest for %s", baseImage)
		}
		if resolvedDigest != pinned {
			return cli.Exit(1, "%s resolves to %s, not pinned digest %s", baseImage, resolvedDigest, pinned)
		}
	}

	phpVersion := strings.TrimSuffix(strings.TrimPrefix(baseImage, "wordpress:"+wordpressVersion+"-php"), "-apache")
	pluginVersion := dockerArg(dockerfile, "SQLITE_DATABASE_INTEGRATION_VERSION")
	if !stableVersion.MatchString(pluginVersion) {
		return cli.Exit(1, "SQLite Database Integration must use a stable semantic version")
	}
	pluginCommit := dockerArg(dockerfile, "SQLITE_DATABASE_INTEGRATION_COMMIT")
	if !fullCommit.MatchString(pluginCommit) {
		return cli.Exit(1, "SQLite Database Integration must pin a full commit SHA")
	}
	rustToolchain := dockerArg(dockerfile, "RUST_TOOLCHAIN_VERSION")
	if !stableVersion.MatchString(rustToolchain) {
		return cli.Exit(1, "Rust toolchain must use an exact stable version")
	}

	readmeBytes, err := read("README.md")
	if err != nil {
		return err
	}
	composeBytes, err := read("docker-compose.yml")
	if err != nil {
		return err
	}
	licensesBytes, err := read("LICENSES.md")
	if err != nil {
		return err
	}
	versioningBytes, err := read("VERSIONING.md")
	if err != nil {
		return err
	}
	changelogBytes, err := read("CHANGELOG.md")
	if err != nil {
		return err
	}
	readme, compose := string(readmeBytes), string(composeBytes)

	if !strings.Contains(readme, "WordPress `"+wordpressVersion+"` on PHP "+phpVersion+"/Apache") {
		return cli.Exit(1, "README runtime version does not match %s", baseImage)
	}
	if !strings.Contains(readme, "`v"+pluginVersion+"`") {
		return cli.Exit(1, "README SQLite Database Integration version does not match %s", pluginVersion)
	}
	if !strings.Contains(readme, "soulteary/sqlite-wordpress:"+releaseVersion) {
		return cli.Exit(1, "README does not contain the %s image tag", releaseVersion)
	}
	if strings.Contains(readme, "<!-- release-availability: pending -->") {
		if !strings.Contains(compose, "image: sqlite-wordpress:main") {
			return cli.Exit(1, "pending release Compose configuration must use sqlite-wordpress:main")
		}
		if !composeBuild.MatchString(compose) {
			return cli.Exit(1, "pending release Compose configuration must build the current repository")
		}
	} else if !strings.Contains(compose, "image: soulteary/sqlite-wordpress:"+releaseVersion) {
		return cli.Exit(1, "docker-compose.yml does not use the %s image tag", releaseVersion)
	}

	checks := []struct {
		content string
		needle  string
		message string
	}{
		{dockerfile, `org.opencontainers.image.version="${IMAGE_VERSION}"`, "OCI image version must use IMAGE_VERSION"},
		{dockerfile, `org.opencontainers.image.revision="${IMAGE_REVISION}"`, "OCI image revision must use IMAGE_REVISION"},
		{dockerfile, `org.opencontainers.image.base.name="docker.io/library/` + baseImage + `"`, "OCI base image name does not match " + baseImage},
		{dockerfile, `org.opencontainers.image.base.digest="` + pinned + `"`, "OCI base image digest does not match " + pinned},
		{dockerfile, `org.opencontainers.image.licenses="Apache-2.0 AND GPL-2.0-or-later"`, "OCI image licenses do not match the documented project and bundled application licenses"},
		{string(licensesBytes), "Apache-2.0 AND GPL-2.0-or-later", "LICENSES.md does not document the OCI image license expression"},
		{string(versioningBytes), "YYYY.MM.DD-rN", "VERSIONING.md does not document the CalVer release format"},
		{string(changelogBytes), "## [" + releaseVersion + "]", "CHANGELOG.md does not contain a " + releaseVersion + " release section"},
		{dockerfile, "COPY sqlite-local-core-update.php", "Dockerfile does not package the local WordPress core update integration"},
	}
	for _, check := range checks {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return cli.Exit(1, "release validation canceled: %v", ctxErr)
		}
		if !strings.Contains(check.content, check.needle) {
			return cli.Exit(1, "%s", check.message)
		}
	}

	if _, err = fmt.Fprintf(stdout, "release_version=%s\nwordpress_version=%s\nwordpress_image=%s\nplugin_version=%s\nplugin_commit=%s\nrust_toolchain=%s\n",
		releaseVersion, wordpressVersion, wordpressImage, pluginVersion, pluginCommit, rustToolchain); err != nil {
		return cli.Exit(1, "unable to write release metadata: %v", err)
	}
	if verifyUpstream {
		if _, err = fmt.Fprintf(stdout, "wordpress_upstream_digest=%s\n", resolvedDigest); err != nil {
			return cli.Exit(1, "unable to write release metadata: %v", err)
		}
	}
	return nil
}

func dockerArg(dockerfile, name string) string {
	pattern := regexp.MustCompile(`(?m)^ARG ` + regexp.QuoteMeta(name) + `=([^[:space:]]+)$`)
	matches := pattern.FindAllStringSubmatch(dockerfile, -1)
	values := make([]string, 0, len(matches))
	for _, match := range matches {
		values = append(values, match[1])
	}
	return strings.Join(values, "\n")
}

func resolveWithRetry(ctx context.Context, image string, options Options) (string, error) {
	attempts := options.Attempts
	if attempts <= 0 {
		attempts = 3
	}
	timeout := options.ResolutionTimeout
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	delay := options.RetryDelay
	if delay == nil {
		delay = func(attempt int) time.Duration { return time.Duration(attempt*2) * time.Second }
	}
	sleep := options.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	resolver := options.Resolver
	if resolver == nil {
		client := options.HTTPClient
		if client == nil {
			client = secureHTTPClient()
			defer client.CloseIdleConnections()
		}
		resolver = &DockerRegistryResolver{
			Client:         client,
			RegistryURL:    options.RegistryBaseURL,
			TokenURL:       options.RegistryTokenURL,
			Service:        options.RegistryService,
			RequestTimeout: timeout,
		}
	}

	for attempt := 1; attempt <= attempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		digest, err := resolver.Resolve(attemptCtx, image)
		cancel()
		if err == nil {
			return digest, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", cli.Exit(1, "unable to resolve official image tag %s: %v", image, ctxErr)
		}
		if attempt == attempts {
			return "", cli.Exit(1, "unable to resolve official image tag %s", image)
		}
		if err = sleep(ctx, delay(attempt)); err != nil {
			return "", cli.Exit(1, "unable to resolve official image tag %s: %v", image, err)
		}
	}
	return "", cli.Exit(1, "unable to resolve official image tag %s", image)
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

// DockerRegistryResolver resolves the content digest of a public Docker
// Distribution manifest without spawning docker. Registry tokens are used
// only in Authorization headers and are never included in argv or logs.
type DockerRegistryResolver struct {
	Client         *http.Client
	RegistryURL    string
	TokenURL       string
	Service        string
	RequestTimeout time.Duration
}

// Resolve implements DigestResolver.
func (r *DockerRegistryResolver) Resolve(ctx context.Context, image string) (string, error) {
	repository, tag, ok := strings.Cut(image, ":")
	if !ok || repository == "" || tag == "" || strings.Contains(repository, "@") {
		return "", fmt.Errorf("invalid Docker Hub image tag %q", image)
	}
	if !strings.Contains(repository, "/") {
		repository = "library/" + repository
	}
	registryURL := r.RegistryURL
	if registryURL == "" {
		registryURL = "https://registry-1.docker.io"
	}
	tokenURL := r.TokenURL
	if tokenURL == "" {
		tokenURL = "https://auth.docker.io/token"
	}
	service := r.Service
	if service == "" {
		service = "registry.docker.io"
	}
	parsedTokenURL, err := url.Parse(tokenURL)
	if err != nil {
		return "", fmt.Errorf("parse registry token URL: %w", err)
	}
	query := parsedTokenURL.Query()
	query.Set("service", service)
	query.Set("scope", "repository:"+repository+":pull")
	parsedTokenURL.RawQuery = query.Encode()

	requestTimeout := r.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = defaultRequestTimeout
	}
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	client := r.Client
	if client == nil {
		client = secureHTTPClient()
		defer client.CloseIdleConnections()
	}
	tokenRequest, err := http.NewRequestWithContext(requestCtx, http.MethodGet, parsedTokenURL.String(), nil)
	if err != nil {
		return "", err
	}
	tokenResponse, err := client.Do(tokenRequest)
	if err != nil {
		return "", err
	}
	tokenPayload, err := readResponse(tokenResponse, maxTokenResponse)
	if err != nil {
		return "", err
	}
	var tokenDocument struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(tokenPayload, &tokenDocument); err != nil {
		return "", fmt.Errorf("decode registry token: %w", err)
	}
	token := tokenDocument.Token
	if token == "" {
		token = tokenDocument.AccessToken
	}
	if token == "" {
		return "", fmt.Errorf("registry did not return a pull token")
	}

	parsedRegistryURL, err := url.Parse(registryURL)
	if err != nil {
		return "", fmt.Errorf("parse registry URL: %w", err)
	}
	parsedRegistryURL.Path = path.Join(parsedRegistryURL.Path, "v2", repository, "manifests", tag)
	manifestRequest, err := http.NewRequestWithContext(requestCtx, http.MethodGet, parsedRegistryURL.String(), nil)
	if err != nil {
		return "", err
	}
	manifestRequest.Header.Set("Authorization", "Bearer "+token)
	manifestRequest.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ", "))
	manifestRequest.Header.Set("Accept-Encoding", "identity")
	manifestResponse, err := client.Do(manifestRequest)
	if err != nil {
		return "", err
	}
	manifestPayload, err := readResponse(manifestResponse, maxManifestResponse)
	if err != nil {
		return "", err
	}
	computedDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(manifestPayload))
	reportedDigest := strings.TrimSpace(manifestResponse.Header.Get("Docker-Content-Digest"))
	if reportedDigest != "" && reportedDigest != computedDigest {
		return "", fmt.Errorf("registry manifest digest header does not match response content")
	}
	return computedDigest, nil
}

func readResponse(response *http.Response, maximum int64) ([]byte, error) {
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.CopyN(io.Discard, response.Body, maximum)
		return nil, fmt.Errorf("registry returned HTTP %d", response.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > maximum {
		return nil, fmt.Errorf("registry response exceeds %d bytes", maximum)
	}
	return payload, nil
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
