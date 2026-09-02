package grantseal

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var diffHunkPattern = regexp.MustCompile(`^@@ -[0-9]+(?:,[0-9]+)? \+([0-9]+)(?:,([0-9]+))? @@`)
var coverageBlockPattern = regexp.MustCompile(`^(.+):([0-9]+)\.[0-9]+,([0-9]+)\.[0-9]+ ([0-9]+) ([0-9]+)$`)

type coverageBlock struct {
	start, end int
	statements int
	count      uint64
}

type coverageProfile struct {
	mode   string
	blocks map[string][]coverageBlock
}

func runPatchCoverage(ctx context.Context, deps dependencies, args []string, stdout, stderr io.Writer) error {
	if len(args) < 1 || len(args) > 3 || args[0] == "" {
		return usage("usage: patch coverage COVERAGE_PROFILE [BASE_REF] [THRESHOLD]")
	}
	profilePath := resolvePath(deps.workDir, args[0])
	data, err := deps.readFile(profilePath)
	if err != nil {
		return usage("read coverage profile %q: %v", args[0], err)
	}
	profile, err := parseCoverageProfile(data)
	if err != nil {
		return usage("parse coverage profile %q: %v", args[0], err)
	}
	threshold := 90.0
	if len(args) == 3 {
		threshold, err = strconv.ParseFloat(args[2], 64)
		if err != nil || math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold < 0 || threshold > 100 {
			return usage("threshold must be a finite percentage from 0 through 100")
		}
	}
	if _, err := deps.runner.LookPath("git"); err != nil {
		return usage("git is required")
	}
	base := ""
	if len(args) >= 2 {
		base = args[1]
	}
	if base == "" {
		base, err = resolvePatchBase(ctx, deps)
		if err != nil {
			return err
		}
		if base == "" {
			fmt.Fprintln(stderr, "note: no base ref available (initial commit?); nothing to score")
			return nil
		}
	}
	if strings.HasPrefix(base, "-") || strings.ContainsAny(base, "\x00\r\n\t ") {
		return usage("base ref contains an invalid option prefix, whitespace, or control character")
	}
	diff, err := runOutput(ctx, deps, "git", "diff", "--unified=0", "--no-color", "--no-ext-diff", base+"...HEAD", "--", "*.go", ":(exclude)*_test.go")
	if err != nil {
		return usage("produce patch diff against %q: %v", base, err)
	}
	if len(bytes.TrimSpace(diff)) == 0 {
		fmt.Fprintf(stdout, "patch coverage: no non-test Go changes vs %s; passing\n", base)
		return nil
	}
	added, err := parseAddedLines(diff)
	if err != nil {
		return usage("parse git diff: %v", err)
	}
	if countAddedLines(added) == 0 {
		fmt.Fprintln(stdout, "patch coverage: no added lines to score; passing")
		return nil
	}
	if err := removeProvablyNonCoverableFiles(deps.workDir, added, profile); err != nil {
		return usage("inspect changed files without coverage data: %v", err)
	}
	covered, uncovered, examples, err := scorePatchCoverage(added, profile)
	if err != nil {
		return usage("score patch coverage: %v", err)
	}
	total := covered + uncovered
	if total == 0 {
		fmt.Fprintln(stdout, "patch coverage: changed lines contain no coverable statements; passing")
		return nil
	}
	percentage := 100 * float64(covered) / float64(total)
	fmt.Fprintf(stdout, "patch coverage: %d/%d changed statements covered = %.2f%% (threshold %.0f%%)\n", covered, total, percentage, threshold)
	if percentage+1e-9 < threshold {
		fmt.Fprintln(stdout, "uncovered changed statements (first 20):")
		for _, example := range examples {
			fmt.Fprintf(stdout, "  %s\n", example)
		}
		return rejected("changed-code coverage %.2f%% is below threshold %.0f%%", percentage, threshold)
	}
	return nil
}

func resolvePatchBase(ctx context.Context, deps dependencies) (string, error) {
	if _, err := runOutput(ctx, deps, "git", "rev-parse", "--verify", "-q", "origin/HEAD"); err == nil {
		base, mergeErr := runOutput(ctx, deps, "git", "merge-base", "origin/HEAD", "HEAD")
		if mergeErr == nil {
			return strings.TrimSpace(string(base)), nil
		}
		if commandExitCode(mergeErr) != 1 || ctx.Err() != nil {
			return "", usage("resolve merge base: %v", mergeErr)
		}
	} else if commandExitCode(err) != 1 || ctx.Err() != nil {
		return "", usage("inspect origin/HEAD: %v", err)
	}
	base, err := runOutput(ctx, deps, "git", "rev-parse", "--verify", "-q", "HEAD~1")
	if err != nil {
		if commandExitCode(err) != 1 || ctx.Err() != nil {
			return "", usage("resolve HEAD~1: %v", err)
		}
		return "", nil
	}
	return strings.TrimSpace(string(base)), nil
}

func parseCoverageProfile(data []byte) (coverageProfile, error) {
	profile := coverageProfile{blocks: make(map[string][]coverageBlock)}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if profile.mode == "" {
			if !strings.HasPrefix(line, "mode: ") {
				return profile, fmt.Errorf("line %d: missing mode header", lineNumber)
			}
			profile.mode = strings.TrimSpace(strings.TrimPrefix(line, "mode: "))
			switch profile.mode {
			case "set", "count", "atomic":
			default:
				return profile, fmt.Errorf("line %d: unsupported mode %q", lineNumber, profile.mode)
			}
			continue
		}
		match := coverageBlockPattern.FindStringSubmatch(line)
		if match == nil {
			return profile, fmt.Errorf("line %d: malformed coverage block", lineNumber)
		}
		start, _ := strconv.Atoi(match[2])
		end, _ := strconv.Atoi(match[3])
		statements, _ := strconv.Atoi(match[4])
		count, countErr := strconv.ParseUint(match[5], 10, 64)
		if start <= 0 || end < start || statements < 0 || countErr != nil {
			return profile, fmt.Errorf("line %d: invalid coverage block range or count", lineNumber)
		}
		name := filepath.ToSlash(match[1])
		profile.blocks[name] = append(profile.blocks[name], coverageBlock{start: start, end: end, statements: statements, count: count})
	}
	if err := scanner.Err(); err != nil {
		return profile, err
	}
	if profile.mode == "" {
		return profile, fmt.Errorf("empty profile")
	}
	if len(profile.blocks) == 0 {
		return profile, fmt.Errorf("profile contains no coverage blocks")
	}
	return profile, nil
}

func parseAddedLines(data []byte) (map[string]map[int]struct{}, error) {
	result := make(map[string]map[int]struct{})
	var current string
	newLine := 0
	inHunk := false
	sawFile := false
	sawHunk := false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "+++ ") {
			current = ""
			inHunk = false
			name := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
			if name == "/dev/null" {
				continue
			}
			if strings.HasPrefix(name, `"`) {
				unquoted, err := strconv.Unquote(name)
				if err != nil {
					return nil, fmt.Errorf("invalid quoted diff path: %w", err)
				}
				name = unquoted
			}
			name = strings.TrimPrefix(name, "b/")
			name = filepath.ToSlash(name)
			if name == "" || strings.ContainsRune(name, 0) {
				return nil, fmt.Errorf("invalid diff path")
			}
			current = name
			sawFile = true
			if result[current] == nil {
				result[current] = make(map[int]struct{})
			}
			continue
		}
		if strings.HasPrefix(line, "@@") {
			match := diffHunkPattern.FindStringSubmatch(line)
			if match == nil {
				return nil, fmt.Errorf("malformed hunk header %q", line)
			}
			newLine, _ = strconv.Atoi(match[1])
			inHunk = true
			sawHunk = true
			continue
		}
		if current == "" || !inHunk {
			continue
		}
		switch {
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			result[current][newLine] = struct{}{}
			newLine++
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
		case strings.HasPrefix(line, "\\ No newline at end of file"):
		default:
			newLine++
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) != 0 && (!sawFile || !sawHunk) {
		return nil, fmt.Errorf("diff contains no parseable file and hunk headers")
	}
	return result, nil
}

func countAddedLines(files map[string]map[int]struct{}) int {
	total := 0
	for _, lines := range files {
		total += len(lines)
	}
	return total
}

func scorePatchCoverage(added map[string]map[int]struct{}, profile coverageProfile) (covered, uncovered int, examples []string, err error) {
	paths := make([]string, 0, len(added))
	for name := range added {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	for _, name := range paths {
		blocks, err := profileBlocksFor(name, profile.blocks)
		if err != nil {
			return 0, 0, nil, err
		}
		lines := make([]int, 0, len(added[name]))
		for line := range added[name] {
			lines = append(lines, line)
		}
		sort.Ints(lines)
		for _, line := range lines {
			for _, block := range blocks {
				if block.statements > 0 && block.start <= line && line <= block.end {
					if block.count > 0 {
						covered++
					} else {
						uncovered++
						if len(examples) < 20 {
							examples = append(examples, fmt.Sprintf("%s:%d", name, line))
						}
					}
					break
				}
			}
		}
	}
	return covered, uncovered, examples, nil
}

// removeProvablyNonCoverableFiles keeps missing coverage data fail-closed for
// executable source, while preserving the documented vacuous pass for files
// that cannot contain an instrumented statement (for example a comment-only
// doc.go or a file excluded by the current platform's build constraints).
func removeProvablyNonCoverableFiles(workDir string, added map[string]map[int]struct{}, profile coverageProfile) error {
	for name, lines := range added {
		if _, err := profileBlocksFor(name, profile.blocks); err == nil {
			continue
		} else if !strings.Contains(err.Error(), "has no coverage profile data") {
			return err
		}

		fullPath := resolvePath(workDir, filepath.FromSlash(name))
		match, err := build.Default.MatchFile(filepath.Dir(fullPath), filepath.Base(fullPath))
		if err != nil {
			return fmt.Errorf("evaluate build constraints for %q: %w", name, err)
		}
		if !match {
			delete(added, name)
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, fullPath, nil, parser.ParseComments)
		if err != nil {
			return fmt.Errorf("parse %q: %w", name, err)
		}
		coverable := false
		ast.Inspect(file, func(node ast.Node) bool {
			if coverable || node == nil {
				return !coverable
			}
			switch node.(type) {
			case *ast.BlockStmt, *ast.EmptyStmt:
				return true
			case ast.Stmt:
				start := fset.Position(node.Pos()).Line
				end := fset.Position(node.End()).Line
				for line := range lines {
					if start <= line && line <= end {
						coverable = true
						return false
					}
				}
			}
			return true
		})
		if !coverable {
			delete(added, name)
		}
	}
	return nil
}

func profileBlocksFor(diffPath string, blocks map[string][]coverageBlock) ([]coverageBlock, error) {
	diffPath = filepath.ToSlash(diffPath)
	if exact, ok := blocks[diffPath]; ok {
		return exact, nil
	}
	var found []coverageBlock
	matches := 0
	for name, candidate := range blocks {
		if strings.HasSuffix(filepath.ToSlash(name), "/"+diffPath) {
			found = candidate
			matches++
		}
	}
	if matches == 0 {
		return nil, fmt.Errorf("changed file %q has no coverage profile data", diffPath)
	}
	if matches > 1 {
		return nil, fmt.Errorf("changed file %q ambiguously matches %d profile paths", diffPath, matches)
	}
	return found, nil
}
