package checks

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveRevisionAcceptsCanonicalGitGlobalHeader(t *testing.T) {
	t.Parallel()
	archive := gitArchiveFixture(t, &tar.Header{
		Name:       "pax_global_header",
		Typeflag:   tar.TypeXGlobalHeader,
		PAXRecords: map[string]string{"comment": strings.Repeat("a", 40)},
	})
	runner := &fakeCommandRunner{run: func(command Command) error {
		_, err := command.Stdout.Write(archive)
		return err
	}}
	destination, err := archiveRevision(context.Background(), runner, t.TempDir(), "HEAD", nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(destination)
	contents, err := os.ReadFile(filepath.Join(destination, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "fixture\n" {
		t.Fatalf("contents=%q", contents)
	}
}

func TestArchiveRevisionRejectsNonCanonicalGlobalHeaders(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		header *tar.Header
	}{
		{name: "wrong name", header: &tar.Header{Name: "global", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": strings.Repeat("a", 40)}}},
		{name: "invalid commit", header: &tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "HEAD"}}},
		{name: "extra record", header: &tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": strings.Repeat("a", 40), "path": "README.md"}}},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			archive := gitArchiveFixture(t, test.header)
			runner := &fakeCommandRunner{run: func(command Command) error {
				_, err := command.Stdout.Write(archive)
				return err
			}}
			if _, err := archiveRevision(context.Background(), runner, t.TempDir(), "HEAD", nil, io.Discard); err == nil {
				t.Fatal("non-canonical global header accepted")
			}
		})
	}
}

func gitArchiveFixture(t *testing.T, global *tar.Header) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(global); err != nil {
		t.Fatal(err)
	}
	contents := []byte("fixture\n")
	if err := writer.WriteHeader(&tar.Header{Name: "README.md", Mode: 0o644, Size: int64(len(contents)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func TestGoVersionContractUsesIsolatedArchive(t *testing.T) {
	t.Parallel()
	root := makeContractFixture(t)
	archive := tarDirectory(t, root)
	runner := &fakeCommandRunner{run: func(command Command) error {
		if command.Name != "git" {
			t.Fatalf("command=%#v", command)
		}
		_, err := command.Stdout.Write(archive)
		return err
	}}
	var stdout bytes.Buffer
	if err := executeGoVersionContract(context.Background(), root, &stdout, io.Discard, runner); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "passed") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	data, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "1.26") {
		t.Fatal("self-test changed the worktree README")
	}
}

func TestGoVersionContractFailsClosedOnCorruptArchive(t *testing.T) {
	t.Parallel()
	runner := &fakeCommandRunner{run: func(command Command) error {
		_, err := io.WriteString(command.Stdout, "not a tar archive")
		return err
	}}
	err := executeGoVersionContract(context.Background(), t.TempDir(), io.Discard, io.Discard, runner)
	if err == nil || !strings.Contains(err.Error(), "archive") {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateReleaseWorkflow(t *testing.T) {
	t.Parallel()
	valid := `
group: ${{ github.repository }}-release-${{ github.event_name == 'workflow_dispatch' && inputs.release_tag || github.ref_name }}
queue: max
name: Prepare and validate curated release notes
name: Resolve existing immutable image
name: Attest release artifacts
name: Extract metadata
pattern={{version}}
name: Build amd64 image
group: ${{ github.repository }}-release-aliases
queue: max
`
	if err := validateReleaseWorkflow(valid, "gh release upload"); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, replace, with string
	}{
		{name: "shared concurrency", replace: "group: ${{ github.repository }}-release-${{ github.event_name == 'workflow_dispatch' && inputs.release_tag || github.ref_name }}", with: "group: releases"},
		{name: "notes after publish", replace: "name: Prepare and validate curated release notes", with: ""},
		{name: "mutable metadata", replace: "pattern={{version}}", with: "pattern={{major}}"},
	}
	for _, test := range cases {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := validateReleaseWorkflow(strings.Replace(valid, test.replace, test.with, 1), ""); err == nil {
				t.Fatal("expected failure")
			}
		})
	}
	if err := validateReleaseWorkflow(valid, "gh release delete v1.0.0 --yes"); err == nil {
		t.Fatal("whole-release deletion unexpectedly accepted")
	}
	if err := validateReleaseWorkflowWiring("run: bash .github/scripts/check-doc-contracts.sh", false); err == nil {
		t.Fatal("workflow reference to a removed shell recipe unexpectedly accepted")
	}
	if err := validateReleaseWorkflowWiring("run: ci-recipes stargate check doc-contracts", false); err != nil {
		t.Fatal(err)
	}
}

func TestCurrentStargateReleaseWorkflow(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "..", "sources", "stargate"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".github", "workflows", "release.yml")); err != nil {
		t.Skip("shared stargate source fixture is unavailable")
	}
	var stdout strings.Builder
	if err := executeReleaseWorkflow(context.Background(), root, &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "passed") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestDispatcherUsage(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, args := range [][]string{nil, {"unknown"}, {"doc-contracts", "a", "b"}, {"release-workflow", "extra"}} {
		if err := executeWithOptions(context.Background(), args, nil, io.Discard, io.Discard, options{root: root}); err == nil {
			t.Fatalf("args=%v unexpectedly succeeded", args)
		}
	}
}

func TestFindStargateRootFromNestedDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	marker := filepath.Join(root, "src", "cmd", "stargate")
	nested := filepath.Join(root, "docs", "enUS", "examples")
	if err := os.MkdirAll(marker, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := findStargateRoot(nested)
	if err != nil {
		t.Fatal(err)
	}
	if got != root {
		t.Fatalf("root=%q, want %q", got, root)
	}
}
