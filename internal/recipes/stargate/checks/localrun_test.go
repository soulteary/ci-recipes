package checks

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/soulteary/ci-recipes/internal/cli"
)

type fakeCommandRunner struct {
	commands []Command
	goPath   bool
	run      func(Command) error
}

func (f *fakeCommandRunner) LookPath(name string) (string, error) {
	if name == "go" && f.goPath {
		return "/fake/go", nil
	}
	return "", errors.New("not found")
}

func (f *fakeCommandRunner) Run(_ context.Context, command Command) error {
	f.commands = append(f.commands, command)
	if f.run != nil {
		return f.run(command)
	}
	return nil
}

func mapGetenv(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func localFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestLocalPortCompatibility(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		environment string
		custom      string
		wantPort    string
		wantHost    string
		wantError   bool
	}{
		{name: "empty environment", wantPort: "8080", wantHost: "8080"},
		{name: "numeric environment", environment: "8080", wantPort: "8080", wantHost: "8080"},
		{name: "colon environment", environment: ":8080", wantPort: ":8080", wantHost: "8080"},
		{name: "numeric CLI", environment: ":9090", custom: "8080", wantPort: "8080", wantHost: "8080"},
		{name: "colon CLI", environment: "9090", custom: ":8080", wantPort: ":8080", wantHost: "8080"},
		{name: "nonnumeric", environment: "nope", wantError: true},
		{name: "out of range", custom: ":65536", wantError: true},
		{name: "zero", custom: "0", wantError: true},
	}
	for _, test := range cases {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config, err := buildLocalConfig(test.custom, mapGetenv(map[string]string{"PORT": test.environment}))
			if test.wantError {
				if err == nil {
					t.Fatalf("expected error, got %#v", config)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if config.port != test.wantPort || config.hostPort != test.wantHost || config.authHost != "localhost:"+test.wantHost || config.callbackAllowedHosts != "localhost:"+test.wantHost {
				t.Fatalf("config=%#v", config)
			}
		})
	}
}

func TestLocalRunExportsConfigurationAndInvokesGo(t *testing.T) {
	t.Parallel()
	root := localFixture(t)
	runner := &fakeCommandRunner{goPath: true}
	runner.run = func(command Command) error {
		if command.Name == "git" {
			if strings.Contains(strings.Join(command.Args, " "), "--short") {
				_, _ = io.WriteString(command.Stdout, "abc123\n")
			} else {
				_, _ = io.WriteString(command.Stdout, strings.Repeat("a", 40)+"\n")
			}
		}
		return nil
	}
	values := map[string]string{
		"PORT":           ":9090",
		"PASSWORDS":      "secret-password",
		"WARDEN_ENABLED": "true",
		"WARDEN_API_KEY": "secret-api-key",
	}
	var stdout bytes.Buffer
	err := executeWithOptions(context.Background(), []string{"local-run", "--port", ":8080"}, nil, &stdout, io.Discard, options{
		root:    root,
		runner:  runner,
		getenv:  mapGetenv(values),
		environ: func() []string { return []string{"PATH=/bin", "PORT=old", "PASSWORDS=old"} },
		now:     func() time.Time { return time.Date(2026, 9, 2, 3, 4, 5, 0, time.FixedZone("test", 8*60*60)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "secret-password") || strings.Contains(stdout.String(), "secret-api-key") {
		t.Fatalf("secret leaked to stdout: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "localhost:8080") || strings.Contains(stdout.String(), "localhost::8080") {
		t.Fatalf("bad launcher output: %q", stdout.String())
	}
	if len(runner.commands) != 3 {
		t.Fatalf("commands=%#v", runner.commands)
	}
	goCommand := runner.commands[2]
	if goCommand.Name != "go" || goCommand.Dir != filepath.Join(root, "src") || strings.Join(goCommand.Args[:2], " ") != "run -ldflags" {
		t.Fatalf("go command=%#v", goCommand)
	}
	environment := strings.Join(goCommand.Env, "\n")
	for _, want := range []string{"PORT=:8080", "AUTH_HOST=localhost:8080", "CALLBACK_ALLOWED_HOSTS=localhost:8080", "PASSWORDS=secret-password", "WARDEN_API_KEY=secret-api-key"} {
		if !strings.Contains(environment, want) {
			t.Errorf("environment missing %q:\n%s", want, environment)
		}
	}
}

func TestLocalRunRejectsMissingPortValue(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := executeWithOptions(context.Background(), []string{"start-local", "--port"}, nil, &stdout, &stderr, options{root: localFixture(t), runner: &fakeCommandRunner{goPath: true}})
	if err == nil || !strings.Contains(err.Error(), "--port 需要端口值") {
		t.Fatalf("err=%v", err)
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Fatalf("exit code = %d, want 2", got)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("local-run duplicated error output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestLocalHelpDoesNotRequireRepository(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	err := executeWithOptions(context.Background(), []string{"local-run", "--help"}, nil, &stdout, io.Discard, options{
		root: filepath.Join(t.TempDir(), "missing"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "--port") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

type localFailingWriter struct{ err error }

func (w localFailingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestLocalRunPropagatesOutputFailures(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("write failed")
	t.Run("help", func(t *testing.T) {
		err := executeWithOptions(context.Background(), []string{"local-run", "--help"}, nil, localFailingWriter{sentinel}, io.Discard, options{})
		if !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want write failure", err)
		}
	})
	t.Run("summary", func(t *testing.T) {
		err := executeWithOptions(context.Background(), []string{"local-run"}, nil, localFailingWriter{sentinel}, io.Discard, options{
			root:   localFixture(t),
			runner: &fakeCommandRunner{goPath: true},
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want write failure", err)
		}
	})
}
