package release

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/soulteary/ci-recipes/internal/cli"
)

const (
	testVersion = "2026.09.02-r2"
	testDigest  = "sha256:d05574507fdb46ad9be0c12a86c54c5e0603c282ea2d967f939081baf9665c6d"
)

func validFS() fstest.MapFS {
	dockerfile := `ARG IMAGE_VERSION=` + testVersion + `
ARG IMAGE_REVISION=unknown
ARG WORDPRESS_VERSION=7.1.0
ARG WORDPRESS_IMAGE=wordpress:7.1.0-php8.5-apache@` + testDigest + `
ARG SQLITE_DATABASE_INTEGRATION_VERSION=3.0.1
ARG SQLITE_DATABASE_INTEGRATION_COMMIT=abf0dac137cf4e17866fea44b8a83d68b43792c4
ARG RUST_TOOLCHAIN_VERSION=1.98.0
FROM ${WORDPRESS_IMAGE} AS ext-builder
FROM ${WORDPRESS_IMAGE} AS runtime
LABEL org.opencontainers.image.version="${IMAGE_VERSION}"
LABEL org.opencontainers.image.revision="${IMAGE_REVISION}"
LABEL org.opencontainers.image.base.name="docker.io/library/wordpress:7.1.0-php8.5-apache"
LABEL org.opencontainers.image.base.digest="` + testDigest + `"
LABEL org.opencontainers.image.licenses="Apache-2.0 AND GPL-2.0-or-later"
COPY plugins/sqlite-local-core-update.php /tmp/
`
	return fstest.MapFS{
		"Dockerfile":         {Data: []byte(dockerfile)},
		"README.md":          {Data: []byte("WordPress `7.1.0` on PHP 8.5/Apache\nSQLite `v3.0.1`\nsoulteary/sqlite-wordpress:" + testVersion + "\n")},
		"docker-compose.yml": {Data: []byte("services:\n  wordpress:\n    image: soulteary/sqlite-wordpress:" + testVersion + "\n")},
		"LICENSES.md":        {Data: []byte("Apache-2.0 AND GPL-2.0-or-later\n")},
		"VERSIONING.md":      {Data: []byte("YYYY.MM.DD-rN\n")},
		"CHANGELOG.md":       {Data: []byte("## [" + testVersion + "] - 2026-09-02\n")},
	}
}

func cloneFS(source fstest.MapFS) fstest.MapFS {
	clone := make(fstest.MapFS, len(source))
	for name, file := range source {
		contents := append([]byte(nil), file.Data...)
		clone[name] = &fstest.MapFile{Data: contents, Mode: file.Mode, ModTime: file.ModTime, Sys: file.Sys}
	}
	return clone
}

func execute(t *testing.T, args []string, filesystem fs.FS, options Options) (string, error) {
	t.Helper()
	options.FS = filesystem
	var stdout bytes.Buffer
	err := ExecuteWithOptions(context.Background(), args, nil, &stdout, io.Discard, options)
	return stdout.String(), err
}

func TestExecuteValidRelease(t *testing.T) {
	t.Parallel()
	stdout, err := execute(t, []string{testVersion}, validFS(), Options{})
	if err != nil {
		t.Fatalf("ExecuteWithOptions() error = %v", err)
	}
	want := "release_version=" + testVersion + "\n" +
		"wordpress_version=7.1.0\n" +
		"wordpress_image=wordpress:7.1.0-php8.5-apache@" + testDigest + "\n" +
		"plugin_version=3.0.1\n" +
		"plugin_commit=abf0dac137cf4e17866fea44b8a83d68b43792c4\n" +
		"rust_toolchain=1.98.0\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestExecuteLegacyRootPluginSource(t *testing.T) {
	t.Parallel()
	fixture := cloneFS(validFS())
	fixture["Dockerfile"].Data = []byte(strings.ReplaceAll(
		string(fixture["Dockerfile"].Data),
		"COPY plugins/sqlite-local-core-update.php",
		"COPY sqlite-local-core-update.php",
	))
	if _, err := execute(t, []string{testVersion}, fixture, Options{}); err != nil {
		t.Fatalf("legacy root plugin source error = %v", err)
	}
}

func TestExecuteArgumentValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing", want: "must use YYYY.MM.DD-rN"},
		{name: "semver", args: []string{"7.1.0"}, want: "must use YYYY.MM.DD-rN"},
		{name: "unpadded", args: []string{"2026.9.02-r1"}, want: "must use YYYY.MM.DD-rN"},
		{name: "revision zero", args: []string{"2026.09.02-r0"}, want: "must use YYYY.MM.DD-rN"},
		{name: "invalid date", args: []string{"2026.02.30-r1"}, want: "invalid calendar date"},
		{name: "unknown option", args: []string{testVersion, "--online"}, want: "usage:"},
		{name: "extra option", args: []string{testVersion, "", "extra"}, want: "usage:"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := execute(t, test.args, validFS(), Options{})
			if cli.ExitCode(err) != 1 || err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want exit 1 containing %q", err, test.want)
			}
		})
	}
}

func TestExecuteRepositoryValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		file   string
		mutate func(string) string
		want   string
	}{
		{name: "missing WordPress image", file: "Dockerfile", mutate: func(s string) string { return strings.ReplaceAll(s, "ARG WORDPRESS_IMAGE=", "ARG OTHER=") }, want: "WORDPRESS_IMAGE must be defined"},
		{name: "one base stage", file: "Dockerfile", mutate: func(s string) string { return strings.Replace(s, "FROM ${WORDPRESS_IMAGE} AS runtime\n", "", 1) }, want: "exactly two stages"},
		{name: "unpinned image", file: "Dockerfile", mutate: func(s string) string { return strings.ReplaceAll(s, "@"+testDigest, "") }, want: "must pin"},
		{name: "WordPress version", file: "Dockerfile", mutate: func(s string) string {
			return strings.Replace(s, "ARG WORDPRESS_VERSION=7.1.0", "ARG WORDPRESS_VERSION=7.1.0-rc1", 1)
		}, want: "exact stable version"},
		{name: "base image mismatch", file: "Dockerfile", mutate: func(s string) string {
			return strings.Replace(s, "ARG WORDPRESS_IMAGE=wordpress:7.1.0", "ARG WORDPRESS_IMAGE=wordpress:7.0.0", 1)
		}, want: "does not match WordPress"},
		{name: "image version mismatch", file: "Dockerfile", mutate: func(s string) string { return strings.Replace(s, testVersion, "2026.09.02-r1", 1) }, want: "does not match release"},
		{name: "plugin version", file: "Dockerfile", mutate: func(s string) string { return strings.Replace(s, "3.0.1", "3.0.1-rc1", 1) }, want: "stable semantic version"},
		{name: "plugin commit", file: "Dockerfile", mutate: func(s string) string {
			return strings.Replace(s, "abf0dac137cf4e17866fea44b8a83d68b43792c4", "deadbeef", 1)
		}, want: "full commit SHA"},
		{name: "Rust toolchain", file: "Dockerfile", mutate: func(s string) string {
			return strings.Replace(s, "ARG RUST_TOOLCHAIN_VERSION=1.98.0", "ARG RUST_TOOLCHAIN_VERSION=nightly", 1)
		}, want: "Rust toolchain"},
		{name: "runtime README", file: "README.md", mutate: func(s string) string { return strings.ReplaceAll(s, "PHP 8.5", "PHP 8.4") }, want: "README runtime version"},
		{name: "plugin README", file: "README.md", mutate: func(s string) string { return strings.ReplaceAll(s, "`v3.0.1`", "v3.0.1") }, want: "README SQLite Database Integration"},
		{name: "image README", file: "README.md", mutate: func(s string) string {
			return strings.ReplaceAll(s, "soulteary/sqlite-wordpress:"+testVersion, "sqlite-wordpress:main")
		}, want: "README does not contain"},
		{name: "compose tag", file: "docker-compose.yml", mutate: func(string) string { return "services: {}\n" }, want: "does not use"},
		{name: "OCI version label", file: "Dockerfile", mutate: func(s string) string {
			return strings.ReplaceAll(s, `org.opencontainers.image.version="${IMAGE_VERSION}"`, `org.opencontainers.image.version="latest"`)
		}, want: "OCI image version"},
		{name: "OCI revision label", file: "Dockerfile", mutate: func(s string) string {
			return strings.ReplaceAll(s, `org.opencontainers.image.revision="${IMAGE_REVISION}"`, `org.opencontainers.image.revision="unknown"`)
		}, want: "OCI image revision"},
		{name: "OCI base name", file: "Dockerfile", mutate: func(s string) string {
			return strings.ReplaceAll(s, "docker.io/library/wordpress:7.1.0-php8.5-apache", "wordpress:latest")
		}, want: "OCI base image name"},
		{name: "OCI base digest", file: "Dockerfile", mutate: func(s string) string {
			return strings.ReplaceAll(s, `org.opencontainers.image.base.digest="`+testDigest+`"`, `org.opencontainers.image.base.digest="sha256:unknown"`)
		}, want: "OCI base image digest"},
		{name: "license label", file: "Dockerfile", mutate: func(s string) string {
			return strings.ReplaceAll(s, `org.opencontainers.image.licenses="Apache-2.0 AND GPL-2.0-or-later"`, `org.opencontainers.image.licenses="Apache-2.0"`)
		}, want: "OCI image licenses"},
		{name: "license documentation", file: "LICENSES.md", mutate: func(string) string { return "Apache-2.0\n" }, want: "LICENSES.md"},
		{name: "versioning documentation", file: "VERSIONING.md", mutate: func(string) string { return "CalVer\n" }, want: "does not document"},
		{name: "changelog", file: "CHANGELOG.md", mutate: func(string) string { return "# Changelog\n" }, want: "release section"},
		{name: "core update integration", file: "Dockerfile", mutate: func(s string) string {
			return strings.ReplaceAll(s, "COPY plugins/sqlite-local-core-update.php", "COPY another.php")
		}, want: "does not package"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := cloneFS(validFS())
			fixture[test.file].Data = []byte(test.mutate(string(fixture[test.file].Data)))
			_, err := execute(t, []string{testVersion}, fixture, Options{})
			if cli.ExitCode(err) != 1 || err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want exit 1 containing %q", err, test.want)
			}
		})
	}
}

func TestExecutePendingRelease(t *testing.T) {
	t.Parallel()
	fixture := cloneFS(validFS())
	fixture["README.md"].Data = append(fixture["README.md"].Data, []byte("<!-- release-availability: pending -->\n")...)
	fixture["docker-compose.yml"].Data = []byte("services:\n  wordpress:\n    image: sqlite-wordpress:main\n    build:\n      context: .\n")
	if _, err := execute(t, []string{testVersion}, fixture, Options{}); err != nil {
		t.Fatalf("pending release error = %v", err)
	}

	withoutBuild := cloneFS(fixture)
	withoutBuild["docker-compose.yml"].Data = []byte("services:\n  wordpress:\n    image: sqlite-wordpress:main\n")
	if _, err := execute(t, []string{testVersion}, withoutBuild, Options{}); err == nil || !strings.Contains(err.Error(), "must build") {
		t.Fatalf("missing build error = %v", err)
	}

	withoutImage := cloneFS(fixture)
	withoutImage["docker-compose.yml"].Data = []byte("services:\n  wordpress:\n    image: another:main\n    build:\n")
	if _, err := execute(t, []string{testVersion}, withoutImage, Options{}); err == nil || !strings.Contains(err.Error(), "must use sqlite-wordpress:main") {
		t.Fatalf("missing pending image error = %v", err)
	}

	topLevelBuild := cloneFS(fixture)
	topLevelBuild["docker-compose.yml"].Data = []byte("services: {}\n\nbuild:\nimage: sqlite-wordpress:main\n")
	if _, err := execute(t, []string{testVersion}, topLevelBuild, Options{}); err == nil || !strings.Contains(err.Error(), "must build") {
		t.Fatalf("unindented build error = %v", err)
	}
}

type sequenceResolver struct {
	results []resolveResult
	calls   int
}

type resolveResult struct {
	digest string
	err    error
}

func (r *sequenceResolver) Resolve(context.Context, string) (string, error) {
	index := r.calls
	r.calls++
	if index >= len(r.results) {
		return "", errors.New("unexpected resolution")
	}
	return r.results[index].digest, r.results[index].err
}

func TestExecuteVerifyUpstream(t *testing.T) {
	t.Parallel()
	resolver := &sequenceResolver{results: []resolveResult{{err: errors.New("temporary")}, {digest: testDigest}}}
	var delays []time.Duration
	stdout, err := execute(t, []string{testVersion, "--verify-upstream"}, validFS(), Options{
		Resolver:   resolver,
		RetryDelay: func(int) time.Duration { return 7 * time.Millisecond },
		Sleep: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("upstream verification error = %v", err)
	}
	if resolver.calls != 2 || len(delays) != 1 || delays[0] != 7*time.Millisecond {
		t.Fatalf("calls=%d delays=%v", resolver.calls, delays)
	}
	if !strings.HasSuffix(stdout, "wordpress_upstream_digest="+testDigest+"\n") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestExecuteVerifyUpstreamFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		results  []resolveResult
		want     string
		wantCall int
	}{
		{name: "mismatch", results: []resolveResult{{digest: "sha256:" + strings.Repeat("0", 64)}}, want: "not pinned digest", wantCall: 1},
		{name: "malformed digest", results: []resolveResult{{digest: "latest"}}, want: "unable to read the registry digest", wantCall: 1},
		{name: "exhausted", results: []resolveResult{{err: errors.New("one")}, {err: errors.New("two")}, {err: errors.New("three")}}, want: "unable to resolve", wantCall: 3},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			resolver := &sequenceResolver{results: test.results}
			_, err := execute(t, []string{testVersion, "--verify-upstream"}, validFS(), Options{
				Resolver: resolver,
				Sleep:    func(context.Context, time.Duration) error { return nil },
			})
			if err == nil || !strings.Contains(err.Error(), test.want) || resolver.calls != test.wantCall {
				t.Fatalf("error=%v calls=%d, want %q and %d", err, resolver.calls, test.want, test.wantCall)
			}
		})
	}
}

func TestExecuteVerifyUpstreamCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	resolver := ResolveFunc(func(ctx context.Context, _ string) (string, error) {
		cancel()
		return "", ctx.Err()
	})
	err := ExecuteWithOptions(ctx, []string{testVersion, "--verify-upstream"}, nil, io.Discard, io.Discard, Options{FS: validFS(), Resolver: resolver})
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestExecuteMissingFileAndWriterFailure(t *testing.T) {
	t.Parallel()
	fixture := cloneFS(validFS())
	delete(fixture, "README.md")
	if _, err := execute(t, []string{testVersion}, fixture, Options{}); err == nil || !strings.Contains(err.Error(), "unable to read README.md") {
		t.Fatalf("missing file error = %v", err)
	}

	err := ExecuteWithOptions(context.Background(), []string{testVersion}, nil, failingWriter{}, io.Discard, Options{FS: validFS()})
	if err == nil || !strings.Contains(err.Error(), "unable to write release metadata") {
		t.Fatalf("writer error = %v", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestDockerRegistryResolver(t *testing.T) {
	t.Parallel()
	manifest := []byte(`{"schemaVersion":2,"manifests":[]}`)
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(manifest))
	var tokenPath, tokenQuery, manifestPath, authorization, acceptEncoding string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			tokenPath, tokenQuery = r.URL.Path, r.URL.RawQuery
			_, _ = io.WriteString(w, `{"token":"registry-token"}`)
		case "/v2/library/wordpress/manifests/7.1.0-php8.5-apache":
			manifestPath = r.URL.Path
			authorization = r.Header.Get("Authorization")
			acceptEncoding = r.Header.Get("Accept-Encoding")
			w.Header().Set("Docker-Content-Digest", digest)
			_, _ = w.Write(manifest)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	resolver := &DockerRegistryResolver{
		Client:         server.Client(),
		RegistryURL:    server.URL,
		TokenURL:       server.URL + "/token",
		Service:        "registry.test",
		RequestTimeout: time.Second,
	}
	gotDigest, err := resolver.Resolve(context.Background(), "wordpress:7.1.0-php8.5-apache")
	if err != nil || gotDigest != digest {
		t.Fatalf("Resolve() = %q, %v", gotDigest, err)
	}
	if tokenPath != "/token" || !strings.Contains(tokenQuery, "service=registry.test") || !strings.Contains(tokenQuery, "scope=repository%3Alibrary%2Fwordpress%3Apull") {
		t.Fatalf("token request = %q?%s", tokenPath, tokenQuery)
	}
	if manifestPath != "/v2/library/wordpress/manifests/7.1.0-php8.5-apache" {
		t.Fatalf("manifest path = %q", manifestPath)
	}
	if authorization != "Bearer registry-token" || acceptEncoding != "identity" {
		t.Fatalf("authorization=%q accept-encoding=%q", authorization, acceptEncoding)
	}
}

func TestDockerRegistryResolverFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		mode string
		want string
	}{
		{name: "token HTTP error", mode: "token-http", want: "HTTP 429"},
		{name: "malformed token", mode: "token-json", want: "decode registry token"},
		{name: "empty token", mode: "token-empty", want: "pull token"},
		{name: "manifest HTTP error", mode: "manifest-http", want: "HTTP 503"},
		{name: "manifest header mismatch", mode: "header-mismatch", want: "does not match"},
		{name: "oversized manifest", mode: "oversized", want: "exceeds"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			manifest := []byte(`{"schemaVersion":2}`)
			digest := fmt.Sprintf("sha256:%x", sha256.Sum256(manifest))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					switch test.mode {
					case "token-http":
						http.Error(w, "later", http.StatusTooManyRequests)
					case "token-json":
						_, _ = io.WriteString(w, `{`)
					case "token-empty":
						_, _ = io.WriteString(w, `{}`)
					default:
						_, _ = io.WriteString(w, `{"token":"do-not-log-me"}`)
					}
					return
				}
				if test.mode == "manifest-http" {
					http.Error(w, "later", http.StatusServiceUnavailable)
					return
				}
				if test.mode == "oversized" {
					_, _ = io.WriteString(w, strings.Repeat("x", maxManifestResponse+1))
					return
				}
				if test.mode == "header-mismatch" {
					w.Header().Set("Docker-Content-Digest", testDigest)
				} else {
					w.Header().Set("Docker-Content-Digest", digest)
				}
				_, _ = w.Write(manifest)
			}))
			defer server.Close()
			resolver := &DockerRegistryResolver{Client: server.Client(), RegistryURL: server.URL, TokenURL: server.URL + "/token", RequestTimeout: 10 * time.Second}
			_, err := resolver.Resolve(context.Background(), "wordpress:tag")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Resolve() error = %v, want %q", err, test.want)
			}
			if strings.Contains(err.Error(), "do-not-log-me") {
				t.Fatalf("Resolve() leaked token: %v", err)
			}
		})
	}
}

func TestDockerRegistryResolverTimeout(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	resolver := &DockerRegistryResolver{Client: server.Client(), RegistryURL: server.URL, TokenURL: server.URL + "/token", RequestTimeout: 20 * time.Millisecond}
	_, err := resolver.Resolve(context.Background(), "wordpress:tag")
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Resolve() error = %v", err)
	}
}
