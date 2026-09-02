package evidence

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/soulteary/ci-recipes/internal/cli"
)

func TestExecute(t *testing.T) {
	t.Parallel()
	expected := `["linux/amd64","linux/arm64"]`
	tests := []struct {
		name      string
		args      []string
		input     string
		wantCode  int
		wantError string
	}{
		{
			name:  "valid SPDX",
			args:  []string{"SPDX", expected},
			input: `{"linux/amd64":{"SPDX":{"spdxVersion":"SPDX-2.3"}},"linux/arm64":{"SPDX":{"spdxVersion":"SPDX-2.3"}}}`,
		},
		{
			name:  "valid SLSA and ignored extra argument",
			args:  []string{"SLSA", expected, "ignored"},
			input: `{"linux/amd64":{"SLSA":{"buildType":"buildkit"}},"linux/arm64":{"SLSA":{"buildType":"buildkit"}}}`,
		},
		{name: "invalid field", args: []string{"CycloneDX", expected}, input: `{}`, wantCode: 2, wantError: "usage:"},
		{name: "missing expected platforms", args: []string{"SPDX"}, input: `{}`, wantCode: 2, wantError: "usage:"},
		{name: "expected is not an array", args: []string{"SPDX", `{}`}, input: `{}`, wantCode: 2, wantError: "usage:"},
		{name: "empty expected array", args: []string{"SPDX", `[]`}, input: `{}`, wantCode: 2, wantError: "usage:"},
		{name: "empty expected entry", args: []string{"SPDX", `[""]`}, input: `{}`, wantCode: 2, wantError: "usage:"},
		{name: "duplicate expected entry", args: []string{"SPDX", `["linux/amd64","linux/amd64"]`}, input: `{"linux/amd64":{"SPDX":{"x":1}}}`, wantCode: 1, wantError: "non-empty SPDX"},
		{name: "malformed evidence", args: []string{"SPDX", expected}, input: `{`, wantCode: 1, wantError: "non-empty SPDX"},
		{name: "empty evidence", args: []string{"SPDX", expected}, wantCode: 1, wantError: "non-empty SPDX"},
		{name: "multiple JSON documents", args: []string{"SPDX", expected}, input: `{}` + "\n" + `{}`, wantCode: 1, wantError: "non-empty SPDX"},
		{name: "missing platform", args: []string{"SPDX", expected}, input: `{"linux/amd64":{"SPDX":{"x":1}}}`, wantCode: 1, wantError: "non-empty SPDX"},
		{name: "extra platform", args: []string{"SPDX", expected}, input: `{"linux/amd64":{"SPDX":{"x":1}},"linux/arm64":{"SPDX":{"x":1}},"linux/arm/v7":{"SPDX":{"x":1}}}`, wantCode: 1, wantError: "non-empty SPDX"},
		{name: "platform value is not object", args: []string{"SPDX", expected}, input: `{"linux/amd64":[],"linux/arm64":{"SPDX":{"x":1}}}`, wantCode: 1, wantError: "non-empty SPDX"},
		{name: "field is missing", args: []string{"SPDX", expected}, input: `{"linux/amd64":{"SLSA":{"x":1}},"linux/arm64":{"SPDX":{"x":1}}}`, wantCode: 1, wantError: "non-empty SPDX"},
		{name: "field is empty", args: []string{"SPDX", expected}, input: `{"linux/amd64":{"SPDX":{}},"linux/arm64":{"SPDX":{"x":1}}}`, wantCode: 1, wantError: "non-empty SPDX"},
		{name: "field is array", args: []string{"SPDX", expected}, input: `{"linux/amd64":{"SPDX":[1]},"linux/arm64":{"SPDX":{"x":1}}}`, wantCode: 1, wantError: "non-empty SPDX"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			err := Execute(context.Background(), test.args, strings.NewReader(test.input), &stdout, &stderr)
			if got := cli.ExitCode(err); got != test.wantCode {
				t.Fatalf("exit code = %d, want %d (error %v)", got, test.wantCode, err)
			}
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("Execute() error = %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("Execute() error = %v, want substring %q", err, test.wantError)
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("unexpected output: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestExecuteCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Execute(ctx, []string{"SPDX", `["linux/amd64"]`}, strings.NewReader(`{"linux/amd64":{"SPDX":{"x":1}}}`), nil, nil)
	if cli.ExitCode(err) != 1 || !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("Execute() error = %v", err)
	}
}

func TestExecuteCancellationClosesBlockedReader(t *testing.T) {
	t.Parallel()
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Execute(ctx, []string{"SPDX", `["linux/amd64"]`}, reader, nil, nil)
	}()
	cancel()
	select {
	case err := <-done:
		if cli.ExitCode(err) != 1 || err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("Execute() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Execute did not unblock after cancellation")
	}
}

func TestUsage(t *testing.T) {
	t.Parallel()
	if got := Usage(); !strings.Contains(got, "SPDX|SLSA") {
		t.Fatalf("Usage() = %q", got)
	}
}
