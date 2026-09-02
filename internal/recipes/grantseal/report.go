package grantseal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const coverageBegin = "<!-- BEGIN:GENERATED-COVERAGE -->"
const coverageEnd = "<!-- END:GENERATED-COVERAGE -->"

type orderedJSONField struct {
	name  string
	value json.RawMessage
}

type reportEnvironment struct {
	Commit      string `json:"commit"`
	GeneratedAt string `json:"generated_at"`
	GoVersion   string `json:"go_version"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
}

func runInjectReportEnvironment(ctx context.Context, deps dependencies, args []string, stdout, _ io.Writer) error {
	if len(args) > 1 {
		return usage("usage: report inject-environment [REPORT_JSON]")
	}
	display := filepath.Join(".github", "go-test-report.json")
	if len(args) == 1 {
		display = args[0]
	}
	name := resolvePath(deps.workDir, display)
	data, err := deps.readFile(name)
	if err != nil {
		return usage("read report %q: %v", display, err)
	}
	fields, err := parseOrderedJSONObject(data)
	if err != nil {
		return usage("parse report %q: %v", display, err)
	}
	environment, err := deriveReportEnvironment(ctx, deps)
	if err != nil {
		return usage("collect report environment: %v", err)
	}
	envJSON, err := json.Marshal(environment)
	if err != nil {
		return usage("encode report environment: %v", err)
	}
	fields = replaceEnvironmentField(fields, envJSON)
	output, err := marshalOrderedJSONObject(fields)
	if err != nil {
		return usage("encode report %q: %v", display, err)
	}
	if err := ctx.Err(); err != nil {
		return usage("write report %q canceled: %v", display, err)
	}
	if err := deps.writeAtomic(ctx, name, output); err != nil {
		return usage("write report %q atomically: %v", display, err)
	}
	fmt.Fprintf(stdout, "injected environment into %s: commit=%q generated_at=%q go_version=%q os=%q arch=%q\n",
		display, environment.Commit, environment.GeneratedAt, environment.GoVersion, environment.OS, environment.Arch)
	return nil
}

func deriveReportEnvironment(ctx context.Context, deps dependencies) (reportEnvironment, error) {
	value := func(key string) string {
		return strings.TrimSpace(deps.getenv(key))
	}
	commandOutput := func(name string, args ...string) ([]byte, error) {
		output, err := runOutput(ctx, deps, name, args...)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if err != nil {
			return nil, nil
		}
		return output, nil
	}
	if err := ctx.Err(); err != nil {
		return reportEnvironment{}, err
	}
	commit := value("REPORT_COMMIT")
	if commit == "" {
		output, err := commandOutput("git", "rev-parse", "HEAD")
		if err != nil {
			return reportEnvironment{}, err
		}
		commit = strings.TrimSpace(string(output))
	}
	if commit == "" {
		commit = "unknown"
	}
	generatedAt := value("REPORT_GENERATED_AT")
	if generatedAt == "" {
		generatedAt = deps.now().UTC().Format(time.RFC3339)
	}
	goVersion := value("REPORT_GO_VERSION")
	if goVersion == "" {
		output, err := commandOutput("go", "version")
		if err != nil {
			return reportEnvironment{}, err
		}
		parts := strings.Fields(string(output))
		if len(parts) >= 3 {
			goVersion = parts[2]
		}
	}
	if goVersion == "" {
		goVersion = "unknown"
	}
	goos := value("REPORT_OS")
	if goos == "" {
		output, err := commandOutput("go", "env", "GOOS")
		if err != nil {
			return reportEnvironment{}, err
		}
		goos = strings.TrimSpace(string(output))
	}
	if goos == "" {
		goos = "unknown"
	}
	arch := value("REPORT_ARCH")
	if arch == "" {
		output, err := commandOutput("go", "env", "GOARCH")
		if err != nil {
			return reportEnvironment{}, err
		}
		arch = strings.TrimSpace(string(output))
	}
	if arch == "" {
		arch = "unknown"
	}
	if err := ctx.Err(); err != nil {
		return reportEnvironment{}, err
	}
	return reportEnvironment{Commit: commit, GeneratedAt: generatedAt, GoVersion: goVersion, OS: goos, Arch: arch}, nil
}

func parseOrderedJSONObject(data []byte) ([]orderedJSONField, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return nil, fmt.Errorf("top-level JSON value must be an object")
	}
	seen := make(map[string]struct{})
	var fields []orderedJSONField
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("object key is not a string")
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("duplicate top-level key %q", name)
		}
		seen[name] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields = append(fields, orderedJSONField{name: name, value: value})
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("unexpected JSON value after top-level object")
		}
		return nil, err
	}
	return fields, nil
}

func replaceEnvironmentField(fields []orderedJSONField, environment json.RawMessage) []orderedJSONField {
	withoutEnvironment := make([]orderedJSONField, 0, len(fields))
	for _, field := range fields {
		if field.name == "environment" {
			continue
		}
		withoutEnvironment = append(withoutEnvironment, field)
	}
	insertAt := 0
	for index, field := range withoutEnvironment {
		if field.name == "schema_version" {
			insertAt = index + 1
			break
		}
	}
	result := make([]orderedJSONField, 0, len(withoutEnvironment)+1)
	result = append(result, withoutEnvironment[:insertAt]...)
	result = append(result, orderedJSONField{name: "environment", value: environment})
	result = append(result, withoutEnvironment[insertAt:]...)
	return result
}

func marshalOrderedJSONObject(fields []orderedJSONField) ([]byte, error) {
	var output bytes.Buffer
	output.WriteString("{\n")
	for index, field := range fields {
		key, _ := json.Marshal(field.name)
		output.WriteString("  ")
		output.Write(key)
		output.WriteString(": ")
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, field.value, "  ", "  "); err != nil {
			return nil, fmt.Errorf("field %q: %w", field.name, err)
		}
		output.Write(pretty.Bytes())
		if index != len(fields)-1 {
			output.WriteByte(',')
		}
		output.WriteByte('\n')
	}
	output.WriteString("}\n")
	return output.Bytes(), nil
}

const qualityReportSchemaVersion = "1.0"

type qualityCoverage struct {
	CoveredStatements int         `json:"covered_statements"`
	TotalStatements   int         `json:"total_statements"`
	Percentage        float64     `json:"percentage"`
	Threshold         json.Number `json:"threshold"`
}

type qualityPackage struct {
	Name     string  `json:"name"`
	Coverage float64 `json:"coverage"`
}

type qualityReport struct {
	Environment reportEnvironment `json:"environment"`
	Coverage    qualityCoverage   `json:"coverage"`
	Packages    []qualityPackage  `json:"packages"`
}

type renderedQualityTarget struct {
	name     string
	original []byte
	data     []byte
}

// The input form uses pointers and raw numbers so missing and null fields do
// not silently become valid-looking zero values in the rendered documents.
type qualityReportInput struct {
	SchemaVersion *string           `json:"schema_version"`
	Environment   reportEnvironment `json:"environment"`
	Coverage      *struct {
		CoveredStatements *int            `json:"covered_statements"`
		TotalStatements   *int            `json:"total_statements"`
		Percentage        *float64        `json:"percentage"`
		Threshold         json.RawMessage `json:"threshold"`
	} `json:"coverage"`
	Packages *[]struct {
		Name     *string  `json:"name"`
		Coverage *float64 `json:"coverage"`
	} `json:"packages"`
}

func parseQualityReport(data []byte) (qualityReport, error) {
	if _, err := parseOrderedJSONObject(data); err != nil {
		return qualityReport{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var input qualityReportInput
	if err := decoder.Decode(&input); err != nil {
		return qualityReport{}, err
	}
	if input.SchemaVersion == nil {
		return qualityReport{}, fmt.Errorf("missing schema_version")
	}
	if *input.SchemaVersion != qualityReportSchemaVersion {
		return qualityReport{}, fmt.Errorf("unsupported schema_version %q", *input.SchemaVersion)
	}
	if input.Coverage == nil {
		return qualityReport{}, fmt.Errorf("missing coverage object")
	}
	if input.Coverage.CoveredStatements == nil {
		return qualityReport{}, fmt.Errorf("missing coverage.covered_statements")
	}
	if input.Coverage.TotalStatements == nil {
		return qualityReport{}, fmt.Errorf("missing coverage.total_statements")
	}
	if input.Coverage.Percentage == nil {
		return qualityReport{}, fmt.Errorf("missing coverage.percentage")
	}
	threshold, err := parseRequiredJSONNumber(input.Coverage.Threshold)
	if err != nil {
		return qualityReport{}, fmt.Errorf("invalid coverage.threshold: %w", err)
	}
	if input.Packages == nil {
		return qualityReport{}, fmt.Errorf("missing packages array")
	}

	report := qualityReport{
		Environment: input.Environment,
		Coverage: qualityCoverage{
			CoveredStatements: *input.Coverage.CoveredStatements,
			TotalStatements:   *input.Coverage.TotalStatements,
			Percentage:        *input.Coverage.Percentage,
			Threshold:         threshold,
		},
		Packages: make([]qualityPackage, 0, len(*input.Packages)),
	}
	for index, pkg := range *input.Packages {
		if pkg.Name == nil {
			return qualityReport{}, fmt.Errorf("missing packages[%d].name", index)
		}
		if pkg.Coverage == nil {
			return qualityReport{}, fmt.Errorf("missing packages[%d].coverage", index)
		}
		report.Packages = append(report.Packages, qualityPackage{Name: *pkg.Name, Coverage: *pkg.Coverage})
	}
	return report, nil
}

func parseRequiredJSONNumber(data json.RawMessage) (json.Number, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("field is missing")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	number, ok := value.(json.Number)
	if !ok {
		return "", fmt.Errorf("must be a JSON number")
	}
	return number, nil
}

func runGenerateQualityDocs(ctx context.Context, deps dependencies, args []string, stdout, _ io.Writer) error {
	if len(args) > 1 {
		return usage("usage: quality docs [REPO_ROOT]")
	}
	root := deps.workDir
	if len(args) == 1 {
		root = resolvePath(deps.workDir, args[0])
	}
	jsonName := filepath.Join(root, ".github", "go-test-report.json")
	data, err := deps.readFile(jsonName)
	if err != nil {
		return usage("quality source not found: %s: %v", jsonName, err)
	}
	report, err := parseQualityReport(data)
	if err != nil {
		return usage("parse quality source %q: %v", jsonName, err)
	}
	if report.Coverage.TotalStatements < 0 || report.Coverage.CoveredStatements < 0 ||
		report.Coverage.CoveredStatements > report.Coverage.TotalStatements || report.Coverage.Threshold == "" {
		return usage("quality source contains invalid coverage totals or threshold")
	}
	threshold, err := strconv.ParseFloat(report.Coverage.Threshold.String(), 64)
	if err != nil {
		return usage("quality source contains an invalid threshold: %v", err)
	}
	if threshold < 0 || threshold > 100 {
		return usage("quality source threshold must be from 0 through 100")
	}
	if report.Coverage.Percentage < 0 || report.Coverage.Percentage > 100 {
		return usage("quality source contains an invalid coverage percentage")
	}
	seenPackages := make(map[string]struct{}, len(report.Packages))
	for _, pkg := range report.Packages {
		if pkg.Name == "" || pkg.Coverage < 0 || pkg.Coverage > 100 {
			return usage("quality source contains an invalid package entry")
		}
		if _, exists := seenPackages[pkg.Name]; exists {
			return usage("quality source contains duplicate package %q", pkg.Name)
		}
		seenPackages[pkg.Name] = struct{}{}
	}

	targets := []struct {
		name string
		lang string
	}{
		{filepath.Join(root, "docs", "enUS", "quality.md"), "en"},
		{filepath.Join(root, "docs", "zhCN", "quality.md"), "zh"},
	}
	rendered := make([]renderedQualityTarget, 0, len(targets))
	for _, target := range targets {
		current, err := deps.readFile(target.name)
		if err != nil {
			return usage("read quality doc %q: %v", target.name, err)
		}
		block := renderQualityBlock(report, target.lang)
		updated, err := replaceUniqueCoverageBlock(current, []byte(block))
		if err != nil {
			return usage("render quality doc %q: %v", target.name, err)
		}
		rendered = append(rendered, renderedQualityTarget{name: target.name, original: current, data: updated})
	}
	if err := commitQualityTargets(ctx, deps, rendered); err != nil {
		return usage("write quality docs atomically: %v", err)
	}
	for _, target := range rendered {
		rel, err := filepath.Rel(root, target.name)
		if err != nil {
			rel = target.name
		}
		fmt.Fprintf(stdout, "regenerated coverage block in %s\n", filepath.ToSlash(rel))
	}
	fmt.Fprintln(stdout, "quality docs regenerated from go-test-report.json")
	return nil
}

func commitQualityTargets(ctx context.Context, deps dependencies, targets []renderedQualityTarget) error {
	for index, target := range targets {
		if err := deps.writeAtomic(ctx, target.name, target.data); err != nil {
			var rollbackFailures []string
			rollbackContext := context.WithoutCancel(ctx)
			for rollbackIndex := index - 1; rollbackIndex >= 0; rollbackIndex-- {
				previous := targets[rollbackIndex]
				if rollbackErr := deps.writeAtomic(rollbackContext, previous.name, previous.original); rollbackErr != nil {
					rollbackFailures = append(rollbackFailures, fmt.Sprintf("%q: %v", previous.name, rollbackErr))
				}
			}
			if len(rollbackFailures) != 0 {
				return fmt.Errorf("commit %q: %w; rollback failed for %s", target.name, err, strings.Join(rollbackFailures, ", "))
			}
			return fmt.Errorf("commit %q: %w", target.name, err)
		}
	}
	return nil
}

func replaceUniqueCoverageBlock(current, replacement []byte) ([]byte, error) {
	beginCount := bytes.Count(current, []byte(coverageBegin))
	endCount := bytes.Count(current, []byte(coverageEnd))
	if beginCount != 1 || endCount != 1 {
		return nil, fmt.Errorf("expected exactly one begin marker and one end marker")
	}
	begin := bytes.Index(current, []byte(coverageBegin))
	end := bytes.Index(current, []byte(coverageEnd))
	if begin < 0 || end < begin {
		return nil, fmt.Errorf("coverage markers are out of order")
	}
	end += len(coverageEnd)
	result := make([]byte, 0, len(current)-end+begin+len(replacement))
	result = append(result, current[:begin]...)
	result = append(result, replacement...)
	result = append(result, current[end:]...)
	return result, nil
}

func renderQualityBlock(report qualityReport, lang string) string {
	packages := make(map[string]float64, len(report.Packages))
	for _, pkg := range report.Packages {
		packages[pkg.Name] = pkg.Coverage
	}
	pkgValue := func(name string) string {
		coverage, ok := packages[name]
		if !ok {
			return "n/a"
		}
		return fmt.Sprintf("%.1f%%", coverage)
	}
	envValue := func(value string) string {
		if value == "" {
			return "unknown"
		}
		return value
	}
	rows := func() string {
		var builder strings.Builder
		for _, name := range []string{"pkg/license", "pkg/fingerprint", "internal/issuer", "cmd/license-tool"} {
			fmt.Fprintf(&builder, "| `%s` | `%s` |\n", name, pkgValue(name))
		}
		return builder.String()
	}
	osArch := envValue(report.Environment.OS) + "/" + envValue(report.Environment.Arch)
	commit := markdownCode(envValue(report.Environment.Commit))
	generatedAt := markdownCode(envValue(report.Environment.GeneratedAt))
	goVersion := markdownCode(envValue(report.Environment.GoVersion))
	osAndArch := markdownCode(osArch)
	total := markdownCode(fmt.Sprintf("%.2f%%", report.Coverage.Percentage))
	threshold := markdownCode(report.Coverage.Threshold.String() + "%")
	var builder strings.Builder
	if lang == "en" {
		fmt.Fprintf(&builder, "%s\n", coverageBegin)
		builder.WriteString("<!-- Generated from .github/go-test-report.json by scripts/generate-quality-docs.sh. Do not edit by hand. -->\n\n")
		builder.WriteString("## Environment of record\n\n")
		fmt.Fprintf(&builder, "- Commit: %s\n- Generated (UTC): %s\n- Go version: %s\n- OS / arch: %s\n\n", commit, generatedAt, goVersion, osAndArch)
		builder.WriteString("These values come from the `environment` block of `.github/go-test-report.json`, the single machine-readable source of truth, so they cannot drift from the recorded run.\n\n")
		builder.WriteString("## Total coverage\n\n")
		fmt.Fprintf(&builder, "- **Total:** %s of statements (%d/%d)\n", total, report.Coverage.CoveredStatements, report.Coverage.TotalStatements)
		fmt.Fprintf(&builder, "- Coverage gate (CI): %s (floor of the measured total; set so the same commit cannot fail its own gate)\n\n", threshold)
		builder.WriteString("The Coverage badge in the root README is generated by CI from the same run.\n\n")
		builder.WriteString("## Per-package coverage\n\n")
		builder.WriteString("| Package | Coverage |\n| ------- | -------- |\n")
		builder.WriteString(rows())
		builder.WriteString("| `examples/client` | `0.0%` (illustrative example, no tests) |\n\n")
		builder.WriteString(coverageEnd)
		return builder.String()
	}
	fmt.Fprintf(&builder, "%s\n", coverageBegin)
	builder.WriteString("<!-- 由 scripts/generate-quality-docs.sh 从 .github/go-test-report.json 生成，请勿手工编辑。 -->\n\n")
	builder.WriteString("## 记录环境\n\n")
	fmt.Fprintf(&builder, "- 提交：%s\n- 生成时间（UTC）：%s\n- Go 版本：%s\n- 操作系统 / 架构：%s\n\n", commit, generatedAt, goVersion, osAndArch)
	builder.WriteString("这些值取自 `.github/go-test-report.json` 的 `environment` 字段（唯一的机器可读来源），因此不会与实际运行漂移。\n\n")
	builder.WriteString("## 总覆盖率\n\n")
	fmt.Fprintf(&builder, "- **总计：** %s 语句覆盖率（%d/%d）\n", total, report.Coverage.CoveredStatements, report.Coverage.TotalStatements)
	fmt.Fprintf(&builder, "- 覆盖率门禁（CI）：%s（实测总覆盖率向下取整；确保同一提交不会失败于自身门禁）\n\n", threshold)
	builder.WriteString("根 README 的 Coverage 徽章由 CI 基于同一次运行生成。\n\n")
	builder.WriteString("## 分包覆盖率\n\n")
	builder.WriteString("| 包 | 覆盖率 |\n| -- | ------ |\n")
	builder.WriteString(rows())
	builder.WriteString("| `examples/client` | `0.0%`（示例代码，无测试） |\n\n")
	builder.WriteString(coverageEnd)
	return builder.String()
}

func markdownCode(value string) string {
	value = strings.Map(func(r rune) rune {
		if r == '`' || unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	return "`" + value + "`"
}
