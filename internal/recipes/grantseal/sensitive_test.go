package grantseal

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func noGitDeps(dir string) dependencies {
	deps := testDeps(dir)
	deps.runner = fakeRunner{lookPath: func(string) (string, error) { return "", fs.ErrNotExist }}
	return deps
}

func TestSensitiveFilesDetectsEachSelftestCaseIndependently(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		content  string
	}{
		{name: "sensitive filename", filename: "k1-private.key", content: "not-a-real-key\n"},
		{name: "OpenSSH header", filename: "config.bin", content: "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n"},
		{name: "disguised header", filename: "notes.txt", content: "-----BEGIN PRIVATE KEY-----\nabc\n"},
		{name: "binary header", filename: "binary.dat", content: "\x00-----BEGIN PRIVATE KEY-----\x00"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTestFile(t, filepath.Join(dir, test.filename), test.content)
			_, stderr, err := executeTest(t, noGitDeps(dir), "sensitive", "files", ".")
			requireExitCode(t, err, 1)
			if !strings.Contains(stderr, test.filename) {
				t.Fatalf("stderr=%q", stderr)
			}
		})
	}
}

func TestSensitiveFilesCleanMissingAndExcluded(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "README.md"), "public\n")
	writeTestFile(t, filepath.Join(dir, "scripts", "fixture.txt"), "-----BEGIN PRIVATE KEY-----\n")
	_, _, err := executeTest(t, noGitDeps(dir), "sensitive", "files", ".")
	requireExitCode(t, err, 0)
	stdout, stderr, err := executeTest(t, noGitDeps(dir), "sensitive", "files", "missing")
	requireExitCode(t, err, 0)
	if !strings.Contains(stdout, "check passed") || !strings.Contains(stderr, "does not exist") {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestSensitiveFilesGitTrackedAndIgnoredSemantics(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	writeTestFile(t, filepath.Join(dir, ".gitignore"), "dist/\nlocal-private.key\nforced-private.key\n")
	writeTestFile(t, filepath.Join(dir, "README.md"), "clean\n")
	runGit(t, dir, "add", ".gitignore", "README.md")
	runGit(t, dir, "commit", "-qm", "base")

	writeTestFile(t, filepath.Join(dir, "local-private.key"), "ignored\n")
	_, _, err := executeTest(t, testDeps(dir), "sensitive", "files", ".")
	requireExitCode(t, err, 0)

	writeTestFile(t, filepath.Join(dir, "dist", "release-private.key"), "artifact\n")
	_, _, err = executeTest(t, testDeps(dir), "sensitive", "files", "dist")
	requireExitCode(t, err, 1)

	writeTestFile(t, filepath.Join(dir, "forced-private.key"), "forced\n")
	runGit(t, dir, "add", "-f", "forced-private.key")
	_, _, err = executeTest(t, testDeps(dir), "sensitive", "files", ".")
	requireExitCode(t, err, 1)
}

func TestSensitiveFilesGitIgnoreThroughSymlinkedParent(t *testing.T) {
	parent := t.TempDir()
	realParent := filepath.Join(parent, "real")
	realRepo := filepath.Join(realParent, "repo")
	if err := os.MkdirAll(realRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, realRepo)
	writeTestFile(t, filepath.Join(realRepo, ".gitignore"), "local-private.key\n")
	writeTestFile(t, filepath.Join(realRepo, "README.md"), "clean\n")
	runGit(t, realRepo, "add", ".gitignore", "README.md")
	runGit(t, realRepo, "commit", "-qm", "base")

	aliasParent := filepath.Join(parent, "alias")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	aliasRepo := filepath.Join(aliasParent, "repo")
	writeTestFile(t, filepath.Join(aliasRepo, "local-private.key"), "ignored\n")

	for _, target := range []string{".", aliasRepo} {
		_, _, err := executeTest(t, testDeps(aliasRepo), "sensitive", "files", target)
		requireExitCode(t, err, 0)
	}
}

func TestSensitiveFilesFailsClosedWhenTrackedEnumerationFails(t *testing.T) {
	dir := t.TempDir()
	deps := testDeps(dir)
	deps.runner = fakeRunner{
		lookPath: func(string) (string, error) { return "/git", nil },
		run: func(_ context.Context, cmd command) error {
			switch {
			case len(cmd.Args) >= 2 && cmd.Args[0] == "rev-parse":
				_, _ = cmd.Stdout.Write([]byte(dir + "\n"))
				return nil
			case len(cmd.Args) >= 4 && cmd.Args[2] == "ls-files":
				return errors.New("index read failed")
			default:
				return errors.New("unexpected command")
			}
		},
	}
	_, _, err := executeTest(t, deps, "sensitive", "files", ".")
	requireExitCode(t, err, 2)
}

func TestSensitiveFilesExplicitArtifactsDoNotPruneExcludedDirectoryNames(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "dist", "release", "scripts", "nested-private.key"), "fixture\n")
	_, stderr, err := executeTest(t, noGitDeps(dir), "sensitive", "files", "dist")
	requireExitCode(t, err, 1)
	if !strings.Contains(stderr, "nested-private.key") {
		t.Fatalf("stderr=%q", stderr)
	}
}

func TestSensitiveFilesGitProbeFailsClosedExceptKnownNoRepository(t *testing.T) {
	dir := t.TempDir()
	for _, test := range []struct {
		name    string
		message string
		want    int
	}{
		{name: "unsafe repository configuration", message: "fatal: detected dubious ownership in repository", want: 2},
		{name: "corrupt repository", message: "fatal: bad config line", want: 2},
		{name: "outside repository", message: "fatal: not a git repository (or any of the parent directories): .git", want: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			deps := testDeps(dir)
			deps.runner = fakeRunner{
				lookPath: func(string) (string, error) { return "/git", nil },
				run: func(_ context.Context, cmd command) error {
					_, _ = io.WriteString(cmd.Stderr, test.message)
					return fakeExitError{code: 128, message: "git failed"}
				},
			}
			_, _, err := executeTest(t, deps, "sensitive", "files", "missing")
			requireExitCode(t, err, test.want)
		})
	}
}

func TestSplitNUL(t *testing.T) {
	got, err := splitNUL([]byte("a\x00b c\x00"))
	if err != nil || len(got) != 2 || got[1] != "b c" {
		t.Fatalf("got=%v err=%v", got, err)
	}
	if _, err := splitNUL([]byte("unterminated")); err == nil {
		t.Fatal("unterminated data accepted")
	}
}

func TestSensitiveUnreadablePathFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("root can read mode-000 files")
	}
	dir := t.TempDir()
	name := filepath.Join(dir, "secret.txt")
	writeTestFile(t, name, "clean")
	if err := os.Chmod(name, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(name, 0o600)
	_, _, err := executeTest(t, noGitDeps(dir), "sensitive", "files", ".")
	requireExitCode(t, err, 2)
}

func TestSensitiveContentScanIsBoundedAndFindsChunkSpanningHeader(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "large.bin")
	contents := strings.Repeat("x", 32*1024-len(privateKeyBegin)+3) + string(privateKeyBegin) +
		strings.Repeat("y", 64*1024) + string(privateKeyEnd)
	writeTestFile(t, name, contents)
	found, err := fileContainsPrivateKeyHeader(name)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	writeTestFile(t, name, strings.Repeat("x", 2*1024*1024))
	found, err = fileContainsPrivateKeyHeader(name)
	if err != nil || found {
		t.Fatalf("clean large file: found=%v err=%v", found, err)
	}
}
