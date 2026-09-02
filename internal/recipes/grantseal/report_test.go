package grantseal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func fixedReportDeps(dir string) dependencies {
	deps := testDeps(dir)
	values := map[string]string{
		"REPORT_COMMIT":       "new-commit",
		"REPORT_GENERATED_AT": "2026-09-02T03:04:05Z",
		"REPORT_GO_VERSION":   "go1.22.9",
		"REPORT_OS":           "linux",
		"REPORT_ARCH":         "amd64",
	}
	deps.getenv = func(name string) string { return values[name] }
	deps.now = func() time.Time { return time.Date(2026, 9, 2, 3, 4, 5, 0, time.UTC) }
	return deps
}

func TestInjectReportEnvironmentReplacesExistingAndPreservesOrder(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "report.json")
	writeTestFile(t, name, `{
  "schema_version": "1.0",
  "environment": {"commit": "old"},
  "tests": {"total": 1}
}
`)
	stdout, _, err := executeTest(t, fixedReportDeps(dir), "report", "inject-environment", "report.json")
	requireExitCode(t, err, 0)
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, `"old"`) || !strings.Contains(text, `"commit": "new-commit"`) {
		t.Fatalf("environment not replaced:\n%s", text)
	}
	if strings.Index(text, `"schema_version"`) > strings.Index(text, `"environment"`) ||
		strings.Index(text, `"environment"`) > strings.Index(text, `"tests"`) {
		t.Fatalf("field order changed:\n%s", text)
	}
	if !strings.Contains(stdout, "new-commit") {
		t.Fatalf("stdout=%q", stdout)
	}
}

func TestInjectReportEnvironmentRejectsDuplicateOrInvalidJSON(t *testing.T) {
	for _, contents := range []string{`{"x":1,"x":2}`, `[1,2]`, `{broken`} {
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, "report.json"), contents)
		_, _, err := executeTest(t, fixedReportDeps(dir), "report", "inject-environment", "report.json")
		requireExitCode(t, err, 2)
	}
}

func TestReplaceEnvironmentMovesItAfterSchemaVersion(t *testing.T) {
	fields := []orderedJSONField{
		{name: "environment", value: json.RawMessage(`{"commit":"old"}`)},
		{name: "schema_version", value: json.RawMessage(`"1"`)},
		{name: "tests", value: json.RawMessage(`{}`)},
	}
	got := replaceEnvironmentField(fields, json.RawMessage(`{"commit":"new"}`))
	if len(got) != 3 || got[0].name != "schema_version" || got[1].name != "environment" || got[2].name != "tests" || strings.Contains(string(got[1].value), "old") {
		t.Fatalf("fields=%+v", got)
	}
}

func TestReplaceEnvironmentIsFirstWithoutSchemaVersion(t *testing.T) {
	fields := []orderedJSONField{
		{name: "tests", value: json.RawMessage(`{}`)},
		{name: "environment", value: json.RawMessage(`{"commit":"old"}`)},
	}
	got := replaceEnvironmentField(fields, json.RawMessage(`{"commit":"new"}`))
	if len(got) != 2 || got[0].name != "environment" || got[1].name != "tests" || strings.Contains(string(got[0].value), "old") {
		t.Fatalf("fields=%+v", got)
	}
}

func TestInjectReportEnvironmentAtomicWriteFailureLeavesInput(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "report.json")
	original := []byte(`{"schema_version":"1"}`)
	if err := os.WriteFile(name, original, 0o640); err != nil {
		t.Fatal(err)
	}
	deps := fixedReportDeps(dir)
	deps.writeAtomic = func(context.Context, string, []byte) error { return errors.New("disk full") }
	_, _, err := executeTest(t, deps, "report", "inject-environment", "report.json")
	requireExitCode(t, err, 2)
	got, readErr := os.ReadFile(name)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("input changed after failed write: %q", got)
	}
}

func TestInjectReportEnvironmentCancellationPreservesInput(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "report.json")
	original := []byte(`{"schema_version":"1"}`)
	if err := os.WriteFile(name, original, 0o640); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps := testDeps(dir)
	deps.getenv = func(string) string { return "" }
	runs := 0
	deps.runner = fakeRunner{run: func(context.Context, command) error {
		runs++
		cancel()
		return context.Canceled
	}}
	writes := 0
	deps.writeAtomic = func(context.Context, string, []byte) error {
		writes++
		return nil
	}
	var stdout, stderr bytes.Buffer
	err := execute(ctx, deps, []string{"report", "inject-environment", "report.json"}, nil, &stdout, &stderr)
	requireExitCode(t, err, 2)
	if !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("error = %v, want cancellation", err)
	}
	if runs != 1 || writes != 0 {
		t.Fatalf("commands=%d writes=%d, want one interrupted command and no write", runs, writes)
	}
	got, readErr := os.ReadFile(name)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("input changed after cancellation: %q", got)
	}
}

func TestInjectReportEnvironmentCancellationDuringAtomicWritePreservesInput(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "report.json")
	original := []byte(`{"schema_version":"1"}`)
	if err := os.WriteFile(name, original, 0o640); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps := fixedReportDeps(dir)
	deps.writeAtomic = func(writeCtx context.Context, name string, data []byte) error {
		cancel()
		return atomicWriteFile(writeCtx, name, data)
	}
	var stdout, stderr bytes.Buffer
	err := execute(ctx, deps, []string{"report", "inject-environment", "report.json"}, nil, &stdout, &stderr)
	requireExitCode(t, err, 2)
	if !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("error = %v, want cancellation", err)
	}
	got, readErr := os.ReadFile(name)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("input changed after cancellation: %q", got)
	}
}

func TestDeriveReportEnvironmentFallbacks(t *testing.T) {
	deps := testDeps(t.TempDir())
	deps.getenv = func(string) string { return "" }
	deps.now = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600)) }
	deps.runner = fakeRunner{run: func(_ context.Context, cmd command) error {
		switch strings.Join(append([]string{cmd.Name}, cmd.Args...), " ") {
		case "git rev-parse HEAD":
			_, _ = cmd.Stdout.Write([]byte("abc123\n"))
		case "go version":
			_, _ = cmd.Stdout.Write([]byte("go version go1.22.8 linux/amd64\n"))
		case "go env GOOS":
			_, _ = cmd.Stdout.Write([]byte("linux\n"))
		case "go env GOARCH":
			_, _ = cmd.Stdout.Write([]byte("arm64\n"))
		default:
			return errors.New("unexpected command")
		}
		return nil
	}}
	got, err := deriveReportEnvironment(context.Background(), deps)
	if err != nil {
		t.Fatal(err)
	}
	if got.Commit != "abc123" || got.GeneratedAt != "2026-01-02T02:04:05Z" || got.GoVersion != "go1.22.8" || got.OS != "linux" || got.Arch != "arm64" {
		t.Fatalf("environment=%+v", got)
	}
}

func qualityFixture() string {
	return `{
  "schema_version": "1.0",
  "environment": {"commit":"abc","generated_at":"2026-09-02T00:00:00Z","go_version":"go1.22.8","os":"linux","arch":"amd64"},
  "coverage": {"covered_statements":95,"total_statements":100,"percentage":95.0,"threshold":93},
  "packages": [
    {"name":"pkg/license","coverage":96.1},
    {"name":"pkg/fingerprint","coverage":94.2},
    {"name":"internal/issuer","coverage":93.3},
    {"name":"cmd/license-tool","coverage":92.4}
  ]
}`
}

func setupQualityDocs(t *testing.T, dir string, english, chinese string) {
	t.Helper()
	setupQualityDocsWithReport(t, dir, qualityFixture(), english, chinese)
}

func setupQualityDocsWithReport(t *testing.T, dir, report, english, chinese string) {
	t.Helper()
	writeTestFile(t, filepath.Join(dir, ".github", "go-test-report.json"), report)
	writeTestFile(t, filepath.Join(dir, "docs", "enUS", "quality.md"), english)
	writeTestFile(t, filepath.Join(dir, "docs", "zhCN", "quality.md"), chinese)
}

func mutateQualityFixture(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	var report map[string]any
	if err := json.Unmarshal([]byte(qualityFixture()), &report); err != nil {
		t.Fatal(err)
	}
	mutate(report)
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestGenerateQualityDocs(t *testing.T) {
	dir := t.TempDir()
	doc := "before\n" + coverageBegin + "\nstale\n" + coverageEnd + "\nafter\n"
	setupQualityDocs(t, dir, doc, doc)
	stdout, _, err := executeTest(t, testDeps(dir), "quality", "docs")
	requireExitCode(t, err, 0)
	for _, name := range []string{filepath.Join(dir, "docs", "enUS", "quality.md"), filepath.Join(dir, "docs", "zhCN", "quality.md")} {
		data, readErr := os.ReadFile(name)
		if readErr != nil {
			t.Fatal(readErr)
		}
		text := string(data)
		for _, want := range []string{"before\n", "after\n", "`95.00%`", "`pkg/license`", "`96.1%`", "`abc`", coverageEnd} {
			if !strings.Contains(text, want) {
				t.Errorf("%s missing %q:\n%s", name, want, text)
			}
		}
	}
	if !strings.Contains(stdout, "docs/enUS/quality.md") || !strings.Contains(stdout, "docs/zhCN/quality.md") {
		t.Fatalf("stdout=%q", stdout)
	}
}

func TestGenerateQualityDocsRejectsMissingRequiredFieldsAndSchema(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "missing schema", mutate: func(report map[string]any) { delete(report, "schema_version") }},
		{name: "unsupported schema", mutate: func(report map[string]any) { report["schema_version"] = "2.0" }},
		{name: "missing covered statements", mutate: func(report map[string]any) {
			delete(report["coverage"].(map[string]any), "covered_statements")
		}},
		{name: "null total statements", mutate: func(report map[string]any) {
			report["coverage"].(map[string]any)["total_statements"] = nil
		}},
		{name: "missing percentage", mutate: func(report map[string]any) {
			delete(report["coverage"].(map[string]any), "percentage")
		}},
		{name: "missing threshold", mutate: func(report map[string]any) {
			delete(report["coverage"].(map[string]any), "threshold")
		}},
		{name: "missing packages", mutate: func(report map[string]any) { delete(report, "packages") }},
		{name: "null packages", mutate: func(report map[string]any) { report["packages"] = nil }},
		{name: "missing package name", mutate: func(report map[string]any) {
			delete(report["packages"].([]any)[0].(map[string]any), "name")
		}},
		{name: "missing package coverage", mutate: func(report map[string]any) {
			delete(report["packages"].([]any)[0].(map[string]any), "coverage")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			doc := "before\n" + coverageBegin + "\nstale\n" + coverageEnd + "\nafter\n"
			setupQualityDocsWithReport(t, dir, mutateQualityFixture(t, tc.mutate), doc, doc)
			english := filepath.Join(dir, "docs", "enUS", "quality.md")
			chinese := filepath.Join(dir, "docs", "zhCN", "quality.md")
			originalEnglish, _ := os.ReadFile(english)
			originalChinese, _ := os.ReadFile(chinese)
			_, _, err := executeTest(t, testDeps(dir), "quality", "docs")
			requireExitCode(t, err, 2)
			gotEnglish, _ := os.ReadFile(english)
			gotChinese, _ := os.ReadFile(chinese)
			if !bytes.Equal(gotEnglish, originalEnglish) || !bytes.Equal(gotChinese, originalChinese) {
				t.Fatal("quality docs changed after invalid report")
			}
		})
	}
}

func TestRenderQualityBlockMatchesLegacyLayout(t *testing.T) {
	decoder := json.NewDecoder(strings.NewReader(qualityFixture()))
	decoder.UseNumber()
	var report qualityReport
	if err := decoder.Decode(&report); err != nil {
		t.Fatal(err)
	}
	want := `<!-- BEGIN:GENERATED-COVERAGE -->
<!-- Generated from .github/go-test-report.json by scripts/generate-quality-docs.sh. Do not edit by hand. -->

## Environment of record

- Commit: ` + "`abc`" + `
- Generated (UTC): ` + "`2026-09-02T00:00:00Z`" + `
- Go version: ` + "`go1.22.8`" + `
- OS / arch: ` + "`linux/amd64`" + `

These values come from the ` + "`environment`" + ` block of ` + "`.github/go-test-report.json`" + `, the single machine-readable source of truth, so they cannot drift from the recorded run.

## Total coverage

- **Total:** ` + "`95.00%`" + ` of statements (95/100)
- Coverage gate (CI): ` + "`93%`" + ` (floor of the measured total; set so the same commit cannot fail its own gate)

The Coverage badge in the root README is generated by CI from the same run.

## Per-package coverage

| Package | Coverage |
| ------- | -------- |
| ` + "`pkg/license`" + ` | ` + "`96.1%`" + ` |
| ` + "`pkg/fingerprint`" + ` | ` + "`94.2%`" + ` |
| ` + "`internal/issuer`" + ` | ` + "`93.3%`" + ` |
| ` + "`cmd/license-tool`" + ` | ` + "`92.4%`" + ` |
| ` + "`examples/client`" + ` | ` + "`0.0%`" + ` (illustrative example, no tests) |

<!-- END:GENERATED-COVERAGE -->`
	if got := renderQualityBlock(report, "en"); got != want {
		t.Fatalf("rendered legacy block differs\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRenderQualityBlockMatchesLegacyChineseLayout(t *testing.T) {
	decoder := json.NewDecoder(strings.NewReader(qualityFixture()))
	decoder.UseNumber()
	var report qualityReport
	if err := decoder.Decode(&report); err != nil {
		t.Fatal(err)
	}
	want := `<!-- BEGIN:GENERATED-COVERAGE -->
<!-- 由 scripts/generate-quality-docs.sh 从 .github/go-test-report.json 生成，请勿手工编辑。 -->

## 记录环境

- 提交：` + "`abc`" + `
- 生成时间（UTC）：` + "`2026-09-02T00:00:00Z`" + `
- Go 版本：` + "`go1.22.8`" + `
- 操作系统 / 架构：` + "`linux/amd64`" + `

这些值取自 ` + "`.github/go-test-report.json`" + ` 的 ` + "`environment`" + ` 字段（唯一的机器可读来源），因此不会与实际运行漂移。

## 总覆盖率

- **总计：** ` + "`95.00%`" + ` 语句覆盖率（95/100）
- 覆盖率门禁（CI）：` + "`93%`" + `（实测总覆盖率向下取整；确保同一提交不会失败于自身门禁）

根 README 的 Coverage 徽章由 CI 基于同一次运行生成。

## 分包覆盖率

| 包 | 覆盖率 |
| -- | ------ |
| ` + "`pkg/license`" + ` | ` + "`96.1%`" + ` |
| ` + "`pkg/fingerprint`" + ` | ` + "`94.2%`" + ` |
| ` + "`internal/issuer`" + ` | ` + "`93.3%`" + ` |
| ` + "`cmd/license-tool`" + ` | ` + "`92.4%`" + ` |
| ` + "`examples/client`" + ` | ` + "`0.0%`" + `（示例代码，无测试） |

<!-- END:GENERATED-COVERAGE -->`
	if got := renderQualityBlock(report, "zh"); got != want {
		t.Fatalf("rendered legacy Chinese block differs\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestGenerateQualityDocsValidatesAllTargetsBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	valid := "before\n" + coverageBegin + "\nstale\n" + coverageEnd + "\nafter\n"
	setupQualityDocs(t, dir, valid, "markers missing\n")
	english := filepath.Join(dir, "docs", "enUS", "quality.md")
	original, _ := os.ReadFile(english)
	_, _, err := executeTest(t, testDeps(dir), "quality", "docs")
	requireExitCode(t, err, 2)
	after, _ := os.ReadFile(english)
	if !bytes.Equal(original, after) {
		t.Fatal("first target changed before second target validation failed")
	}
}

func TestGenerateQualityDocsRollsBackFirstCommitWhenSecondFails(t *testing.T) {
	dir := t.TempDir()
	doc := "before\n" + coverageBegin + "\nstale\n" + coverageEnd + "\nafter\n"
	setupQualityDocs(t, dir, doc, doc)
	english := filepath.Join(dir, "docs", "enUS", "quality.md")
	chinese := filepath.Join(dir, "docs", "zhCN", "quality.md")
	originalEnglish, _ := os.ReadFile(english)
	originalChinese, _ := os.ReadFile(chinese)

	deps := testDeps(dir)
	realWriteAtomic := deps.writeAtomic
	writes := 0
	deps.writeAtomic = func(ctx context.Context, name string, data []byte) error {
		writes++
		if writes == 2 {
			return errors.New("second commit failed")
		}
		return realWriteAtomic(ctx, name, data)
	}
	stdout, _, err := executeTest(t, deps, "quality", "docs")
	requireExitCode(t, err, 2)
	if writes != 3 {
		t.Fatalf("atomic writes=%d, want first commit, failed second commit, and rollback", writes)
	}
	if stdout != "" {
		t.Fatalf("stdout announced an update that was rolled back: %q", stdout)
	}
	gotEnglish, _ := os.ReadFile(english)
	gotChinese, _ := os.ReadFile(chinese)
	if !bytes.Equal(gotEnglish, originalEnglish) || !bytes.Equal(gotChinese, originalChinese) {
		t.Fatal("quality docs were not restored after the second commit failed")
	}
}

func TestGenerateQualityDocsRejectsDuplicateAndReversedMarkers(t *testing.T) {
	for _, doc := range []string{
		coverageBegin + coverageBegin + coverageEnd,
		coverageEnd + coverageBegin,
	} {
		dir := t.TempDir()
		setupQualityDocs(t, dir, doc, doc)
		_, _, err := executeTest(t, testDeps(dir), "quality", "docs")
		requireExitCode(t, err, 2)
	}
}

func TestAtomicWritePreservesMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix permission bits consistently")
	}
	dir := t.TempDir()
	name := filepath.Join(dir, "file")
	if err := os.WriteFile(name, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(context.Background(), name, []byte("new")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
}

func TestAtomicWriteRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	writeTestFile(t, target, "old")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := atomicWriteFile(context.Background(), link, []byte("new")); err == nil {
		t.Fatal("symlink target accepted")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "old" {
		t.Fatalf("symlink target changed: %q", data)
	}
}
