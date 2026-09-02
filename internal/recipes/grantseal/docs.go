package grantseal

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var bannedQualityLabel = regexp.MustCompile(`(?i)(commercial-grade|enterprise-grade|military-grade|bank-grade|banking-grade|production-grade|商业级|企业级|军工级|银行级)`)
var literalGoVersion = regexp.MustCompile(`^\s*go-version:\s`)
var deniedAliasMarker = regexp.MustCompile(`\b(no|not|alias)\b|不存在|别名|不等于`)

type languageException struct {
	pathSuffix string
	substring  string
}

func runDocLanguage(deps dependencies, args []string, stdout, stderr io.Writer) error {
	rootArg := "."
	allowlistArg := filepath.Join("scripts", "doc-language-allowlist.txt")
	rootSet := false
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--allowlist":
			index++
			if index >= len(args) || args[index] == "" {
				return usage("usage: doc language [ROOT] [--allowlist FILE]")
			}
			allowlistArg = args[index]
		default:
			if rootSet || strings.HasPrefix(args[index], "-") {
				return usage("usage: doc language [ROOT] [--allowlist FILE]")
			}
			rootArg = args[index]
			rootSet = true
		}
	}
	root := resolvePath(deps.workDir, rootArg)
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		if err == nil {
			err = fmt.Errorf("not a directory")
		}
		return usage("root dir not found: %s: %v", rootArg, err)
	}
	exceptions, err := readLanguageExceptions(resolvePath(deps.workDir, allowlistArg), deps.readFile)
	if err != nil {
		return usage("read doc-language allowlist %q: %v", allowlistArg, err)
	}
	files, err := collectFiles(root, func(name string, entry fs.DirEntry) bool {
		return !entry.IsDir() && filepath.Ext(name) == ".md"
	})
	if err != nil {
		return usage("enumerate Markdown docs: %v", err)
	}
	violations := 0
	for _, name := range files {
		file, err := os.Open(name)
		if err != nil {
			return usage("open doc %q: %v", name, err)
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		lineNumber := 0
		for scanner.Scan() {
			lineNumber++
			line := scanner.Text()
			if !bannedQualityLabel.MatchString(line) {
				continue
			}
			rel, relErr := filepath.Rel(root, name)
			if relErr != nil {
				_ = file.Close()
				return usage("resolve doc path %q: %v", name, relErr)
			}
			rel = filepath.ToSlash(rel)
			if languageLineAllowed(rel, line, exceptions) {
				continue
			}
			fmt.Fprintf(stderr, "banned quality label: %s:%d:%s\n", rel, lineNumber, line)
			violations++
		}
		scanErr := scanner.Err()
		closeErr := file.Close()
		if scanErr != nil {
			return usage("read doc %q: %v", name, scanErr)
		}
		if closeErr != nil {
			return usage("close doc %q: %v", name, closeErr)
		}
	}
	if violations != 0 {
		return rejected("found %d non-allowlisted banned quality label(s)", violations)
	}
	fmt.Fprintln(stdout, "doc-language check passed: no banned quality labels found, or all hits are allowlisted")
	return nil
}

func readLanguageExceptions(name string, readFile func(string) ([]byte, error)) ([]languageException, error) {
	data, err := readFile(name)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result []languageException
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "|||", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("line %d: expected path-suffix|||line-substring", lineNumber)
		}
		result = append(result, languageException{pathSuffix: filepath.ToSlash(parts[0]), substring: parts[1]})
	}
	return result, scanner.Err()
}

func languageLineAllowed(name, line string, exceptions []languageException) bool {
	for _, exception := range exceptions {
		if strings.HasSuffix(name, exception.pathSuffix) && strings.Contains(line, exception.substring) {
			return true
		}
	}
	return false
}

type consistencyRule struct {
	description string
	matches     []string
}

func runDocConsistency(deps dependencies, args []string, stdout, stderr io.Writer) error {
	if len(args) > 1 {
		return usage("usage: doc consistency [ROOT]")
	}
	rootArg := "."
	if len(args) == 1 {
		rootArg = args[0]
	}
	root := resolvePath(deps.workDir, rootArg)
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		if err == nil {
			err = fmt.Errorf("not a directory")
		}
		return usage("root dir not found: %s: %v", rootArg, err)
	}

	rules := []consistencyRule{
		{description: "workflow references a Go version matrix (matrix.go)"},
		{description: "workflow pins a literal go-version: (use go-version-file: go.mod)"},
		{description: "doc hardcodes a stale error-code count (23 codes / 23 个)"},
		{description: "doc references LICENSE_FEATURE_DENIED as a live wire code (use LICENSE_FEATURE_UNAVAILABLE)"},
		{description: "doc uses deprecated 'state is reset' anti-rollback phrasing"},
	}

	workflowDir := filepath.Join(root, ".github", "workflows")
	if info, statErr := os.Stat(workflowDir); statErr == nil && info.IsDir() {
		files, err := collectFiles(workflowDir, func(_ string, entry fs.DirEntry) bool { return !entry.IsDir() })
		if err != nil {
			return usage("enumerate workflows: %v", err)
		}
		for _, name := range files {
			if err := inspectLines(name, root, func(location, line string) {
				if strings.Contains(line, "matrix.go") {
					rules[0].matches = append(rules[0].matches, location+line)
				}
				if literalGoVersion.MatchString(line) {
					rules[1].matches = append(rules[1].matches, location+line)
				}
			}); err != nil {
				return usage("inspect workflow %q: %v", name, err)
			}
		}
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return usage("inspect workflow dir: %v", statErr)
	}

	docs, err := collectFiles(root, func(name string, entry fs.DirEntry) bool {
		return !entry.IsDir() && filepath.Ext(name) == ".md"
	})
	if err != nil {
		return usage("enumerate Markdown docs: %v", err)
	}
	for _, name := range docs {
		if err := inspectLines(name, root, func(location, line string) {
			if strings.Contains(line, "23 codes") || strings.Contains(line, "23 个") {
				rules[2].matches = append(rules[2].matches, location+line)
			}
			if strings.Contains(line, "LICENSE_FEATURE_DENIED") && !deniedAliasMarker.MatchString(line) {
				rules[3].matches = append(rules[3].matches, location+line)
			}
			if strings.Contains(line, "state is reset") || strings.Contains(line, "the state is reset") ||
				strings.Contains(line, "并重置状态") || strings.Contains(line, "容忍并重置") {
				rules[4].matches = append(rules[4].matches, location+line)
			}
		}); err != nil {
			return usage("inspect doc %q: %v", name, err)
		}
	}

	violationKinds := 0
	for _, rule := range rules {
		if len(rule.matches) == 0 {
			continue
		}
		violationKinds++
		fmt.Fprintf(stderr, "drift: %s\n", rule.description)
		for _, match := range rule.matches {
			fmt.Fprintf(stderr, "  %s\n", match)
		}
	}
	if violationKinds != 0 {
		return rejected("found %d documentation-consistency drift condition(s)", violationKinds)
	}
	fmt.Fprintln(stdout, "doc-consistency check passed: no drift found")
	return nil
}

func inspectLines(name, root string, visit func(location, line string)) error {
	file, err := os.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()
	rel, err := filepath.Rel(root, name)
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		visit(fmt.Sprintf("%s:%d:", filepath.ToSlash(rel), lineNumber), scanner.Text())
	}
	return scanner.Err()
}

func collectFiles(root string, include func(string, fs.DirEntry) bool) ([]string, error) {
	var result []string
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && entry.Name() == ".git" && name != root {
			return filepath.SkipDir
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if include(name, entry) {
			result = append(result, name)
		}
		return nil
	})
	sort.Strings(result)
	return result, err
}
