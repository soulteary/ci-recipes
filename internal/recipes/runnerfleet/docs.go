package runnerfleet

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// runDocsStructure replaces scripts/check-docs-structure.sh.
//
// Heading text is supposed to differ between languages, so the comparison is of
// the heading-level sequence: the order in which "#", "##" and "###" appear. A
// missing section, an extra one, or a level moved breaks the sequence, and an
// honest translation does not.
func runDocsStructure(deps dependencies, args []string, stdout, stderr io.Writer) error {
	parsed, err := parseRecipeArgs("ci-recipes runner-fleet check-docs-structure [ROOT] [--config FILE]", args)
	if err != nil {
		return err
	}
	root, err := resolveRoot(deps, parsed.root)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(deps, root, parsed.configPath)
	if err != nil {
		return err
	}

	languages := strings.Join(cfg.docsLanguages, " ")
	compared := 0
	failed := 0
	for _, doc := range cfg.docsFiles {
		base := path.Join(cfg.docsRoot, doc)
		basePath := filepath.Join(root, filepath.FromSlash(base))
		if !regularFile(basePath) {
			continue
		}
		baseSequence, err := headingSequence(basePath)
		if err != nil {
			return usage("read %s: %v", base, err)
		}
		compared++

		docFailed := false
		for _, language := range cfg.docsLanguages {
			target := path.Join(cfg.docsRoot, language, doc)
			targetPath := filepath.Join(root, filepath.FromSlash(target))
			if !regularFile(targetPath) {
				fmt.Fprintf(stdout, "::error file=%s::缺少译文 %s\n", base, target)
				docFailed = true
				continue
			}
			targetSequence, err := headingSequence(targetPath)
			if err != nil {
				return usage("read %s: %v", target, err)
			}
			if sameSequence(baseSequence, targetSequence) {
				continue
			}
			fmt.Fprintf(stdout, "::error file=%s::章节结构与 %s 不一致（英文 %d 个标题，本文件 %d 个）。\n",
				target, base, len(baseSequence), len(targetSequence))
			fmt.Fprintf(stdout, "  英文版新增或调整了章节而这里没跟上时就会这样。逐节对照 %s 补齐后重跑本检查。\n", base)
			fmt.Fprintf(stdout, "  英文层级序列: %s\n", strings.Join(baseSequence, " "))
			fmt.Fprintf(stdout, "  本文件层级序列: %s\n", strings.Join(targetSequence, " "))
			docFailed = true
		}

		// Tracked per document rather than once for the whole run: sharing one
		// flag meant that after the first failing document, every document
		// after it stopped printing its result line and read as unchecked.
		if docFailed {
			failed++
			continue
		}
		fmt.Fprintf(stdout, "%s: %d 个标题，%s 全部一致\n", base, len(baseSequence), languages)
	}

	if failed != 0 {
		return rejected("found %d document(s) whose translations do not match the English section structure", failed)
	}
	// The shell skipped a document whose English original was absent and, with
	// no documentation tree at all, compared nothing and printed its all-clear.
	// Having compared nothing is not the same as having found no drift.
	if compared == 0 {
		return usage("none of %s exist under %s, so nothing was compared", strings.Join(cfg.docsFiles, ", "), cfg.docsRoot)
	}
	fmt.Fprintln(stdout, "各语言文档章节结构一致")
	return nil
}

func regularFile(name string) bool {
	info, err := os.Stat(name)
	return err == nil && info.Mode().IsRegular()
}

func sameSequence(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// headingSequence returns the run of "#" for each ATX heading outside a fenced
// code block, in document order.
func headingSequence(name string) ([]string, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var sequence []string
	insideFence := false
	var opener byte
	for scanner.Scan() {
		line := scanner.Text()
		if marker, isFence := fenceMarker(line); isFence {
			switch {
			case !insideFence:
				insideFence = true
				opener = marker
			case marker == opener:
				insideFence = false
			}
			continue
		}
		if insideFence {
			continue
		}
		if level := headingLevel(line); level != "" {
			sequence = append(sequence, level)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return sequence, nil
}

// fenceMarker reports the character opening or closing a fenced code block.
//
// Both of Markdown's fence markers count, and a fence closes only on the
// character that opened it. The shell this replaces knew about backticks only,
// so a tilde-fenced block was not a block at all and a column-1 "# comment"
// inside one counted as a heading: where both sides used tildes the phantom
// heading appeared in both sequences and the check passed while mismodelling
// the document, and where the two sides used different markers around the same
// block every translation was reported as wrong when all of them were right.
func fenceMarker(line string) (byte, bool) {
	switch {
	case strings.HasPrefix(line, "```"):
		return '`', true
	case strings.HasPrefix(line, "~~~"):
		return '~', true
	}
	return 0, false
}

// headingLevel returns the leading "#" run of an ATX heading, or "" for any
// other line. A heading needs whitespace after its hashes, so "#nope" is text.
func headingLevel(line string) string {
	count := 0
	for count < len(line) && line[count] == '#' {
		count++
	}
	if count == 0 || count >= len(line) {
		return ""
	}
	if line[count] != ' ' && line[count] != '\t' {
		return ""
	}
	return line[:count]
}
