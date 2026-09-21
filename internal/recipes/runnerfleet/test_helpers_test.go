package runnerfleet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/soulteary/ci-recipes/internal/cli"
)

type fakeRunner struct {
	run func(context.Context, command) error
}

func (f fakeRunner) Run(ctx context.Context, spec command) error {
	if f.run == nil {
		return errors.New("unexpected command: " + spec.Name)
	}
	return f.run(ctx, spec)
}

type fakeExitError struct {
	code    int
	message string
}

func (e fakeExitError) Error() string { return e.message }
func (e fakeExitError) ExitCode() int { return e.code }

// failingGit stands in for a git that refuses to run at all: no work tree, an
// unreadable index, or a bind-mounted repository owned by another UID.
func failingGit() fakeRunner {
	return fakeRunner{run: func(_ context.Context, spec command) error {
		fmt.Fprintln(spec.Stderr, "fatal: detected dubious ownership in repository")
		return fakeExitError{code: 128, message: "exit status 128"}
	}}
}

func testDeps(dir string) dependencies {
	deps := defaultDependencies()
	deps.workDir = dir
	return deps
}

func executeTest(t *testing.T, deps dependencies, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := execute(context.Background(), deps, args, &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

func cliCode(err error) int { return cli.ExitCode(err) }

func requireExitCode(t *testing.T, err error, code int) {
	t.Helper()
	if got := cli.ExitCode(err); got != code {
		t.Fatalf("exit code = %d, want %d; err=%v", got, code, err)
	}
}

func writeTestFile(t *testing.T, name, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitAvailable() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	if !gitAvailable() {
		t.Skip("git not installed")
	}
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "config", "user.email", "selftest@example.com")
	runGit(t, dir, "config", "user.name", "selftest")
	runGit(t, dir, "config", "commit.gpgsign", "false")
}

// newVersionRepo builds the smallest tree the version recipe needs: a git work
// tree whose baseline source declares version.
func newVersionRepo(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	initGitRepo(t, dir)
	writeTestFile(t, filepath.Join(dir, "internal", "config", "config.go"),
		"package config\n\n// DefaultRunnerContainerImage returns the fallback image.\nfunc DefaultRunnerContainerImage() string {\n\ttag = \""+version+"\"\n\treturn tag\n}\n")
	return dir
}

// newDocsRepo builds a documentation tree whose English originals hold base and
// whose every translation holds translation.
func newDocsRepo(t *testing.T, doc, base, translation string) string {
	t.Helper()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "docs", doc), base)
	for _, language := range defaultConfig().docsLanguages {
		writeTestFile(t, filepath.Join(dir, "docs", language, doc), translation)
	}
	return dir
}

// onlyDoc narrows the recipe to a single document name so a fixture does not
// have to provide all three.
func onlyDoc(t *testing.T, dir, doc string) {
	t.Helper()
	writeTestFile(t, filepath.Join(dir, "scripts", "ci-recipes.conf"), "docs_files = "+doc+"\n")
}
