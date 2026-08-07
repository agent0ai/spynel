package harness

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	factory     Factory
}

var catalog = []Definition{
	{
		Name: "codex", DisplayName: "Codex", Command: "codex",
		Description: "OpenAI Codex CLI", InstallURL: "https://developers.openai.com/codex/cli/",
		factory: newCodexFromHarnessConfig,
	},
	{
		Name: "claude-code", DisplayName: "Claude Code", Command: "claude",
		Description: "Anthropic Claude Code CLI", InstallURL: "https://docs.anthropic.com/en/docs/claude-code/overview",
		factory: func(cfg HarnessConfig) (Harness, error) { return NewClaude(cfg) },
	},
}

func newCodexFromHarnessConfig(cfg HarnessConfig) (Harness, error) {
	return NewCodex(CodexConfig{
		Command: cfg.Command, Cwd: cfg.Cwd, Model: cfg.Model, Effort: cfg.Effort,
		ApprovalPolicy: cfg.ApprovalPolicy, Sandbox: cfg.Sandbox, Network: cfg.Network,
		SessionsFile: cfg.SessionsFile, Version: cfg.Version, Stderr: cfg.Stderr,
	})
}

// NewBuiltinRegistry returns factories for every entry in Catalog. Adding a
// built-in harness therefore keeps its discovery metadata and constructor in
// one package; application and channel code remain provider-neutral.
func NewBuiltinRegistry() *Registry {
	registry := NewRegistry()
	for _, definition := range catalog {
		registry.Register(definition.Name, definition.factory)
	}
	return registry
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
	homeDir := (func() (string, error))(nil)
	if lookPath == nil {
		lookPath = exec.LookPath
		homeDir = os.UserHomeDir
	}
	for _, definition := range catalog {
		if command, err := resolveDefinitionCommand(definition, lookPath, homeDir); err == nil {
			return definition, command, true
		}
	}
	return Definition{}, "", false
}

// ResolveCommand finds the selected harness executable from PATH or the
// conventional per-user .local/bin directory each time a runtime is
// constructed or reconfigured.
func ResolveCommand(name string, lookPath func(string) (string, error)) (string, error) {
	definition, ok := Lookup(name)
	if !ok {
		if strings.TrimSpace(name) == "" {
			return "", fmt.Errorf("no coding harness is selected; run /harness")
		}
		return "", fmt.Errorf("unknown coding harness %q", name)
	}
	homeDir := (func() (string, error))(nil)
	if lookPath == nil {
		lookPath = exec.LookPath
		homeDir = os.UserHomeDir
	}
	command, err := resolveDefinitionCommand(definition, lookPath, homeDir)
	if err != nil {
		return "", fmt.Errorf("%s is not installed on PATH or in the standard user-local bin directory", definition.DisplayName)
	}
	return command, nil
}

func resolveDefinitionCommand(definition Definition, lookPath func(string) (string, error), homeDir func() (string, error)) (string, error) {
	if command, err := lookPath(definition.Command); err == nil {
		return command, nil
	}
	if homeDir == nil {
		return "", exec.ErrNotFound
	}
	home, err := homeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "", exec.ErrNotFound
	}
	name := definition.Command
	if runtime.GOOS == "windows" && filepath.Ext(name) == "" {
		name += ".exe"
	}
	candidate := filepath.Join(home, ".local", "bin", name)
	info, err := os.Stat(candidate)
	if err != nil || !info.Mode().IsRegular() {
		return "", exec.ErrNotFound
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return "", exec.ErrNotFound
	}
	return candidate, nil
}
