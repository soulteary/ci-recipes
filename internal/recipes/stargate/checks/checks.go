// Package checks implements the repository checks and local launcher formerly
// provided by shell scripts in soulteary/stargate.
package checks

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/soulteary/ci-recipes/internal/cli"
)

// Command describes an external process without invoking a shell.
type Command struct {
	Name   string
	Args   []string
	Dir    string
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

type commandRunner interface {
	Run(context.Context, Command) error
	LookPath(string) (string, error)
}

type osCommandRunner struct{}

func (osCommandRunner) Run(ctx context.Context, invocation Command) error {
	cmd := exec.CommandContext(ctx, invocation.Name, invocation.Args...)
	cmd.Dir = invocation.Dir
	cmd.Env = invocation.Env
	cmd.Stdin = invocation.Stdin
	cmd.Stdout = invocation.Stdout
	cmd.Stderr = invocation.Stderr
	return cmd.Run()
}

func (osCommandRunner) LookPath(name string) (string, error) { return exec.LookPath(name) }

type options struct {
	root    string
	runner  commandRunner
	getenv  func(string) string
	environ func() []string
	now     func() time.Time
}

func defaultOptions() options {
	return options{
		runner:  osCommandRunner{},
		getenv:  os.Getenv,
		environ: os.Environ,
		now:     time.Now,
	}
}

func (o options) normalized() options {
	if o.runner == nil {
		o.runner = osCommandRunner{}
	}
	if o.getenv == nil {
		o.getenv = os.Getenv
	}
	if o.environ == nil {
		o.environ = os.Environ
	}
	if o.now == nil {
		o.now = time.Now
	}
	return o
}

// Execute dispatches a stargate check or the native local launcher.
//
// Supported subcommands are doc-contracts, go-version-contract,
// release-workflow, start-local, and local-run. start-local is retained as an
// alias for local-run so replacing both the old launcher and its CI self-test
// does not require two implementations.
func Execute(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return executeWithOptions(ctx, args, stdin, stdout, stderr, defaultOptions())
}

func executeWithOptions(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, opts options) error {
	if ctx == nil {
		return errors.New("stargate checks: nil context")
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	opts = opts.normalized()
	if len(args) == 0 {
		return cli.Exit(2, "usage: stargate checks <doc-contracts|go-version-contract|release-workflow|start-local|local-run> ...")
	}
	switch args[0] {
	case "doc-contracts":
		if len(args) > 2 {
			return cli.Exit(2, "usage: stargate checks doc-contracts [BASE_SHA]")
		}
	case "go-version-contract":
		if len(args) != 1 {
			return cli.Exit(2, "usage: stargate checks go-version-contract")
		}
	case "release-workflow":
		if len(args) != 1 {
			return cli.Exit(2, "usage: stargate checks release-workflow")
		}
	case "start-local", "local-run":
		_, help, parseErr := parseLocalArgs(args[1:])
		if parseErr != nil {
			return cli.Exit(2, "%v", parseErr)
		}
		if help {
			return writeLocalHelp(stdout)
		}
	default:
		return cli.Exit(2, "unknown stargate checks recipe %q", args[0])
	}

	root, err := resolveRoot(opts.root)
	if err != nil {
		return err
	}

	switch args[0] {
	case "doc-contracts":
		base := ""
		if len(args) == 2 {
			base = args[1]
		}
		return executeDocContracts(ctx, root, base, stdout, stderr, opts.runner)
	case "go-version-contract":
		return executeGoVersionContract(ctx, root, stdout, stderr, opts.runner)
	case "release-workflow":
		return executeReleaseWorkflow(ctx, root, stdout)
	case "start-local", "local-run":
		return executeLocalRun(ctx, root, args[1:], stdin, stdout, stderr, opts)
	}
	return errors.New("validated stargate checks command was not dispatched")
}

func resolveRoot(path string) (string, error) {
	if path == "" {
		workingDirectory, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve current working directory: %w", err)
		}
		return findStargateRoot(workingDirectory)
	}
	root, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve repository root %q: %w", path, err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("inspect repository root %q: %w", root, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("repository root %q is not a directory", root)
	}
	return root, nil
}

func findStargateRoot(start string) (string, error) {
	root, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("resolve starting directory %q: %w", start, err)
	}
	for {
		marker := filepath.Join(root, "src", "cmd", "stargate")
		info, statErr := os.Stat(marker)
		switch {
		case statErr == nil && info.IsDir():
			return root, nil
		case statErr == nil:
			return "", fmt.Errorf("Stargate repository marker %q is not a directory", marker)
		case !os.IsNotExist(statErr):
			return "", fmt.Errorf("inspect Stargate repository marker %q: %w", marker, statErr)
		}
		parent := filepath.Dir(root)
		if parent == root {
			return "", fmt.Errorf("cannot find Stargate repository root above %q", start)
		}
		root = parent
	}
}

func writef(w io.Writer, format string, values ...any) error {
	_, err := fmt.Fprintf(w, format, values...)
	return err
}

var baseArchivePaths = []string{
	"README*.md",
	"CHANGELOG.md",
	"docker-compose.yml",
	"docs",
	"src/internal/config/config.go",
	"src/cmd/stargate/constants.go",
	"src/cmd/stargate/server.go",
}

func archiveRevision(ctx context.Context, runner commandRunner, root, revision string, paths []string, stderr io.Writer) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var archive bytes.Buffer
	args := []string{"-C", root, "archive", revision}
	if len(paths) > 0 {
		args = append(args, "--")
		args = append(args, paths...)
	}
	if err := runner.Run(ctx, Command{Name: "git", Args: args, Stdout: &archive, Stderr: stderr}); err != nil {
		return "", fmt.Errorf("archive git revision %q: %w", revision, err)
	}

	destination, err := os.MkdirTemp("", "ci-recipes-stargate-archive-*")
	if err != nil {
		return "", fmt.Errorf("create archive directory: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(destination)
		}
	}()

	reader := tar.NewReader(bytes.NewReader(archive.Bytes()))
	sawGlobalHeader := false
	sawFilesystemEntry := false
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read git archive: %w", err)
		}
		clean := filepath.Clean(filepath.FromSlash(header.Name))
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("git archive contains unsafe path %q", header.Name)
		}
		path := filepath.Join(destination, clean)
		switch header.Typeflag {
		case tar.TypeXGlobalHeader:
			if sawGlobalHeader || sawFilesystemEntry || !validGitGlobalHeader(header) {
				return "", fmt.Errorf("git archive contains unsupported global PAX header %q", header.Name)
			}
			sawGlobalHeader = true
			continue
		case tar.TypeDir:
			sawFilesystemEntry = true
			if err := os.MkdirAll(path, 0o755); err != nil {
				return "", fmt.Errorf("create archive directory %q: %w", clean, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			sawFilesystemEntry = true
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return "", fmt.Errorf("create archive parent for %q: %w", clean, err)
			}
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return "", fmt.Errorf("create archived file %q: %w", clean, err)
			}
			_, copyErr := io.Copy(file, reader)
			closeErr := file.Close()
			if copyErr != nil {
				return "", fmt.Errorf("extract archived file %q: %w", clean, copyErr)
			}
			if closeErr != nil {
				return "", fmt.Errorf("close archived file %q: %w", clean, closeErr)
			}
		default:
			return "", fmt.Errorf("git archive contains unsupported entry %q of type %d", header.Name, header.Typeflag)
		}
	}
	ok = true
	return destination, nil
}

func validGitGlobalHeader(header *tar.Header) bool {
	if header.Name != "pax_global_header" || len(header.PAXRecords) != 1 {
		return false
	}
	commit, ok := header.PAXRecords["comment"]
	if !ok || len(commit) != 40 && len(commit) != 64 {
		return false
	}
	for _, character := range commit {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}
