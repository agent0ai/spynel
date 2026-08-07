package harness

import (
	"fmt"
	"os/exec"
	"strings"
)

// Definition is one built-in coding harness that Spynel can discover and
// launch without exposing executable paths or provider-specific flags.
type Definition struct {
	Name        string
	DisplayName string
	Command     string
	Description string
	InstallURL  string
}

var catalog = []Definition{
	{
		Name: "codex", DisplayName: "Codex", Command: "codex",
		Description: "OpenAI Codex CLI", InstallURL: "https://developers.openai.com/codex/cli/",
	},
	{
		Name: "claude-code", DisplayName: "Claude Code", Command: "claude",
		Description: "Anthropic Claude Code CLI", InstallURL: "https://docs.anthropic.com/en/docs/claude-code/overview",
	},
}

// Catalog returns the small, deterministic built-in harness list. Codex is
// intentionally first and wins automatic selection when both CLIs exist.
func Catalog() []Definition { return append([]Definition(nil), catalog...) }

func Names() []string {
	result := make([]string, 0, len(catalog))
	for _, definition := range catalog {
		result = append(result, definition.Name)
	}
	return result
}

func Lookup(name string) (Definition, bool) {
	name = NormalizeName(name)
	for _, definition := range catalog {
		if definition.Name == name {
			return definition, true
		}
	}
	return Definition{}, false
}

func NormalizeName(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "claude", "claude_code", "claude-code":
		return "claude-code"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

// Detect returns the first installed harness in catalog priority order.
func Detect(lookPath func(string) (string, error)) (Definition, string, bool) {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	for _, definition := range catalog {
		if command, err := lookPath(definition.Command); err == nil {
			return definition, command, true
		}
	}
	return Definition{}, "", false
}

// ResolveCommand finds the selected harness executable from PATH each time a
// runtime is constructed or reconfigured.
func ResolveCommand(name string, lookPath func(string) (string, error)) (string, error) {
	definition, ok := Lookup(name)
	if !ok {
		if strings.TrimSpace(name) == "" {
			return "", fmt.Errorf("no coding harness is selected; run /harness")
		}
		return "", fmt.Errorf("unknown coding harness %q", name)
	}
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	command, err := lookPath(definition.Command)
	if err != nil {
		return "", fmt.Errorf("%s is not installed or is not on PATH", definition.DisplayName)
	}
	return command, nil
}
