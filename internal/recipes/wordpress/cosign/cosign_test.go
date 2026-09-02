package cosign

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/soulteary/ci-recipes/internal/cli"
)

const (
	testImage    = "soulteary/sqlite-wordpress@sha256:9716786a5213d89f0d77bbad5bd04723aad8791018d5a8811c5974df73eb40c1"
	testIdentity = "https://github.com/soulteary/docker-sqlite-wordpress/.github/workflows/release.yaml@refs/tags/2026.09.02-r2"
	testIssuer   = "https://token.actions.githubusercontent.com"
)

type statusError int

func (e statusError) Error() string { return "command failed" }
func (e statusError) ExitCode() int { return int(e) }

type runnerResult struct {
	stderr string
	err    error
}

type sequenceRunner struct {
	mu      sync.Mutex
	results []runnerResult
	calls   [][]string
}

func (r *sequenceRunner) Run(_ context.Context, name string, args []string, _, stderr io.Writer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	call := append([]string{name}, args...)
	r.calls = append(r.calls, call)
	result := r.results[len(r.calls)-1]
	_, _ = io.WriteString(stderr, result.stderr)
	return result.err
}

func lookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func TestExecuteRetriesUntilSuccess(t *testing.T) {
	t.Parallel()
	runner := &sequenceRunner{results: []runnerResult{
		{stderr: "Error: no signatures found\n", err: statusError(10)},
		{stderr: "ERROR: NO SIGNATURES FOUND\n", err: statusError(10)},
		{},
	}}
	var delays []time.Duration
	var stderr bytes.Buffer
	err := ExecuteWithOptions(context.Background(), []string{testImage, testIdentity, testIssuer, "ignored"}, nil, io.Discard, &stderr, Options{
		Runner:    runner,
		LookupEnv: lookup(map[string]string{"COSIGN_VERIFY_ATTEMPTS": "3", "COSIGN_VERIFY_DELAY_SECONDS": "7"}),
		Sleep: func(_ context.Context, duration time.Duration) error {
			delays = append(delays, duration)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("ExecuteWithOptions() error = %v", err)
	}
	if len(runner.calls) != 3 || !reflect.DeepEqual(delays, []time.Duration{7 * time.Second, 7 * time.Second}) {
		t.Fatalf("calls=%d delays=%v", len(runner.calls), delays)
	}
	wantArgs := []string{"cosign", "verify", "--certificate-identity", testIdentity, "--certificate-oidc-issuer", testIssuer, testImage}
	if !reflect.DeepEqual(runner.calls[0], wantArgs) {
		t.Fatalf("command = %q, want %q", runner.calls[0], wantArgs)
	}
	if strings.Count(stderr.String(), "retrying in 7s") != 2 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestExecuteFatalFailurePreservesStatus(t *testing.T) {
	t.Parallel()
	runner := &sequenceRunner{results: []runnerResult{{stderr: "Error: certificate identity mismatch\n", err: statusError(17)}}}
	err := ExecuteWithOptions(context.Background(), []string{testImage, testIdentity, testIssuer}, nil, nil, nil, Options{Runner: runner, LookupEnv: lookup(nil)})
	if cli.ExitCode(err) != 17 || err == nil || err.Error() != "Error: certificate identity mismatch" {
		t.Fatalf("error = %#v, exit = %d", err, cli.ExitCode(err))
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %d", len(runner.calls))
	}
}

func TestExecuteExhaustsMissingSignature(t *testing.T) {
	t.Parallel()
	runner := &sequenceRunner{results: []runnerResult{
		{stderr: "Error: no signatures found\n", err: statusError(10)},
		{stderr: "Error: no signatures found\n", err: statusError(10)},
	}}
	err := ExecuteWithOptions(context.Background(), []string{testImage, testIdentity, testIssuer}, nil, nil, io.Discard, Options{
		Runner:    runner,
		LookupEnv: lookup(map[string]string{"COSIGN_VERIFY_ATTEMPTS": "2", "COSIGN_VERIFY_DELAY_SECONDS": "0"}),
		Sleep:     func(context.Context, time.Duration) error { return nil },
	})
	if cli.ExitCode(err) != 10 || err == nil || !strings.Contains(err.Error(), "not visible after 2 attempts") || !strings.Contains(err.Error(), "no signatures found") {
		t.Fatalf("error = %v, exit = %d", err, cli.ExitCode(err))
	}
}

func TestExecuteBoundsStderrWhileStillDetectingRetryMarker(t *testing.T) {
	t.Parallel()
	oversized := strings.Repeat("x", maxVerifyLogBytes+128) + "no signatures found"
	runner := &sequenceRunner{results: []runnerResult{
		{stderr: oversized, err: statusError(10)},
		{},
	}}
	err := ExecuteWithOptions(context.Background(), []string{testImage, testIdentity, testIssuer}, nil, nil, io.Discard, Options{
		Runner:    runner,
		LookupEnv: lookup(map[string]string{"COSIGN_VERIFY_ATTEMPTS": "2", "COSIGN_VERIFY_DELAY_SECONDS": "0"}),
		Sleep:     func(context.Context, time.Duration) error { return nil },
	})
	if err != nil || len(runner.calls) != 2 {
		t.Fatalf("error=%v calls=%d", err, len(runner.calls))
	}

	log := newBoundedLog(32)
	_, _ = log.Write([]byte(strings.Repeat("z", 64)))
	if got := log.String(); len(got) > 80 || !strings.Contains(got, "truncated") {
		t.Fatalf("bounded log = %q", got)
	}
}

func TestExecuteArgumentAndEnvironmentValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		env  map[string]string
	}{
		{name: "missing image", args: []string{"", testIdentity, testIssuer}},
		{name: "missing identity", args: []string{testImage, "", testIssuer}},
		{name: "missing issuer", args: []string{testImage, testIdentity}},
		{name: "zero attempts", args: []string{testImage, testIdentity, testIssuer}, env: map[string]string{"COSIGN_VERIFY_ATTEMPTS": "0"}},
		{name: "negative delay", args: []string{testImage, testIdentity, testIssuer}, env: map[string]string{"COSIGN_VERIFY_DELAY_SECONDS": "-1"}},
		{name: "nonnumeric", args: []string{testImage, testIdentity, testIssuer}, env: map[string]string{"COSIGN_VERIFY_ATTEMPTS": "many"}},
		{name: "overflow", args: []string{testImage, testIdentity, testIssuer}, env: map[string]string{"COSIGN_VERIFY_DELAY_SECONDS": strings.Repeat("9", 40)}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := ExecuteWithOptions(context.Background(), test.args, nil, nil, nil, Options{LookupEnv: lookup(test.env), Runner: &sequenceRunner{}})
			if cli.ExitCode(err) != 2 || err == nil || !strings.Contains(err.Error(), "usage:") {
				t.Fatalf("error = %v, exit = %d", err, cli.ExitCode(err))
			}
		})
	}
}

func TestExecuteEmptyEnvironmentUsesDefaults(t *testing.T) {
	t.Parallel()
	runner := &sequenceRunner{results: []runnerResult{{}}}
	err := ExecuteWithOptions(context.Background(), []string{testImage, testIdentity, testIssuer}, nil, nil, nil, Options{
		Runner: runner,
		LookupEnv: lookup(map[string]string{
			"COSIGN_VERIFY_ATTEMPTS":      "",
			"COSIGN_VERIFY_DELAY_SECONDS": "",
		}),
	})
	if err != nil || len(runner.calls) != 1 {
		t.Fatalf("error=%v calls=%d", err, len(runner.calls))
	}
}

func TestExecuteCancellationDuringSleep(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	runner := &sequenceRunner{results: []runnerResult{{stderr: "no signatures found", err: statusError(10)}}}
	err := ExecuteWithOptions(ctx, []string{testImage, testIdentity, testIssuer}, nil, nil, io.Discard, Options{
		Runner:    runner,
		LookupEnv: lookup(map[string]string{"COSIGN_VERIFY_ATTEMPTS": "2", "COSIGN_VERIFY_DELAY_SECONDS": "10"}),
		Sleep: func(ctx context.Context, _ time.Duration) error {
			cancel()
			<-ctx.Done()
			return ctx.Err()
		},
	})
	if cli.ExitCode(err) != 1 || err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("error = %v", err)
	}
}

func TestExecuteAttemptTimeout(t *testing.T) {
	t.Parallel()
	runner := RunnerFunc(func(ctx context.Context, _ string, _ []string, _, _ io.Writer) error {
		<-ctx.Done()
		return ctx.Err()
	})
	err := ExecuteWithOptions(context.Background(), []string{testImage, testIdentity, testIssuer}, nil, nil, nil, Options{
		Runner:         runner,
		LookupEnv:      lookup(nil),
		AttemptTimeout: 10 * time.Millisecond,
	})
	if cli.ExitCode(err) != 1 || err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v", err)
	}
}

func TestCommandExitCode(t *testing.T) {
	t.Parallel()
	if got := commandExitCode(errorsWithoutStatus{}); got != 1 {
		t.Fatalf("ordinary error exit = %d", got)
	}
	if got := commandExitCode(&exec.Error{Name: "cosign", Err: exec.ErrNotFound}); got != 127 {
		t.Fatalf("not found exit = %d", got)
	}
	if got := commandExitCode(&os.PathError{Op: "fork/exec", Path: "cosign", Err: os.ErrPermission}); got != 126 {
		t.Fatalf("permission exit = %d", got)
	}
}

type errorsWithoutStatus struct{}

func (errorsWithoutStatus) Error() string { return "missing" }
