package published

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/soulteary/ci-recipes/internal/cli"
)

const (
	testVersion  = "2026.09.02-r2"
	testManifest = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`
	testDigest   = "sha256:dff9de10919148711140d349bf03f1a99eb06f94b03e51715ccebfa7cdc518e2"
	testToken    = "secret-ghcr-pull-token"
)

type requestRecord struct {
	path           string
	rawQuery       string
	authorization  string
	accept         string
	acceptEncoding string
}

func successServer(t *testing.T, dockerDigest, ghcrDigest string) (*httptest.Server, *[]requestRecord) {
	return registryServer(t, dockerDigest, []string{ghcrDigest}, testManifest)
}

func registryServer(t *testing.T, dockerDigest string, ghcrDigests []string, manifestBody string) (*httptest.Server, *[]requestRecord) {
	t.Helper()
	var mu sync.Mutex
	records := make([]requestRecord, 0, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		records = append(records, requestRecord{
			path:           r.URL.Path,
			rawQuery:       r.URL.RawQuery,
			authorization:  r.Header.Get("Authorization"),
			accept:         r.Header.Get("Accept"),
			acceptEncoding: r.Header.Get("Accept-Encoding"),
		})
		mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/repositories/"):
			_, _ = io.WriteString(w, `{"digest":"`+dockerDigest+`"}`)
		case r.URL.Path == "/token":
			_, _ = io.WriteString(w, `{"token":"`+testToken+`"}`)
		case strings.HasPrefix(r.URL.Path, "/v2/soulteary/sqlite-wordpress/manifests/"):
			for _, digest := range ghcrDigests {
				w.Header().Add("Docker-Content-Digest", digest)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, manifestBody)
		default:
			http.NotFound(w, r)
		}
	}))
	return server, &records
}

func localOptions(server *httptest.Server) Options {
	return Options{
		Client:           server.Client(),
		DockerHubBaseURL: server.URL,
		GHCRBaseURL:      server.URL,
		GHCRTokenURL:     server.URL + "/token",
		RequestTimeout:   time.Second,
		Sleep:            func(context.Context, time.Duration) error { return nil },
	}
}

func TestExecuteSuccess(t *testing.T) {
	t.Parallel()
	server, records := successServer(t, testDigest, testDigest)
	defer server.Close()
	var stdout bytes.Buffer
	err := ExecuteWithOptions(context.Background(), []string{testVersion}, nil, &stdout, io.Discard, localOptions(server))
	if err != nil {
		t.Fatalf("ExecuteWithOptions() error = %v", err)
	}
	wantOutput := "published_release=" + testVersion + "\nmanifest_digest=" + testDigest + "\n"
	if stdout.String() != wantOutput {
		t.Fatalf("stdout = %q, want %q", stdout.String(), wantOutput)
	}
	if len(*records) != 3 {
		t.Fatalf("requests = %#v", *records)
	}
	if (*records)[0].path != "/v2/repositories/soulteary/sqlite-wordpress/tags/"+testVersion {
		t.Fatalf("Docker Hub path = %q", (*records)[0].path)
	}
	if !strings.Contains((*records)[1].rawQuery, "service=ghcr.io") || !strings.Contains((*records)[1].rawQuery, "scope=repository%3Asoulteary%2Fsqlite-wordpress%3Apull") {
		t.Fatalf("token query = %q", (*records)[1].rawQuery)
	}
	if (*records)[2].authorization != "Bearer "+testToken {
		t.Fatalf("authorization header = %q", (*records)[2].authorization)
	}
	if !strings.Contains((*records)[2].accept, "application/vnd.oci.image.index.v1+json") {
		t.Fatalf("Accept = %q", (*records)[2].accept)
	}
	if (*records)[2].acceptEncoding != "identity" {
		t.Fatalf("Accept-Encoding = %q, want identity", (*records)[2].acceptEncoding)
	}
}

func TestExecuteVersionCompatibility(t *testing.T) {
	t.Parallel()
	tests := []struct {
		version string
		valid   bool
	}{
		{version: "2026.09.02-r1", valid: true},
		// The shell script validates shape, not calendar correctness.
		{version: "2026.99.99-r1", valid: true},
		{version: "2026.09.02-r0"},
		{version: "2026.9.02-r1"},
		{version: "7.1.0"},
		{version: ""},
	}
	for _, test := range tests {
		test := test
		t.Run(test.version, func(t *testing.T) {
			t.Parallel()
			if test.valid {
				server, _ := successServer(t, testDigest, testDigest)
				defer server.Close()
				err := ExecuteWithOptions(context.Background(), []string{test.version}, nil, io.Discard, io.Discard, localOptions(server))
				if err != nil {
					t.Fatalf("valid version error = %v", err)
				}
				return
			}
			err := ExecuteWithOptions(context.Background(), []string{test.version}, nil, nil, nil, Options{})
			if cli.ExitCode(err) != 2 || err == nil || !strings.Contains(err.Error(), "usage:") {
				t.Fatalf("invalid version error = %v", err)
			}
		})
	}
}

func TestExecuteRejectsExtraArguments(t *testing.T) {
	t.Parallel()
	err := ExecuteWithOptions(context.Background(), []string{testVersion, "unexpected"}, nil, nil, nil, Options{})
	if cli.ExitCode(err) != 2 || err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("extra argument error = %v, exit = %d", err, cli.ExitCode(err))
	}
}

func TestExecuteResponseValidation(t *testing.T) {
	t.Parallel()
	differentDigest := "sha256:" + strings.Repeat("0", 64)
	tests := []struct {
		name         string
		dockerDigest string
		ghcrDigest   string
		want         string
		wantCode     int
	}{
		{name: "invalid Docker Hub digest", dockerDigest: "latest", ghcrDigest: testDigest, want: "Docker Hub did not return", wantCode: 4},
		{name: "invalid GHCR digest", dockerDigest: testDigest, ghcrDigest: "latest", want: "GHCR did not return", wantCode: 1},
		{name: "digest mismatch", dockerDigest: differentDigest, ghcrDigest: testDigest, want: "registry digest mismatch", wantCode: 1},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server, _ := successServer(t, test.dockerDigest, test.ghcrDigest)
			defer server.Close()
			err := ExecuteWithOptions(context.Background(), []string{testVersion}, nil, nil, nil, localOptions(server))
			if cli.ExitCode(err) != test.wantCode || err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestExecuteManifestIntegrity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		body    string
		headers []string
		want    string
	}{
		{name: "missing digest header", body: testManifest, want: "valid manifest digest"},
		{name: "malformed digest header", body: testManifest, headers: []string{"sha256:broken"}, want: "valid manifest digest"},
		{name: "multiple digest headers", body: testManifest, headers: []string{testDigest, testDigest}, want: "valid manifest digest"},
		{name: "empty body", body: "", headers: []string{contentDigest(nil)}, want: "response body is empty"},
		{name: "corrupt body", body: "{", headers: []string{contentDigest([]byte("{"))}, want: "not valid JSON"},
		{name: "body header mismatch", body: testManifest, headers: []string{"sha256:" + strings.Repeat("0", 64)}, want: "does not match its response body"},
		{name: "valid body", body: testManifest, headers: []string{testDigest}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server, _ := registryServer(t, testDigest, test.headers, test.body)
			defer server.Close()
			err := ExecuteWithOptions(context.Background(), []string{testVersion}, nil, io.Discard, io.Discard, localOptions(server))
			if test.want == "" {
				if err != nil {
					t.Fatalf("ExecuteWithOptions() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestExecuteInvalidTokenNeverLeaksIt(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/repositories/"):
			_, _ = io.WriteString(w, `{"digest":"`+testDigest+`"}`)
		case r.URL.Path == "/token":
			_, _ = io.WriteString(w, `{"token":"`+testToken+`"}`)
		default:
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
	}))
	defer server.Close()
	options := localOptions(server)
	options.Attempts = 1
	err := ExecuteWithOptions(context.Background(), []string{testVersion}, nil, nil, nil, options)
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error leaked bearer token: %v", err)
	}
}

func TestExecuteTokenValidation(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v2/repositories/") {
			_, _ = io.WriteString(w, `{"digest":"`+testDigest+`"}`)
			return
		}
		_, _ = io.WriteString(w, `{"token":""}`)
	}))
	defer server.Close()
	err := ExecuteWithOptions(context.Background(), []string{testVersion}, nil, nil, nil, localOptions(server))
	if err == nil || !strings.Contains(err.Error(), "valid pull token") {
		t.Fatalf("error = %v", err)
	}
}

func TestExecuteMalformedRegistryJSON(t *testing.T) {
	t.Parallel()
	for _, malformedAt := range []string{"dockerhub", "token"} {
		malformedAt := malformedAt
		t.Run(malformedAt, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/v2/repositories/") {
					if malformedAt == "dockerhub" {
						_, _ = io.WriteString(w, `{`)
					} else {
						_, _ = io.WriteString(w, `{"digest":"`+testDigest+`"}`)
					}
					return
				}
				_, _ = io.WriteString(w, `{`)
			}))
			defer server.Close()
			err := ExecuteWithOptions(context.Background(), []string{testVersion}, nil, nil, nil, localOptions(server))
			if cli.ExitCode(err) != 4 {
				t.Fatalf("error = %v, exit = %d", err, cli.ExitCode(err))
			}
		})
	}
}

func TestRequestRetries(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 3 {
			http.Error(w, "later", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	var delays []time.Duration
	configuration := withDefaults(Options{
		Client:         server.Client(),
		Attempts:       4,
		RequestTimeout: time.Second,
		RetryDelay:     func(retry int) time.Duration { return time.Duration(retry) * time.Millisecond },
		Sleep: func(_ context.Context, duration time.Duration) error {
			delays = append(delays, duration)
			return nil
		},
	})
	response, err := requestWithRetry(context.Background(), configuration, server.URL, nil)
	if err != nil || string(response.Body) != "ok" {
		t.Fatalf("requestWithRetry() = %q, %v", response.Body, err)
	}
	if attempts.Load() != 3 || len(delays) != 2 || delays[0] != time.Millisecond || delays[1] != 2*time.Millisecond {
		t.Fatalf("attempts=%d delays=%v", attempts.Load(), delays)
	}
}

func TestRequestHonorsBoundedRetryAfter(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "30")
			http.Error(w, "later", http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	var gotDelay time.Duration
	configuration := withDefaults(Options{
		Client:         server.Client(),
		Attempts:       2,
		RequestTimeout: time.Second,
		MaxRetryAfter:  5 * time.Second,
		RetryDelay:     func(int) time.Duration { return time.Millisecond },
		Sleep: func(_ context.Context, duration time.Duration) error {
			gotDelay = duration
			return nil
		},
	})
	response, err := requestWithRetry(context.Background(), configuration, server.URL, nil)
	if err != nil || string(response.Body) != "ok" || gotDelay != 5*time.Second {
		t.Fatalf("body=%q error=%v delay=%v", response.Body, err, gotDelay)
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		value string
		want  time.Duration
		ok    bool
	}{
		{value: "7", want: 7 * time.Second, ok: true},
		{value: "99", want: 10 * time.Second, ok: true},
		{value: now.Add(4 * time.Second).Format(http.TimeFormat), want: 4 * time.Second, ok: true},
		{value: now.Add(-time.Minute).Format(http.TimeFormat), want: 0, ok: true},
		{value: "not-a-date"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.value, func(t *testing.T) {
			t.Parallel()
			got, ok := parseRetryAfter(test.value, now, 10*time.Second)
			if got != test.want || ok != test.ok {
				t.Fatalf("parseRetryAfter() = %v, %v; want %v, %v", got, ok, test.want, test.ok)
			}
		})
	}
}

func TestRequestCancellationDuringBackoff(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "later", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	configuration := withDefaults(Options{
		Client:         server.Client(),
		Attempts:       2,
		RequestTimeout: time.Second,
		Sleep: func(ctx context.Context, _ time.Duration) error {
			cancel()
			<-ctx.Done()
			return ctx.Err()
		},
	})
	_, err := requestWithRetry(ctx, configuration, server.URL, nil)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("backoff cancellation error = %v", err)
	}
}

func TestRequestExitCode(t *testing.T) {
	t.Parallel()
	if got := requestExitCode(&httpStatusError{statusCode: 503}); got != 22 {
		t.Fatalf("HTTP exit = %d", got)
	}
	if got := requestExitCode(context.DeadlineExceeded); got != 28 {
		t.Fatalf("timeout exit = %d", got)
	}
	if got := requestExitCode(&net.DNSError{Err: "not found", Name: "registry.invalid"}); got != 6 {
		t.Fatalf("DNS exit = %d", got)
	}
}

func TestRequestTimeoutAndCancellation(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	configuration := withDefaults(Options{
		Client:         server.Client(),
		Attempts:       1,
		RequestTimeout: 15 * time.Millisecond,
	})
	_, err := requestWithRetry(context.Background(), configuration, server.URL, nil)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = requestWithRetry(ctx, configuration, server.URL, nil)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestRequestBodyLimit(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", maxResponseBytes+1))
	}))
	defer server.Close()
	configuration := withDefaults(Options{Client: server.Client(), Attempts: 1, RequestTimeout: time.Second})
	_, err := requestWithRetry(context.Background(), configuration, server.URL, nil)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("body limit error = %v", err)
	}
}

func TestSecureHTTPClient(t *testing.T) {
	t.Parallel()
	client := secureHTTPClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("transport TLS config = %#v", client.Transport)
	}
	if client.CheckRedirect == nil {
		t.Fatal("redirect policy is not set")
	}
}

func TestWriterFailure(t *testing.T) {
	t.Parallel()
	server, _ := successServer(t, testDigest, testDigest)
	defer server.Close()
	err := ExecuteWithOptions(context.Background(), []string{testVersion}, nil, failingWriter{}, nil, localOptions(server))
	if err == nil || !strings.Contains(err.Error(), "unable to write") {
		t.Fatalf("writer error = %v", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }
