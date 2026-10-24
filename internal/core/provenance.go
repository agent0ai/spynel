package core

import (
	"strings"
	"unicode"
)

// ForwardedSource bounds untrusted provider names to one inert header label.
// Call only for a provider-confirmed forward; empty names become explicit unknowns.
func ForwardedSource(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) || strings.ContainsRune("()[]:<>", r) {
			return ' '
		}
		return r
	}, name)
	name = strings.Join(strings.Fields(name), " ")
	runes := []rune(name)
	if len(runes) > 128 {
		name = string(runes[:127]) + "…"
	}
	if name == "" {
		return "unknown"
	}
	return name
}
