package runnerfleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDocsStructurePassesWhenSequencesMatch(t *testing.T) {
	dir := newDocsRepo(t, "guide.md",
		"# Guide\n\n## Install\n\n### Docker\n\n## Upgrade\n",
		"# 指南\n\n## 安装\n\n### Docker\n\n## 升级\n")

	stdout, _, err := executeTest(t, testDeps(dir), "docs-structure")
	requireExitCode(t, err, 0)
	if !strings.Contains(stdout, "docs/guide.md: 4 个标题，zh fr de ko ja 全部一致") {
		t.Fatalf("stdout=%q", stdout)
	}
	if !strings.Contains(stdout, "各语言文档章节结构一致") {
		t.Fatalf("stdout=%q", stdout)
	}
}

func TestDocsStructureReportsMissingTranslation(t *testing.T) {
	dir := newDocsRepo(t, "guide.md", "# Guide\n\n## Install\n", "# 指南\n\n## 安装\n")
	if err := os.Remove(filepath.Join(dir, "docs", "ko", "guide.md")); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := executeTest(t, testDeps(dir), "docs-structure")
	requireExitCode(t, err, 1)
	if !strings.Contains(stdout, "::error file=docs/guide.md::缺少译文 docs/ko/guide.md") {
		t.Fatalf("stdout=%q", stdout)
	}
	if strings.Contains(stdout, "各语言文档章节结构一致") {
		t.Fatalf("a failing run must not print the all-clear: %q", stdout)
	}
}

func TestDocsStructureReportsSequenceDrift(t *testing.T) {
	dir := newDocsRepo(t, "development.md",
		"# Development\n\n## Build\n\n## Tests\n",
		"# 开发\n\n## 构建\n")

	stdout, _, err := executeTest(t, testDeps(dir), "docs-structure")
	requireExitCode(t, err, 1)
	if !strings.Contains(stdout, "::error file=docs/zh/development.md::章节结构与 docs/development.md 不一致（英文 3 个标题，本文件 2 个）。") {
		t.Fatalf("stdout=%q", stdout)
	}
	if !strings.Contains(stdout, "  英文层级序列: # ## ##") {
		t.Fatalf("stdout=%q", stdout)
	}
	if !strings.Contains(stdout, "  本文件层级序列: # ##") {
		t.Fatalf("stdout=%q", stdout)
	}
	if got := strings.Count(stdout, "章节结构与"); got != 5 {
		t.Fatalf("want one report per language, got %d: %q", got, stdout)
	}
}

// The shell toggled its in-a-code-block flag on backticks only, so a
// tilde-fenced block was not a block and a column-1 "# comment" inside one
// counted as a heading. Where the two sides used different markers around the
// same block, five structurally identical translations were all reported wrong.
func TestDocsStructureTildeFenceIsACodeBlock(t *testing.T) {
	dir := newDocsRepo(t, "guide.md",
		"# Guide\n\n~~~sh\n# not a heading\n~~~\n\n## Install\n",
		"# 指南\n\n```sh\n# 不是标题\n```\n\n## 安装\n")

	stdout, _, err := executeTest(t, testDeps(dir), "docs-structure")
	requireExitCode(t, err, 0)
	if !strings.Contains(stdout, "docs/guide.md: 2 个标题，") {
		t.Fatalf("the fenced comment must not be counted as a heading: %q", stdout)
	}
}

// A fence closes only on the character that opened it, so the inner marker is
// content and the block does not end early.
func TestDocsStructureFenceClosesOnlyOnItsOwnMarker(t *testing.T) {
	body := "# Guide\n\n```\n~~~\n# still inside the block\n```\n\n## Install\n"
	dir := newDocsRepo(t, "guide.md", body, body)

	stdout, _, err := executeTest(t, testDeps(dir), "docs-structure")
	requireExitCode(t, err, 0)
	if !strings.Contains(stdout, "docs/guide.md: 2 个标题，") {
		t.Fatalf("stdout=%q", stdout)
	}
}

func TestDocsStructureHeadingNeedsWhitespaceAfterHashes(t *testing.T) {
	body := "# Guide\n\n#nope\n\n## Install\n"
	dir := newDocsRepo(t, "guide.md", body, body)

	stdout, _, err := executeTest(t, testDeps(dir), "docs-structure")
	requireExitCode(t, err, 0)
	if !strings.Contains(stdout, "docs/guide.md: 2 个标题，") {
		t.Fatalf("stdout=%q", stdout)
	}
}

// The shell skipped any document whose English original was absent, so a run
// with no documentation tree at all compared nothing and printed its all-clear.
func TestDocsStructureFailsClosedWhenNothingWasCompared(t *testing.T) {
	dir := t.TempDir()
	stdout, _, err := executeTest(t, testDeps(dir), "docs-structure")
	requireExitCode(t, err, 2)
	if strings.Contains(stdout, "各语言文档章节结构一致") {
		t.Fatalf("a run that compared nothing must not print the all-clear: %q", stdout)
	}
}

// One failing document used to suppress the result line of every document after
// it, which read as though those had not been checked.
func TestDocsStructureReportsEachDocumentSeparately(t *testing.T) {
	dir := newDocsRepo(t, "development.md", "# Development\n\n## Build\n", "# 开发\n")
	writeTestFile(t, filepath.Join(dir, "docs", "guide.md"), "# Guide\n\n## Install\n")
	for _, language := range defaultConfig().docsLanguages {
		writeTestFile(t, filepath.Join(dir, "docs", language, "guide.md"), "# 指南\n\n## 安装\n")
	}

	stdout, _, err := executeTest(t, testDeps(dir), "docs-structure")
	requireExitCode(t, err, 1)
	if !strings.Contains(stdout, "docs/guide.md: 2 个标题，zh fr de ko ja 全部一致") {
		t.Fatalf("the healthy document should still report its result: %q", stdout)
	}
	if !strings.Contains(stdout, "::error file=docs/zh/development.md::") {
		t.Fatalf("stdout=%q", stdout)
	}
}

func TestDocsStructureHonoursConfiguredLanguagesAndFiles(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "handbook", "intro.md"), "# Intro\n\n## Setup\n")
	writeTestFile(t, filepath.Join(dir, "handbook", "es", "intro.md"), "# Intro\n\n## Preparación\n")
	writeTestFile(t, filepath.Join(dir, "scripts", "ci-recipes.conf"), strings.Join([]string{
		"docs_root = handbook",
		"docs_languages = es",
		"docs_files = intro.md",
		"",
	}, "\n"))

	stdout, _, err := executeTest(t, testDeps(dir), "docs-structure")
	requireExitCode(t, err, 0)
	if !strings.Contains(stdout, "handbook/intro.md: 2 个标题，es 全部一致") {
		t.Fatalf("stdout=%q", stdout)
	}
}

func TestDocsStructureOnlyDocConfigNarrowsTheRun(t *testing.T) {
	dir := newDocsRepo(t, "guide.md", "# Guide\n\n## Install\n", "# 指南\n\n## 安装\n")
	onlyDoc(t, dir, "guide.md")

	stdout, _, err := executeTest(t, testDeps(dir), "docs-structure")
	requireExitCode(t, err, 0)
	if strings.Contains(stdout, "development.md") {
		t.Fatalf("stdout=%q", stdout)
	}
}
