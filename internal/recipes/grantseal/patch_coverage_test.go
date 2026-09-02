package grantseal

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestPatchCoverageSelftestCases(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	name := filepath.Join(dir, "pkg", "demo", "demo.go")
	writeTestFile(t, name, "package demo\n\nfunc Base() int {\n\treturn 1\n}\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "base")
	base := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	writeTestFile(t, name, "package demo\n\nfunc Base() int {\n\treturn 1\n}\n\nfunc Added() int {\n\treturn 2\n}\n\nfunc Uncovered() int {\n\treturn 3\n}\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "change")
	covered := "mode: atomic\nexample.com/m/pkg/demo/demo.go:3.14,5.2 1 1\nexample.com/m/pkg/demo/demo.go:7.15,9.2 1 1\nexample.com/m/pkg/demo/demo.go:11.20,13.2 1 1\n"
	partial := strings.Replace(covered, "11.20,13.2 1 1", "11.20,13.2 1 0", 1)
	writeTestFile(t, filepath.Join(dir, "covered.out"), covered)
	writeTestFile(t, filepath.Join(dir, "partial.out"), partial)

	_, _, err := executeTest(t, testDeps(dir), "patch", "coverage", "covered.out", base, "90")
	requireExitCode(t, err, 0)
	stdout, _, err := executeTest(t, testDeps(dir), "patch", "coverage", "partial.out", base, "90")
	requireExitCode(t, err, 1)
	if !strings.Contains(stdout, "50.00%") {
		t.Fatalf("stdout=%q", stdout)
	}
	_, _, err = executeTest(t, testDeps(dir), "patch", "coverage", "partial.out", base, "40")
	requireExitCode(t, err, 0)
	// With no explicit base, the recipe falls back to HEAD~1 in this local repo.
	_, _, err = executeTest(t, testDeps(dir), "patch", "coverage", "covered.out")
	requireExitCode(t, err, 0)
}

func TestPatchCoverageInitialCommitHasNoBase(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	writeTestFile(t, filepath.Join(dir, "x.go"), "package x\n")
	writeTestFile(t, filepath.Join(dir, "coverage.out"), "mode: atomic\nexample/x.go:1.1,1.10 1 1\n")
	runGit(t, dir, "add", "x.go")
	runGit(t, dir, "commit", "-qm", "initial")
	_, stderr, err := executeTest(t, testDeps(dir), "patch", "coverage", "coverage.out")
	requireExitCode(t, err, 0)
	if !strings.Contains(stderr, "no base ref") {
		t.Fatalf("stderr=%q", stderr)
	}
}

func TestPatchCoverageRejectsMalformedProfileAndThreshold(t *testing.T) {
	dir := t.TempDir()
	for _, profile := range []string{
		"", "mode: atomic\n", "not-a-mode\nfoo", "mode: mystery\nx.go:1.1,1.2 1 1\n", "mode: atomic\nmalformed\n",
	} {
		writeTestFile(t, filepath.Join(dir, "coverage.out"), profile)
		_, _, err := executeTest(t, testDeps(dir), "patch", "coverage", "coverage.out", "HEAD", "90")
		requireExitCode(t, err, 2)
	}
	writeTestFile(t, filepath.Join(dir, "coverage.out"), "mode: atomic\nx.go:1.1,1.2 1 1\n")
	for _, threshold := range []string{"NaN", "+Inf", "-1", "101", "wat"} {
		_, _, err := executeTest(t, testDeps(dir), "patch", "coverage", "coverage.out", "HEAD", threshold)
		requireExitCode(t, err, 2)
	}
}

func TestPatchCoverageFailsClosedOnDiffFailure(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "coverage.out"), "mode: atomic\nx.go:1.1,1.2 1 1\n")
	deps := testDeps(dir)
	deps.runner = fakeRunner{
		lookPath: func(string) (string, error) { return "/git", nil },
		run: func(_ context.Context, cmd command) error {
			if cmd.Name == "git" && len(cmd.Args) != 0 && cmd.Args[0] == "diff" {
				_, _ = io.WriteString(cmd.Stderr, "bad base")
				return errors.New("diff failed")
			}
			return errors.New("unexpected command")
		},
	}
	_, _, err := executeTest(t, deps, "patch", "coverage", "coverage.out", "missing", "90")
	requireExitCode(t, err, 2)
}

func TestPatchCoverageMissingAndAmbiguousProfilePathsFail(t *testing.T) {
	added := map[string]map[int]struct{}{"pkg/demo.go": {3: {}}}
	profile := coverageProfile{blocks: map[string][]coverageBlock{"example/a.go": {{start: 1, end: 3, count: 1}}}}
	if _, _, _, err := scorePatchCoverage(added, profile); err == nil {
		t.Fatal("missing profile path accepted")
	}
	profile.blocks = map[string][]coverageBlock{
		"one/pkg/demo.go": {{start: 1, end: 3, count: 1}},
		"two/pkg/demo.go": {{start: 1, end: 3, count: 1}},
	}
	if _, _, _, err := scorePatchCoverage(added, profile); err == nil {
		t.Fatal("ambiguous profile path accepted")
	}
}

func TestPatchCoverageZeroStatementBlockIsValidAndNotScored(t *testing.T) {
	profile, err := parseCoverageProfile([]byte("mode: atomic\nexample/x.go:1.1,2.1 0 0\n"))
	if err != nil {
		t.Fatalf("parse zero-statement block: %v", err)
	}
	covered, uncovered, _, err := scorePatchCoverage(map[string]map[int]struct{}{"x.go": {1: {}}}, profile)
	if err != nil || covered != 0 || uncovered != 0 {
		t.Fatalf("covered=%d uncovered=%d err=%v", covered, uncovered, err)
	}
}

func TestPatchCoverageCommentOnlyFileWithoutProfilePasses(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	writeTestFile(t, filepath.Join(dir, "base.go"), "package demo\n\nfunc Base() int { return 1 }\n")
	runGit(t, dir, "add", "base.go")
	runGit(t, dir, "commit", "-qm", "base")
	base := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	writeTestFile(t, filepath.Join(dir, "doc.go"), "// Package demo documents the package.\npackage demo\n")
	runGit(t, dir, "add", "doc.go")
	runGit(t, dir, "commit", "-qm", "docs")
	writeTestFile(t, filepath.Join(dir, "coverage.out"), "mode: atomic\nexample/base.go:3.1,3.29 1 1\n")
	stdout, _, err := executeTest(t, testDeps(dir), "patch", "coverage", "coverage.out", base, "90")
	requireExitCode(t, err, 0)
	if !strings.Contains(stdout, "no coverable statements") {
		t.Fatalf("stdout=%q", stdout)
	}
}

func TestPatchCoverageMergeBaseOnlyFallsBackOnExitOne(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "coverage.out"), "mode: atomic\nx.go:1.1,1.2 1 1\n")
	for _, test := range []struct {
		name      string
		mergeCode int
		want      int
	}{
		{name: "no common ancestor", mergeCode: 1, want: 0},
		{name: "repository error", mergeCode: 2, want: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			deps := testDeps(dir)
			deps.runner = fakeRunner{
				lookPath: func(string) (string, error) { return "/git", nil },
				run: func(_ context.Context, cmd command) error {
					joined := strings.Join(cmd.Args, " ")
					switch {
					case strings.Contains(joined, "origin/HEAD") && cmd.Args[0] == "rev-parse":
						_, _ = io.WriteString(cmd.Stdout, "origin/HEAD\n")
						return nil
					case cmd.Args[0] == "merge-base":
						_, _ = io.WriteString(cmd.Stderr, "merge-base failed")
						return fakeExitError{code: test.mergeCode, message: "merge-base failed"}
					case strings.Contains(joined, "HEAD~1"):
						_, _ = io.WriteString(cmd.Stdout, "base\n")
						return nil
					case cmd.Args[0] == "diff":
						return nil
					default:
						return errors.New("unexpected command: " + joined)
					}
				},
			}
			_, _, err := executeTest(t, deps, "patch", "coverage", "coverage.out")
			requireExitCode(t, err, test.want)
		})
	}
}

func TestParseAddedLines(t *testing.T) {
	diff := []byte("diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1,0 +2,2 @@\n+one\n+two\n")
	added, err := parseAddedLines(diff)
	if err != nil || len(added["x.go"]) != 2 {
		t.Fatalf("added=%v err=%v", added, err)
	}
	if _, err := parseAddedLines([]byte("+++ b/x.go\n@@ broken\n+x\n")); err == nil {
		t.Fatal("malformed hunk accepted")
	}
	if _, err := parseAddedLines([]byte("not a unified diff\n")); err == nil {
		t.Fatal("non-diff input accepted")
	}
}
