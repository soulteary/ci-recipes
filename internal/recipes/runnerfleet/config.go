package runnerfleet

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// defaultConfigPath is read from the repository root when --config is absent.
// A missing file is not an error: the defaults below are runner-fleet's current
// shell behavior, so a checkout with no config file behaves exactly as the
// scripts did.
const defaultConfigPath = "scripts/ci-recipes.conf"

// Both recipes encode facts about the repository they guard: which file holds
// the baseline version and in what shape, the comment marker that exempts a
// line, which paths are scanned, and which documents exist in which languages.
// Compiling those into this binary would turn "add a seventh UI language" into
// a release here plus a pin bump there, so they are read from the repository
// instead and this file only holds the defaults.
type config struct {
	versionSource     string
	versionBaseline   *regexp.Regexp
	versionReferences []referencePattern
	versionIgnore     string
	versionPathspecs  []string
	versionExclude    []string
	docsRoot          string
	docsLanguages     []string
	docsFiles         []string
}

// referencePattern matches one shape of version reference. Capture group 1
// holds the version, and template renders it for comparison — "main.Version=1.2.3"
// is an ldflags assignment whose value is compared as "v1.2.3".
type referencePattern struct {
	expr     *regexp.Regexp
	template string
}

func defaultConfig() config {
	return config{
		versionSource:   "internal/config/config.go",
		versionBaseline: regexp.MustCompile(`tag = "(v[0-9]+\.[0-9]+\.[0-9]+)"`),
		versionReferences: []referencePattern{
			{expr: regexp.MustCompile(`(v[0-9]+\.[0-9]+\.[0-9]+)`), template: "${1}"},
			{expr: regexp.MustCompile(`main\.Version=([0-9]+\.[0-9]+\.[0-9]+)`), template: "v${1}"},
		},
		versionIgnore: "version-check-ignore",
		// '*Dockerfile*' rather than 'Dockerfile*': the latter only matches the
		// repository root and would miss the examples that reference this
		// repository's image tag.
		versionPathspecs: []string{"*.md", "*.yml", "*.yaml", "*.example", "Makefile", "*Dockerfile*", "*.sh", "*.go"},
		// A changelog naturally contains every historical version.
		versionExclude: []string{"CHANGELOG.md"},
		docsRoot:       "docs",
		docsLanguages:  []string{"zh", "fr", "de", "ko", "ja"},
		docsFiles:      []string{"development.md", "guide.md", "README.md"},
	}
}

// loadConfig returns the defaults overlaid with the repository's config file.
// An explicitly named file that cannot be read is an error rather than a silent
// fall back to the defaults, because a mistyped path would otherwise run a
// different check than the one the caller asked for.
func loadConfig(deps dependencies, root, explicit string) (config, error) {
	cfg := defaultConfig()
	name := explicit
	required := explicit != ""
	if name == "" {
		name = defaultConfigPath
	}
	path := filepath.Join(root, filepath.FromSlash(name))
	if filepath.IsAbs(name) {
		path = filepath.Clean(name)
	}
	data, err := deps.readFile(path)
	if err != nil {
		if !required && os.IsNotExist(err) {
			return cfg, nil
		}
		return config{}, usage("read recipe config %s: %v", name, err)
	}
	if err := applyConfig(&cfg, data); err != nil {
		return config{}, usage("parse recipe config %s: %v", name, err)
	}
	if err := cfg.validate(); err != nil {
		return config{}, usage("recipe config %s: %v", name, err)
	}
	return cfg, nil
}

// applyConfig parses "KEY = VALUE" lines. An unknown key is an error: a key
// silently ignored because of a typo is how a configured check quietly stops
// checking what it was configured for.
//
// A list-valued key replaces the default on its first appearance and appends on
// every later one, so a config file can either restate a list or extend it.
func applyConfig(cfg *config, data []byte) error {
	seen := make(map[string]bool)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		separator := strings.Index(line, "=")
		if separator < 0 {
			return fmt.Errorf("line %d: expected KEY = VALUE", lineNumber)
		}
		key := strings.TrimSpace(line[:separator])
		value := strings.TrimSpace(line[separator+1:])
		if key == "" || value == "" {
			return fmt.Errorf("line %d: expected KEY = VALUE", lineNumber)
		}
		if err := applyConfigEntry(cfg, key, value, seen[key]); err != nil {
			return fmt.Errorf("line %d: %w", lineNumber, err)
		}
		seen[key] = true
	}
	return scanner.Err()
}

func applyConfigEntry(cfg *config, key, value string, repeated bool) error {
	switch key {
	case "version_source":
		cfg.versionSource = value
	case "version_ignore_marker":
		cfg.versionIgnore = value
	case "docs_root":
		cfg.docsRoot = value
	case "version_baseline_regex":
		expr, err := compileCapturing(value)
		if err != nil {
			return err
		}
		cfg.versionBaseline = expr
	case "version_reference_regex":
		reference, err := parseReferencePattern(value)
		if err != nil {
			return err
		}
		if !repeated {
			cfg.versionReferences = nil
		}
		cfg.versionReferences = append(cfg.versionReferences, reference)
	case "version_globs":
		cfg.versionPathspecs = appendFields(cfg.versionPathspecs, value, repeated)
	case "version_exclude":
		cfg.versionExclude = appendFields(cfg.versionExclude, value, repeated)
	case "docs_languages":
		cfg.docsLanguages = appendFields(cfg.docsLanguages, value, repeated)
	case "docs_files":
		cfg.docsFiles = appendFields(cfg.docsFiles, value, repeated)
	default:
		return fmt.Errorf("unknown key %q", key)
	}
	return nil
}

func appendFields(current []string, value string, repeated bool) []string {
	if !repeated {
		current = nil
	}
	return append(current, strings.Fields(value)...)
}

// parseReferencePattern accepts "REGEX" or "REGEX => TEMPLATE", splitting on
// the first " => " so that a regex may contain "=".
func parseReferencePattern(value string) (referencePattern, error) {
	expression := value
	template := "${1}"
	if index := strings.Index(value, " => "); index >= 0 {
		expression = strings.TrimSpace(value[:index])
		template = strings.TrimSpace(value[index+len(" => "):])
	}
	if expression == "" || template == "" {
		return referencePattern{}, fmt.Errorf("expected REGEX or REGEX => TEMPLATE")
	}
	expr, err := compileCapturing(expression)
	if err != nil {
		return referencePattern{}, err
	}
	return referencePattern{expr: expr, template: template}, nil
}

func compileCapturing(expression string) (*regexp.Regexp, error) {
	expr, err := regexp.Compile(expression)
	if err != nil {
		return nil, fmt.Errorf("compile %q: %v", expression, err)
	}
	if expr.NumSubexp() < 1 {
		return nil, fmt.Errorf("compile %q: needs a capturing group holding the version", expression)
	}
	return expr, nil
}

// validate refuses a configuration that would make a recipe compare nothing
// and still succeed.
func (c config) validate() error {
	switch {
	case c.versionSource == "":
		return fmt.Errorf("version_source must not be empty")
	case c.versionBaseline == nil:
		return fmt.Errorf("version_baseline_regex must not be empty")
	case len(c.versionReferences) == 0:
		return fmt.Errorf("at least one version_reference_regex is required")
	case len(c.versionPathspecs) == 0:
		return fmt.Errorf("at least one version_globs entry is required")
	case c.docsRoot == "":
		return fmt.Errorf("docs_root must not be empty")
	case len(c.docsLanguages) == 0:
		return fmt.Errorf("at least one docs_languages entry is required")
	case len(c.docsFiles) == 0:
		return fmt.Errorf("at least one docs_files entry is required")
	}
	return nil
}

func (c config) excluded(name string) bool {
	for _, entry := range c.versionExclude {
		if entry == name {
			return true
		}
	}
	return false
}

// versions returns the distinct versions one reference pattern finds on a line,
// sorted, mirroring the "grep -oE ... | sort -u" the shell used.
func (p referencePattern) versions(line string) []string {
	matches := p.expr.FindAllStringSubmatchIndex(line, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(matches))
	var result []string
	for _, match := range matches {
		value := string(p.expr.ExpandString(nil, p.template, line, match))
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
