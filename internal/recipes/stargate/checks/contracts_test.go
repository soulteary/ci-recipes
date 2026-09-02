package checks

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFixtureFile(t *testing.T, root, relative, value string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func makeContractFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFixtureFile(t, root, "src/internal/config/config.go", `package config
type EnvVariable struct { Name string }
var Example = EnvVariable{Name: "EXAMPLE"}
var envVariables = []*EnvVariable{
    &Example,
}
`)
	writeFixtureFile(t, root, "src/cmd/stargate/constants.go", `package main
const RouteExample = "/example"
const RouteHealth = "/health"
`)
	writeFixtureFile(t, root, "src/cmd/stargate/server.go", `package main
func setupRoutes() {
    app.Get(RouteExample, handler)
    app.Get(RouteHealth, handler)
}

// findAssetsPath finds assets.
`)
	writeFixtureFile(t, root, "README.md", "# Fixture\n")
	writeFixtureFile(t, root, "CHANGELOG.md", `# Changes

## [1.0.0] - 2026-09-02

### Breaking changes

- The official container now listens on `+"`8080`"+` rather than `+"`80`"+`.
- Request-header authentication requires PASSWORD_HEADER_AUTH_ENABLED=true.
`)
	writeFixtureFile(t, root, "docker-compose.yml", `services:
  one:
    networks:
      - traefik
  two:
    networks:
      - traefik
  three:
    networks:
      - traefik
    environment:
      - TRUSTED_PROXIES=172.30.0.0/24
    labels:
      - "traefik.docker.network=stargate-traefik"

networks:
  traefik:
    name: stargate-traefik
    ipam:
      config:
        - subnet: 172.30.0.0/24
`)
	writeFixtureFile(t, root, "docs/enUS/CONFIG.md", "# Config\n\n### `EXAMPLE`\n\n### `LOG_LEVEL`\n")
	writeFixtureFile(t, root, "docs/enUS/API.md", minimalAPIContract())
	writeFixtureFile(t, root, "docs/enUS/SECURITY.md", "DEBUG=true DEBUG=false HERALD_TEST_MODE debug_code POST /_send_verify_code\n")
	fixtureMigrationTerms := append([]string(nil), migrationTerms...)
	fixtureMigrationTerms[2] = "`WARDEN_OTP_ENABLED`"
	fixtureMigrationTerms[3] = "`WARDEN_OTP_SECRET_KEY`"
	migration := strings.Join(fixtureMigrationTerms, "\n") + "\n`WARDEN_ENABLED=true` and `WARDEN_URL`\n"
	deployment := "SESSION_STORAGE_ENABLED=false\nSESSION_STORAGE_ENABLED=true\nSESSION_STORAGE_REDIS_*\nSESSION_EXCHANGE_SECRET\n" + migration + strings.Join([]string{
		"stargate-traefik",
		"docker compose config",
		`export TRAEFIK_NETWORK_NAME="${TRAEFIK_NETWORK_NAME:-traefik}"`,
		`docker network inspect "$TRAEFIK_NETWORK_NAME"`,
		"--subnet 172.30.0.0/24",
		"export TRAEFIK_NETWORK_CIDR=172.30.0.0/24",
		`TRUSTED_PROXIES=${TRAEFIK_NETWORK_CIDR:?set TRAEFIK_NETWORK_CIDR from docker network inspect}`,
		`name: ${TRAEFIK_NETWORK_NAME:-traefik}`,
		"external: true",
		`traefik.docker.network=${TRAEFIK_NETWORK_NAME:-traefik}`,
	}, "\n") + "\n"
	writeFixtureFile(t, root, "docs/enUS/DEPLOYMENT.md", deployment)
	writeFixtureFile(t, root, "docs/enUS/MIGRATION_V1.md", migration)
	return root
}

func minimalAPIContract() string {
	return `# API

The compatibility endpoint is ` + "`GET /health`" + `.
The logger also accepts ` + "`PUT /log/level`" + ` and ` + "`POST /log/level`" + `.

### ` + "`GET /example`" + `

Example.

### ` + "`GET /log/level`" + `

Logger.

### ` + "`POST /totp/enroll/confirm`" + `

` + "`enroll_id` `code` `401 Unauthorized` 10" + `

<!-- api-contract: totp-enroll-confirm-request-body -->
#### Request body

| Media type | Supported |
| --- | --- |
| ` + "`application/x-www-form-urlencoded`" + ` | ✅ |
| ` + "`multipart/form-data`" + ` | ✅ |
| ` + "`application/json`" + ` | ❌ |

### ` + "`POST /_login`" + `

| Field | Meaning |
| --- | --- |
| ` + "`auth_method`" + ` | password |
| ` + "`auth_method`" + ` | warden |
| ` + "`callback`" + ` | password callback |
| ` + "`callback`" + ` | warden callback |
| ` + "`password`" + ` | value |
| ` + "`phone`" + ` | value |
| ` + "`mail`" + ` | value |
| ` + "`challenge_id`" + ` | value |
| ` + "`verify_code`" + ` | value |
| ` + "`use_otp`" + ` | value |
| ` + "`otp_code`" + ` | value |

` + "`phone` + `mail` `HERALD_TOTP_ENABLED=true`" + `
auth_method=password&password=yourpassword
auth_method=warden&mail=user@example.com&challenge_id=ch_xxx&verify_code=123456&callback=app.example.com
auth_method=warden&mail=user@example.com&use_otp=true&otp_code=123456&callback=app.example.com
` + "`400 Bad Request` `401 Unauthorized` `502 Bad Gateway` `503 Service Unavailable` `500 Internal Server Error`" + `

### ` + "`POST /_send_verify_code`" + `

` + "`deliver_via` `deliver_via=dingtalk` `phone` + `mail` `401 Unauthorized` `503 Service Unavailable`" + `

<!-- api-contract: send-verify-code-request-body -->
#### Request body

| Media type | Supported |
| --- | --- |
| ` + "`application/x-www-form-urlencoded`" + ` | ✅ |
| ` + "`multipart/form-data`" + ` | ✅ |
| ` + "`application/json`" + ` | ❌ |

### ` + "`GET /totp/enroll`" + `

10 ` + "`/_login` `302 Found` `401 Unauthorized`" + `

### ` + "`POST /totp/enroll`" + `

10 ` + "`/_login` `302 Found` `401 Unauthorized`" + `

### ` + "`GET /totp/revoke`" + `

` + "`/_login` `302 Found`" + `

### ` + "`POST /totp/revoke`" + `

` + "`password` `code` `401 Unauthorized`" + `
`
}

func tarDirectory(t *testing.T, root string) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		header := &tar.Header{Name: filepath.ToSlash(relative), Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		_, err = writer.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestDocContractsStrictAndBaseComparison(t *testing.T) {
	root := makeContractFixture(t)
	var stdout, stderr strings.Builder
	if err := executeDocContracts(context.Background(), root, "", &stdout, &stderr, &fakeCommandRunner{}); err != nil {
		t.Fatalf("valid fixture failed: %v\nstderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "passed") {
		t.Fatalf("stdout=%q", stdout.String())
	}

	baseArchive := tarDirectory(t, root)
	readme := filepath.Join(root, "README.md")
	file, err := os.OpenFile(readme, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, "\nRequires Go 1.26+.\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	runner := &fakeCommandRunner{run: func(command Command) error {
		if command.Name == "git" {
			_, err := command.Stdout.Write(baseArchive)
			return err
		}
		return nil
	}}
	stdout.Reset()
	stderr.Reset()
	err = executeDocContracts(context.Background(), root, "base", &stdout, &stderr, runner)
	if err == nil || !strings.Contains(err.Error(), "increased") {
		t.Fatalf("err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "base=0 current=1") || !strings.Contains(stderr.String(), "stale Go 1.26 requirement") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestDocContractsStopsAfterMarkdownStructureFailure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFixtureFile(t, root, "broken.md", "```text\nunclosed\n")
	var stderr strings.Builder
	err := executeDocContracts(context.Background(), root, "", io.Discard, &stderr, &fakeCommandRunner{})
	if err == nil || !strings.Contains(err.Error(), "Markdown structure") {
		t.Fatalf("err=%v stderr=%q", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Unclosed fenced code block") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestRepositoryContractMutations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		path        string
		old         string
		replacement string
		appendText  string
		want        string
	}{
		{name: "compatibility route is exact", path: "docs/enUS/API.md", old: "`GET /health`", replacement: "`GET /healthz`", want: "compatibility route"},
		{name: "runtime route needs heading", path: "docs/enUS/API.md", old: "### `GET /example`", replacement: "The `GET /example` route is available.", want: "Missing route heading"},
		{name: "unsafe reordered htpasswd argument", path: "docs/enUS/CONFIG.md", appendText: "\nhtpasswd -C 10 -bn \"\" password\n", want: "unsafe htpasswd"},
		{name: "container port migration", path: "CHANGELOG.md", old: "- The official container now listens on `8080` rather than `80`.\n", replacement: "", want: "container port"},
		{name: "v1 heading must start a line", path: "CHANGELOG.md", old: "## [1.0.0]", replacement: "prefix ## [1.0.0]", want: "Breaking changes section"},
		{name: "breaking heading must be exact", path: "CHANGELOG.md", old: "### Breaking changes", replacement: "### Breaking changes obsolete", want: "Breaking changes section"},
		{name: "session storage scope", path: "docs/enUS/DEPLOYMENT.md", old: "SESSION_STORAGE_ENABLED=false", replacement: "SESSION_STORAGE_IN_MEMORY=unknown", want: "session-storage"},
		{name: "migration output stays private", path: "docs/enUS/DEPLOYMENT.md", old: "umask 077", replacement: "umask 022", want: "safe v1 environment migration"},
		{name: "retired Warden setting is active", path: "docs/enUS/MIGRATION_V1.md", appendText: "\nWARDEN_OTP_ENABLED=true\n", want: "retired Warden"},
		{name: "TOTP field stays scoped", path: "docs/enUS/API.md", old: "`enroll_id` `code`", replacement: "`enrollment_token` `code`", want: "TOTP confirmation"},
		{name: "request marker stays next to heading", path: "docs/enUS/API.md", old: "<!-- api-contract: totp-enroll-confirm-request-body -->\n#### Request body", replacement: "<!-- api-contract: totp-enroll-confirm-request-body -->\nmisplaced\n#### Request body", want: "TOTP confirmation"},
		{name: "JSON request remains unsupported", path: "docs/enUS/API.md", old: "| `application/json` | ❌ |", replacement: "| `application/json` | ✅ |", want: "form or"},
		{name: "Compose logical network key", path: "docker-compose.yml", old: "networks:\n  traefik:\n    name: stargate-traefik", replacement: "networks:\n  gateway:\n    name: stargate-traefik", want: "logical key"},
		{name: "external network is inspected", path: "docs/enUS/DEPLOYMENT.md", old: `docker network inspect "$TRAEFIK_NETWORK_NAME"`, replacement: `docker network show "$TRAEFIK_NETWORK_NAME"`, want: "network inspection"},
		{name: "documented setting is registered", path: "docs/enUS/CONFIG.md", appendText: "\n### `NOT_REGISTERED`\n", want: "not registered"},
		{name: "Compose bcrypt spacing", path: "docs/enUS/CONFIG.md", appendText: "\n-   PASSWORDS=bcrypt:$2y$unsafe\n", want: "unescaped Compose bcrypt"},
		{name: "header auth spacing", path: "docs/enUS/CONFIG.md", appendText: "\n  curl   -H \"Stargate-Password: secret\" http://localhost\n", want: "header-auth"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := makeContractFixture(t)
			path := filepath.Join(root, filepath.FromSlash(test.path))
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			if test.old != "" {
				if !strings.Contains(text, test.old) {
					t.Fatalf("fixture %s does not contain %q", test.path, test.old)
				}
				text = strings.Replace(text, test.old, test.replacement, 1)
			}
			text += test.appendText
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			violations, err := contractViolations(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if !containsDiagnostic(violations, test.want) {
				t.Fatalf("violations=%v; want fragment %q", violations, test.want)
			}
		})
	}
}
