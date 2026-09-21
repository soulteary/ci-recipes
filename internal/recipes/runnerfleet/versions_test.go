package runnerfleet

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionConsistencyPassesWhenEveryReferenceAgrees(t *testing.T) {
	dir := newVersionRepo(t, "v1.6.0")
	writeTestFile(t, filepath.Join(dir, "README.md"), "Pull ghcr.io/soulteary/runner-fleet:v1.6.0 to start.\n")
	writeTestFile(t, filepath.Join(dir, "docker-compose.yml"), "    image: ghcr.io/soulteary/runner-fleet:v1.6.0\n")
	writeTestFile(t, filepath.Join(dir, "Makefile"), "RUNNER_IMAGE ?= ghcr.io/soulteary/runner-fleet:v1.6.0-runner\n")

	stdout, _, err := executeTest(t, testDeps(dir), "version-consistency")
	requireExitCode(t, err, 0)
	if !strings.Contains(stdout, "基准版本（取自 internal/config/config.go）: v1.6.0") {
		t.Fatalf("stdout=%q", stdout)
	}
	if !strings.Contains(stdout, "所有文档与示例中的版本号均为 v1.6.0") {
		t.Fatalf("stdout=%q", stdout)
	}
}

func TestVersionConsistencyAnnotatesStaleReference(t *testing.T) {
	dir := newVersionRepo(t, "v1.6.0")
	writeTestFile(t, filepath.Join(dir, "README.md"), "current\n\nPull runner-fleet:v1.5.1 to start.\n")

	stdout, stderr, err := executeTest(t, testDeps(dir), "version-consistency")
	requireExitCode(t, err, 1)
	if !strings.Contains(stdout, "::error file=README.md,line=3::版本号 v1.5.1 与基准 v1.6.0 不一致") {
		t.Fatalf("stdout=%q", stdout)
	}
	if !strings.Contains(stderr, "  README.md:3 出现 v1.5.1，应为 v1.6.0") {
		t.Fatalf("stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "version-check-ignore") {
		t.Fatalf("stderr should explain the escape hatch: %q", stderr)
	}
	if strings.Contains(stdout, "所有文档与示例中的版本号均为") {
		t.Fatalf("a failing run must not print the all-clear: %q", stdout)
	}
}

// A path containing a space was dropped by the shell's unquoted "for" loop:
// word splitting turned it into two nonexistent paths, both skipped by the
// "[ -f ]" guard, and a tree whose only stale reference lived in such a file
// passed. The list is a []string here, so there is no splitting step.
func TestVersionConsistencyScansPathsContainingSpaces(t *testing.T) {
	dir := newVersionRepo(t, "v1.6.0")
	writeTestFile(t, filepath.Join(dir, "stale doc.md"), "Pull runner-fleet:v0.0.1 to start.\n")

	stdout, _, err := executeTest(t, testDeps(dir), "version-consistency")
	requireExitCode(t, err, 1)
	if !strings.Contains(stdout, "::error file=stale doc.md,line=1::版本号 v0.0.1 与基准 v1.6.0 不一致") {
		t.Fatalf("stdout=%q", stdout)
	}
}

// The shell built its file list inside a command substitution ending in
// "|| true", so a git that refused to run produced an empty list and a green
// check. Not having enumerated anything is not the same as having found nothing.
func TestVersionConsistencyFailsClosedWhenGitRefuses(t *testing.T) {
	dir := newVersionRepo(t, "v1.6.0")
	writeTestFile(t, filepath.Join(dir, "stale.md"), "runner-fleet:v0.0.1\n")
	deps := testDeps(dir)
	deps.runner = failingGit()

	stdout, _, err := executeTest(t, deps, "version-consistency")
	requireExitCode(t, err, 2)
	if strings.Contains(stdout, "所有文档与示例中的版本号均为") {
		t.Fatalf("an unenumerated run must not print the all-clear: %q", stdout)
	}
	if !strings.Contains(err.Error(), "dubious ownership") {
		t.Fatalf("git's own diagnosis should survive: %v", err)
	}
}

func TestVersionConsistencyIgnoreMarkerExemptsALine(t *testing.T) {
	dir := newVersionRepo(t, "v1.6.0")
	writeTestFile(t, filepath.Join(dir, "HISTORY.md"),
		"Releases v1.0.1 and v1.1.0 missed this. version-check-ignore\nStill v1.6.0 today.\n")

	_, _, err := executeTest(t, testDeps(dir), "version-consistency")
	requireExitCode(t, err, 0)
}

func TestVersionConsistencyReadsLdflagsAssignments(t *testing.T) {
	dir := newVersionRepo(t, "v1.6.0")
	writeTestFile(t, filepath.Join(dir, "release.yml"), "        LDFLAGS=\"-X main.Version=1.5.1\"\n")

	stdout, _, err := executeTest(t, testDeps(dir), "version-consistency")
	requireExitCode(t, err, 1)
	if !strings.Contains(stdout, "::error file=release.yml,line=1::版本号 v1.5.1 与基准 v1.6.0 不一致") {
		t.Fatalf("stdout=%q", stdout)
	}
}

func TestVersionConsistencyReportsEveryDistinctVersionOnALine(t *testing.T) {
	dir := newVersionRepo(t, "v1.6.0")
	writeTestFile(t, filepath.Join(dir, "notes.md"), "from v1.4.0 to v1.5.1, and v1.4.0 again\n")

	stdout, _, err := executeTest(t, testDeps(dir), "version-consistency")
	requireExitCode(t, err, 1)
	if got := strings.Count(stdout, "::error file=notes.md,line=1::"); got != 2 {
		t.Fatalf("want 2 annotations for 2 distinct stale versions, got %d: %q", got, stdout)
	}
}

func TestVersionConsistencySkipsExcludedPaths(t *testing.T) {
	dir := newVersionRepo(t, "v1.6.0")
	writeTestFile(t, filepath.Join(dir, "CHANGELOG.md"), "## [1.0.1]\n\nSee v1.0.1 and v1.1.0.\n")

	_, _, err := executeTest(t, testDeps(dir), "version-consistency")
	requireExitCode(t, err, 0)
}

// --others --exclude-standard is what makes a local run see what CI will see:
// a file that has been added but not committed is still scanned, and an ignored
// local file is not.
func TestVersionConsistencyScansUntrackedButNotIgnoredFiles(t *testing.T) {
	dir := newVersionRepo(t, "v1.6.0")
	writeTestFile(t, filepath.Join(dir, ".gitignore"), "local.md\n")
	writeTestFile(t, filepath.Join(dir, "local.md"), "runner-fleet:v0.0.1\n")
	_, _, err := executeTest(t, testDeps(dir), "version-consistency")
	requireExitCode(t, err, 0)

	writeTestFile(t, filepath.Join(dir, "added.md"), "runner-fleet:v0.0.2\n")
	stdout, _, err := executeTest(t, testDeps(dir), "version-consistency")
	requireExitCode(t, err, 1)
	if !strings.Contains(stdout, "::error file=added.md,line=1::") {
		t.Fatalf("stdout=%q", stdout)
	}
}

func TestVersionConsistencyTrackedFilesAreScanned(t *testing.T) {
	dir := newVersionRepo(t, "v1.6.0")
	writeTestFile(t, filepath.Join(dir, "tracked.md"), "runner-fleet:v0.0.3\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "seed")

	stdout, _, err := executeTest(t, testDeps(dir), "version-consistency")
	requireExitCode(t, err, 1)
	if !strings.Contains(stdout, "::error file=tracked.md,line=1::") {
		t.Fatalf("stdout=%q", stdout)
	}
}

func TestVersionConsistencyBaselineProblemsFailClosed(t *testing.T) {
	missing := t.TempDir()
	initGitRepo(t, missing)
	_, _, err := executeTest(t, testDeps(missing), "version-consistency")
	requireExitCode(t, err, 2)

	unparsable := t.TempDir()
	initGitRepo(t, unparsable)
	writeTestFile(t, filepath.Join(unparsable, "internal", "config", "config.go"), "package config\n\nvar tag = \"latest\"\n")
	stdout, _, err := executeTest(t, testDeps(unparsable), "version-consistency")
	requireExitCode(t, err, 2)
	if strings.Contains(stdout, "所有文档与示例中的版本号均为") {
		t.Fatalf("stdout=%q", stdout)
	}
}

func TestVersionConsistencyHonoursConfiguredBaselineAndMarker(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	writeTestFile(t, filepath.Join(dir, "version.txt"), "release: v2.0.0\n")
	writeTestFile(t, filepath.Join(dir, "scripts", "ci-recipes.conf"), strings.Join([]string{
		"version_source = version.txt",
		"version_baseline_regex = release: (v[0-9]+\\.[0-9]+\\.[0-9]+)",
		"version_ignore_marker = keep-old-version",
		"version_globs = *.md *.txt",
		"",
	}, "\n"))
	writeTestFile(t, filepath.Join(dir, "guide.md"), "use v1.0.0 keep-old-version\nuse v2.0.0\n")

	stdout, _, err := executeTest(t, testDeps(dir), "version-consistency")
	requireExitCode(t, err, 0)
	if !strings.Contains(stdout, "基准版本（取自 version.txt）: v2.0.0") {
		t.Fatalf("stdout=%q", stdout)
	}

	writeTestFile(t, filepath.Join(dir, "guide.md"), "use v1.0.0\n")
	_, _, err = executeTest(t, testDeps(dir), "version-consistency")
	requireExitCode(t, err, 1)
}
