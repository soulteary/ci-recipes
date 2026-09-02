package grantseal

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWritebackAllowlistSelftestCases(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	writeTestFile(t, filepath.Join(dir, "seed.txt"), "seed\n")
	runGit(t, dir, "add", "seed.txt")
	runGit(t, dir, "commit", "-qm", "seed")

	for name, content := range map[string]string{
		".github/go-test-report.json": "{}\n", ".github/go-test-report.md": "# report\n",
		".github/coverage.svg": "<svg/>\n", ".github/goreportcard.svg": "<svg/>\n",
		".github/goreportcard-report.md": "# grc\n", "docs/enUS/quality.md": "# quality\n",
		"docs/zhCN/quality.md": "# quality\n",
	} {
		writeTestFile(t, filepath.Join(dir, filepath.FromSlash(name)), content)
	}
	runGit(t, dir, "add", ".github", "docs")
	_, _, err := executeTest(t, testDeps(dir), "writeback", "allowlist")
	requireExitCode(t, err, 0)
	runGit(t, dir, "reset", "-q")

	writeTestFile(t, filepath.Join(dir, "evil.sh"), "malicious\n")
	runGit(t, dir, "add", ".github/go-test-report.json", "evil.sh")
	_, stderr, err := executeTest(t, testDeps(dir), "writeback", "allowlist")
	requireExitCode(t, err, 1)
	if !strings.Contains(stderr, "evil.sh") {
		t.Fatalf("stderr=%q", stderr)
	}
	runGit(t, dir, "reset", "-q")

	stdout, _, err := executeTest(t, testDeps(dir), "writeback", "allowlist")
	requireExitCode(t, err, 0)
	if !strings.Contains(stdout, "nothing staged") {
		t.Fatalf("stdout=%q", stdout)
	}
}

func TestWritebackAllowlistFailsClosedOnGitError(t *testing.T) {
	deps := testDeps(t.TempDir())
	deps.runner = fakeRunner{
		lookPath: func(string) (string, error) { return "/git", nil },
		run: func(_ context.Context, cmd command) error {
			_, _ = io.WriteString(cmd.Stderr, "not a repository")
			return errors.New("git failed")
		},
	}
	_, _, err := executeTest(t, deps, "writeback", "allowlist")
	requireExitCode(t, err, 2)
}

func TestWritebackAllowlistRejectsDeletionOutsideAllowlist(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	writeTestFile(t, filepath.Join(dir, "evil.sh"), "tracked\n")
	runGit(t, dir, "add", "evil.sh")
	runGit(t, dir, "commit", "-qm", "base")
	if err := os.Remove(filepath.Join(dir, "evil.sh")); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "-u")
	_, _, err := executeTest(t, testDeps(dir), "writeback", "allowlist")
	requireExitCode(t, err, 1)
}
