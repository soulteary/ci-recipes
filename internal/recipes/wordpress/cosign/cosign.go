// Package cosign retries verification while a newly-pushed signature referrer
// is becoming visible. It replaces verify-cosign-signature.sh.
package cosign

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/soulteary/ci-recipes/internal/cli"
)

const (
	usage                 = "usage: verify-cosign-signature IMAGE_REF CERTIFICATE_IDENTITY CERTIFICATE_ISSUER"
	defaultAttempts       = 12
	defaultDelay          = 10 * time.Second
	defaultAttemptTimeout = 2 * time.Minute
	maxVerifyLogBytes     = 1 << 20
)

var (
	positiveInteger    = regexp.MustCompile(`^[1-9][0-9]*$`)
	nonnegativeInteger = regexp.MustCompile(`^[0-9]+$`)
)

// Runner executes an external command. Implementations must honor ctx and
// direct command output only to the supplied writers.
type Runner interface {
	Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error
}

// RunnerFunc adapts a function into a Runner.
type RunnerFunc func(context.Context, string, []string, io.Writer, io.Writer) error

func (f RunnerFunc) Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	return f(ctx, name, args, stdout, stderr)
}

// SleepFunc is injectable for deterministic retry tests.
type SleepFunc func(context.Context, time.Duration) error

// Options controls environment lookup, command execution, and retry timing.
type Options struct {
	Runner         Runner
	Sleep          SleepFunc
	LookupEnv      func(string) (string, bool)
	AttemptTimeout time.Duration
}

// Execute runs with the real environment, os/exec, and context-aware timers.
func Execute(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return ExecuteWithOptions(ctx, args, stdin, stdout, stderr, Options{})
}

// ExecuteWithOptions runs the compatibility command with injectable process
// and timing dependencies.
func ExecuteWithOptions(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, options Options) error {
	_ = stdin
	_ = stdout
	if ctx == nil {
		return cli.Exit(1, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return cli.Exit(1, "cosign verification canceled: %v", err)
	}
	if stderr == nil {
		stderr = io.Discard
	}

	imageRef, identity, issuer := "", "", ""
	if len(args) > 0 {
		imageRef = args[0]
	}
	if len(args) > 1 {
		identity = args[1]
	}
	if len(args) > 2 {
		issuer = args[2]
	}

	lookupEnv := options.LookupEnv
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	attemptsText := environmentOrDefault(lookupEnv, "COSIGN_VERIFY_ATTEMPTS", strconv.Itoa(defaultAttempts))
	delayText := environmentOrDefault(lookupEnv, "COSIGN_VERIFY_DELAY_SECONDS", strconv.FormatInt(int64(defaultDelay/time.Second), 10))
	if imageRef == "" || identity == "" || issuer == "" || !positiveInteger.MatchString(attemptsText) || !nonnegativeInteger.MatchString(delayText) {
		return cli.Exit(2, usage)
	}
	attempts64, err := strconv.ParseInt(attemptsText, 10, 32)
	if err != nil {
		return cli.Exit(2, usage)
	}
	delaySeconds, err := strconv.ParseInt(delayText, 10, 64)
	if err != nil || delaySeconds > int64(^uint64(0)>>1)/int64(time.Second) {
		return cli.Exit(2, usage)
	}
	attempts := int(attempts64)
	delay := time.Duration(delaySeconds) * time.Second

	runner := options.Runner
	if runner == nil {
		runner = commandRunner{}
	}
	sleep := options.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	attemptTimeout := options.AttemptTimeout
	if attemptTimeout <= 0 {
		attemptTimeout = defaultAttemptTimeout
	}
	commandArgs := []string{
		"verify",
		"--certificate-identity", identity,
		"--certificate-oidc-issuer", issuer,
		imageRef,
	}

	for attempt := 1; attempt <= attempts; attempt++ {
		verifyLog := newBoundedLog(maxVerifyLogBytes)
		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		err = runner.Run(attemptCtx, "cosign", commandArgs, io.Discard, verifyLog)
		attemptContextErr := attemptCtx.Err()
		cancel()
		if err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return cli.Exit(1, "cosign verification canceled: %v", ctxErr)
		}
		if attemptContextErr != nil {
			return cli.Exit(1, "cosign verification timed out: %v", attemptContextErr)
		}

		status := commandExitCode(err)
		logText := verifyLog.String()
		if !verifyLog.missingSignature {
			if logText == "" {
				logText = err.Error()
			}
			return cli.Exit(status, "%s", logText)
		}
		if attempt == attempts {
			message := fmt.Sprintf("signature referrer for %s was not visible after %d attempts", imageRef, attempts)
			if logText != "" {
				message = logText + "\n" + message
			}
			return cli.Exit(status, "%s", message)
		}
		if _, writeErr := fmt.Fprintf(stderr, "signature referrer for %s is not visible yet; retrying in %ds (%d/%d)\n", imageRef, delaySeconds, attempt, attempts); writeErr != nil {
			return cli.Exit(1, "unable to write retry status: %v", writeErr)
		}
		if err = sleep(ctx, delay); err != nil {
			return cli.Exit(1, "cosign verification canceled: %v", err)
		}
	}
	return cli.Exit(1, "cosign verification failed")
}

const missingSignatureText = "no signatures found"

// boundedLog continues draining subprocess stderr while retaining a bounded
// prefix. It detects the retry marker across write boundaries, including after
// retained storage is full.
type boundedLog struct {
	data             []byte
	maximum          int
	total            int64
	matcherCarry     string
	missingSignature bool
}

func newBoundedLog(maximum int) *boundedLog {
	return &boundedLog{maximum: maximum, data: make([]byte, 0, min(maximum, 4096))}
}

func (b *boundedLog) Write(p []byte) (int, error) {
	written := len(p)
	b.total += int64(written)
	lower := strings.ToLower(string(p))
	search := b.matcherCarry + lower
	if strings.Contains(search, missingSignatureText) {
		b.missingSignature = true
	}
	carryLength := len(missingSignatureText) - 1
	if len(search) > carryLength {
		b.matcherCarry = search[len(search)-carryLength:]
	} else {
		b.matcherCarry = search
	}

	if b.maximum <= 0 || written == 0 || len(b.data) >= b.maximum {
		return written, nil
	}
	remaining := b.maximum - len(b.data)
	if written > remaining {
		p = p[:remaining]
	}
	b.data = append(b.data, p...)
	return written, nil
}

func (b *boundedLog) String() string {
	text := strings.TrimRight(string(b.data), "\r\n")
	if b.total > int64(len(b.data)) {
		if text == "" {
			return "[cosign stderr truncated]"
		}
		return text + "\n[cosign stderr truncated]"
	}
	return text
}

func environmentOrDefault(lookup func(string) (string, bool), name, fallback string) string {
	value, ok := lookup(name)
	if !ok || value == "" {
		return fallback
	}
	return value
}

func commandExitCode(err error) int {
	type exitCoder interface{ ExitCode() int }
	var exitErr exitCoder
	if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 {
		return exitErr.ExitCode()
	}
	var processError *exec.ExitError
	if errors.As(err, &processError) {
		if waitStatus, ok := processError.Sys().(syscall.WaitStatus); ok && waitStatus.Signaled() {
			return 128 + int(waitStatus.Signal())
		}
	}
	var executableError *exec.Error
	if errors.As(err, &executableError) || errors.Is(err, exec.ErrNotFound) {
		return 127
	}
	if errors.Is(err, os.ErrPermission) {
		return 126
	}
	return 1
}

type commandRunner struct{}

func (commandRunner) Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Stdout = stdout
	command.Stderr = stderr
	return command.Run()
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
