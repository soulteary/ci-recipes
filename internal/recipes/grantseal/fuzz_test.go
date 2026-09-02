package grantseal

import (
	"path"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func FuzzNormalizeArchivePath(f *testing.F) {
	for _, seed := range []string{"LICENSE", "./pkg/license-tool", "../LICENSE", "/README", "C:/README", "safe/C:stream/README", "a\\b", "a\nREADME", "日本語/README.md"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		got, directory, err := normalizeArchivePath(input, false)
		if err != nil {
			return
		}
		if got == "" && !directory {
			t.Fatal("accepted an empty non-directory path")
		}
		if path.IsAbs(got) || got == ".." || strings.HasPrefix(got, "../") || strings.ContainsAny(got, `\:`) || !utf8.ValidString(got) {
			t.Fatalf("accepted unsafe path %q from %q", got, input)
		}
		for _, r := range got {
			if unicode.IsControl(r) {
				t.Fatalf("accepted control character in %q", got)
			}
		}
	})
}

func FuzzBaseImagePrefixesRejectSensitiveNames(f *testing.F) {
	for _, seed := range []string{"private.key", "config.json", "source.go", ".env", "id_rsa"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, basename string) {
		if basename == "" || strings.ContainsAny(basename, `/\\:`) || !utf8.ValidString(basename) {
			return
		}
		path := "usr/share/zoneinfo/" + basename
		if sensitiveImagePath(path) && baseImageMemberAllowed(imageMember{Path: path, Mode: "0644", Regular: true, TypeKnown: true}) && imageMemberSafe(imageMember{Path: path, Mode: "0644", Regular: true, TypeKnown: true}) {
			t.Fatalf("sensitive base path was accepted: %q", path)
		}
	})
}

func FuzzCoverageAndDiffParsersDoNotPanic(f *testing.F) {
	f.Add("mode: atomic\nx.go:1.1,1.2 1 1\n", "+++ b/x.go\n@@ -0,0 +1 @@\n+x\n")
	f.Add("", "")
	f.Fuzz(func(t *testing.T, profile, diff string) {
		_, _ = parseCoverageProfile([]byte(profile))
		_, _ = parseAddedLines([]byte(diff))
	})
}
