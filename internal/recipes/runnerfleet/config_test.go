package runnerfleet

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigDefaultsReproduceTheShellBehaviour(t *testing.T) {
	cfg := defaultConfig()
	if cfg.versionSource != "internal/config/config.go" {
		t.Fatalf("versionSource=%q", cfg.versionSource)
	}
	if got := cfg.versionBaseline.FindStringSubmatch(`		tag = "v1.6.0"`); got == nil || got[1] != "v1.6.0" {
		t.Fatalf("baseline match=%v", got)
	}
	// The comment beside the baseline names the shape rather than a version, so
	// it must not be mistaken for the baseline itself.
	if cfg.versionBaseline.MatchString(`// the regex is tag = "vX.Y.Z"`) {
		t.Fatal("baseline regex should not match the shape comment")
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	if !cfg.excluded("CHANGELOG.md") || cfg.excluded("docs/CHANGELOG.md") {
		t.Fatal("exclusion is an exact repository-relative path")
	}
}

func TestConfigListsReplaceThenAppend(t *testing.T) {
	cfg := defaultConfig()
	if err := applyConfig(&cfg, []byte("docs_languages = es\ndocs_languages = pt br\n")); err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.docsLanguages, " ") != "es pt br" {
		t.Fatalf("docsLanguages=%v", cfg.docsLanguages)
	}
}

func TestConfigCommentsBlanksAndSpacingAreTolerated(t *testing.T) {
	cfg := defaultConfig()
	body := "# heading\n\n   docs_root   =   handbook   \n\n# trailing\n"
	if err := applyConfig(&cfg, []byte(body)); err != nil {
		t.Fatal(err)
	}
	if cfg.docsRoot != "handbook" {
		t.Fatalf("docsRoot=%q", cfg.docsRoot)
	}
}

// A key silently ignored because of a typo is how a configured check quietly
// stops checking what it was configured for.
func TestConfigRejectsUnknownAndMalformedEntries(t *testing.T) {
	for _, body := range []string{
		"docs_rooot = handbook\n",
		"docs_root\n",
		"docs_root =\n",
		"= handbook\n",
	} {
		cfg := defaultConfig()
		if err := applyConfig(&cfg, []byte(body)); err == nil {
			t.Fatalf("expected %q to be rejected", body)
		}
	}
}

func TestConfigRejectsRegexWithoutCaptureGroup(t *testing.T) {
	cfg := defaultConfig()
	if err := applyConfig(&cfg, []byte("version_baseline_regex = tag = \"v[0-9.]+\"\n")); err == nil {
		t.Fatal("a baseline regex without a capture group has nothing to report")
	}
	if err := applyConfig(&cfg, []byte("version_baseline_regex = tag = \"(v[0-9.]+\n")); err == nil {
		t.Fatal("an uncompilable regex must be rejected")
	}
}

// A regex may contain "=", so only the first " => " separates it from the
// replacement template.
func TestConfigReferenceTemplateRendersTheComparedValue(t *testing.T) {
	cfg := defaultConfig()
	body := "version_reference_regex = build\\.Tag=([0-9]+\\.[0-9]+\\.[0-9]+) => v${1}\n"
	if err := applyConfig(&cfg, []byte(body)); err != nil {
		t.Fatal(err)
	}
	if len(cfg.versionReferences) != 1 {
		t.Fatalf("the first occurrence replaces the defaults: %d", len(cfg.versionReferences))
	}
	got := cfg.versionReferences[0].versions("go build -X build.Tag=1.2.3")
	if len(got) != 1 || got[0] != "v1.2.3" {
		t.Fatalf("versions=%v", got)
	}
}

func TestConfigReferenceVersionsAreDistinctAndSorted(t *testing.T) {
	pattern := defaultConfig().versionReferences[0]
	got := pattern.versions("v1.2.0 then v1.10.0 then v1.2.0")
	if strings.Join(got, ",") != "v1.10.0,v1.2.0" {
		t.Fatalf("versions=%v", got)
	}
}

// An explicitly named config file that cannot be read is an error: falling back
// to the defaults would run a different check than the caller asked for, while
// a repository that simply has no config file is expected to use the defaults.
func TestConfigExplicitPathMustExist(t *testing.T) {
	dir := t.TempDir()
	deps := testDeps(dir)

	cfg, err := loadConfig(deps, dir, "")
	if err != nil {
		t.Fatalf("a missing default config must fall back: %v", err)
	}
	if cfg.docsRoot != "docs" {
		t.Fatalf("docsRoot=%q", cfg.docsRoot)
	}

	if _, err := loadConfig(deps, dir, "scripts/nope.conf"); err == nil {
		t.Fatal("an explicitly named missing config must fail")
	}

	writeTestFile(t, filepath.Join(dir, "other.conf"), "docs_root = handbook\n")
	cfg, err = loadConfig(deps, dir, "other.conf")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.docsRoot != "handbook" {
		t.Fatalf("docsRoot=%q", cfg.docsRoot)
	}
}

// A configuration that would make a recipe compare nothing and still succeed is
// refused rather than obeyed.
func TestConfigValidateRefusesEmptyRequiredFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*config)
	}{
		{"source", func(c *config) { c.versionSource = "" }},
		{"baseline", func(c *config) { c.versionBaseline = nil }},
		{"references", func(c *config) { c.versionReferences = nil }},
		{"globs", func(c *config) { c.versionPathspecs = nil }},
		{"docs root", func(c *config) { c.docsRoot = "" }},
		{"languages", func(c *config) { c.docsLanguages = nil }},
		{"files", func(c *config) { c.docsFiles = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := defaultConfig()
			test.mutate(&cfg)
			if err := cfg.validate(); err == nil {
				t.Fatal("expected validation to fail")
			}
		})
	}
}

func TestConfigLoadReportsAnInvalidFile(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "scripts", "ci-recipes.conf"), "docs_languages =\n")
	if _, err := loadConfig(testDeps(dir), dir, ""); err == nil {
		t.Fatal("expected the malformed config to be rejected")
	}
	writeTestFile(t, filepath.Join(dir, "scripts", "ci-recipes.conf"), "docs_files = guide.md\ndocs_root = docs\n")
	if _, err := loadConfig(testDeps(dir), dir, ""); err != nil {
		t.Fatal(err)
	}
}

// The README documents a config file that restates every default. Parsing it
// has to produce exactly the defaults, or the documentation is wrong.
func TestConfigDocumentedExampleReproducesTheDefaults(t *testing.T) {
	documented := strings.Join([]string{
		`version_source = internal/config/config.go`,
		`version_baseline_regex = tag = "(v[0-9]+\.[0-9]+\.[0-9]+)"`,
		`version_reference_regex = (v[0-9]+\.[0-9]+\.[0-9]+)`,
		`version_reference_regex = main\.Version=([0-9]+\.[0-9]+\.[0-9]+) => v${1}`,
		`version_ignore_marker = version-check-ignore`,
		`version_globs = *.md *.yml *.yaml *.example Makefile *Dockerfile* *.sh *.go`,
		`version_exclude = CHANGELOG.md`,
		`docs_root = docs`,
		`docs_languages = zh fr de ko ja`,
		`docs_files = development.md guide.md README.md`,
		``,
	}, "\n")

	loaded := defaultConfig()
	if err := applyConfig(&loaded, []byte(documented)); err != nil {
		t.Fatalf("the documented example must parse: %v", err)
	}
	if err := loaded.validate(); err != nil {
		t.Fatal(err)
	}

	want := defaultConfig()
	if loaded.versionSource != want.versionSource || loaded.versionIgnore != want.versionIgnore || loaded.docsRoot != want.docsRoot {
		t.Fatalf("scalars differ: %+v", loaded)
	}
	if loaded.versionBaseline.String() != want.versionBaseline.String() {
		t.Fatalf("baseline = %q, want %q", loaded.versionBaseline, want.versionBaseline)
	}
	for _, pair := range [][2][]string{
		{loaded.versionPathspecs, want.versionPathspecs},
		{loaded.versionExclude, want.versionExclude},
		{loaded.docsLanguages, want.docsLanguages},
		{loaded.docsFiles, want.docsFiles},
	} {
		if strings.Join(pair[0], "\x00") != strings.Join(pair[1], "\x00") {
			t.Fatalf("list differs: %v vs %v", pair[0], pair[1])
		}
	}
	if len(loaded.versionReferences) != len(want.versionReferences) {
		t.Fatalf("reference count = %d, want %d", len(loaded.versionReferences), len(want.versionReferences))
	}
	for index := range loaded.versionReferences {
		if loaded.versionReferences[index].expr.String() != want.versionReferences[index].expr.String() ||
			loaded.versionReferences[index].template != want.versionReferences[index].template {
			t.Fatalf("reference %d differs: %+v", index, loaded.versionReferences[index])
		}
	}
}
