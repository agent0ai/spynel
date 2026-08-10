package config

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ParseCommandLineArguments parses the one-line, shell-free argument syntax
// used by the shared settings surfaces. Backslashes only introduce an escape
// before whitespace, quotes, or another backslash, so ordinary Windows paths
// retain their separators.
func ParseCommandLineArguments(text string) ([]string, error) {
	if !utf8.ValidString(text) {
		return nil, fmt.Errorf("harness.acp_args contains invalid UTF-8")
	}
	if strings.ContainsRune(text, '\x00') {
		return nil, fmt.Errorf("harness.acp_args contains a NUL byte")
	}
	if strings.ContainsAny(text, "\r\n") {
		return nil, fmt.Errorf("harness.acp_args must be one line")
	}

	var arguments []string
	var argument strings.Builder
	quote := rune(0)
	started := false
	runes := []rune(text)
	for index := 0; index < len(runes); index++ {
		current := runes[index]
		if quote == 0 && unicode.IsSpace(current) {
			if started {
				arguments = append(arguments, argument.String())
				argument.Reset()
				started = false
			}
			continue
		}
		if current == '\'' || current == '"' {
			if quote == 0 {
				quote = current
				started = true
				continue
			}
			if quote == current {
				quote = 0
				continue
			}
		}
		if current == '\\' {
			if index+1 == len(runes) {
				return nil, fmt.Errorf("harness.acp_args ends with a dangling escape")
			}
			next := runes[index+1]
			if unicode.IsSpace(next) || next == '\\' || next == '\'' || next == '"' {
				argument.WriteRune(next)
				started = true
				index++
				continue
			}
		}
		argument.WriteRune(current)
		started = true
	}
	if quote != 0 {
		return nil, fmt.Errorf("harness.acp_args has an unmatched %c quote", quote)
	}
	if started {
		arguments = append(arguments, argument.String())
	}
	return arguments, nil
}

// FormatCommandLineArguments returns a deterministic shell-free scalar whose
// parse result is exactly arguments. It quotes only values that need quoting;
// shell metacharacters remain ordinary data because this string is never sent
// to a shell.
func FormatCommandLineArguments(arguments []string) (string, error) {
	formatted := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		if !utf8.ValidString(argument) {
			return "", fmt.Errorf("harness.acp_args contains invalid UTF-8")
		}
		if strings.ContainsRune(argument, '\x00') {
			return "", fmt.Errorf("harness.acp_args contains a NUL byte")
		}
		if strings.ContainsAny(argument, "\r\n") {
			return "", fmt.Errorf("harness.acp_args cannot represent multiline arguments")
		}
		if argument != "" && !strings.ContainsFunc(argument, func(r rune) bool {
			return unicode.IsSpace(r) || r == '\\' || r == '\'' || r == '"'
		}) {
			formatted = append(formatted, argument)
			continue
		}
		var quoted strings.Builder
		quoted.WriteByte('"')
		for _, current := range argument {
			if current == '\\' || current == '"' {
				quoted.WriteByte('\\')
			}
			quoted.WriteRune(current)
		}
		quoted.WriteByte('"')
		formatted = append(formatted, quoted.String())
	}
	return strings.Join(formatted, " "), nil
}
