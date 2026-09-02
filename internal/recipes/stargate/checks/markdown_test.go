package checks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFencedBlockParserCommonMarkEdges(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		markdown  string
		violation bool
	}{
		{name: "root closed", markdown: "```go\ncode\n```\n"},
		{name: "three-space opener", markdown: "   ```go\ncode\n   ```\n"},
		{name: "tilde longer closer", markdown: "~~~text\ncode\n~~~~\n"},
		{name: "nested list", markdown: "1. outer\n   - inner\n\n     ```json\n     {}\n     ```\n"},
		{name: "interleaved containers", markdown: "- > ```json\n  > {}\n  > ```\n"},
		{name: "quote then list", markdown: "> - ~~~text\n>   quoted list content\n>   ~~~\n"},
		{name: "html comment to eof", markdown: "<!--\n``` literal in HTML\n"},
		{name: "lowercase declaration", markdown: "<!doctype\n``` literal\n>\n"},
		{name: "inexact script closer", markdown: "<script>\n</script >\n``` literal\n"},
		{name: "invalid backtick info is text", markdown: "paragraph\n```bad`info\n"},
		{name: "reference shaped paragraph", markdown: "paragraph\n[ref]: /target\n22. text\n    ``` literal\n"},
		{name: "invalid nested reference label", markdown: "[a[b]: /target\n22. text\n    ``` literal\n"},
		{name: "whitespace reference label", markdown: "[   ]: /target\n22. text\n    ``` literal\n"},
		{name: "incomplete multiline label", markdown: "[incomplete\nlabel without close\n22. text\n    ``` literal\n"},
		{name: "unicode whitespace label", markdown: "[\u00a0]: /target\n22. text\n    ``` literal\n"},
		{name: "unicode whitespace destination", markdown: "[ref]: /foo\u00a0bar\n22. text\n    ``` literal\n"},
		{name: "unbalanced destination", markdown: "[ref]: /unbalanced(\n22. text\n    ``` literal\n"},
		{name: "indented code list does not interrupt", markdown: "paragraph\n-     code\n22. text\n    ``` literal\n"},
		{name: "root unclosed", markdown: "```go\ncode\n", violation: true},
		{name: "indented unclosed", markdown: "   ```go\ncode\n", violation: true},
		{name: "quoted tab unclosed", markdown: ">\t```\n> content\n", violation: true},
		{name: "mismatched marker", markdown: "```text\nwrong\n~~~\n", violation: true},
		{name: "short closer", markdown: "````text\nshort\n```\n", violation: true},
		{name: "nested list unclosed", markdown: "1. outer\n   - inner\n\n     ```json\n     {}\n", violation: true},
		{name: "container exit", markdown: "1. item\n   ```text\n   missing\nroot paragraph\n   ```\n", violation: true},
		{name: "lazy wide list", markdown: "-   paragraph\nlazy continuation\n    ```text\n    missing\n", violation: true},
		{name: "blank list item", markdown: "-    \n    ```text\n    missing\n", violation: true},
		{name: "invalid opener preserves list", markdown: "-   paragraph\n```bad`info\n    ```text\n    missing\n", violation: true},
		{name: "non-one list after indented code", markdown: "    code\n22. item\n    ```text\n    missing\n", violation: true},
		{name: "reference definition is leaf", markdown: "[ref]: /target\n22. item\n    ```text\n    missing\n", violation: true},
		{name: "multiline reference is leaf", markdown: "[ref]:\n  /target\n  \"title\"\n22. item\n    ```text\n    missing\n", violation: true},
		{name: "continued reference title", markdown: "[ref]: /target\n  \"title\"\n22. item\n    ```text\n    missing\n", violation: true},
		{name: "multiline title", markdown: "[ref]: /target \"multi\nline\ntitle\"\n22. item\n    ```text\n    missing\n", violation: true},
		{name: "escaped title", markdown: "[ref]: /target \"escaped \\\"title\\\"\"\n22. item\n    ```text\n    missing\n", violation: true},
		{name: "literal backslash label", markdown: "[foo\\bar]: /target\n22. item\n    ```text\n    missing\n", violation: true},
		{name: "escaped opening bracket label", markdown: "[a\\[b]: /target\n22. item\n    ```text\n    missing\n", violation: true},
		{name: "literal backslash title", markdown: "[ref]: /target \"foo\\bar\"\n22. item\n    ```text\n    missing\n", violation: true},
		{name: "escaped next-line title", markdown: "[ref]:\n  /target (escaped \\(title\\))\n22. item\n    ```text\n    missing\n", violation: true},
		{name: "multiline label", markdown: "[foo\nbar]: /target\n22. item\n    ```text\n    missing\n", violation: true},
		{name: "backslash ended label", markdown: "[foo\\\nbar]: /target\n22. item\n    ```text\n    missing\n", violation: true},
		{name: "balanced destination", markdown: "[ref]: /balanced(one)/escaped\\(two\\)\n22. item\n    ```text\n    missing\n", violation: true},
		{name: "setext closes paragraph", markdown: "Heading\n===\n22. item\n    ```text\n    missing\n", violation: true},
		{name: "lazy setext closes list paragraph", markdown: "- paragraph\n===\n  22. nested item\n      ```text\n      missing\n", violation: true},
		{name: "html then fence", markdown: "<!--\n``` ignored\n-->\n```text\nmissing\n", violation: true},
		{name: "type seven cannot interrupt", markdown: "paragraph\n<custom-element>\n```text\nmissing\n", violation: true},
		{name: "embedded block tag", markdown: "paragraph before <div>\n```text\nmissing\n", violation: true},
		{name: "incomplete label interrupted", markdown: "[foo\n```text\nbar]: /target\n", violation: true},
		{name: "setext interrupts incomplete label", markdown: "[foo\n===\nbar]: /target\n22. text\n    ``` literal\n"},
		{name: "unfinished title interrupted", markdown: "[ref]: /target \"foo\n```text\nbar\"\n", violation: true},
		{name: "incomplete reference interrupted", markdown: "[ref]:\n```text\nmissing\n", violation: true},
		{name: "incomplete reference tilde interrupted", markdown: "[ref]:\n~~~text\nmissing\n", violation: true},
		{name: "five-space nested list fence", markdown: "- item\n\n  - nested\n\n     ```json\n     {}\n", violation: true},
	}
	for _, test := range cases {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := len(fencedBlockViolations(test.markdown, "fixture.md")) != 0
			if got != test.violation {
				t.Fatalf("violation=%v, want %v; diagnostics=%v", got, test.violation, fencedBlockViolations(test.markdown, "fixture.md"))
			}
		})
	}
}

func TestReferenceLabelUsesUnicodeCharacters(t *testing.T) {
	t.Parallel()
	label := strings.Repeat("界", 400)
	markdown := "[" + label + "]: /target\n22. item\n    ```text\n    missing\n"
	if violations := fencedBlockViolations(markdown, "unicode.md"); len(violations) == 0 {
		t.Fatal("valid multibyte reference label hid the following unclosed list fence")
	}
	if validReferenceLabel(strings.Repeat("a", 1000)) {
		t.Fatal("1000-character reference label unexpectedly accepted")
	}
}

func TestRelativeLinks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	docs := filepath.Join(root, "docs")
	if err := os.Mkdir(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	page := filepath.Join(docs, "page.md")
	target := filepath.Join(docs, "target.md")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	text := "[ok](target.md#part) [web](https://example.com) [bad](missing.md \"title\")\n"
	if err := os.WriteFile(page, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	violations, err := markdownViolations(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 1 || !strings.Contains(violations[0], "missing.md") {
		t.Fatalf("violations=%v", violations)
	}
}
