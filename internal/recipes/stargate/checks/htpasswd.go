package checks

import (
	"path/filepath"
	"strings"
)

// unsafeHTPasswdBatchInvocations returns one entry for every command-like
// htpasswd invocation whose short options enable batch password mode (-b).
// The lightweight scanner is intentionally limited to static documentation
// examples; it does not try to evaluate shell expansions.
func unsafeHTPasswdBatchInvocations(text string) []struct{} {
	violations := make([]struct{}, 0)
	for _, command := range shellCommandFragments(strings.ReplaceAll(text, "\\\n", " ")) {
		commandWords, ok := htpasswdCommandWords(shellLiteralWords(command))
		if !ok {
			continue
		}
		for _, word := range commandWords[1:] {
			if word == "--" {
				break
			}
			if len(word) > 1 && word[0] == '-' && word[1] != '-' && strings.ContainsRune(word[1:], 'b') {
				violations = append(violations, struct{}{})
				break
			}
		}
	}
	return violations
}

func shellCommandFragments(text string) []string {
	fragments := make([]string, 0)
	start := 0
	lineStart := 0
	quote := byte(0)
	escaped := false
	flush := func(end int) {
		if fragment := strings.TrimSpace(text[start:end]); fragment != "" {
			fragments = append(fragments, fragment)
		}
	}
	for index := 0; index < len(text); index++ {
		character := text[index]
		if character == '\n' || character == '\r' {
			flush(index)
			start = index + 1
			lineStart = index + 1
			quote = 0
			escaped = false
			continue
		}
		if escaped {
			escaped = false
			continue
		}
		if quote != '\'' && character == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			}
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			continue
		}
		if character == '#' && shellCommentStart(text, index) && !markdownRootPrompt(text, lineStart, index) {
			flush(index)
			for index < len(text) && text[index] != '\n' && text[index] != '\r' {
				index++
			}
			if index < len(text) {
				start = index + 1
				lineStart = index + 1
			} else {
				start = len(text)
			}
			quote = 0
			escaped = false
			continue
		}
		switch character {
		case ';', '|', '&', '(', ')', '`':
			flush(index)
			start = index + 1
		}
	}
	flush(len(text))
	return fragments
}

func shellCommentStart(text string, index int) bool {
	if index == 0 {
		return true
	}
	return strings.ContainsRune(" \t;&|()<>", rune(text[index-1]))
}

func markdownRootPrompt(text string, lineStart, index int) bool {
	for _, word := range shellLiteralWords(text[lineStart:index]) {
		if !markdownCommandPrefix(word) {
			return false
		}
	}
	lineEnd := strings.IndexAny(text[index+1:], "\r\n")
	if lineEnd < 0 {
		lineEnd = len(text)
	} else {
		lineEnd += index + 1
	}
	_, ok := htpasswdCommandWords(shellLiteralWords(text[index+1 : lineEnd]))
	return ok
}

func shellLiteralWords(command string) []string {
	words := make([]string, 0)
	var word strings.Builder
	quote := byte(0)
	escaped := false
	started := false
	flush := func() {
		if started {
			words = append(words, word.String())
			word.Reset()
			started = false
		}
	}
	for index := 0; index < len(command); index++ {
		character := command[index]
		if escaped {
			word.WriteByte(character)
			started = true
			escaped = false
			continue
		}
		if quote != '\'' && character == '\\' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
				started = true
			} else {
				word.WriteByte(character)
			}
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			started = true
			continue
		}
		if character == ' ' || character == '\t' {
			flush()
			continue
		}
		word.WriteByte(character)
		started = true
	}
	if escaped {
		word.WriteByte('\\')
	}
	flush()
	return words
}

func htpasswdCommandWords(words []string) ([]string, bool) {
	index := 0
	for index < len(words) && markdownCommandPrefix(words[index]) {
		index++
	}
	for wrapperDepth := 0; index < len(words) && wrapperDepth < 64; wrapperDepth++ {
		for index < len(words) && shellAssignmentWord(words[index]) {
			index++
		}
		if index >= len(words) {
			return nil, false
		}
		word := words[index]
		switch filepath.Base(word) {
		case "htpasswd":
			return words[index:], true
		case "sudo":
			var executable bool
			index, executable = skipSudoOptions(words, index+1)
			if !executable {
				return nil, false
			}
		case "env":
			var executable bool
			words, executable = unwrapEnvWords(words[index+1:])
			if !executable {
				return nil, false
			}
			index = 0
		case "command":
			var executable bool
			index, executable = skipCommandOptions(words, index+1)
			if !executable {
				return nil, false
			}
		case "ionice":
			var executable bool
			index, executable = skipIoniceOptions(words, index+1)
			if !executable {
				return nil, false
			}
		default:
			return nil, false
		}
	}
	return nil, false
}

func shellAssignmentWord(word string) bool {
	name, _, found := strings.Cut(word, "=")
	if !found || name == "" {
		return false
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		if character == '_' || character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' {
			continue
		}
		if index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}

type wrapperOption struct {
	short        byte
	long         string
	operand      bool
	optional     bool
	nonExecuting bool
	split        bool
}

var sudoOptions = []wrapperOption{
	{short: 'A', long: "askpass"},
	{short: 'b', long: "background"},
	{short: 'B', long: "bell"},
	{short: 'C', long: "close-from", operand: true},
	{short: 'D', long: "chdir", operand: true},
	{short: 'E', long: "preserve-env", optional: true},
	{short: 'e', long: "edit", nonExecuting: true},
	{short: 'g', long: "group", operand: true},
	{short: 'H', long: "set-home"},
	{short: 'h', long: "host", operand: true},
	{long: "help", nonExecuting: true},
	{short: 'i', long: "login"},
	{short: 'K', long: "remove-timestamp", nonExecuting: true},
	{short: 'k', long: "reset-timestamp"},
	{short: 'l', long: "list", nonExecuting: true},
	{short: 'n', long: "non-interactive"},
	{short: 'N', long: "no-update"},
	{short: 'P', long: "preserve-groups"},
	{short: 'p', long: "prompt", operand: true},
	{short: 'R', long: "chroot", operand: true},
	{short: 'r', long: "role", operand: true},
	{short: 'S', long: "stdin"},
	{short: 's', long: "shell"},
	{short: 't', long: "type", operand: true},
	{short: 'T', long: "command-timeout", operand: true},
	{short: 'U', long: "other-user", operand: true},
	{short: 'u', long: "user", operand: true},
	{short: 'V', long: "version", nonExecuting: true},
	{short: 'v', long: "validate", nonExecuting: true},
}

func skipSudoOptions(words []string, index int) (int, bool) {
	for index < len(words) {
		word := words[index]
		if word == "--" {
			return index + 1, true
		}
		if word == "-" || !strings.HasPrefix(word, "-") {
			return index, true
		}
		consumeNext, executable := parseWrapperOption(word, sudoOptions)
		if !executable {
			return index, false
		}
		index++
		if consumeNext && index < len(words) {
			index++
		}
	}
	return index, true
}

func parseWrapperOption(word string, options []wrapperOption) (consumeNext, executable bool) {
	if strings.HasPrefix(word, "--") {
		name, _, attached := strings.Cut(word[2:], "=")
		matched, ok := matchWrapperLongOption(name, options)
		if !ok || matched.nonExecuting || attached && !matched.operand && !matched.optional {
			return false, false
		}
		return matched.operand && !attached, true
	}
	for offset := 1; offset < len(word); offset++ {
		matched, ok := matchWrapperShortOption(word[offset], options)
		if !ok || matched.nonExecuting {
			return false, false
		}
		if matched.operand {
			return offset == len(word)-1, true
		}
	}
	return false, true
}

func matchWrapperLongOption(name string, options []wrapperOption) (wrapperOption, bool) {
	matched := wrapperOption{}
	matches := 0
	for _, option := range options {
		if option.long == name {
			return option, true
		}
		if name != "" && strings.HasPrefix(option.long, name) {
			matched = option
			matches++
		}
	}
	return matched, matches == 1
}

func matchWrapperShortOption(short byte, options []wrapperOption) (wrapperOption, bool) {
	for _, option := range options {
		if option.short == short {
			return option, true
		}
	}
	return wrapperOption{}, false
}

var envOptions = []wrapperOption{
	{short: 'i', long: "ignore-environment"},
	{short: '0', long: "null", nonExecuting: true},
	{short: 'u', long: "unset", operand: true},
	{short: 'C', long: "chdir", operand: true},
	{short: 'S', long: "split-string", operand: true, split: true},
	{long: "block-signal", optional: true},
	{long: "default-signal", optional: true},
	{long: "ignore-signal", optional: true},
	{long: "list-signal-handling"},
	{short: 'v', long: "debug"},
	{long: "help", nonExecuting: true},
	{long: "version", nonExecuting: true},
}

func unwrapEnvWords(arguments []string) ([]string, bool) {
	words := append([]string(nil), arguments...)
	for expansion := 0; expansion < 64; expansion++ {
		expandedSplit := false
		for index := 0; index < len(words); {
			word := words[index]
			if shellAssignmentWord(word) {
				index++
				continue
			}
			if word == "--" {
				return words[index+1:], true
			}
			if word == "-" {
				index++
				continue
			}
			if !strings.HasPrefix(word, "-") {
				return words[index:], true
			}

			option, value, attached, ok := parseEnvOption(word)
			if !ok || option.nonExecuting {
				return nil, false
			}
			next := index + 1
			if option.operand && !attached {
				if next >= len(words) {
					return nil, false
				}
				value = words[next]
				next++
			}
			if !option.split {
				index = next
				continue
			}

			splitWords := shellLiteralWords(value)
			expanded := make([]string, 0, len(splitWords)+len(words)-next)
			expanded = append(expanded, splitWords...)
			expanded = append(expanded, words[next:]...)
			words = expanded
			expandedSplit = true
			break
		}
		if !expandedSplit {
			return nil, false
		}
	}
	return nil, false
}

func parseEnvOption(word string) (option wrapperOption, value string, attached, ok bool) {
	if strings.HasPrefix(word, "--") {
		name, value, attached := strings.Cut(word[2:], "=")
		option, ok := matchWrapperLongOption(name, envOptions)
		if !ok || attached && !option.operand && !option.optional {
			return wrapperOption{}, "", false, false
		}
		return option, value, attached, true
	}
	for offset := 1; offset < len(word); offset++ {
		matched, found := matchWrapperShortOption(word[offset], envOptions)
		if !found {
			return wrapperOption{}, "", false, false
		}
		if matched.nonExecuting {
			return matched, "", false, true
		}
		if matched.operand {
			if offset+1 < len(word) {
				return matched, word[offset+1:], true, true
			}
			return matched, "", false, true
		}
		option = matched
	}
	return option, "", false, true
}

func skipCommandOptions(words []string, index int) (int, bool) {
	for index < len(words) {
		word := words[index]
		if word == "--" {
			return index + 1, true
		}
		if word == "-" || !strings.HasPrefix(word, "-") {
			return index, true
		}
		if strings.ContainsAny(strings.TrimLeft(word, "-"), "vV") {
			return index, false
		}
		index++
	}
	return index, true
}

func skipIoniceOptions(words []string, index int) (int, bool) {
	for index < len(words) {
		word := words[index]
		if word == "--" {
			return index + 1, true
		}
		if word == "-" || !strings.HasPrefix(word, "-") {
			return index, true
		}
		consumeNext, executable := parseIoniceOption(word)
		if !executable {
			return index, false
		}
		index++
		if consumeNext && index < len(words) {
			index++
		}
	}
	return index, true
}

type ioniceOption struct {
	short   byte
	long    string
	operand bool
	query   bool
}

var ioniceOptions = []ioniceOption{
	{short: 'c', long: "class", operand: true},
	{short: 'n', long: "classdata", operand: true},
	{short: 'p', long: "pid", operand: true, query: true},
	{short: 'P', long: "pgid", operand: true, query: true},
	{short: 't', long: "ignore"},
	{short: 'u', long: "uid", operand: true, query: true},
	{short: 'h', long: "help", query: true},
	{short: 'V', long: "version", query: true},
}

func parseIoniceOption(word string) (consumeNext, executable bool) {
	if strings.HasPrefix(word, "--") {
		name, _, attached := strings.Cut(word[2:], "=")
		matched := ioniceOption{}
		matches := 0
		for _, option := range ioniceOptions {
			if option.long == name {
				matched = option
				matches = 1
				break
			}
			if name != "" && strings.HasPrefix(option.long, name) {
				matched = option
				matches++
			}
		}
		if matches != 1 || matched.query || attached && !matched.operand {
			return false, false
		}
		return matched.operand && !attached, true
	}
	for offset := 1; offset < len(word); offset++ {
		var matched *ioniceOption
		for optionIndex := range ioniceOptions {
			if ioniceOptions[optionIndex].short == word[offset] {
				matched = &ioniceOptions[optionIndex]
				break
			}
		}
		if matched == nil || matched.query {
			return false, false
		}
		if matched.operand {
			return offset == len(word)-1, true
		}
	}
	return false, true
}

func markdownCommandPrefix(word string) bool {
	if word == "$" || word == "%" || word == ">" || word == "#" || word == "!" {
		return true
	}
	if word == "-" || word == "*" || word == "+" {
		return true
	}
	return false
}
