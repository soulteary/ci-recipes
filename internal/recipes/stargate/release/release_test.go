package release

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/soulteary/ci-recipes/internal/cli"
)

type fakeResult struct {
	stdout string
	stderr string
	err    error
}

type fakeRunner struct {
	commands []Command
	results  []fakeResult
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func (f *fakeRunner) Run(_ context.Context, command Command) error {
	f.commands = append(f.commands, command)
	index := len(f.commands) - 1
	if index >= len(f.results) {
		return nil
	}
	result := f.results[index]
	if command.Stdout != nil {
		io.WriteString(command.Stdout, result.stdout)
	}
	if command.Stderr != nil {
		io.WriteString(command.Stderr, result.stderr)
	}
	return result.err
}

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func writeFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExtractNotes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	changelog := filepath.Join(dir, "CHANGELOG.md")
	writeFile(t, changelog, "# Changes\n\n## [1.2.3] - 2026-09-02\n\n- Read [migration](docs/enUS/MIGRATION_V1.md).\n\n### Release verification\nnot released\n\n## [1.2.2] - 2026-08-01\n- old\n")

	cases := []struct {
		name       string
		tag        string
		wantHeader string
	}{
		{name: "stable", tag: "v1.2.3", wantHeader: "## [1.2.3] - 2026-09-02"},
		{name: "prerelease", tag: "v1.2.3-rc.1", wantHeader: "## [1.2.3-rc.1] - Prerelease"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			output := filepath.Join(dir, tc.name+".md")
			err := executeExtract([]string{tc.tag, output, changelog}, io.Discard, Options{Getenv: env(map[string]string{"GITHUB_REPOSITORY": "owner/repo"})}.normalized())
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			got := string(data)
			for _, want := range []string{tc.wantHeader, "https://github.com/owner/repo/blob/" + tc.tag + "/docs/enUS/MIGRATION_V1.md", "Upgrade instructions:"} {
				if !strings.Contains(got, want) {
					t.Errorf("notes missing %q:\n%s", want, got)
				}
			}
			if strings.Contains(got, "Release verification") || strings.Contains(got, "not released") {
				t.Errorf("notes leaked verification section:\n%s", got)
			}
		})
	}
}

func TestExtractNotesRejectsInvalidInputWithoutReplacingOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	output := filepath.Join(dir, "notes.md")
	changelog := filepath.Join(dir, "CHANGELOG.md")
	writeFile(t, output, "keep")
	writeFile(t, changelog, "## [1.2.3] - not-a-date\n- body\n")

	cases := []struct{ name, tag string }{
		{"leading zero", "v01.2.3"},
		{"metadata", "v1.2.3+build"},
		{"bad heading", "v1.2.3"},
		{"missing section", "v1.2.4"},
	}
	for _, tc := range cases {
		err := executeExtract([]string{tc.tag, output, changelog}, io.Discard, Options{Getenv: env(nil)}.normalized())
		if err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
		data, readErr := os.ReadFile(output)
		if readErr != nil || string(data) != "keep" {
			t.Fatalf("%s: output changed: %q, %v", tc.name, data, readErr)
		}
	}
}

func TestExtractNotesValidatesCalendarDate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		date    string
		wantErr bool
	}{
		{name: "leap day", date: "2024-02-29"},
		{name: "non-leap day", date: "2025-02-29", wantErr: true},
		{name: "invalid month", date: "2026-13-01", wantErr: true},
		{name: "invalid day", date: "2026-04-31", wantErr: true},
		{name: "year zero", date: "0000-01-01", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			changelog := filepath.Join(t.TempDir(), "CHANGELOG.md")
			writeFile(t, changelog, "## [1.2.3] - "+tc.date+"\n\n- body\n")
			notes, err := extractNotes("v1.2.3", changelog, "owner/repo")
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "exact dated CHANGELOG heading") {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(notes), "## [1.2.3] - "+tc.date+"\n") {
				t.Fatalf("unexpected output: %q", notes)
			}
		})
	}
}

func TestParseTagUsesStrictSemVerPrereleaseRules(t *testing.T) {
	t.Parallel()
	for _, tag := range []string{
		"v0.0.0",
		"v1.2.3-alpha",
		"v1.2.3-alpha.1",
		"v1.2.3-0.3.7",
		"v1.2.3-x-7.z--",
		"v1.2.3-01a",
	} {
		if _, ok := parseTag(tag); !ok {
			t.Errorf("parseTag(%q) rejected a valid release tag", tag)
		}
	}
	for _, tag := range []string{
		"1.2.3",
		"v01.2.3",
		"v1.02.3",
		"v1.2.03",
		"v1.2.3-",
		"v1.2.3-alpha..1",
		"v1.2.3-01",
		"v1.2.3-alpha.001",
		"v1.2.3+build",
		"v1.2.3-alpha+build",
		"v1.2.3 --repo=attacker/repo",
	} {
		if _, ok := parseTag(tag); ok {
			t.Errorf("parseTag(%q) accepted an invalid release tag", tag)
		}
	}
}

func TestEveryReleaseEntryPointRejectsInvalidTagBeforeSideEffects(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{}
	opts := Options{Getenv: env(map[string]string{"GITHUB_REPOSITORY": "owner/repo"}), Runner: runner}.normalized()
	dir := t.TempDir()
	output := filepath.Join(dir, "notes")
	writeFile(t, output, "keep")

	tests := []struct {
		name string
		run  func() error
	}{
		{name: "extract", run: func() error { return executeExtract([]string{"v1.2.3-01", output}, io.Discard, opts) }},
		{name: "prepare", run: func() error {
			return executePrepare(context.Background(), []string{"v1.2.3-01", output}, io.Discard, io.Discard, opts)
		}},
		{name: "plan", run: func() error { return executePlan([]string{"v1.2.3-01", "missing"}, io.Discard) }},
		{name: "publish", run: func() error {
			return executePublish(context.Background(), []string{"v1.2.3-01", "missing", "missing"}, io.Discard, io.Discard, opts)
		}},
		{name: "reconcile", run: func() error {
			return executeReconcile(context.Background(), []string{"v1.2.3-01", "ghcr.io/owner/image"}, io.Discard, io.Discard, opts)
		}},
	}
	for _, tc := range tests {
		if err := tc.run(); err == nil || !strings.Contains(err.Error(), "unsupported release tag") {
			t.Errorf("%s error = %v", tc.name, err)
		}
	}
	if len(runner.commands) != 0 {
		t.Fatalf("invalid tags ran external commands: %#v", runner.commands)
	}
	data, err := os.ReadFile(output)
	if err != nil || string(data) != "keep" {
		t.Fatalf("invalid tags changed output: %q, %v", data, err)
	}
}

func TestReleaseCommandsRejectExtraArgumentsAsUsageErrors(t *testing.T) {
	t.Parallel()
	opts := Options{Getenv: env(nil), Runner: &fakeRunner{}}.normalized()
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "extract", run: func() error { return executeExtract([]string{"v1.2.3", "out", "changes", "extra"}, io.Discard, opts) }},
		{name: "prepare", run: func() error {
			return executePrepare(context.Background(), []string{"v1.2.3", "out", "changes", "extra"}, io.Discard, io.Discard, opts)
		}},
		{name: "plan", run: func() error { return executePlan([]string{"v1.2.3", "tags", "extra"}, io.Discard) }},
		{name: "publish", run: func() error {
			return executePublish(context.Background(), []string{"v1.2.3", "notes", "dist", "extra"}, io.Discard, io.Discard, opts)
		}},
		{name: "reconcile", run: func() error {
			return executeReconcile(context.Background(), []string{"v1.2.3", "ghcr.io/owner/image", "extra"}, io.Discard, io.Discard, opts)
		}},
	}
	for _, tc := range tests {
		if err := tc.run(); cli.ExitCode(err) != 2 {
			t.Errorf("%s exit code = %d, want 2 (error: %v)", tc.name, cli.ExitCode(err), err)
		}
	}
}

func TestExecuteAcceptsCanonicalPublicCommandName(t *testing.T) {
	t.Parallel()
	err := ExecuteWithOptions(context.Background(), []string{"plan-release-aliases", "v1.2.3-rc.1", "not-read"}, nil, io.Discard, io.Discard, Options{Getenv: env(nil), Runner: &fakeRunner{}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPlanAliasesUsesSemanticHighWaterMark(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "tags")
	writeFile(t, path, "junk\nv1.2.9\nv1.2.10\nv2.0.0-rc.1\nv1.9.0\nv2.0.0\n")
	got, err := planAliases("v1.2.8", path)
	if err != nil {
		t.Fatal(err)
	}
	want := []alias{{"latest", "v2.0.0"}, {"1", "v1.9.0"}, {"1.2", "v1.2.10"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan = %#v, want %#v", got, want)
	}
}

func TestPlanAliasesPrereleaseDoesNotReadFile(t *testing.T) {
	t.Parallel()
	got, err := planAliases("v1.2.3-rc.1", filepath.Join(t.TempDir(), "missing"))
	if err != nil || got != nil {
		t.Fatalf("plan = %#v, err = %v", got, err)
	}
}

func TestPrepareFallback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	changelog := filepath.Join(dir, "CHANGELOG.md")
	output := filepath.Join(dir, "notes.md")
	writeFile(t, changelog, "# no matching release\n")
	runner := &fakeRunner{results: []fakeResult{{stdout: "historical notes\n"}}}
	var stdout bytes.Buffer
	err := executePrepare(context.Background(), []string{"v0.1.0", output, changelog}, &stdout, io.Discard, Options{
		Getenv: env(map[string]string{"ALLOW_EXISTING_RELEASE_NOTES": "true", "GITHUB_REPOSITORY": "owner/repo"}),
		Runner: runner,
	}.normalized())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(output)
	if string(data) != "historical notes\n" || !strings.Contains(stdout.String(), "Reusing existing") {
		t.Fatalf("data=%q stdout=%q", data, stdout.String())
	}
	wantArgs := []string{"release", "view", "--repo", "owner/repo", "--json", "body", "--jq", ".body", "--", "v0.1.0"}
	if len(runner.commands) != 1 || !reflect.DeepEqual(runner.commands[0].Args, wantArgs) {
		t.Fatalf("commands = %#v", runner.commands)
	}
}

func TestPrepareFallbackRejectsNullAndDisabledFallback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	changelog := filepath.Join(dir, "CHANGELOG.md")
	writeFile(t, changelog, "missing\n")
	for _, tc := range []struct {
		name   string
		values map[string]string
		result fakeResult
	}{
		{name: "disabled", values: map[string]string{}},
		{name: "null", values: map[string]string{"ALLOW_EXISTING_RELEASE_NOTES": "true", "GITHUB_REPOSITORY": "owner/repo"}, result: fakeResult{stdout: "null\n"}},
		{name: "empty", values: map[string]string{"ALLOW_EXISTING_RELEASE_NOTES": "true", "GITHUB_REPOSITORY": "owner/repo"}, result: fakeResult{stdout: " \n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{results: []fakeResult{tc.result}}
			err := executePrepare(context.Background(), []string{"v1.0.0", filepath.Join(dir, tc.name), changelog}, io.Discard, io.Discard, Options{Getenv: env(tc.values), Runner: runner}.normalized())
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestPublishExistingReleaseReconcilesAssets(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	notes := filepath.Join(dir, "notes.md")
	dist := filepath.Join(dir, "dist")
	if err := os.Mkdir(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, notes, "release notes")
	writeFile(t, filepath.Join(dist, "current.tgz"), "data")
	writeFile(t, filepath.Join(dist, ".hidden"), "ignored")
	runner := &fakeRunner{results: []fakeResult{{stdout: `{"assets":[{"name":"current.tgz"},{"name":"obsolete.zip"}]}`}, {}, {}, {}}}
	err := executePublish(context.Background(), []string{"v1.0.0", notes, dist}, io.Discard, io.Discard, Options{Getenv: env(map[string]string{"GITHUB_REPOSITORY": "owner/repo"}), Runner: runner}.normalized())
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.commands) != 4 {
		t.Fatalf("got %d commands", len(runner.commands))
	}
	if got := strings.Join(runner.commands[1].Args, " "); !strings.Contains(got, "release upload --repo owner/repo --clobber -- v1.0.0") || !strings.Contains(got, "current.tgz") || strings.Contains(got, ".hidden") {
		t.Errorf("upload args = %q", got)
	}
	if got := strings.Join(runner.commands[2].Args, " "); !strings.Contains(got, "delete-asset --repo owner/repo --yes -- v1.0.0 obsolete.zip") {
		t.Errorf("delete args = %q", got)
	}
}

func TestPublishMissingReleaseCreatesIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	notes, dist := filepath.Join(dir, "notes"), filepath.Join(dir, "dist")
	writeFile(t, notes, "notes")
	if err := os.Mkdir(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dist, "a"), "a")
	runner := &fakeRunner{results: []fakeResult{{stderr: "release not found\n", err: errors.New("exit status 1")}, {}}}
	err := executePublish(context.Background(), []string{"v1.0.0-rc.1", notes, dist}, io.Discard, io.Discard, Options{Getenv: env(map[string]string{"GITHUB_REPOSITORY": "owner/repo"}), Runner: runner}.normalized())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(runner.commands[1].Args, " "); !strings.Contains(got, "release create --repo owner/repo") || !strings.Contains(got, "--prerelease=true") || !strings.Contains(got, "--verify-tag -- v1.0.0-rc.1") {
		t.Fatalf("create args = %q", got)
	}
}

func TestPublishDoesNotTreatArbitraryViewFailureAsMissing(t *testing.T) {
	t.Parallel()
	for _, diagnostics := range []string{
		"authentication failed\n",
		"HTTP 404: repository not found\n",
		"HTTP 404: Not Found\n",
	} {
		dir := t.TempDir()
		notes, dist := filepath.Join(dir, "notes"), filepath.Join(dir, "dist")
		writeFile(t, notes, "notes")
		if err := os.Mkdir(dist, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dist, "asset"), "asset")
		runner := &fakeRunner{results: []fakeResult{{stderr: diagnostics, err: errors.New("exit status 1")}}}
		err := executePublish(context.Background(), []string{"v1.0.0", notes, dist}, io.Discard, io.Discard, Options{
			Getenv: env(map[string]string{"GITHUB_REPOSITORY": "owner/repo"}), Runner: runner,
		}.normalized())
		if err == nil || !strings.Contains(err.Error(), strings.TrimSpace(diagnostics)) {
			t.Fatalf("diagnostics %q: error = %v", diagnostics, err)
		}
		if len(runner.commands) != 1 {
			t.Fatalf("diagnostics %q ran mutation commands: %#v", diagnostics, runner.commands)
		}
	}
}

func TestPublishRejectsMalformedExistingAssetOutputBeforeMutation(t *testing.T) {
	t.Parallel()
	for _, output := range []string{"not JSON", `{}`, `{"assets":null}`} {
		dir := t.TempDir()
		notes, dist := filepath.Join(dir, "notes"), filepath.Join(dir, "dist")
		writeFile(t, notes, "notes")
		if err := os.Mkdir(dist, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dist, "asset"), "asset")
		runner := &fakeRunner{results: []fakeResult{{stdout: output}}}
		err := executePublish(context.Background(), []string{"v1.0.0", notes, dist}, io.Discard, io.Discard, Options{
			Getenv: env(map[string]string{"GITHUB_REPOSITORY": "owner/repo"}), Runner: runner,
		}.normalized())
		if err == nil || !strings.Contains(err.Error(), "decode GitHub Release assets") {
			t.Fatalf("output %q: error = %v", output, err)
		}
		if len(runner.commands) != 1 {
			t.Fatalf("output %q ran mutation commands: %#v", output, runner.commands)
		}
	}
}

func TestPublishUsesOptionSeparatorsForUntrustedAssetNames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	notes, dist := filepath.Join(dir, "notes"), filepath.Join(dir, "dist")
	writeFile(t, notes, "notes")
	if err := os.Mkdir(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dist, "--local-option"), "asset")
	runner := &fakeRunner{results: []fakeResult{{stdout: `{"assets":[{"name":"--remote-option"}]}`}, {}, {}, {}}}
	err := executePublish(context.Background(), []string{"v1.0.0", notes, dist}, io.Discard, io.Discard, Options{
		Getenv: env(map[string]string{"GITHUB_REPOSITORY": "owner/repo"}), Runner: runner,
	}.normalized())
	if err != nil {
		t.Fatal(err)
	}
	if got := runner.commands[1].Args; len(got) < 8 || got[5] != "--" || got[6] != "v1.0.0" {
		t.Fatalf("upload lacks option separator: %#v", got)
	}
	if got := runner.commands[2].Args; len(got) != 8 || got[5] != "--" || got[6] != "v1.0.0" || got[7] != "--remote-option" {
		t.Fatalf("delete lacks option separator: %#v", got)
	}
}

func TestPublishRejectsNonRegularOrSymlinkInputs(t *testing.T) {
	t.Parallel()
	makeBase := func(t *testing.T) (string, string, Options) {
		t.Helper()
		dir := t.TempDir()
		notes, dist := filepath.Join(dir, "notes"), filepath.Join(dir, "dist")
		writeFile(t, notes, "notes")
		if err := os.Mkdir(dist, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dist, "asset"), "asset")
		return notes, dist, Options{Getenv: env(map[string]string{"GITHUB_REPOSITORY": "owner/repo"}), Runner: &fakeRunner{}}.normalized()
	}
	t.Run("notes symlink", func(t *testing.T) {
		notes, dist, opts := makeBase(t)
		link := filepath.Join(filepath.Dir(notes), "notes-link")
		if err := os.Symlink(notes, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		err := executePublish(context.Background(), []string{"v1.0.0", link, dist}, io.Discard, io.Discard, opts)
		if err == nil || !strings.Contains(err.Error(), "non-symlink regular file") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("asset symlink escapes directory", func(t *testing.T) {
		notes, dist, opts := makeBase(t)
		if err := os.Remove(filepath.Join(dist, "asset")); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(filepath.Dir(dist), "outside")
		writeFile(t, outside, "outside")
		if err := os.Symlink(outside, filepath.Join(dist, "asset")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		err := executePublish(context.Background(), []string{"v1.0.0", notes, dist}, io.Discard, io.Discard, opts)
		if err == nil || !strings.Contains(err.Error(), "non-symlink regular file") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("visible directory asset", func(t *testing.T) {
		notes, dist, opts := makeBase(t)
		if err := os.Mkdir(filepath.Join(dist, "directory"), 0o755); err != nil {
			t.Fatal(err)
		}
		err := executePublish(context.Background(), []string{"v1.0.0", notes, dist}, io.Discard, io.Discard, opts)
		if err == nil || !strings.Contains(err.Error(), "non-symlink regular file") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("dist symlink", func(t *testing.T) {
		notes, dist, opts := makeBase(t)
		link := filepath.Join(filepath.Dir(dist), "dist-link")
		if err := os.Symlink(dist, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		err := executePublish(context.Background(), []string{"v1.0.0", notes, link}, io.Discard, io.Discard, opts)
		if err == nil || !strings.Contains(err.Error(), "non-symlink directory") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestReconcileAliases(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("a", 64)
	runner := &fakeRunner{results: []fakeResult{
		{stdout: "v1.2.3\nv1.2.4\nv2.0.0\n"},
		{stdout: "Name: x\nDigest: " + digest + "\n"},
		{stdout: "Digest: " + digest + "\n"},
		{stdout: "Digest: " + digest + "\n"},
		{}, {}, {}, {},
	}}
	err := executeReconcile(context.Background(), []string{"v1.2.3", "ghcr.io/owner/image"}, io.Discard, io.Discard, Options{Getenv: env(map[string]string{"GITHUB_REPOSITORY": "owner/repo"}), Runner: runner}.normalized())
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.commands) != 8 {
		t.Fatalf("got %d commands: %#v", len(runner.commands), runner.commands)
	}
	for index := 1; index <= 3; index++ {
		if got := strings.Join(runner.commands[index].Args, " "); !strings.Contains(got, "imagetools inspect --") {
			t.Fatalf("command %d mutated before all validation completed: %q", index, got)
		}
	}
	if got := strings.Join(runner.commands[7].Args, " "); got != "release edit --repo owner/repo --latest -- v2.0.0" {
		t.Fatalf("latest args = %q", got)
	}
	for index, want := range []string{"--tag ghcr.io/owner/image:1.2", "--tag ghcr.io/owner/image:1", "--tag ghcr.io/owner/image:latest"} {
		if got := strings.Join(runner.commands[index+4].Args, " "); !strings.Contains(got, want) {
			t.Errorf("mutation %d = %q, want %q", index, got, want)
		}
	}
}

func TestReconcileValidationFailureDoesNotMutateAnyAlias(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("a", 64)
	runner := &fakeRunner{results: []fakeResult{
		{stdout: "v1.2.3\nv2.0.0\n"},
		{stdout: "Digest: " + digest + "\n"},
		{stdout: "Digest: not-a-digest\n"},
	}}
	err := executeReconcile(context.Background(), []string{"v1.2.3", "ghcr.io/owner/image"}, io.Discard, io.Discard, Options{
		Getenv: env(map[string]string{"GITHUB_REPOSITORY": "owner/repo"}), Runner: runner,
	}.normalized())
	if err == nil || !strings.Contains(err.Error(), "immutable digest") {
		t.Fatalf("error = %v", err)
	}
	if len(runner.commands) != 3 {
		t.Fatalf("validation failure should stop before mutation: %#v", runner.commands)
	}
	for _, command := range runner.commands {
		if strings.Contains(strings.Join(command.Args, " "), "imagetools create") {
			t.Fatalf("validation failure mutated alias: %#v", command)
		}
	}
}

func TestReconcileMutationFailureReportsPartialStateAndSkipsGitHubLatest(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("b", 64)
	runner := &fakeRunner{results: []fakeResult{
		{stdout: "v1.2.3\nv2.0.0\n"},
		{stdout: "Digest: " + digest + "\n"},
		{stdout: "Digest: " + digest + "\n"},
		{stdout: "Digest: " + digest + "\n"},
		{},
		{err: errors.New("registry unavailable")},
	}}
	err := executeReconcile(context.Background(), []string{"v1.2.3", "ghcr.io/owner/image"}, io.Discard, io.Discard, Options{
		Getenv: env(map[string]string{"GITHUB_REPOSITORY": "owner/repo"}), Runner: runner,
	}.normalized())
	if err == nil || !strings.Contains(err.Error(), "after 1 successful alias update") || !strings.Contains(err.Error(), "partially reconciled") {
		t.Fatalf("error = %v", err)
	}
	if len(runner.commands) != 6 {
		t.Fatalf("unexpected commands after mutation failure: %#v", runner.commands)
	}
	for _, command := range runner.commands {
		if command.Name == "gh" && len(command.Args) > 1 && command.Args[0] == "release" && command.Args[1] == "edit" {
			t.Fatalf("GitHub Latest changed after alias failure: %#v", command)
		}
	}
}

func TestReconcileRejectsOptionInjectionBeforeCommands(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		tag   string
		image string
	}{
		{name: "tag", tag: "--help", image: "ghcr.io/owner/image"},
		{name: "tag trailing option", tag: "v1.2.3 --help", image: "ghcr.io/owner/image"},
		{name: "image option", tag: "v1.2.3", image: "--help/owner/image"},
		{name: "image tag", tag: "v1.2.3", image: "ghcr.io/owner/image:latest"},
		{name: "image digest", tag: "v1.2.3", image: "ghcr.io/owner/image@sha256:deadbeef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{}
			err := executeReconcile(context.Background(), []string{tc.tag, tc.image}, io.Discard, io.Discard, Options{
				Getenv: env(map[string]string{"GITHUB_REPOSITORY": "owner/repo"}), Runner: runner,
			}.normalized())
			if err == nil {
				t.Fatal("expected error")
			}
			if len(runner.commands) != 0 {
				t.Fatalf("invalid input ran commands: %#v", runner.commands)
			}
		})
	}
}

func TestParseDigestReportsScannerFailure(t *testing.T) {
	t.Parallel()
	valid := "sha256:" + strings.Repeat("a", 64)
	for _, output := range []string{
		strings.Repeat("x", 70*1024) + "\nDigest: " + valid,
		"Digest: " + valid + "\n" + strings.Repeat("x", 70*1024),
	} {
		_, err := parseDigest(output)
		if err == nil || !strings.Contains(err.Error(), "token too long") {
			t.Fatalf("error = %v", err)
		}
	}
	_, err := parseDigest("Digest: " + valid + "\nDigest: sha256:" + strings.Repeat("b", 64) + "\n")
	if err == nil || !strings.Contains(err.Error(), "conflicting digests") {
		t.Fatalf("conflicting digest error = %v", err)
	}
}

func TestPlanAliasesReportsScannerFailure(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "tags")
	writeFile(t, path, strings.Repeat("x", 70*1024)+"\n")
	_, err := planAliases("v1.2.3", path)
	if err == nil || !strings.Contains(err.Error(), "token too long") {
		t.Fatalf("error = %v", err)
	}
}

func TestReconcilePrereleaseStillQueriesGitHub(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{results: []fakeResult{{stdout: "v1.0.0\n"}}}
	var stdout bytes.Buffer
	err := executeReconcile(context.Background(), []string{"v1.1.0-rc.1", "ghcr.io/owner/image"}, &stdout, io.Discard, Options{Getenv: env(map[string]string{"GITHUB_REPOSITORY": "owner/repo"}), Runner: runner}.normalized())
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.commands) != 1 || !strings.Contains(stdout.String(), "immutable full-version image") {
		t.Fatalf("commands=%d stdout=%q", len(runner.commands), stdout.String())
	}
}

func TestStatusAndPlanOutputErrorsAreReturned(t *testing.T) {
	t.Parallel()
	t.Run("plan", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tags")
		writeFile(t, path, "v1.2.3\n")
		err := executePlan([]string{"v1.2.3", path}, failingWriter{})
		if err == nil || !strings.Contains(err.Error(), "write alias plan") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("prerelease status", func(t *testing.T) {
		runner := &fakeRunner{results: []fakeResult{{stdout: "v1.2.3\n"}}}
		err := executeReconcile(context.Background(), []string{"v1.2.4-rc.1", "ghcr.io/owner/image"}, failingWriter{}, io.Discard, Options{
			Getenv: env(map[string]string{"GITHUB_REPOSITORY": "owner/repo"}), Runner: runner,
		}.normalized())
		if err == nil || !strings.Contains(err.Error(), "write status") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestInvalidRepositoryIsRejectedBeforeExternalCommands(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{}
	opts := Options{Getenv: env(map[string]string{"GITHUB_REPOSITORY": "--repo/attacker/repo"}), Runner: runner}.normalized()
	err := executeReconcile(context.Background(), []string{"v1.2.3", "ghcr.io/owner/image"}, io.Discard, io.Discard, opts)
	if err == nil || !strings.Contains(err.Error(), "invalid GITHUB_REPOSITORY") {
		t.Fatalf("error = %v", err)
	}
	if len(runner.commands) != 0 {
		t.Fatalf("invalid repository ran commands: %#v", runner.commands)
	}
}

func TestExecuteUsageExitCode(t *testing.T) {
	t.Parallel()
	err := ExecuteWithOptions(context.Background(), nil, nil, io.Discard, io.Discard, Options{Getenv: env(nil), Runner: &fakeRunner{}})
	if got := cli.ExitCode(err); got != 2 {
		t.Fatalf("exit code = %d, want 2", got)
	}
}
