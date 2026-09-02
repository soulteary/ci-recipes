package checks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

type markdownContainer struct {
	kind  byte
	width int
}

type fenceState struct {
	marker     byte
	length     int
	line       int
	containers []markdownContainer
}

type htmlState struct {
	end        *regexp.Regexp
	untilBlank bool
	containers []markdownContainer
}

type listMarker struct {
	width                  int
	ordered                bool
	start                  int
	hasContent             bool
	startsWithIndentedCode bool
}

var (
	inlineLinkPattern = regexp.MustCompile(`\[[^\]]*\]\(([^)]+)\)`)
	linkTitlePattern  = regexp.MustCompile(`[[:space:]]+["'].*$`)
	apiHTMLBlockTags  = regexp.MustCompile(`(?i)^</?(?:address|article|aside|base|basefont|blockquote|body|caption|center|col|colgroup|dd|details|dialog|dir|div|dl|dt|fieldset|figcaption|figure|footer|form|frame|frameset|h[1-6]|head|header|hr|html|iframe|legend|li|link|main|menu|menuitem|nav|noframes|ol|optgroup|option|p|param|search|section|summary|table|tbody|td|tfoot|th|thead|title|tr|track|ul)(?:[ \t]|/?>|$)`)
	completeOpenTag   = regexp.MustCompile(`(?i)^<[A-Za-z][A-Za-z0-9-]*(?:[ \t]+[A-Za-z_:][A-Za-z0-9_.:-]*(?:[ \t]*=[ \t]*(?:[^\x00-\x20"'=<>` + "`" + `]+|"[^"]*"|'[^']*'))?)*[ \t]*/?>[ \t]*$`)
	completeCloseTag  = regexp.MustCompile(`(?i)^</[A-Za-z][A-Za-z0-9-]*[ \t]*>[ \t]*$`)
)

func markdownViolations(ctx context.Context, root string) ([]string, error) {
	paths := make([]string, 0)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".md" {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk Markdown files: %w", err)
	}
	sort.Strings(paths)
	violations := make([]string, 0)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read Markdown file %q: %w", path, err)
		}
		if !utf8.Valid(data) {
			return nil, fmt.Errorf("Markdown file %q is not valid UTF-8", path)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return nil, fmt.Errorf("make Markdown path relative: %w", err)
		}
		violations = append(violations, fencedBlockViolations(string(data), filepath.ToSlash(relative))...)
		linkViolations, err := relativeLinkViolations(path, string(data))
		if err != nil {
			return nil, err
		}
		violations = append(violations, linkViolations...)
	}
	return violations, nil
}

func expandTabs(line string) string {
	if !strings.ContainsRune(line, '\t') {
		return line
	}
	var expanded strings.Builder
	column := 0
	for _, character := range line {
		if character == '\t' {
			width := 4 - column%4
			expanded.WriteString(strings.Repeat(" ", width))
			column += width
			continue
		}
		expanded.WriteRune(character)
		column++
	}
	return expanded.String()
}

func leadingSpaces(value string, maximum int) int {
	count := 0
	for count < len(value) && count < maximum && value[count] == ' ' {
		count++
	}
	return count
}

func quotePrefix(value string) (int, bool) {
	spaces := leadingSpaces(value, 4)
	if spaces > 3 || spaces >= len(value) || value[spaces] != '>' {
		return 0, false
	}
	consumed := spaces + 1
	if consumed < len(value) && value[consumed] == ' ' {
		consumed++
	}
	return consumed, true
}

func consumeContainer(line string, offset int, container markdownContainer) (int, bool) {
	if offset > len(line) {
		return offset, false
	}
	remaining := line[offset:]
	if container.kind == 'q' {
		consumed, ok := quotePrefix(remaining)
		return offset + consumed, ok
	}
	if strings.Trim(remaining, " ") == "" {
		return len(line), true
	}
	if len(remaining) < container.width || remaining[:container.width] != strings.Repeat(" ", container.width) {
		return offset, false
	}
	return offset + container.width, true
}

func continueContainers(line string, containers []markdownContainer) (offset, matched int) {
	for _, container := range containers {
		next, ok := consumeContainer(line, offset, container)
		if !ok {
			break
		}
		offset = next
		matched++
	}
	return offset, matched
}

func thematicBreak(value string) bool {
	spaces := leadingSpaces(value, 4)
	if spaces > 3 {
		return false
	}
	value = strings.TrimSpace(value[spaces:])
	if value == "" {
		return false
	}
	marker := byte(0)
	count := 0
	for index := 0; index < len(value); index++ {
		if value[index] == ' ' {
			continue
		}
		if marker == 0 {
			marker = value[index]
			if marker != '*' && marker != '-' && marker != '_' {
				return false
			}
		}
		if value[index] != marker {
			return false
		}
		count++
	}
	return count >= 3
}

func parseListMarker(value string) (listMarker, bool) {
	spaces := leadingSpaces(value, 4)
	if spaces > 3 || spaces >= len(value) {
		return listMarker{}, false
	}
	index := spaces
	ordered := false
	start := 0
	markerLength := 0
	switch value[index] {
	case '-', '+', '*':
		markerLength = 1
	default:
		digitStart := index
		for index < len(value) && index-digitStart < 9 && value[index] >= '0' && value[index] <= '9' {
			index++
		}
		if index == digitStart || index >= len(value) || (value[index] != '.' && value[index] != ')') {
			return listMarker{}, false
		}
		if index+1 < len(value) && value[index+1] >= '0' && value[index+1] <= '9' && index-digitStart == 9 {
			return listMarker{}, false
		}
		ordered = true
		for _, digit := range value[digitStart:index] {
			start = start*10 + int(digit-'0')
		}
		markerLength = index - digitStart + 1
		index = digitStart
	}
	afterMarker := spaces + markerLength
	if afterMarker < len(value) && value[afterMarker] != ' ' {
		return listMarker{}, false
	}
	spaceCount := 1
	if afterMarker < len(value) {
		spaceCount = 0
		for afterMarker+spaceCount < len(value) && value[afterMarker+spaceCount] == ' ' {
			spaceCount++
		}
	}
	hasContent := strings.Trim(value[afterMarker:], " ") != ""
	padding := 1
	if hasContent && spaceCount <= 4 {
		padding = spaceCount
	}
	width := spaces + markerLength + padding
	content := ""
	if width <= len(value) {
		content = value[width:]
	}
	return listMarker{
		width:                  width,
		ordered:                ordered,
		start:                  start,
		hasContent:             hasContent,
		startsWithIndentedCode: strings.HasPrefix(content, "    "),
	}, true
}

func fenceOpener(value string) (marker byte, length int, ok bool) {
	spaces := leadingSpaces(value, 4)
	if spaces > 3 || spaces >= len(value) || (value[spaces] != '`' && value[spaces] != '~') {
		return 0, 0, false
	}
	marker = value[spaces]
	index := spaces
	for index < len(value) && value[index] == marker {
		index++
	}
	length = index - spaces
	if length < 3 || (marker == '`' && strings.ContainsRune(value[index:], '`')) {
		return 0, 0, false
	}
	return marker, length, true
}

func fenceCloser(value string, marker byte, minimum int) bool {
	spaces := leadingSpaces(value, 4)
	if spaces > 3 || spaces >= len(value) || value[spaces] != marker {
		return false
	}
	index := spaces
	for index < len(value) && value[index] == marker {
		index++
	}
	return index-spaces >= minimum && strings.Trim(value[index:], " ") == ""
}

func setextUnderline(value string) bool {
	spaces := leadingSpaces(value, 4)
	if spaces > 3 || spaces >= len(value) || (value[spaces] != '=' && value[spaces] != '-') {
		return false
	}
	marker := value[spaces]
	index := spaces
	for index < len(value) && value[index] == marker {
		index++
	}
	return index > spaces && strings.Trim(value[index:], " \t") == ""
}

func htmlBlockStart(value string) (state *htmlState, interrupts bool) {
	spaces := leadingSpaces(value, 4)
	if spaces > 3 {
		return nil, false
	}
	content := value[spaces:]
	patterns := []struct {
		prefix string
		end    string
	}{
		{"<!--", `-->`},
		{"<?", `\?>`},
		{"<![CDATA[", `\]\]>`},
	}
	for _, item := range patterns {
		if strings.HasPrefix(content, item.prefix) {
			return &htmlState{end: regexp.MustCompile(item.end)}, true
		}
	}
	if len(content) > 2 && strings.HasPrefix(content, "<!") && ((content[2] >= 'A' && content[2] <= 'Z') || (content[2] >= 'a' && content[2] <= 'z')) {
		return &htmlState{end: regexp.MustCompile(`>`)}, true
	}
	lower := strings.ToLower(content)
	for _, tag := range []string{"script", "pre", "style", "textarea"} {
		prefix := "<" + tag
		if strings.HasPrefix(lower, prefix) && (len(lower) == len(prefix) || strings.ContainsRune(" \t>", rune(lower[len(prefix)]))) {
			return &htmlState{end: regexp.MustCompile(`(?i)</` + tag + `>`)}, true
		}
	}
	if apiHTMLBlockTags.MatchString(content) {
		return &htmlState{untilBlank: true}, true
	}
	if completeOpenTag.MatchString(content) || completeCloseTag.MatchString(content) {
		return &htmlState{untilBlank: true}, false
	}
	return nil, false
}

func htmlBlockFinished(value string, state *htmlState) bool {
	if state.untilBlank {
		return strings.Trim(value, " ") == ""
	}
	return state.end != nil && state.end.MatchString(value)
}

func interruptsParagraph(value string) bool {
	if strings.Trim(value, " ") == "" {
		return true
	}
	if _, ok := quotePrefix(value); ok {
		return true
	}
	if _, _, ok := fenceOpener(value); ok {
		return true
	}
	spaces := leadingSpaces(value, 4)
	if spaces <= 3 && spaces < len(value) && value[spaces] == '#' {
		index := spaces
		for index < len(value) && value[index] == '#' && index-spaces < 6 {
			index++
		}
		if index > spaces && index-spaces <= 6 && (index == len(value) || value[index] == ' ') {
			return true
		}
	}
	if thematicBreak(value) {
		return true
	}
	if state, htmlInterrupts := htmlBlockStart(value); state != nil && htmlInterrupts {
		return true
	}
	if marker, ok := parseListMarker(value); ok && marker.hasContent && !marker.startsWithIndentedCode && (!marker.ordered || marker.start == 1) {
		return true
	}
	return false
}

func paragraphContent(value string) bool {
	if strings.Trim(value, " ") == "" || thematicBreak(value) {
		return false
	}
	spaces := leadingSpaces(value, 4)
	if spaces <= 3 && spaces < len(value) && value[spaces] == '#' {
		index := spaces
		for index < len(value) && value[index] == '#' && index-spaces < 6 {
			index++
		}
		if index > spaces && (index == len(value) || value[index] == ' ') {
			return false
		}
	}
	return true
}

func openContainers(line string, offset int, containers *[]markdownContainer, paragraph bool) (int, int) {
	opened := 0
	for offset <= len(line) {
		remaining := line[offset:]
		if thematicBreak(remaining) {
			break
		}
		if consumed, ok := quotePrefix(remaining); ok {
			*containers = append(*containers, markdownContainer{kind: 'q'})
			offset += consumed
			paragraph = false
			opened++
			continue
		}
		marker, ok := parseListMarker(remaining)
		if !ok {
			break
		}
		if paragraph && (!marker.hasContent || marker.startsWithIndentedCode || (marker.ordered && marker.start != 1)) {
			break
		}
		*containers = append(*containers, markdownContainer{kind: 'l', width: marker.width})
		offset += marker.width
		if offset > len(line) {
			offset = len(line)
		}
		paragraph = false
		opened++
	}
	return offset, opened
}

func fencedBlockViolations(text, relative string) []string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	containers := make([]markdownContainer, 0)
	var fence *fenceState
	var html *htmlState
	paragraphActive := false
	paragraphDepth := 0
	referenceLinesToSkip := 0
	violations := make([]string, 0)

lineLoop:
	for lineIndex, rawLine := range lines {
		line := expandTabs(strings.TrimSuffix(rawLine, "\r"))
		lineNumber := lineIndex + 1
		if referenceLinesToSkip > 0 {
			referenceLinesToSkip--
			continue
		}

		for {
			if fence != nil {
				offset, matched := continueContainers(line, fence.containers)
				if matched < len(fence.containers) {
					violations = append(violations, fmt.Sprintf("Unclosed fenced code block in %s:%d before container ended at line %d", relative, fence.line, lineNumber))
					containers = containers[:matched]
					fence = nil
					paragraphActive = false
					continue
				}
				if fenceCloser(line[offset:], fence.marker, fence.length) {
					fence = nil
					paragraphActive = false
				}
				continue lineLoop
			}

			if html != nil {
				offset, matched := continueContainers(line, html.containers)
				if matched < len(html.containers) {
					containers = containers[:matched]
					html = nil
					paragraphActive = false
					continue
				}
				if htmlBlockFinished(line[offset:], html) {
					html = nil
				}
				continue lineLoop
			}

			offset, matched := continueContainers(line, containers)
			if matched < len(containers) {
				remaining := line[offset:]
				if paragraphActive && paragraphDepth == len(containers) && !interruptsParagraph(remaining) {
					continue lineLoop
				}
				containers = containers[:matched]
				if paragraphDepth > matched {
					paragraphActive = false
				}
			}

			paragraphHere := paragraphActive && paragraphDepth == len(containers)
			var opened int
			offset, opened = openContainers(line, offset, &containers, paragraphHere)
			if opened > 0 {
				paragraphActive = false
				paragraphHere = false
			}
			content := line[offset:]
			if !paragraphHere && strings.HasPrefix(content, "    ") {
				paragraphActive = false
				continue lineLoop
			}
			if paragraphHere && setextUnderline(content) {
				paragraphActive = false
				continue lineLoop
			}

			if !paragraphHere {
				if valid, title := linkReferenceDefinition(content); valid {
					if title != nil {
						if continued, ok := referenceTitleContinuationLines(*title, containers, lineIndex, lines); ok {
							referenceLinesToSkip = continued
							paragraphActive = false
							continue lineLoop
						}
					} else {
						if following, ok := followingReferenceTitleLines(containers, lineIndex, lines); ok {
							referenceLinesToSkip = following
						}
						paragraphActive = false
						continue lineLoop
					}
				}
				if continuation := multilineReferenceLines(content, containers, lineIndex, lines); continuation > 0 {
					referenceLinesToSkip = continuation
					paragraphActive = false
					continue lineLoop
				}
				if continuation := multilineReferenceLabelLines(content, containers, lineIndex, lines); continuation > 0 {
					referenceLinesToSkip = continuation
					paragraphActive = false
					continue lineLoop
				}
			}

			if state, htmlInterrupts := htmlBlockStart(content); state != nil && (!paragraphHere || htmlInterrupts) {
				paragraphActive = false
				if !htmlBlockFinished(content, state) {
					state.containers = append([]markdownContainer(nil), containers...)
					html = state
				}
				continue lineLoop
			}
			if marker, length, ok := fenceOpener(content); ok {
				fence = &fenceState{marker: marker, length: length, line: lineNumber, containers: append([]markdownContainer(nil), containers...)}
				paragraphActive = false
				continue lineLoop
			}
			if paragraphContent(content) {
				paragraphActive = true
				paragraphDepth = len(containers)
			} else {
				paragraphActive = false
			}
			continue lineLoop
		}
	}
	if fence != nil {
		violations = append(violations, fmt.Sprintf("Unclosed fenced code block in %s:%d", relative, fence.line))
	}
	return violations
}

func isASCIIPunctuation(value byte) bool {
	return value >= 0x21 && value <= 0x2f || value >= 0x3a && value <= 0x40 || value >= 0x5b && value <= 0x60 || value >= 0x7b && value <= 0x7e
}

func validReferenceLabel(label string) bool {
	if utf8.RuneCountInString(label) > 999 {
		return false
	}
	for _, character := range label {
		if !unicode.IsSpace(character) {
			return true
		}
	}
	return false
}

func referenceDestinationPrefix(content string) (string, bool) {
	if content == "" {
		return "", false
	}
	if content[0] == '<' {
		for index := 1; index < len(content); {
			character := content[index]
			if character == '\\' && index+1 < len(content) && isASCIIPunctuation(content[index+1]) {
				index += 2
				continue
			}
			if character == '>' {
				return content[index+1:], true
			}
			if character == '<' || character == '\r' || character == '\n' {
				return "", false
			}
			_, size := utf8.DecodeRuneInString(content[index:])
			index += size
		}
		return "", false
	}

	parentheses := 0
	index := 0
	for index < len(content) {
		character, size := utf8.DecodeRuneInString(content[index:])
		if unicode.IsSpace(character) {
			break
		}
		if unicode.IsControl(character) {
			return "", false
		}
		if character == '\\' && index+1 < len(content) && isASCIIPunctuation(content[index+1]) {
			index += 2
			continue
		}
		switch character {
		case '(':
			parentheses++
		case ')':
			if parentheses == 0 {
				return "", false
			}
			parentheses--
		}
		index += size
	}
	if index == 0 || parentheses != 0 {
		return "", false
	}
	return content[index:], true
}

func parseReferenceLabel(content string) (label, tail string, ok bool) {
	spaces := leadingSpaces(content, 4)
	if spaces > 3 || spaces >= len(content) || content[spaces] != '[' {
		return "", "", false
	}
	var labelBuilder strings.Builder
	for index := spaces + 1; index < len(content); {
		character := content[index]
		if character == '\\' && index+1 < len(content) && content[index+1] != '\r' && content[index+1] != '\n' {
			labelBuilder.WriteByte(character)
			labelBuilder.WriteByte(content[index+1])
			index += 2
			continue
		}
		if character == '[' {
			return "", "", false
		}
		if character == ']' {
			if index+1 >= len(content) || content[index+1] != ':' || !validReferenceLabel(labelBuilder.String()) {
				return "", "", false
			}
			return labelBuilder.String(), content[index+2:], true
		}
		_, size := utf8.DecodeRuneInString(content[index:])
		labelBuilder.WriteString(content[index : index+size])
		index += size
	}
	return "", "", false
}

func linkReferenceDefinition(content string) (bool, *string) {
	_, destinationText, ok := parseReferenceLabel(content)
	if !ok {
		return false, nil
	}
	destinationText = strings.TrimLeft(destinationText, " \t")
	remainder, ok := referenceDestinationPrefix(destinationText)
	if !ok {
		return false, nil
	}
	if strings.Trim(remainder, " \t") == "" {
		return true, nil
	}
	if len(remainder) == 0 || (remainder[0] != ' ' && remainder[0] != '\t') {
		return false, nil
	}
	title := strings.TrimLeft(remainder, " \t")
	if title == "" || !strings.ContainsRune("\"'(", rune(title[0])) {
		return false, nil
	}
	return true, &title
}

func validReferenceTitle(title string) bool {
	if title == "" {
		return false
	}
	opener := title[0]
	closer := opener
	if opener == '(' {
		closer = ')'
	} else if opener != '\'' && opener != '"' {
		return false
	}
	for index := 1; index < len(title); {
		character := title[index]
		if character == '\\' && index+1 < len(title) && isASCIIPunctuation(title[index+1]) {
			index += 2
			continue
		}
		if character == closer {
			return strings.Trim(title[index+1:], " \t") == ""
		}
		if opener == '(' && character == '(' {
			return false
		}
		_, size := utf8.DecodeRuneInString(title[index:])
		index += size
	}
	return false
}

func referenceContinuationBoundary(content string) bool {
	return setextUnderline(content) || interruptsParagraph(content)
}

func referenceTitleContinuationLines(title string, containers []markdownContainer, lineIndex int, lines []string) (int, bool) {
	if validReferenceTitle(title) {
		return 0, true
	}
	continued := 0
	for nextIndex := lineIndex + 1; nextIndex < len(lines); nextIndex++ {
		line := expandTabs(strings.TrimSuffix(lines[nextIndex], "\r"))
		offset, matched := continueContainers(line, containers)
		if matched < len(containers) {
			return 0, false
		}
		content := line[offset:]
		if strings.Trim(content, " ") == "" || referenceContinuationBoundary(content) {
			return 0, false
		}
		title += "\n" + content
		continued++
		if validReferenceTitle(title) {
			return continued, true
		}
	}
	return 0, false
}

func followingReferenceTitleLines(containers []markdownContainer, lineIndex int, lines []string) (int, bool) {
	if lineIndex+1 >= len(lines) {
		return 0, false
	}
	line := expandTabs(strings.TrimSuffix(lines[lineIndex+1], "\r"))
	offset, matched := continueContainers(line, containers)
	if matched < len(containers) {
		return 0, false
	}
	content := line[offset:]
	spaces := leadingSpaces(content, 4)
	if spaces > 3 || spaces >= len(content) || !strings.ContainsRune("\"'(", rune(content[spaces])) {
		return 0, false
	}
	continued, ok := referenceTitleContinuationLines(content[spaces:], containers, lineIndex+1, lines)
	if !ok {
		return 0, false
	}
	return 1 + continued, true
}

func multilineReferenceLines(content string, containers []markdownContainer, lineIndex int, lines []string) int {
	_, tail, ok := parseReferenceLabel(content)
	if !ok || strings.Trim(tail, " \t") != "" || lineIndex+1 >= len(lines) {
		return 0
	}
	line := expandTabs(strings.TrimSuffix(lines[lineIndex+1], "\r"))
	offset, matched := continueContainers(line, containers)
	if matched < len(containers) {
		return 0
	}
	destinationContent := line[offset:]
	if referenceContinuationBoundary(destinationContent) {
		return 0
	}
	spaces := leadingSpaces(destinationContent, 4)
	if spaces > 3 {
		return 0
	}
	remainder, ok := referenceDestinationPrefix(destinationContent[spaces:])
	if !ok {
		return 0
	}
	if strings.Trim(remainder, " \t") != "" {
		if len(remainder) == 0 || (remainder[0] != ' ' && remainder[0] != '\t') {
			return 0
		}
		title := strings.TrimLeft(remainder, " \t")
		continued, ok := referenceTitleContinuationLines(title, containers, lineIndex+1, lines)
		if !ok {
			return 0
		}
		return 1 + continued
	}
	if following, ok := followingReferenceTitleLines(containers, lineIndex+1, lines); ok {
		return 1 + following
	}
	return 1
}

func multilineReferenceLabelLines(content string, containers []markdownContainer, lineIndex int, lines []string) int {
	spaces := leadingSpaces(content, 4)
	if spaces > 3 || spaces >= len(content) || content[spaces] != '[' {
		return 0
	}
	label := ""
	fragment := content[spaces+1:]
	currentIndex := lineIndex
	for {
		for index := 0; index < len(fragment); {
			character := fragment[index]
			if character == '\\' {
				if index+1 >= len(fragment) {
					label += "\\"
					index++
					continue
				}
				label += fragment[index : index+2]
				index += 2
				continue
			}
			if character == '[' {
				return 0
			}
			if character == ']' {
				if index+1 >= len(fragment) || fragment[index+1] != ':' || !validReferenceLabel(label) {
					return 0
				}
				tail := fragment[index+2:]
				labelLines := currentIndex - lineIndex
				if strings.Trim(tail, " \t") == "" {
					destinationLines := multilineReferenceLines("[reference]:", containers, currentIndex, lines)
					if destinationLines == 0 {
						return 0
					}
					return labelLines + destinationLines
				}
				valid, title := linkReferenceDefinition("[reference]:" + tail)
				if !valid {
					return 0
				}
				if title != nil {
					continued, ok := referenceTitleContinuationLines(*title, containers, currentIndex, lines)
					if !ok {
						return 0
					}
					return labelLines + continued
				}
				if following, ok := followingReferenceTitleLines(containers, currentIndex, lines); ok {
					return labelLines + following
				}
				return labelLines
			}
			_, size := utf8.DecodeRuneInString(fragment[index:])
			label += fragment[index : index+size]
			index += size
		}
		if currentIndex+1 >= len(lines) {
			return 0
		}
		next := expandTabs(strings.TrimSuffix(lines[currentIndex+1], "\r"))
		offset, matched := continueContainers(next, containers)
		if matched < len(containers) {
			return 0
		}
		fragment = next[offset:]
		if strings.Trim(fragment, " ") == "" || referenceContinuationBoundary(fragment) {
			return 0
		}
		label += "\n"
		currentIndex++
	}
}

func relativeLinkViolations(path, text string) ([]string, error) {
	violations := make([]string, 0)
	for _, match := range inlineLinkPattern.FindAllStringSubmatch(text, -1) {
		target := match[1]
		target = strings.TrimPrefix(target, "<")
		target = strings.TrimSuffix(target, ">")
		target = linkTitlePattern.ReplaceAllString(target, "")
		if strings.HasPrefix(target, "#") || strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "data:") {
			continue
		}
		if fragment := strings.IndexByte(target, '#'); fragment >= 0 {
			target = target[:fragment]
		}
		resolved := filepath.Join(filepath.Dir(path), filepath.FromSlash(target))
		if _, err := os.Stat(resolved); err != nil {
			if os.IsNotExist(err) {
				violations = append(violations, fmt.Sprintf("Broken relative link in %s: %s", path, match[1]))
				continue
			}
			return nil, fmt.Errorf("inspect relative link %q from %q: %w", target, path, err)
		}
	}
	return violations, nil
}
