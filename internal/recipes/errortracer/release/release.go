// Package release builds the deterministic Error-Tracer release bundle.
package release

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/soulteary/ci-recipes/internal/cli"
)

const (
	usage = "usage: ci-recipes error-tracer build-release <major.minor.patch> [new-output-directory]"

	programName = "error-tracer"
	mainPackage = "./cmd/error-tracer"
	ldPackage   = "github.com/soulteary/Error-Tracer/internal/buildinfo"
)

var (
	versionPattern   = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	commitPattern    = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	buildDatePattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`)

	targets = []target{
		{operatingSystem: "linux", architecture: "amd64"},
		{operatingSystem: "linux", architecture: "arm64"},
		{operatingSystem: "darwin", architecture: "amd64"},
		{operatingSystem: "darwin", architecture: "arm64"},
		{operatingSystem: "windows", architecture: "amd64"},
		{operatingSystem: "windows", architecture: "arm64"},
	}

	releaseFiles = []string{"LICENSE", "NOTICE", "README.md"}
)

type target struct {
	operatingSystem string
	architecture    string
}

func (t target) String() string {
	return t.operatingSystem + "/" + t.architecture
}

type command struct {
	dir    string
	env    map[string]string
	name   string
	args   []string
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

type commandRunner interface {
	Run(context.Context, command) error
	Output(context.Context, command) ([]byte, error)
}

type buildVerifier interface {
	Verify(context.Context, string, string, io.Writer) error
}

type dependencies struct {
	root       string
	getenv     func(string) string
	runner     commandRunner
	verifier   buildVerifier
	mkdirTemp  func(string, string) (string, error)
	rename     func(string, string) error
	removeAll  func(string) error
	archiveTar func(context.Context, string, string, time.Time) error
	archiveZip func(context.Context, string, string, time.Time) error
}

type systemRunner struct{}

func (systemRunner) Run(ctx context.Context, spec command) error {
	cmd := exec.CommandContext(ctx, spec.name, spec.args...)
	cmd.Dir = spec.dir
	cmd.Env = environmentWithOverrides(os.Environ(), spec.env)
	cmd.Stdin = spec.stdin
	cmd.Stdout = spec.stdout
	cmd.Stderr = spec.stderr
	return cmd.Run()
}

func (systemRunner) Output(ctx context.Context, spec command) ([]byte, error) {
	var output bytes.Buffer
	spec.stdout = &output
	if err := (systemRunner{}).Run(ctx, spec); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

type executableVerifier struct {
	runner commandRunner
	dir    string
}

func (v executableVerifier) Verify(ctx context.Context, binary, expected string, stderr io.Writer) error {
	output, err := v.runner.Output(ctx, command{
		dir:    v.dir,
		name:   binary,
		args:   []string{"version"},
		stderr: stderr,
	})
	if err != nil {
		return err
	}
	if strings.TrimRight(string(output), "\n") != expected {
		return errors.New("metadata output mismatch")
	}
	return nil
}

// Execute implements the Error-Tracer scripts/build-release.sh contract.
// It accepts the repository root or any of its descendants as the current
// working directory. Relative output paths are always rooted at the repository.
func Execute(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	root, err := os.Getwd()
	if err != nil {
		return cli.Exit(1, "determine repository root: %v", err)
	}
	if discoveredRoot, ok := findRepositoryRoot(root); ok {
		root = discoveredRoot
	}
	runner := systemRunner{}
	return execute(ctx, args, stdin, stdout, stderr, dependencies{
		root:       root,
		getenv:     os.Getenv,
		runner:     runner,
		verifier:   executableVerifier{runner: runner, dir: root},
		mkdirTemp:  os.MkdirTemp,
		rename:     os.Rename,
		removeAll:  os.RemoveAll,
		archiveTar: writeTarGzip,
		archiveZip: writeZip,
	})
}

func findRepositoryRoot(start string) (string, bool) {
	current := filepath.Clean(start)
	for {
		module, err := os.ReadFile(filepath.Join(current, "go.mod"))
		if err == nil && hasErrorTracerModule(module) {
			if info, statErr := os.Stat(filepath.Join(current, "VERSION")); statErr == nil && info.Mode().IsRegular() {
				return current, true
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			return start, false
		}
		current = parent
	}
}

func hasErrorTracerModule(goMod []byte) bool {
	for _, line := range strings.Split(string(goMod), "\n") {
		if strings.TrimSpace(line) == "module github.com/soulteary/Error-Tracer" {
			return true
		}
	}
	return false
}

func execute(
	ctx context.Context,
	args []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	deps dependencies,
) (returnErr error) {
	if ctx == nil {
		return cli.Exit(1, "context must not be nil")
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
	if err := ctx.Err(); err != nil {
		return cli.Exit(1, "%v", err)
	}

	version := ""
	if len(args) > 0 {
		version = args[0]
	}
	outputArgument := "dist"
	if len(args) > 1 && args[1] != "" {
		outputArgument = args[1]
	}
	if !versionPattern.MatchString(version) {
		return cli.Exit(2, "%s", usage)
	}

	expectedVersionBytes, err := os.ReadFile(filepath.Join(deps.root, "VERSION"))
	if err != nil {
		return cli.Exit(1, "read VERSION: %v", err)
	}
	expectedVersion := removePOSIXSpace(string(expectedVersionBytes))
	if version != expectedVersion {
		return cli.Exit(1, "version %s does not match VERSION (%s)", version, expectedVersion)
	}

	outputPath := outputArgument
	if !filepath.IsAbs(outputPath) {
		outputPath = filepath.Join(deps.root, outputPath)
	}
	outputPath = filepath.Clean(outputPath)
	if _, err := os.Lstat(outputPath); err == nil {
		return cli.Exit(1, "output path already exists: %s", outputArgument)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return cli.Exit(1, "inspect output path: %v", err)
	}

	commit, err := metadataValue(ctx, deps, stderr, "GITHUB_SHA", "git", "rev-parse", "HEAD")
	if err != nil {
		return cli.Exit(1, "resolve build commit: %v", err)
	}
	if !commitPattern.MatchString(commit) {
		return cli.Exit(1, "build commit must be a full hexadecimal Git commit")
	}

	sourceEpochText, err := metadataValue(
		ctx,
		deps,
		stderr,
		"SOURCE_DATE_EPOCH",
		"git",
		"show",
		"-s",
		"--format=%ct",
		"HEAD",
	)
	if err != nil {
		return cli.Exit(1, "resolve source date epoch: %v", err)
	}
	if sourceEpochText == "" || strings.IndexFunc(sourceEpochText, func(r rune) bool {
		return r < '0' || r > '9'
	}) >= 0 {
		return cli.Exit(1, "SOURCE_DATE_EPOCH must be a non-negative integer")
	}
	sourceEpoch, err := strconv.ParseInt(sourceEpochText, 10, 64)
	if err != nil || sourceEpoch < 0 {
		return cli.Exit(1, "SOURCE_DATE_EPOCH must be a non-negative integer")
	}
	archiveTime := time.Unix(sourceEpoch, 0).UTC()

	buildDate := deps.getenv("BUILD_DATE")
	if buildDate == "" {
		buildDate = archiveTime.Format("2006-01-02T15:04:05Z")
	}
	if !buildDatePattern.MatchString(buildDate) {
		return cli.Exit(1, "BUILD_DATE must be an RFC 3339 UTC timestamp")
	}

	parentDirectory := filepath.Dir(outputPath)
	if err := os.MkdirAll(parentDirectory, 0o777); err != nil {
		return cli.Exit(1, "create output parent: %v", err)
	}
	temporaryContainer, err := deps.mkdirTemp(parentDirectory, "."+filepath.Base(outputPath)+".tmp-")
	if err != nil {
		return cli.Exit(1, "create temporary output: %v", err)
	}
	defer func() {
		if err := deps.removeAll(temporaryContainer); err != nil {
			cleanupErr := fmt.Errorf("clean temporary output: %w", err)
			if returnErr == nil {
				returnErr = cli.Exit(1, "%v", cleanupErr)
			} else {
				returnErr = fmt.Errorf("%w; %v", returnErr, cleanupErr)
			}
		}
	}()
	temporaryOutput := filepath.Join(temporaryContainer, "output")
	if err := os.Mkdir(temporaryOutput, 0o777); err != nil {
		return cli.Exit(1, "create temporary output payload: %v", err)
	}

	ldflags := strings.Join([]string{
		"-s",
		"-w",
		"-X", ldPackage + ".version=" + version,
		"-X", ldPackage + ".commit=" + commit,
		"-X", ldPackage + ".builtAt=" + buildDate,
	}, " ")

	for _, currentTarget := range targets {
		if err := ctx.Err(); err != nil {
			return cli.Exit(1, "%v", err)
		}
		name := archiveBaseName(version, currentTarget)
		stagingDirectory := filepath.Join(temporaryOutput, name)
		if err := os.Mkdir(stagingDirectory, 0o777); err != nil {
			return cli.Exit(1, "create staging directory %s: %v", name, err)
		}
		binaryName := programName
		if currentTarget.operatingSystem == "windows" {
			binaryName += ".exe"
		}
		binaryPath := filepath.Join(stagingDirectory, binaryName)
		if err := deps.runner.Run(ctx, command{
			dir: deps.root,
			env: map[string]string{
				"CGO_ENABLED": "0",
				"GOOS":        currentTarget.operatingSystem,
				"GOARCH":      currentTarget.architecture,
			},
			name:   "go",
			args:   []string{"build", "-trimpath", "-buildvcs=false", "-ldflags", ldflags, "-o", binaryPath, mainPackage},
			stdin:  stdin,
			stdout: stdout,
			stderr: stderr,
		}); err != nil {
			return cli.Exit(1, "build %s: %v", currentTarget, err)
		}

		for _, fileName := range releaseFiles {
			if err := copyFile(
				filepath.Join(deps.root, fileName),
				filepath.Join(stagingDirectory, fileName),
			); err != nil {
				return cli.Exit(1, "copy %s for %s: %v", fileName, currentTarget, err)
			}
		}
		if err := setTreeTimestamp(stagingDirectory, archiveTime); err != nil {
			return cli.Exit(1, "set timestamps for %s: %v", currentTarget, err)
		}

		if currentTarget.operatingSystem == "windows" {
			archivePath := filepath.Join(temporaryOutput, name+".zip")
			if err := deps.archiveZip(ctx, archivePath, stagingDirectory, archiveTime); err != nil {
				return cli.Exit(1, "archive %s: %v", currentTarget, err)
			}
		} else {
			archivePath := filepath.Join(temporaryOutput, name+".tar.gz")
			if err := deps.archiveTar(ctx, archivePath, stagingDirectory, archiveTime); err != nil {
				return cli.Exit(1, "archive %s: %v", currentTarget, err)
			}
		}
	}

	linuxBinary := filepath.Join(
		temporaryOutput,
		archiveBaseName(version, target{operatingSystem: "linux", architecture: "amd64"}),
		programName,
	)
	expectedMetadata := fmt.Sprintf(
		"error-tracer %s (commit %s, built %s)",
		version,
		commit,
		buildDate,
	)
	if err := deps.verifier.Verify(ctx, linuxBinary, expectedMetadata, stderr); err != nil {
		return cli.Exit(1, "release binary metadata verification failed: %v", err)
	}

	manifest := releaseManifest(version, commit, buildDate, sourceEpochText)
	manifestPath := filepath.Join(temporaryOutput, "release-manifest.txt")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o666); err != nil {
		return cli.Exit(1, "write release manifest: %v", err)
	}
	if err := os.Chtimes(manifestPath, archiveTime, archiveTime); err != nil {
		return cli.Exit(1, "set release manifest timestamp: %v", err)
	}

	if _, err := os.Lstat(outputPath); err == nil {
		return cli.Exit(1, "output path already exists: %s", outputArgument)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return cli.Exit(1, "inspect output path before publish: %v", err)
	}
	if err := deps.rename(temporaryOutput, outputPath); err != nil {
		return cli.Exit(1, "publish output: %v", err)
	}

	if _, err := fmt.Fprintf(stdout, "Built Error-Tracer %s release archives in %s\n", version, outputArgument); err != nil {
		return cli.Exit(1, "write success message: %v", err)
	}
	return nil
}

func metadataValue(
	ctx context.Context,
	deps dependencies,
	stderr io.Writer,
	environmentVariable, commandName string,
	args ...string,
) (string, error) {
	if value := deps.getenv(environmentVariable); value != "" {
		return value, nil
	}
	output, err := deps.runner.Output(ctx, command{
		dir:    deps.root,
		name:   commandName,
		args:   args,
		stderr: stderr,
	})
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(output), "\n"), nil
}

func archiveBaseName(version string, target target) string {
	return fmt.Sprintf(
		"%s_%s_%s_%s",
		programName,
		version,
		target.operatingSystem,
		target.architecture,
	)
}

func releaseManifest(version, commit, buildDate, sourceEpoch string) string {
	targetNames := make([]string, 0, len(targets))
	for _, currentTarget := range targets {
		targetNames = append(targetNames, currentTarget.String())
	}
	return fmt.Sprintf(
		"version=%s\ncommit=%s\nbuilt_at=%s\nsource_date_epoch=%s\ntargets=%s\n",
		version,
		commit,
		buildDate,
		sourceEpoch,
		strings.Join(targetNames, " "),
	)
}

func copyFile(source, destination string) (returnErr error) {
	sourceFile, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() {
		if err := sourceFile.Close(); returnErr == nil && err != nil {
			returnErr = err
		}
	}()
	info, err := sourceFile.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source is not a regular file")
	}
	destinationFile, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer func() {
		if err := destinationFile.Close(); returnErr == nil && err != nil {
			returnErr = err
		}
	}()
	if _, err := io.Copy(destinationFile, sourceFile); err != nil {
		return err
	}
	return nil
}

func setTreeTimestamp(root string, timestamp time.Time) error {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(paths)))
	for _, path := range paths {
		if err := os.Chtimes(path, timestamp, timestamp); err != nil {
			return err
		}
	}
	return nil
}

func removePOSIXSpace(value string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\v', '\f', '\r':
			return -1
		default:
			return r
		}
	}, value)
}

func environmentWithOverrides(base []string, overrides map[string]string) []string {
	if len(overrides) == 0 {
		return base
	}
	result := make([]string, 0, len(base)+len(overrides))
	seen := make(map[string]bool, len(overrides))
	for _, entry := range base {
		key, _, found := strings.Cut(entry, "=")
		if found {
			if value, overridden := overrides[key]; overridden {
				if !seen[key] {
					result = append(result, key+"="+value)
					seen[key] = true
				}
				continue
			}
		}
		result = append(result, entry)
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		if !seen[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, key+"="+overrides[key])
	}
	return result
}
