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
		words := shellLiteralWords(command)
		commandIndex := directHTPasswdCommand(words)
		if commandIndex < 0 {
			continue
		}
		for _, word := range words[commandIndex+1:] {
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
	return directHTPasswdCommand(shellLiteralWords(text[index+1:lineEnd])) >= 0
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

func directHTPasswdCommand(words []string) int {
	index := 0
	for index < len(words) && markdownCommandPrefix(words[index]) {
		index++
	}
	for index < len(words) {
		word := words[index]
		if strings.ContainsRune(word, '=') && !strings.HasPrefix(word, "=") {
			index++
			continue
		}
		switch filepath.Base(word) {
		case "htpasswd":
			return index
		case "sudo":
			index = skipWrapperOptions(words, index+1, sudoOperandOptions)
		case "env":
			index = skipWrapperOptions(words, index+1, envOperandOptions)
		case "command":
			var executable bool
			index, executable = skipCommandOptions(words, index+1)
			if !executable {
				return -1
			}
		default:
			return -1
		}
	}
	return -1
}

var sudoOperandOptions = map[string]bool{
	"-C": true, "--close-from": true,
	"-D": true, "--chdir": true,
	"-g": true, "--group": true,
	"-h": true, "--host": true,
	"-p": true, "--prompt": true,
	"-R": true, "--chroot": true,
	"-r": true, "--role": true,
	"-T": true, "--command-timeout": true,
	"-t": true, "--type": true,
	"-U": true, "--other-user": true,
	"-u": true, "--user": true,
}

var envOperandOptions = map[string]bool{
	"-C": true, "--chdir": true,
	"-S": true, "--split-string": true,
	"-u": true, "--unset": true,
}

func skipWrapperOptions(words []string, index int, operandOptions map[string]bool) int {
	for index < len(words) {
		word := words[index]
		if word == "--" {
			return index + 1
		}
		if word == "-" || !strings.HasPrefix(word, "-") {
			return index
		}
		consumeNext := wrapperOptionConsumesNext(word, operandOptions)
		index++
		if consumeNext && index < len(words) {
			index++
		}
	}
	return index
}

func wrapperOptionConsumesNext(word string, operandOptions map[string]bool) bool {
	if strings.HasPrefix(word, "--") {
		name, _, attached := strings.Cut(word, "=")
		return operandOptions[name] && !attached
	}
	for index := 1; index < len(word); index++ {
		if operandOptions["-"+word[index:index+1]] {
			return index == len(word)-1
		}
	}
	return false
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

func markdownCommandPrefix(word string) bool {
	if word == "$" || word == "%" || word == ">" || word == "#" || word == "!" {
		return true
	}
	if word == "-" || word == "*" || word == "+" {
		return true
	}
	return false
}
