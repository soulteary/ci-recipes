// Package grantseal contains the CI recipes migrated from grantseal's shell
// scripts.  The package deliberately keeps process exit policy out of os.Exit
// so callers and tests can run recipes in-process.
package grantseal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/soulteary/ci-recipes/internal/cli"
)

type command struct {
	Name   string
	Args   []string
	Dir    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

type runner interface {
	LookPath(file string) (string, error)
	Run(ctx context.Context, command command) error
}

type osRunner struct{}

func (osRunner) LookPath(file string) (string, error) { return exec.LookPath(file) }

func (osRunner) Run(ctx context.Context, spec command) error {
	cmd := exec.CommandContext(ctx, spec.Name, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Stdin = spec.Stdin
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr
	return cmd.Run()
}

type dependencies struct {
	runner      runner
	workDir     string
	getenv      func(string) string
	now         func() time.Time
	readFile    func(string) ([]byte, error)
	writeAtomic func(context.Context, string, []byte) error
}

func defaultDependencies() dependencies {
	workDir, err := os.Getwd()
	if err != nil {
		workDir = "."
	}
	return dependencies{
		runner:      osRunner{},
		workDir:     workDir,
		getenv:      os.Getenv,
		now:         time.Now,
		readFile:    os.ReadFile,
		writeAtomic: atomicWriteFile,
	}
}

// Execute dispatches one Grantseal recipe. Arguments use two-level names:
//
//   - archive allowlist
//   - sensitive files
//   - patch coverage
//   - doc language
//   - doc consistency
//   - writeback allowlist
//   - report inject-environment
//   - quality docs
func Execute(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return execute(ctx, defaultDependencies(), args, stdin, stdout, stderr)
}

func execute(ctx context.Context, deps dependencies, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return usage("grantseal recipe canceled: %v", err)
	}
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if deps.runner == nil {
		deps.runner = osRunner{}
	}
	if deps.workDir == "" {
		deps.workDir = "."
	}
	if deps.getenv == nil {
		deps.getenv = os.Getenv
	}
	if deps.now == nil {
		deps.now = time.Now
	}
	if deps.readFile == nil {
		deps.readFile = os.ReadFile
	}
	if deps.writeAtomic == nil {
		deps.writeAtomic = atomicWriteFile
	}
	if len(args) < 2 {
		return usage("grantseal recipe requires a two-part command")
	}

	rest := args[2:]
	switch args[0] + " " + args[1] {
	case "archive allowlist":
		return runArchiveAllowlist(ctx, deps, rest, stdout, stderr)
	case "sensitive files":
		return runSensitiveFiles(ctx, deps, rest, stdout, stderr)
	case "patch coverage":
		return runPatchCoverage(ctx, deps, rest, stdout, stderr)
	case "doc language":
		return runDocLanguage(deps, rest, stdout, stderr)
	case "doc consistency":
		return runDocConsistency(deps, rest, stdout, stderr)
	case "writeback allowlist", "staged writeback":
		return runWritebackAllowlist(ctx, deps, rest, stdout, stderr)
	case "report inject-environment":
		return runInjectReportEnvironment(ctx, deps, rest, stdout, stderr)
	case "quality docs", "quality generate":
		return runGenerateQualityDocs(ctx, deps, rest, stdout, stderr)
	default:
		return usage("unknown grantseal recipe %q", args[0]+" "+args[1])
	}
}

func usage(format string, args ...any) error    { return cli.Exit(2, format, args...) }
func rejected(format string, args ...any) error { return cli.Exit(1, format, args...) }

func runOutput(ctx context.Context, deps dependencies, name string, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := deps.runner.Run(ctx, command{
		Name: name, Args: args, Dir: deps.workDir, Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%s %s: %s: %w", name, strings.Join(args, " "), msg, err)
	}
	return stdout.Bytes(), nil
}

func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr interface{ ExitCode() int }
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
