// Package runnerfleet contains the CI recipes migrated from runner-fleet's
// scripts/check-*.sh.
//
// Both shell originals reported success in situations where they had checked
// nothing: a failing "git ls-files" was indistinguishable from an empty match
// because its status was swallowed by a pipeline, an unquoted "for" loop
// dropped any path containing a space, and an absent documentation tree
// compared zero files and printed its all-clear. Every enumeration, read and
// subprocess failure here is therefore a failure of the check rather than an
// empty result, and a run that compared nothing is an error.
//
// Operator-facing messages stay in the original Chinese so that migrating the
// workflow does not change what a runner-fleet contributor reads on a pull
// request; the GitHub annotation format is preserved byte for byte.
package runnerfleet

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/soulteary/ci-recipes/internal/cli"
)

// command describes an external process without invoking a shell.
type command struct {
	Name   string
	Args   []string
	Dir    string
	Stdout io.Writer
	Stderr io.Writer
}

type runner interface {
	Run(ctx context.Context, spec command) error
}

type osRunner struct{}

func (osRunner) Run(ctx context.Context, spec command) error {
	cmd := exec.CommandContext(ctx, spec.Name, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr
	return cmd.Run()
}

type dependencies struct {
	runner   runner
	workDir  string
	readFile func(string) ([]byte, error)
}

func defaultDependencies() dependencies {
	workDir, err := os.Getwd()
	if err != nil {
		workDir = "."
	}
	return dependencies{runner: osRunner{}, workDir: workDir, readFile: os.ReadFile}
}

// Execute dispatches one runner-fleet recipe:
//
//   - version-consistency
//   - docs-structure
func Execute(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) error {
	return execute(ctx, defaultDependencies(), args, stdout, stderr)
}

func execute(ctx context.Context, deps dependencies, args []string, stdout, stderr io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return usage("runner-fleet recipe canceled: %v", err)
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
	if deps.readFile == nil {
		deps.readFile = os.ReadFile
	}
	if len(args) < 1 {
		return usage("runner-fleet recipe requires a command")
	}
	switch args[0] {
	case "version-consistency":
		return runVersionConsistency(ctx, deps, args[1:], stdout, stderr)
	case "docs-structure":
		return runDocsStructure(deps, args[1:], stdout, stderr)
	default:
		return usage("unknown runner-fleet recipe %q", args[0])
	}
}

func usage(format string, args ...any) error    { return cli.Exit(2, format, args...) }
func rejected(format string, args ...any) error { return cli.Exit(1, format, args...) }

// recipeArgs holds the two options both recipes accept.
type recipeArgs struct {
	root       string
	configPath string
}

func parseRecipeArgs(usageLine string, args []string) (recipeArgs, error) {
	parsed := recipeArgs{root: "."}
	rootSet := false
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--config":
			index++
			if index >= len(args) || args[index] == "" {
				return recipeArgs{}, usage("usage: %s", usageLine)
			}
			parsed.configPath = args[index]
		default:
			if rootSet || strings.HasPrefix(args[index], "-") {
				return recipeArgs{}, usage("usage: %s", usageLine)
			}
			parsed.root = args[index]
			rootSet = true
		}
	}
	return parsed, nil
}

func resolvePath(workDir, name string) string {
	if filepath.IsAbs(name) {
		return filepath.Clean(name)
	}
	return filepath.Join(workDir, name)
}

// resolveRoot resolves the repository root the recipe operates on. Neither
// recipe searches upward for a marker: like the shell they replace, they use
// the current directory, which is GitHub Actions' default working directory.
func resolveRoot(deps dependencies, rootArg string) (string, error) {
	root := resolvePath(deps.workDir, rootArg)
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		if err == nil {
			err = fmt.Errorf("not a directory")
		}
		return "", usage("root dir not found: %s: %v", rootArg, err)
	}
	return root, nil
}

// listRepositoryFiles enumerates tracked and untracked-but-not-ignored paths
// matching pathspecs.
//
// The shell built this list inside a command substitution that ended in
// "|| true", and a pipeline reports the status of its last command, so git
// failing to run — no work tree, an unreadable index, or the common one, a
// bind-mounted repository owned by another UID that git rejects with "detected
// dubious ownership" — produced an empty list and a green check. git's status
// is checked here and a list that cannot be obtained fails the recipe.
//
// -z keeps git from quoting paths that contain spaces, newlines or non-ASCII
// bytes, so there is nothing to unquote and nothing to silently skip.
func listRepositoryFiles(ctx context.Context, deps dependencies, root string, pathspecs []string) ([]string, error) {
	if len(pathspecs) == 0 {
		// An empty pathspec list makes git list the whole tree, which is a
		// different check than the one that was configured.
		return nil, fmt.Errorf("no version_globs configured, so there is nothing to enumerate")
	}
	args := []string{"ls-files", "-z", "--cached", "--others", "--exclude-standard", "--"}
	args = append(args, pathspecs...)
	var stdout, stderr bytes.Buffer
	if err := deps.runner.Run(ctx, command{Name: "git", Args: args, Dir: root, Stdout: &stdout, Stderr: &stderr}); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("git ls-files: %s: the file list is unknown, so the check cannot pass", message)
	}
	var result []string
	for _, entry := range strings.Split(stdout.String(), "\x00") {
		if entry != "" {
			result = append(result, entry)
		}
	}
	sort.Strings(result)
	return dedupe(result), nil
}

// dedupe collapses the duplicate entries git prints for an unmerged path, one
// per stage, during a merge conflict.
func dedupe(sorted []string) []string {
	result := sorted[:0:0]
	previous := ""
	for index, entry := range sorted {
		if index > 0 && entry == previous {
			continue
		}
		result = append(result, entry)
		previous = entry
	}
	return result
}
