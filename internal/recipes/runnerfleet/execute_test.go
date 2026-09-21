package runnerfleet

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
)

func TestExecuteRejectsMissingAndUnknownRecipes(t *testing.T) {
	dir := t.TempDir()
	_, _, err := executeTest(t, testDeps(dir))
	requireExitCode(t, err, 2)

	_, _, err = executeTest(t, testDeps(dir), "check-everything")
	requireExitCode(t, err, 2)
}

func TestExecuteRejectsUnknownFlagsAndSecondRoot(t *testing.T) {
	dir := newDocsRepo(t, "guide.md", "# Guide\n", "# 指南\n")
	for _, args := range [][]string{
		{"docs-structure", "--verbose"},
		{"docs-structure", ".", "."},
		{"docs-structure", "--config"},
		{"version-consistency", "--config", ""},
	} {
		if _, _, err := executeTest(t, testDeps(dir), args...); cliCode(err) != 2 {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
}

func TestExecuteHonoursAnExplicitRoot(t *testing.T) {
	parent := t.TempDir()
	inner := filepath.Join(parent, "repo")
	writeTestFile(t, filepath.Join(inner, "docs", "guide.md"), "# Guide\n\n## Install\n")
	for _, language := range defaultConfig().docsLanguages {
		writeTestFile(t, filepath.Join(inner, "docs", language, "guide.md"), "# 指南\n\n## 安装\n")
	}
	deps := testDeps(parent)

	_, _, err := executeTest(t, deps, "docs-structure", "repo")
	requireExitCode(t, err, 0)

	_, _, err = executeTest(t, deps, "docs-structure", "missing")
	requireExitCode(t, err, 2)
}

func TestExecuteReportsACanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	err := execute(ctx, testDeps(t.TempDir()), []string{"docs-structure"}, &stdout, &stderr)
	requireExitCode(t, err, 2)
}

func TestExecuteToleratesNilWriters(t *testing.T) {
	dir := newDocsRepo(t, "guide.md", "# Guide\n", "# 指南\n")
	if err := execute(context.Background(), testDeps(dir), []string{"docs-structure"}, nil, nil); err != nil {
		t.Fatal(err)
	}
}
