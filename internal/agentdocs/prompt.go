package agentdocs

import (
	"os"
	"path/filepath"
	"strings"
)

const PromptPlaceholder = "{{SPYNEL_DOCS_GUIDANCE}}"

// PromptGuidance returns one concise instruction with a directly callable
// executable path. Slash-separated absolute paths are accepted by supported
// Windows shells and avoid backslash escaping ambiguity.
func PromptGuidance() string {
	executable, err := os.Executable()
	if err != nil || !filepath.IsAbs(executable) {
		executable = "spynel"
	}
	return "When Spynel-specific behavior is missing or may be stale, query `" + promptCommand(executable) + " docs <topic>` and follow its references. Do not query on every turn. Explicit user instructions and the nearest workspace/repository `AGENTS.md` or DOX contract take precedence."
}

func promptCommand(executable string) string {
	executable = strings.ReplaceAll(filepath.ToSlash(executable), `\`, "/")
	if strings.ContainsAny(executable, " \t") {
		executable = `"` + strings.ReplaceAll(executable, `"`, `\"`) + `"`
	}
	return executable
}

// InjectPromptGuidance upgrades both stock and user-owned legacy prompts while
// guaranteeing that the current guidance is present exactly once.
func InjectPromptGuidance(prompt string) string {
	guidance := PromptGuidance()
	if strings.Contains(prompt, PromptPlaceholder) {
		prompt = strings.Replace(prompt, PromptPlaceholder, guidance, 1)
		return strings.ReplaceAll(prompt, PromptPlaceholder, "")
	}
	if strings.Contains(prompt, guidance) {
		return prompt
	}
	return strings.TrimRight(prompt, "\r\n") + "\n\n" + guidance
}
