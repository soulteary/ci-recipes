package grantseal

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/soulteary/ci-recipes/internal/cli"
)

type fakeRunner struct {
	lookPath func(string) (string, error)
	run      func(context.Context, command) error
}

type fakeExitError struct {
	code    int
	message string
}

func (e fakeExitError) Error() string { return e.message }
func (e fakeExitError) ExitCode() int { return e.code }

func (f fakeRunner) LookPath(name string) (string, error) {
	if f.lookPath != nil {
		return f.lookPath(name)
	}
	return "/usr/bin/" + name, nil
}

func (f fakeRunner) Run(ctx context.Context, cmd command) error {
	if f.run == nil {
		return errors.New("unexpected command: " + cmd.Name)
	}
	return f.run(ctx, cmd)
}

func testDeps(dir string) dependencies {
	deps := defaultDependencies()
	deps.workDir = dir
	return deps
}

func executeTest(t *testing.T, deps dependencies, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := execute(context.Background(), deps, args, bytes.NewReader(nil), &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

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

type tarEntry struct {
	name     string
	contents string
	mode     int64
	typeflag byte
}

func tarGzipBytes(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	writeTarEntries(t, gz, entries)
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func tarBytes(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	writeTarEntries(t, &output, entries)
	return output.Bytes()
}

func writeTarEntries(t *testing.T, output io.Writer, entries []tarEntry) {
	t.Helper()
	tw := tar.NewWriter(output)
	for _, entry := range entries {
		mode := entry.mode
		if mode == 0 {
			mode = 0o644
		}
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		header := &tar.Header{Name: entry.name, Mode: mode, Typeflag: typeflag, Size: int64(len(entry.contents))}
		if typeflag != tar.TypeReg && typeflag != tar.TypeRegA {
			header.Size = 0
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size != 0 {
			if _, err := io.WriteString(tw, entry.contents); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeTarGzip(t *testing.T, name string, entries []tarEntry) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, tarGzipBytes(t, entries), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeZip(t *testing.T, name string, entries []tarEntry) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(file)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Store}
		mode := os.FileMode(entry.mode)
		if mode == 0 {
			mode = 0o644
		}
		if entry.typeflag == tar.TypeSymlink {
			mode |= os.ModeSymlink
		}
		header.SetMode(mode)
		writer, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(writer, entry.contents); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
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
