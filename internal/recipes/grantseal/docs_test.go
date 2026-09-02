package grantseal

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDocLanguageBannedAllowedAndClean(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "clean.md"), "The enterprise edition is supported.\n")
	writeTestFile(t, filepath.Join(dir, "scripts", "doc-language-allowlist.txt"), "allowed.md|||not military-grade\n")
	writeTestFile(t, filepath.Join(dir, "allowed.md"), "This is not military-grade software.\n")
	_, _, err := executeTest(t, testDeps(dir), "doc", "language", ".")
	requireExitCode(t, err, 0)

	writeTestFile(t, filepath.Join(dir, "bad.md"), "This is enterprise-grade.\n这是企业级软件。\n")
	_, stderr, err := executeTest(t, testDeps(dir), "doc", "language", ".")
	requireExitCode(t, err, 1)
	if strings.Count(stderr, "banned quality label") != 2 {
		t.Fatalf("stderr=%q", stderr)
	}
}

func TestDocLanguageInputErrorsFailClosed(t *testing.T) {
	dir := t.TempDir()
	_, _, err := executeTest(t, testDeps(dir), "doc", "language", "missing")
	requireExitCode(t, err, 2)
	writeTestFile(t, filepath.Join(dir, "scripts", "doc-language-allowlist.txt"), "malformed\n")
	_, _, err = executeTest(t, testDeps(dir), "doc", "language", ".")
	requireExitCode(t, err, 2)
}

func TestDocConsistencySelftestCases(t *testing.T) {
	tests := []struct {
		name string
		path string
		text string
	}{
		{name: "workflow matrix", path: ".github/workflows/ci.yml", text: "- run: echo ${{ matrix.go }}\n"},
		{name: "literal go version", path: ".github/workflows/ci.yml", text: "  go-version: \"1.26.6\"\n"},
		{name: "stale count", path: "README.md", text: "The full set (23 codes).\n"},
		{name: "denied wire", path: "x.md", text: "Returns LICENSE_FEATURE_DENIED when denied.\n"},
		{name: "reset", path: "x.md", text: "the state is reset.\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTestFile(t, filepath.Join(dir, test.path), test.text)
			_, _, err := executeTest(t, testDeps(dir), "doc", "consistency", ".")
			requireExitCode(t, err, 1)
		})
	}

	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, ".github", "workflows", "ci.yml"), "go-version-file: go.mod\n")
	writeTestFile(t, filepath.Join(dir, "README.md"), strings.Join([]string{
		"The full set is 31 distinct wire codes.",
		"CodeFeatureDenied is a Go alias for the same wire code.",
		"A corrupt state is never silently reset; lifetime licenses do not touch it.",
	}, "\n"))
	_, _, err := executeTest(t, testDeps(dir), "doc", "consistency", ".")
	requireExitCode(t, err, 0)
}

func TestDocConsistencyCountsRuleKinds(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "README.md"), "23 codes\n23 个\nLICENSE_FEATURE_DENIED\n")
	_, stderr, err := executeTest(t, testDeps(dir), "doc", "consistency", ".")
	requireExitCode(t, err, 1)
	if !strings.Contains(err.Error(), "2 documentation-consistency") {
		t.Fatalf("err=%v stderr=%q", err, stderr)
	}
}

func TestDocConsistencyMissingRootFailsClosed(t *testing.T) {
	_, _, err := executeTest(t, testDeps(t.TempDir()), "doc", "consistency", "missing")
	requireExitCode(t, err, 2)
}
