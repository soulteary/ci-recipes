package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/soulteary/ci-recipes/internal/cli"
)

type publicCommandSpec struct {
	source      string
	name        string
	usage       string
	minimumArgs int
}

var publicCommandSpecs = []publicCommandSpec{
	{"stargate", "check-doc-contracts", "[BASE_SHA]", 0},
	{"stargate", "check-go-version-contract", "", 0},
	{"stargate", "check-release-workflow", "", 0},
	{"stargate", "local-run", "[-port PORT]", 0},
	{"stargate", "extract-release-notes", "TAG OUTPUT [CHANGELOG]", 2},
	{"stargate", "prepare-release-notes", "TAG OUTPUT [CHANGELOG]", 2},
	{"stargate", "plan-release-aliases", "TAG RELEASE_TAGS_FILE", 2},
	{"stargate", "publish-github-release", "TAG NOTES_FILE DIST_DIR", 3},
	{"stargate", "reconcile-release-aliases", "TAG IMAGE", 2},
	{"docker-sqlite-wordpress", "validate-buildx-evidence", "SPDX|SLSA PLATFORMS_JSON", 2},
	{"docker-sqlite-wordpress", "validate-release", "RELEASE_VERSION [--verify-upstream]", 1},
	{"docker-sqlite-wordpress", "verify-cosign-signature", "IMAGE IDENTITY ISSUER", 3},
	{"docker-sqlite-wordpress", "verify-published-release", "RELEASE_VERSION", 1},
	{"error-tracer", "build-release", "VERSION [OUTPUT_DIR]", 1},
	{"grantseal", "check-archive-allowlist", "[DIR]|--image REF|--manifest FILE [BASE]", 0},
	{"grantseal", "check-sensitive-files", "[PATH ...]", 0},
	{"grantseal", "check-patch-coverage", "PROFILE [BASE_REF] [THRESHOLD]", 1},
	{"grantseal", "check-doc-language", "[ROOT] [--allowlist FILE]", 0},
	{"grantseal", "check-doc-consistency", "[ROOT]", 0},
	{"grantseal", "check-writeback-allowlist", "", 0},
	{"grantseal", "inject-report-environment", "[REPORT_JSON]", 0},
	{"grantseal", "generate-quality-docs", "[REPO_ROOT]", 0},
}

func TestHelpListsOnlyActiveSources(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	if err := Execute(context.Background(), nil, nil, &output, nil); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, expected := range []string{"stargate:", "docker-sqlite-wordpress:", "error-tracer:", "grantseal:"} {
		if !strings.Contains(text, expected) {
			t.Errorf("help missing %q", expected)
		}
	}
	for _, excluded := range []string{"apt-proxy:", "webhook:"} {
		if strings.Contains(text, excluded) {
			t.Errorf("help unexpectedly includes inactive source %q", excluded)
		}
	}
}

func TestVersion(t *testing.T) {
	oldVersion, oldCommit, oldBuiltAt := Version, Commit, BuiltAt
	t.Cleanup(func() { Version, Commit, BuiltAt = oldVersion, oldCommit, oldBuiltAt })
	Version, Commit, BuiltAt = "1.2.3", "abcdef", "2026-09-02T00:00:00Z"
	var output bytes.Buffer
	if err := Execute(context.Background(), []string{"version"}, nil, &output, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "ci-recipes 1.2.3 (commit abcdef, built 2026-09-02T00:00:00Z)\n"; got != want {
		t.Fatalf("version = %q, want %q", got, want)
	}
}

func TestUnknownRecipeIsUsageError(t *testing.T) {
	t.Parallel()
	err := Execute(context.Background(), []string{"stargate", "missing"}, nil, nil, nil)
	if got := cli.ExitCode(err); got != 2 {
		t.Fatalf("exit code = %d, want 2", got)
	}
}

func TestEveryPublicCommandDispatchesAndAdvertisesUsage(t *testing.T) {
	var help bytes.Buffer
	if err := Execute(context.Background(), []string{"help"}, nil, &help, nil); err != nil {
		t.Fatal(err)
	}

	registered := make(map[string]command, len(commands))
	for _, candidate := range commands {
		key := candidate.source + "\x00" + candidate.name
		if _, exists := registered[key]; exists {
			t.Fatalf("duplicate public command %s %s", candidate.source, candidate.name)
		}
		registered[key] = candidate
	}
	if len(registered) != len(publicCommandSpecs) {
		t.Fatalf("registered command count = %d, want %d", len(registered), len(publicCommandSpecs))
	}

	for _, spec := range publicCommandSpecs {
		spec := spec
		t.Run(spec.source+"/"+spec.name, func(t *testing.T) {
			key := spec.source + "\x00" + spec.name
			candidate, exists := registered[key]
			if !exists {
				t.Fatalf("public command is not registered")
			}
			if candidate.usage != spec.usage {
				t.Fatalf("usage = %q, want %q", candidate.usage, spec.usage)
			}
			if candidate.minimumArgs != spec.minimumArgs {
				t.Fatalf("minimum arguments = %d, want %d", candidate.minimumArgs, spec.minimumArgs)
			}
			if !strings.Contains(help.String(), candidate.invocation()) {
				t.Fatalf("help does not list %q", candidate.invocation())
			}

			registry := append([]command(nil), commands...)
			arguments := make([]string, spec.minimumArgs)
			for index := range arguments {
				arguments[index] = "argument"
			}
			stdin := strings.NewReader("input")
			var stdout, stderr bytes.Buffer
			ctx := context.WithValue(context.Background(), struct{}{}, spec.name)
			sentinel := errors.New("dispatched")
			called := false
			for index := range registry {
				if registry[index].source != spec.source || registry[index].name != spec.name {
					continue
				}
				registry[index].execute = func(gotContext context.Context, gotArgs []string, gotStdin io.Reader, gotStdout, gotStderr io.Writer) error {
					called = true
					if gotContext != ctx || gotStdin != stdin || gotStdout != &stdout || gotStderr != &stderr {
						t.Error("dispatcher did not preserve context or streams")
					}
					if !slices.Equal(gotArgs, arguments) {
						t.Errorf("arguments = %#v, want %#v", gotArgs, arguments)
					}
					return sentinel
				}
			}
			invocationArgs := append([]string{spec.source, spec.name}, arguments...)
			err := executeWithCommands(ctx, invocationArgs, stdin, &stdout, &stderr, registry)
			if !errors.Is(err, sentinel) || !called {
				t.Fatalf("dispatch error = %v, called = %t", err, called)
			}
		})
	}
}

func TestRequiredArgumentsAreUsageErrors(t *testing.T) {
	for _, spec := range publicCommandSpecs {
		for supplied := 0; supplied < spec.minimumArgs; supplied++ {
			name := spec.source + "/" + spec.name + "/supplied-" + strings.Repeat("x", supplied)
			t.Run(name, func(t *testing.T) {
				arguments := []string{spec.source, spec.name}
				for range supplied {
					arguments = append(arguments, "argument")
				}
				var stdout, stderr bytes.Buffer
				err := Execute(context.Background(), arguments, nil, &stdout, &stderr)
				if got := cli.ExitCode(err); got != 2 {
					t.Fatalf("exit code = %d, want 2 (error: %v)", got, err)
				}
				wantUsage := "usage: ci-recipes " + spec.source + " " + spec.name
				if err == nil || !strings.Contains(err.Error(), wantUsage) {
					t.Fatalf("error = %v, want %q", err, wantUsage)
				}
				if stdout.Len() != 0 || stderr.Len() != 0 {
					t.Fatalf("usage validation wrote stdout=%q stderr=%q", stdout.String(), stderr.String())
				}
			})
		}
	}
}

func TestLocalRunUsageErrorIsReturnedWithoutDuplicateOutput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Execute(context.Background(), []string{"stargate", "local-run", "--port"}, nil, &stdout, &stderr)
	if got := cli.ExitCode(err); got != 2 {
		t.Fatalf("exit code = %d, want 2 (error: %v)", got, err)
	}
	if err == nil || !strings.Contains(err.Error(), "--port 需要端口值") {
		t.Fatalf("error = %v", err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("local-run duplicated error output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestTopLevelOutputErrorsArePropagated(t *testing.T) {
	sentinel := errors.New("write failed")
	for _, arguments := range [][]string{nil, {"version"}} {
		err := Execute(context.Background(), arguments, nil, failingWriter{sentinel}, nil)
		if !errors.Is(err, sentinel) {
			t.Errorf("Execute(%#v) error = %v, want write failure", arguments, err)
		}
	}
}
