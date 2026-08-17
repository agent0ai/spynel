package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Definition is one built-in coding harness that Spynel can discover and
// launch without exposing executable paths or provider-specific flags.
type Definition struct {
	Name        string
	DisplayName string
	Command     string
	Args        []string
	Env         []string
	Description string
	InstallURL  string
	CheckArgs   []string
	CheckOutput string
	Custom      bool
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
	acpDefinitionWithCheck(
		"agent-zero", "Agent Zero CLI", "a0", []string{"acp"}, []string{"acp", "--check"}, "A0 ACP check OK",
		"Agent Zero CLI via ACP", "https://github.com/agent0ai/a0-connector",
	),
	{
		Name: "pi", DisplayName: "Pi", Command: "pi",
		Description: "Pi coding agent via native RPC", InstallURL: "https://github.com/earendil-works/pi",
		factory: func(cfg HarnessConfig) (Harness, error) { return NewPi(cfg) },
	},
	acpDefinition("opencode", "OpenCode", "opencode", []string{"acp"}, "OpenCode via ACP", "https://opencode.ai/docs/acp/"),
	acpDefinition("qwen-code", "Qwen Code", "qwen", []string{"--acp", "--experimental-skills"}, "Qwen Code via ACP", "https://qwenlm.github.io/qwen-code-docs/"),
	acpDefinition("kimi", "Kimi CLI", "kimi", []string{"acp"}, "Kimi CLI via ACP", "https://www.kimi.com/code/docs/en/kimi-code-cli.html"),
	acpDefinition("goose", "Goose", "goose", []string{"acp"}, "Goose via ACP", "https://block.github.io/goose/"),
	acpDefinition("cursor", "Cursor", "cursor-agent", []string{"acp"}, "Cursor Agent via ACP", "https://cursor.com/docs/cli/acp"),
	acpDefinition("gemini-cli", "Gemini CLI", "gemini", []string{"--acp"}, "Gemini CLI via ACP", "https://github.com/google-gemini/gemini-cli"),
	acpDefinition("github-copilot", "GitHub Copilot", "copilot", []string{"--acp"}, "GitHub Copilot CLI via ACP", "https://docs.github.com/en/copilot/reference/copilot-cli-reference/acp-server"),
	acpDefinition("factory-droid", "Factory Droid", "droid", []string{"exec", "--output-format", "acp-daemon"}, "Factory Droid via ACP", "https://docs.factory.ai/droid-exec/overview", "DROID_DISABLE_AUTO_UPDATE=true", "FACTORY_DROID_AUTO_UPDATE_ENABLED=false"),
	{
		Name: "acp", DisplayName: "Custom ACP", Custom: true,
		Description: "User-configured ACP stdio command", InstallURL: "https://agentclientprotocol.com/protocol/v1/transports",
		factory: func(cfg HarnessConfig) (Harness, error) { return NewACP(cfg) },
	},
}

func acpDefinition(name, displayName, command string, args []string, description, installURL string, env ...string) Definition {
	defaults := append([]string(nil), args...)
	environment := append([]string(nil), env...)
	return Definition{
		Name: name, DisplayName: displayName, Command: command, Args: defaults, Env: environment,
		Description: description, InstallURL: installURL,
		factory: func(cfg HarnessConfig) (Harness, error) {
			if len(cfg.Args) == 0 {
				cfg.Args = append([]string(nil), defaults...)
			}
			if len(cfg.Env) == 0 {
				cfg.Env = append([]string(nil), environment...)
			}
			return NewACP(cfg)
		},
	}
}

func acpDefinitionWithCheck(name, displayName, command string, args, checkArgs []string, checkOutput, description, installURL string) Definition {
	definition := acpDefinition(name, displayName, command, args, description, installURL)
	definition.CheckArgs = append([]string(nil), checkArgs...)
	definition.CheckOutput = checkOutput
	return definition
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

// Catalog returns the deterministic built-in harness list. Codex is
// intentionally first and wins automatic selection when several CLIs exist.
func Catalog() []Definition {
	result := append([]Definition(nil), catalog...)
	for index := range result {
		result[index] = cloneDefinition(result[index])
	}
	return result
}

func cloneDefinition(definition Definition) Definition {
	definition.Args = append([]string(nil), definition.Args...)
	definition.Env = append([]string(nil), definition.Env...)
	definition.CheckArgs = append([]string(nil), definition.CheckArgs...)
	return definition
}

// CapabilityError means an executable was found but failed its profile's
// bounded capability check. It is distinct from an absent executable so user
// interfaces can recommend an update instead of reporting a false absence.
type CapabilityError struct {
	DisplayName string
}

func (e *CapabilityError) Error() string {
	return e.DisplayName + " is installed but does not support ACP; install or update to an ACP-capable release"
}

type capabilityProbe func(command string, args []string) (string, error)

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
			return cloneDefinition(definition), true
		}
	}
	return Definition{}, false
}

func NormalizeName(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// Detect returns the first installed harness in catalog priority order.
func Detect(lookPath func(string) (string, error)) (Definition, string, bool) {
	return detectWithProbe(lookPath, runCapabilityProbe)
}

func detectWithProbe(lookPath func(string) (string, error), probe capabilityProbe) (Definition, string, bool) {
	homeDir := (func() (string, error))(nil)
	if lookPath == nil {
		lookPath = exec.LookPath
		homeDir = os.UserHomeDir
	}
	for _, definition := range catalog {
		if definition.Custom || definition.Command == "" {
			continue
		}
		if command, err := resolveDefinitionCommand(definition, lookPath, homeDir); err == nil && checkDefinitionCapability(definition, command, probe) == nil {
			return cloneDefinition(definition), command, true
		}
	}
	return Definition{}, "", false
}

// ResolveConfiguredCommand resolves a built-in alias or the executable
// explicitly configured for the custom ACP profile. Arguments are never
// interpreted by a shell.
func ResolveConfiguredCommand(name, customCommand string, lookPath func(string) (string, error)) (string, error) {
	definition, ok := Lookup(name)
	if !ok {
		if strings.TrimSpace(name) == "" {
			return "", fmt.Errorf("no coding harness is selected; run /harness")
		}
		return "", fmt.Errorf("unknown coding harness %q", name)
	}
	if !definition.Custom {
		return ResolveCommand(name, lookPath)
	}
	command := strings.TrimSpace(customCommand)
	if command == "" {
		return "", errors.New("custom ACP requires harness.acp_command")
	}
	if strings.ContainsRune(command, '\x00') {
		return "", errors.New("custom ACP command contains an invalid NUL byte")
	}
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	resolved, err := lookPath(command)
	if err != nil {
		return "", fmt.Errorf("custom ACP command %q is not installed or executable", command)
	}
	return resolved, nil
}

// CommandArgs returns the fixed arguments for a built-in ACP alias or a copy
// of the custom ACP argument list.
func CommandArgs(name string, custom []string) []string {
	definition, ok := Lookup(name)
	if !ok {
		return nil
	}
	if definition.Custom {
		return append([]string(nil), custom...)
	}
	return append([]string(nil), definition.Args...)
}

// ResolveCommand finds the selected harness executable from PATH or the
// conventional per-user .local/bin directory each time a runtime is
// constructed or reconfigured.
func ResolveCommand(name string, lookPath func(string) (string, error)) (string, error) {
	return resolveCommandWithProbe(name, lookPath, runCapabilityProbe)
}

func resolveCommandWithProbe(name string, lookPath func(string) (string, error), probe capabilityProbe) (string, error) {
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
	if err := checkDefinitionCapability(definition, command, probe); err != nil {
		return "", err
	}
	return command, nil
}

func checkDefinitionCapability(definition Definition, command string, probe capabilityProbe) error {
	if len(definition.CheckArgs) == 0 {
		return nil
	}
	output, err := probe(command, append([]string(nil), definition.CheckArgs...))
	if err != nil || !strings.Contains(output, definition.CheckOutput) {
		return &CapabilityError{DisplayName: definition.DisplayName}
	}
	return nil
}

func runCapabilityProbe(command string, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	output := newTailBuffer(64 * 1024)
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	if ctx.Err() != nil {
		return output.String(), ctx.Err()
	}
	return output.String(), err
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
