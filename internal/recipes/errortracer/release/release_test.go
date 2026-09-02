package release

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/soulteary/ci-recipes/internal/cli"
)

const (
	testVersion   = "1.2.3"
	testCommit    = "0123456789abcdef0123456789abcdef01234567"
	testEpoch     = "1700000001"
	testBuildDate = "2023-11-14T22:13:21Z"
)

type fakeRunner struct {
	runs       []command
	outputs    []command
	failRunAt  int
	runErr     error
	outputFunc func(command) ([]byte, error)
}

func (runner *fakeRunner) Run(_ context.Context, spec command) error {
	runner.runs = append(runner.runs, cloneCommand(spec))
	if runner.failRunAt > 0 && len(runner.runs) == runner.failRunAt {
		if runner.runErr != nil {
			return runner.runErr
		}
		return errors.New("injected build failure")
	}
	if spec.name != "go" {
		return fmt.Errorf("unexpected command: %s", spec.name)
	}
	outputPath := argumentAfter(spec.args, "-o")
	if outputPath == "" {
		return errors.New("go build is missing -o")
	}
	contents := fmt.Sprintf("binary:%s/%s\n", spec.env["GOOS"], spec.env["GOARCH"])
	if err := os.WriteFile(outputPath, []byte(contents), 0o755); err != nil {
		return err
	}
	return os.Chmod(outputPath, 0o755)
}

func (runner *fakeRunner) Output(_ context.Context, spec command) ([]byte, error) {
	runner.outputs = append(runner.outputs, cloneCommand(spec))
	if runner.outputFunc == nil {
		return nil, fmt.Errorf("unexpected output command: %s %s", spec.name, strings.Join(spec.args, " "))
	}
	return runner.outputFunc(spec)
}

type fakeVerifier struct {
	err      error
	before   func() error
	calls    int
	binary   string
	expected string
	existed  bool
}

func (verifier *fakeVerifier) Verify(_ context.Context, binary, expected string, _ io.Writer) error {
	verifier.calls++
	verifier.binary = binary
	verifier.expected = expected
	_, err := os.Stat(binary)
	verifier.existed = err == nil
	if verifier.before != nil {
		if err := verifier.before(); err != nil {
			return err
		}
	}
	return verifier.err
}

func TestExecuteBuildsCompleteReleaseBundle(t *testing.T) {
	root := newFixture(t, "  1.\t2.3 \n")
	runner := &fakeRunner{}
	verifier := &fakeVerifier{}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	err := execute(
		context.Background(),
		[]string{testVersion, "release output", "ignored"},
		strings.NewReader("unused input"),
		&stdout,
		&stderr,
		testDependencies(root, testEnvironment(), runner, verifier),
	)
	if err != nil {
		t.Fatalf("execute() error = %v; stderr = %q", err, stderr.String())
	}
	if got, want := stdout.String(), "Built Error-Tracer 1.2.3 release archives in release output\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if len(runner.runs) != len(targets) {
		t.Fatalf("go build calls = %d, want %d", len(runner.runs), len(targets))
	}

	expectedLDFlags := "-s -w" +
		" -X github.com/soulteary/Error-Tracer/internal/buildinfo.version=" + testVersion +
		" -X github.com/soulteary/Error-Tracer/internal/buildinfo.commit=" + testCommit +
		" -X github.com/soulteary/Error-Tracer/internal/buildinfo.builtAt=" + testBuildDate
	for index, currentTarget := range targets {
		spec := runner.runs[index]
		if spec.name != "go" || spec.dir != root {
			t.Errorf("build %d command = %q in %q", index, spec.name, spec.dir)
		}
		wantEnvironment := map[string]string{
			"CGO_ENABLED": "0",
			"GOOS":        currentTarget.operatingSystem,
			"GOARCH":      currentTarget.architecture,
		}
		if !reflect.DeepEqual(spec.env, wantEnvironment) {
			t.Errorf("build %d environment = %#v, want %#v", index, spec.env, wantEnvironment)
		}
		binaryName := programName
		if currentTarget.operatingSystem == "windows" {
			binaryName += ".exe"
		}
		wantOutputSuffix := filepath.Join(archiveBaseName(testVersion, currentTarget), binaryName)
		if got := argumentAfter(spec.args, "-o"); !strings.HasSuffix(got, wantOutputSuffix) {
			t.Errorf("build %d output = %q, want suffix %q", index, got, wantOutputSuffix)
		}
		wantArgumentsWithoutOutput := []string{
			"build", "-trimpath", "-buildvcs=false", "-ldflags", expectedLDFlags, "-o",
		}
		if len(spec.args) != 8 || !reflect.DeepEqual(spec.args[:6], wantArgumentsWithoutOutput) || spec.args[7] != mainPackage {
			t.Errorf("build %d args = %#v", index, spec.args)
		}
	}

	if verifier.calls != 1 || !verifier.existed {
		t.Fatalf("verifier calls = %d, binary existed = %v", verifier.calls, verifier.existed)
	}
	if got, want := verifier.expected,
		"error-tracer 1.2.3 (commit "+testCommit+", built "+testBuildDate+")"; got != want {
		t.Fatalf("verification expectation = %q, want %q", got, want)
	}
	if !strings.HasSuffix(
		verifier.binary,
		filepath.Join("error-tracer_1.2.3_linux_amd64", "error-tracer"),
	) {
		t.Fatalf("verified binary = %q", verifier.binary)
	}

	output := filepath.Join(root, "release output")
	assertBundle(t, output, time.Unix(1700000001, 0).UTC())
	manifestBytes, err := os.ReadFile(filepath.Join(output, "release-manifest.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(manifestBytes), releaseManifest(testVersion, testCommit, testBuildDate, testEpoch); got != want {
		t.Fatalf("manifest = %q, want %q", got, want)
	}
	assertModTime(t, filepath.Join(output, "release-manifest.txt"), 1700000001)
}

func TestExecuteUsesLazyGitFallbacksAndDerivedBuildDate(t *testing.T) {
	root := newFixture(t, testVersion+"\n")
	runner := &fakeRunner{}
	runner.outputFunc = func(spec command) ([]byte, error) {
		switch strings.Join(append([]string{spec.name}, spec.args...), " ") {
		case "git rev-parse HEAD":
			return []byte(testCommit + "\n\n"), nil
		case "git show -s --format=%ct HEAD":
			return []byte(testEpoch + "\n"), nil
		default:
			return nil, fmt.Errorf("unexpected output command: %#v", spec)
		}
	}
	verifier := &fakeVerifier{}
	environment := map[string]string{
		"GITHUB_SHA":        "",
		"SOURCE_DATE_EPOCH": "",
		"BUILD_DATE":        "",
	}

	err := execute(
		context.Background(),
		[]string{testVersion, "dist"},
		nil,
		io.Discard,
		io.Discard,
		testDependencies(root, environment, runner, verifier),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.outputs) != 2 {
		t.Fatalf("git calls = %d, want 2", len(runner.outputs))
	}
	if got, want := verifier.expected,
		"error-tracer 1.2.3 (commit "+testCommit+", built 2023-11-14T22:13:21Z)"; got != want {
		t.Fatalf("verification expectation = %q, want %q", got, want)
	}
	if !strings.Contains(readFile(t, filepath.Join(root, "dist", "release-manifest.txt")), "source_date_epoch=1700000001\n") {
		t.Fatal("manifest did not retain source epoch text")
	}
}

func TestExecuteValidation(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		version     string
		environment map[string]string
		wantCode    int
		wantError   string
	}{
		{
			name:      "missing version",
			wantCode:  2,
			wantError: usage,
		},
		{
			name:      "prefixed version",
			args:      []string{"v1.2.3"},
			wantCode:  2,
			wantError: usage,
		},
		{
			name:      "prerelease version",
			args:      []string{"1.2.3-rc.1"},
			wantCode:  2,
			wantError: usage,
		},
		{
			name:      "version mismatch",
			args:      []string{"1.2.4"},
			version:   "1.2.3\n",
			wantCode:  1,
			wantError: "version 1.2.4 does not match VERSION (1.2.3)",
		},
		{
			name:        "short commit",
			args:        []string{testVersion},
			environment: mergeEnvironment(testEnvironment(), map[string]string{"GITHUB_SHA": "abc"}),
			wantCode:    1,
			wantError:   "build commit must be a full hexadecimal Git commit",
		},
		{
			name:        "negative epoch",
			args:        []string{testVersion},
			environment: mergeEnvironment(testEnvironment(), map[string]string{"SOURCE_DATE_EPOCH": "-1"}),
			wantCode:    1,
			wantError:   "SOURCE_DATE_EPOCH must be a non-negative integer",
		},
		{
			name:        "epoch overflow",
			args:        []string{testVersion},
			environment: mergeEnvironment(testEnvironment(), map[string]string{"SOURCE_DATE_EPOCH": "999999999999999999999999"}),
			wantCode:    1,
			wantError:   "SOURCE_DATE_EPOCH must be a non-negative integer",
		},
		{
			name:        "invalid build date shape",
			args:        []string{testVersion},
			environment: mergeEnvironment(testEnvironment(), map[string]string{"BUILD_DATE": "2023-11-14T22:13:21+00:00"}),
			wantCode:    1,
			wantError:   "BUILD_DATE must be an RFC 3339 UTC timestamp",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			version := test.version
			if version == "" {
				version = testVersion + "\n"
			}
			root := newFixture(t, version)
			runner := &fakeRunner{}
			verifier := &fakeVerifier{}
			environment := test.environment
			if environment == nil {
				environment = testEnvironment()
			}
			var stderr bytes.Buffer
			err := execute(
				context.Background(),
				test.args,
				nil,
				io.Discard,
				&stderr,
				testDependencies(root, environment, runner, verifier),
			)
			if got := cli.ExitCode(err); got != test.wantCode {
				t.Fatalf("exit code = %d, want %d (error %v)", got, test.wantCode, err)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty before dispatcher prints the error", stderr.String())
			}
			if got := err.Error(); got != test.wantError {
				t.Fatalf("error = %q, want %q", got, test.wantError)
			}
			if len(runner.runs) != 0 {
				t.Fatalf("build calls = %d, want 0", len(runner.runs))
			}
		})
	}
}

func TestExecuteRejectsNilOrCanceledContextBeforeWork(t *testing.T) {
	root := newFixture(t, testVersion)
	tests := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{name: "nil", want: "context must not be nil"},
		{name: "canceled", ctx: canceledContext(), want: "context canceled"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{}
			err := execute(
				test.ctx,
				[]string{testVersion},
				nil,
				io.Discard,
				io.Discard,
				testDependencies(root, testEnvironment(), runner, &fakeVerifier{}),
			)
			if err == nil || err.Error() != test.want || cli.ExitCode(err) != 1 {
				t.Fatalf("execute() error = %v, code = %d; want %q, code 1", err, cli.ExitCode(err), test.want)
			}
			if len(runner.runs) != 0 || len(runner.outputs) != 0 {
				t.Fatal("commands ran for an invalid context")
			}
		})
	}
}

func TestExecuteAcceptsLexicallyValidBuildDateLikeShellScript(t *testing.T) {
	root := newFixture(t, testVersion)
	runner := &fakeRunner{}
	verifier := &fakeVerifier{}
	environment := mergeEnvironment(testEnvironment(), map[string]string{
		"BUILD_DATE": "2023-99-99T99:99:99Z",
	})
	if err := execute(
		context.Background(),
		[]string{testVersion, "dist"},
		nil,
		io.Discard,
		io.Discard,
		testDependencies(root, environment, runner, verifier),
	); err != nil {
		t.Fatalf("execute() rejected shell-compatible BUILD_DATE: %v", err)
	}
}

func TestExecuteRejectsExistingOutputWithoutRunningCommands(t *testing.T) {
	tests := []struct {
		name   string
		create func(*testing.T, string)
	}{
		{
			name: "file",
			create: func(t *testing.T, path string) {
				t.Helper()
				writeFile(t, path, "occupied", 0o644)
			},
		},
		{
			name: "directory",
			create: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink",
			create: func(t *testing.T, path string) {
				t.Helper()
				target := path + "-target"
				writeFile(t, target, "occupied", 0o644)
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			},
		},
		{
			name: "dangling symlink",
			create: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Symlink(path+"-missing", path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := newFixture(t, testVersion)
			output := filepath.Join(root, "dist")
			test.create(t, output)
			runner := &fakeRunner{}
			var stderr bytes.Buffer
			err := execute(
				context.Background(),
				[]string{testVersion},
				nil,
				io.Discard,
				&stderr,
				testDependencies(root, testEnvironment(), runner, &fakeVerifier{}),
			)
			if got := cli.ExitCode(err); got != 1 {
				t.Fatalf("exit code = %d, want 1", got)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty before dispatcher prints the error", stderr.String())
			}
			if got, want := err.Error(), "output path already exists: dist"; got != want {
				t.Fatalf("error = %q, want %q", got, want)
			}
			if len(runner.runs) != 0 || len(runner.outputs) != 0 {
				t.Fatalf("commands ran before output rejection: run=%d output=%d", len(runner.runs), len(runner.outputs))
			}
		})
	}
}

func TestExecuteDefaultsEmptyOutputArgumentAndIgnoresExtraArguments(t *testing.T) {
	root := newFixture(t, testVersion)
	err := execute(
		context.Background(),
		[]string{testVersion, "", "ignored", "also ignored"},
		nil,
		io.Discard,
		io.Discard,
		testDependencies(root, testEnvironment(), &fakeRunner{}, &fakeVerifier{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "dist")); err != nil {
		t.Fatalf("default output missing: %v", err)
	}
}

func TestExecuteCleansTemporaryOutputOnFailures(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*fakeRunner, *fakeVerifier, *dependencies)
	}{
		{
			name: "third build",
			configure: func(runner *fakeRunner, _ *fakeVerifier, _ *dependencies) {
				runner.failRunAt = 3
			},
		},
		{
			name: "archive",
			configure: func(_ *fakeRunner, _ *fakeVerifier, deps *dependencies) {
				deps.archiveTar = func(context.Context, string, string, time.Time) error {
					return errors.New("injected archive failure")
				}
			},
		},
		{
			name: "verification",
			configure: func(_ *fakeRunner, verifier *fakeVerifier, _ *dependencies) {
				verifier.err = errors.New("injected verification failure")
			},
		},
		{
			name: "rename",
			configure: func(_ *fakeRunner, _ *fakeVerifier, deps *dependencies) {
				deps.rename = func(string, string) error { return errors.New("injected rename failure") }
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := newFixture(t, testVersion)
			runner := &fakeRunner{}
			verifier := &fakeVerifier{}
			deps := testDependencies(root, testEnvironment(), runner, verifier)
			test.configure(runner, verifier, &deps)
			var stdout bytes.Buffer
			err := execute(
				context.Background(),
				[]string{testVersion, "dist"},
				nil,
				&stdout,
				io.Discard,
				deps,
			)
			if err == nil || cli.ExitCode(err) != 1 {
				t.Fatalf("execute() error = %v, want exit 1", err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want no success message", stdout.String())
			}
			if _, err := os.Lstat(filepath.Join(root, "dist")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("published output exists after failure: %v", err)
			}
			matches, err := filepath.Glob(filepath.Join(root, ".dist.tmp-*"))
			if err != nil {
				t.Fatal(err)
			}
			if len(matches) != 0 {
				t.Fatalf("temporary outputs remain: %#v", matches)
			}
		})
	}
}

func TestExecuteReportsCleanupFailure(t *testing.T) {
	root := newFixture(t, testVersion)
	runner := &fakeRunner{failRunAt: 1}
	deps := testDependencies(root, testEnvironment(), runner, &fakeVerifier{})
	deps.removeAll = func(path string) error {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		return errors.New("injected cleanup failure")
	}
	err := execute(
		context.Background(),
		[]string{testVersion, "dist"},
		nil,
		io.Discard,
		io.Discard,
		deps,
	)
	if err == nil || cli.ExitCode(err) != 1 {
		t.Fatalf("execute() error = %v, want exit 1", err)
	}
	if got := err.Error(); !strings.Contains(got, "injected build failure") ||
		!strings.Contains(got, "clean temporary output: injected cleanup failure") {
		t.Fatalf("cleanup failure was not reported: %q", got)
	}
}

func TestExecuteCleansTemporaryOutputWhenReleaseFileIsMissing(t *testing.T) {
	root := newFixture(t, testVersion)
	if err := os.Remove(filepath.Join(root, "README.md")); err != nil {
		t.Fatal(err)
	}
	err := execute(
		context.Background(),
		[]string{testVersion, "dist"},
		nil,
		io.Discard,
		io.Discard,
		testDependencies(root, testEnvironment(), &fakeRunner{}, &fakeVerifier{}),
	)
	if err == nil {
		t.Fatal("execute() succeeded with missing README.md")
	}
	if _, err := os.Stat(filepath.Join(root, "dist")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output exists after copy failure: %v", err)
	}
}

func TestExecuteDoesNotReplaceOutputCreatedBeforePublish(t *testing.T) {
	root := newFixture(t, testVersion)
	output := filepath.Join(root, "dist")
	verifier := &fakeVerifier{
		before: func() error {
			return os.WriteFile(output, []byte("keep me"), 0o600)
		},
	}
	var stderr bytes.Buffer
	err := execute(
		context.Background(),
		[]string{testVersion, "dist"},
		nil,
		io.Discard,
		&stderr,
		testDependencies(root, testEnvironment(), &fakeRunner{}, verifier),
	)
	if err == nil || cli.ExitCode(err) != 1 || err.Error() != "output path already exists: dist" {
		t.Fatalf("execute() error = %v, want output conflict", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want return-only diagnostic", stderr.String())
	}
	if got := readFile(t, output); got != "keep me" {
		t.Fatalf("concurrent output was overwritten: %q", got)
	}
	matches, globErr := filepath.Glob(filepath.Join(root, ".dist.tmp-*"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary outputs remain: %#v", matches)
	}
}

func TestArchivesAreDeterministic(t *testing.T) {
	root := newFixture(t, testVersion)
	for _, output := range []string{"first", "second"} {
		if err := execute(
			context.Background(),
			[]string{testVersion, output},
			nil,
			io.Discard,
			io.Discard,
			testDependencies(root, testEnvironment(), &fakeRunner{}, &fakeVerifier{}),
		); err != nil {
			t.Fatalf("build %s: %v", output, err)
		}
	}
	for _, currentTarget := range targets {
		name := archiveBaseName(testVersion, currentTarget)
		extension := ".tar.gz"
		if currentTarget.operatingSystem == "windows" {
			extension = ".zip"
		}
		first := readBytes(t, filepath.Join(root, "first", name+extension))
		second := readBytes(t, filepath.Join(root, "second", name+extension))
		if !bytes.Equal(first, second) {
			t.Errorf("%s archives differ across identical builds", currentTarget)
		}
	}
	if !bytes.Equal(
		readBytes(t, filepath.Join(root, "first", "release-manifest.txt")),
		readBytes(t, filepath.Join(root, "second", "release-manifest.txt")),
	) {
		t.Error("release manifests differ across identical builds")
	}
}

func TestArchivesUseCanonicalPermissionsIndependentOfHostModes(t *testing.T) {
	timestamp := time.Unix(1700000001, 0).UTC()
	tests := []struct {
		name       string
		binaryName string
		extension  string
		archive    func(context.Context, string, string, time.Time) error
		assert     func(*testing.T, string, string, string, time.Time)
	}{
		{
			name:       "tar gzip",
			binaryName: programName,
			extension:  ".tar.gz",
			archive:    writeTarGzip,
			assert:     assertTarGzip,
		},
		{
			name:       "zip",
			binaryName: programName + ".exe",
			extension:  ".zip",
			archive:    writeZip,
			assert:     assertZip,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			baseName := "release"
			source := filepath.Join(root, baseName)
			if err := os.Mkdir(source, 0o700); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(source, "LICENSE"), "license\n", 0o777)
			writeFile(t, filepath.Join(source, "NOTICE"), "notice\n", 0o600)
			writeFile(t, filepath.Join(source, "README.md"), "readme\n", 0o700)
			writeFile(t, filepath.Join(source, test.binaryName), "binary\n", 0o600)

			destination := filepath.Join(root, "release"+test.extension)
			if err := test.archive(context.Background(), destination, source, timestamp); err != nil {
				t.Fatal(err)
			}
			test.assert(t, destination, baseName, test.binaryName, timestamp)
		})
	}
}

func TestExecuteWithRealGoBuild(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("the original release verification requires a linux/amd64 host")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module github.com/soulteary/Error-Tracer\n\ngo 1.22\n", 0o644)
	writeFile(t, filepath.Join(root, "VERSION"), testVersion+"\n", 0o644)
	writeFile(t, filepath.Join(root, "LICENSE"), "license\n", 0o644)
	writeFile(t, filepath.Join(root, "NOTICE"), "notice\n", 0o644)
	writeFile(t, filepath.Join(root, "README.md"), "readme\n", 0o644)
	writeFile(t, filepath.Join(root, "internal", "buildinfo", "buildinfo.go"), `package buildinfo

import "fmt"

var version = "development"
var commit = "unknown"
var builtAt = "unknown"

func String() string {
	return fmt.Sprintf("error-tracer %s (commit %s, built %s)", version, commit, builtAt)
}
`, 0o644)
	writeFile(t, filepath.Join(root, "cmd", "error-tracer", "main.go"), `package main

import (
	"fmt"
	"os"

	"github.com/soulteary/Error-Tracer/internal/buildinfo"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println(buildinfo.String())
	}
}
`, 0o644)

	runner := systemRunner{}
	var stderr bytes.Buffer
	err := execute(
		context.Background(),
		[]string{testVersion, "dist"},
		nil,
		io.Discard,
		&stderr,
		dependencies{
			root:       root,
			getenv:     environmentGetter(testEnvironment()),
			runner:     runner,
			verifier:   executableVerifier{runner: runner, dir: root},
			mkdirTemp:  os.MkdirTemp,
			rename:     os.Rename,
			removeAll:  os.RemoveAll,
			archiveTar: writeTarGzip,
			archiveZip: writeZip,
		},
	)
	if err != nil {
		t.Fatalf("real build failed: %v; stderr: %s", err, stderr.String())
	}
	binary := filepath.Join(root, "dist", "error-tracer_1.2.3_linux_amd64", "error-tracer")
	if _, err := os.Stat(binary); err != nil {
		t.Fatalf("linux/amd64 binary required by SBOM step is missing: %v", err)
	}
}

func TestEnvironmentWithOverrides(t *testing.T) {
	got := environmentWithOverrides(
		[]string{"PATH=/bin", "GOOS=old", "DUP=first", "DUP=second", "NOEQUALS"},
		map[string]string{"GOOS": "linux", "GOARCH": "arm64", "DUP": "new"},
	)
	want := []string{"PATH=/bin", "GOOS=linux", "DUP=new", "NOEQUALS", "GOARCH=arm64"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environmentWithOverrides() = %#v, want %#v", got, want)
	}
}

func TestFindRepositoryRootFromDescendant(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "// comment\nmodule github.com/soulteary/Error-Tracer\n\ngo 1.22\n", 0o644)
	writeFile(t, filepath.Join(root, "VERSION"), testVersion, 0o644)
	nested := filepath.Join(root, "some", "deep", "directory")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok := findRepositoryRoot(nested)
	if !ok || got != root {
		t.Fatalf("findRepositoryRoot() = %q, %v; want %q, true", got, ok, root)
	}

	other := t.TempDir()
	writeFile(t, filepath.Join(other, "go.mod"), "module example.com/not-error-tracer\n", 0o644)
	if got, ok := findRepositoryRoot(other); ok || got != other {
		t.Fatalf("findRepositoryRoot(non-project) = %q, %v; want %q, false", got, ok, other)
	}
}

func TestExecuteDiscoversRepositoryRootFromDescendant(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module github.com/soulteary/Error-Tracer\n\ngo 1.22\n", 0o644)
	writeFile(t, filepath.Join(root, "VERSION"), testVersion+"\n", 0o644)
	nested := filepath.Join(root, "some", "directory")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	originalDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(nested); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalDirectory); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	err = Execute(context.Background(), []string{"1.2.4"}, nil, io.Discard, io.Discard)
	if err == nil || err.Error() != "version 1.2.4 does not match VERSION (1.2.3)" {
		t.Fatalf("Execute() error = %v; repository root was not discovered", err)
	}
}

func assertBundle(t *testing.T, output string, timestamp time.Time) {
	t.Helper()
	for _, currentTarget := range targets {
		name := archiveBaseName(testVersion, currentTarget)
		staging := filepath.Join(output, name)
		binaryName := programName
		archivePath := filepath.Join(output, name+".tar.gz")
		if currentTarget.operatingSystem == "windows" {
			binaryName += ".exe"
			archivePath = filepath.Join(output, name+".zip")
		}
		for _, fileName := range append([]string{binaryName}, releaseFiles...) {
			path := filepath.Join(staging, fileName)
			if _, err := os.Stat(path); err != nil {
				t.Errorf("staging file %s missing: %v", path, err)
				continue
			}
			assertModTime(t, path, timestamp.Unix())
		}
		assertModTime(t, staging, timestamp.Unix())
		if currentTarget.operatingSystem == "windows" {
			assertZip(t, archivePath, name, binaryName, timestamp)
		} else {
			assertTarGzip(t, archivePath, name, binaryName, timestamp)
		}
	}
}

func assertTarGzip(t *testing.T, path, baseName, binaryName string, timestamp time.Time) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gzipReader.Close()
	if !gzipReader.Header.ModTime.IsZero() {
		t.Errorf("gzip ModTime = %v, want zero", gzipReader.Header.ModTime)
	}
	if gzipReader.Header.Name != "" || gzipReader.Header.Comment != "" || len(gzipReader.Header.Extra) != 0 {
		t.Errorf("gzip header contains nondeterministic metadata: %#v", gzipReader.Header)
	}
	if gzipReader.Header.OS != 3 {
		t.Errorf("gzip OS = %d, want 3", gzipReader.Header.OS)
	}

	wantNames := archiveEntryNames(baseName, binaryName)
	var gotNames []string
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		gotNames = append(gotNames, header.Name)
		if header.ModTime.Unix() != timestamp.Unix() {
			t.Errorf("tar %s mtime = %d, want %d", header.Name, header.ModTime.Unix(), timestamp.Unix())
		}
		if header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" {
			t.Errorf("tar %s ownership = %d:%d %q:%q", header.Name, header.Uid, header.Gid, header.Uname, header.Gname)
		}
		if got, want := os.FileMode(header.Mode).Perm(), expectedArchiveMode(header.Name, binaryName); got != want {
			t.Errorf("tar %s mode = %v, want %v", header.Name, got, want)
		}
		if !strings.HasSuffix(header.Name, "/") {
			contents, err := io.ReadAll(tarReader)
			if err != nil {
				t.Fatal(err)
			}
			assertArchiveContents(t, header.Name, contents, binaryName)
		}
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Errorf("tar names = %#v, want %#v", gotNames, wantNames)
	}
}

func assertZip(t *testing.T, path, baseName, binaryName string, timestamp time.Time) {
	t.Helper()
	reader, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	wantNames := archiveEntryNames(baseName, binaryName)
	gotNames := make([]string, 0, len(reader.File))
	wantZipEpoch := timestamp.Unix() - timestamp.Unix()%2
	if wantZipEpoch < time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC).Unix() {
		wantZipEpoch = time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC).Unix()
	}
	for _, file := range reader.File {
		gotNames = append(gotNames, file.Name)
		if file.Modified.UTC().Unix() != wantZipEpoch {
			t.Errorf("zip %s mtime = %d, want %d", file.Name, file.Modified.UTC().Unix(), wantZipEpoch)
		}
		if len(file.Extra) != 0 {
			t.Errorf("zip %s has extra metadata %x", file.Name, file.Extra)
		}
		if got, want := file.Mode().Perm(), expectedArchiveMode(file.Name, binaryName); got != want {
			t.Errorf("zip %s mode = %v, want %v", file.Name, got, want)
		}
		if strings.HasSuffix(file.Name, "/") {
			continue
		}
		entry, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		contents, readErr := io.ReadAll(entry)
		closeErr := entry.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		assertArchiveContents(t, file.Name, contents, binaryName)
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Errorf("zip names = %#v, want %#v", gotNames, wantNames)
	}
}

func archiveEntryNames(baseName, binaryName string) []string {
	fileNames := append([]string{}, releaseFiles...)
	fileNames = append(fileNames, binaryName)
	sort.Strings(fileNames)
	names := []string{baseName + "/"}
	for _, fileName := range fileNames {
		names = append(names, baseName+"/"+fileName)
	}
	return names
}

func expectedArchiveMode(entryName, binaryName string) os.FileMode {
	switch filepath.Base(strings.TrimSuffix(entryName, "/")) {
	case binaryName:
		return 0o755
	case "LICENSE", "NOTICE", "README.md":
		return 0o644
	default:
		return 0o755
	}
}

func assertArchiveContents(t *testing.T, entryName string, contents []byte, binaryName string) {
	t.Helper()
	switch filepath.Base(entryName) {
	case "LICENSE":
		if string(contents) != "license\n" {
			t.Errorf("%s contents = %q", entryName, contents)
		}
	case "NOTICE":
		if string(contents) != "notice\n" {
			t.Errorf("%s contents = %q", entryName, contents)
		}
	case "README.md":
		if string(contents) != "readme\n" {
			t.Errorf("%s contents = %q", entryName, contents)
		}
	case binaryName:
		if len(contents) == 0 {
			t.Errorf("%s binary is empty", entryName)
		}
	default:
		t.Errorf("unexpected archive entry %s", entryName)
	}
}

func testDependencies(
	root string,
	environment map[string]string,
	runner commandRunner,
	verifier buildVerifier,
) dependencies {
	return dependencies{
		root:       root,
		getenv:     environmentGetter(environment),
		runner:     runner,
		verifier:   verifier,
		mkdirTemp:  os.MkdirTemp,
		rename:     os.Rename,
		removeAll:  os.RemoveAll,
		archiveTar: writeTarGzip,
		archiveZip: writeZip,
	}
}

func testEnvironment() map[string]string {
	return map[string]string{
		"GITHUB_SHA":        testCommit,
		"SOURCE_DATE_EPOCH": testEpoch,
		"BUILD_DATE":        testBuildDate,
	}
}

func mergeEnvironment(base, overrides map[string]string) map[string]string {
	result := make(map[string]string, len(base)+len(overrides))
	for key, value := range base {
		result[key] = value
	}
	for key, value := range overrides {
		result[key] = value
	}
	return result
}

func environmentGetter(environment map[string]string) func(string) string {
	return func(key string) string {
		return environment[key]
	}
}

func newFixture(t *testing.T, version string) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "VERSION"), version, 0o644)
	writeFile(t, filepath.Join(root, "LICENSE"), "license\n", 0o644)
	writeFile(t, filepath.Join(root, "NOTICE"), "notice\n", 0o640)
	writeFile(t, filepath.Join(root, "README.md"), "readme\n", 0o600)
	return root
}

func writeFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	return string(readBytes(t, path))
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func assertModTime(t *testing.T, path string, epoch int64) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.ModTime().Unix(); got != epoch {
		t.Errorf("%s mtime = %d, want %d", path, got, epoch)
	}
}

func cloneCommand(spec command) command {
	clone := spec
	clone.args = append([]string(nil), spec.args...)
	clone.env = make(map[string]string, len(spec.env))
	for key, value := range spec.env {
		clone.env[key] = value
	}
	return clone
}

func argumentAfter(args []string, name string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == name {
			return args[index+1]
		}
	}
	return ""
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
