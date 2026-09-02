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
		switch character {
		case ';', '|', '&', '(', ')', '`':
			flush(index)
			start = index + 1
		}
	}
	flush(len(text))
	return fragments
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
		if character == '#' && !started {
			break
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
	offset := 0
	for offset < len(words) && markdownCommandPrefix(words[offset]) {
		offset++
	}
	for index := offset; index < len(words); index++ {
		word := words[index]
		if strings.ContainsRune(word, '=') && !strings.HasPrefix(word, "=") {
			continue
		}
		if filepath.Base(word) == "htpasswd" {
			return index
		}
		return -1
	}
	return -1
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
