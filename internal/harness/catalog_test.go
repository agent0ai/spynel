package harness

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDetectUsesCatalogPriorityAndAutomaticCommands(t *testing.T) {
	seen := []string{}
	definition, command, ok := Detect(func(name string) (string, error) {
		seen = append(seen, name)
		return "/tools/" + name, nil
	})
	if !ok || definition.Name != "codex" || command != "/tools/codex" || len(seen) != 1 {
		t.Fatalf("detection = %#v, %q, %t, probes %#v", definition, command, ok, seen)
	}
	definition, command, ok = Detect(func(name string) (string, error) {
		if name == "codex" {
			return "", errors.New("missing")
		}
		return "/tools/claude", nil
	})
	if !ok || definition.Name != "claude-code" || command != "/tools/claude" {
		t.Fatalf("fallback detection = %#v, %q, %t", definition, command, ok)
	}
}

func TestResolveCommandRejectsManualOrUnknownHarnesses(t *testing.T) {
	if _, err := ResolveCommand("/tmp/custom", func(string) (string, error) { return "", nil }); err == nil {
		t.Fatal("manual executable path was accepted as a harness")
	}
	command, err := ResolveCommand("claude", func(name string) (string, error) { return "/bin/" + name, nil })
	if err != nil || command != "/bin/claude" {
		t.Fatalf("Claude alias resolution = %q, %v", command, err)
	}
}

func TestACPProfilesResolveFixedAndCustomCommandsWithoutShellParsing(t *testing.T) {
	definition, ok := Lookup("qwen")
	if !ok || definition.Name != "qwen-code" || definition.Command != "qwen" {
		t.Fatalf("Qwen ACP alias = %#v, %t", definition, ok)
	}
	arguments := CommandArgs("qwen-code", nil)
	if len(arguments) != 2 || arguments[0] != "--acp" || arguments[1] != "--experimental-skills" {
		t.Fatalf("Qwen ACP arguments = %#v", arguments)
	}
	arguments[0] = "changed"
	if CommandArgs("qwen-code", nil)[0] != "--acp" {
		t.Fatal("catalog ACP arguments were returned by reference")
	}
	droid, ok := Lookup("factory-droid")
	if !ok || len(droid.Env) != 2 || droid.Env[0] != "DROID_DISABLE_AUTO_UPDATE=true" {
		t.Fatalf("Factory Droid ACP environment = %#v, %t", droid.Env, ok)
	}
	customArgs := []string{"--stdio", "value with spaces"}
	if got := CommandArgs("acp", customArgs); len(got) != 2 || got[1] != "value with spaces" {
		t.Fatalf("custom ACP arguments = %#v", got)
	}
	resolved, err := ResolveConfiguredCommand("acp", "custom-agent", func(name string) (string, error) {
		if name != "custom-agent" {
			t.Fatalf("custom command lookup received %q", name)
		}
		return "/tools/custom-agent", nil
	})
	if err != nil || resolved != "/tools/custom-agent" {
		t.Fatalf("custom ACP command = %q, %v", resolved, err)
	}
	if _, err := ResolveConfiguredCommand("acp", "", func(string) (string, error) { return "", nil }); err == nil {
		t.Fatal("empty custom ACP command was accepted")
	}
}

func TestResolveDefinitionCommandUsesStandardUserLocalBin(t *testing.T) {
	definition, ok := Lookup("claude-code")
	if !ok {
		t.Fatal("Claude Code definition is missing")
	}
	home := t.TempDir()
	command := filepath.Join(home, ".local", "bin", definition.Command)
	if err := os.MkdirAll(filepath.Dir(command), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(command, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveDefinitionCommand(
		definition,
		func(string) (string, error) { return "", errors.New("not on PATH") },
		func() (string, error) { return home, nil },
	)
	if err != nil || resolved != command {
		t.Fatalf("user-local resolution = %q, %v", resolved, err)
	}
}

func TestBuiltinRegistryCoversCatalog(t *testing.T) {
	registry := NewBuiltinRegistry()
	for _, definition := range Catalog() {
		target, err := registry.Create(HarnessConfig{Name: definition.Name, Command: "fixture", Cwd: t.TempDir()})
		if err != nil {
			t.Fatalf("create %s: %v", definition.Name, err)
		}
		if target == nil {
			t.Fatalf("factory for %s returned nil", definition.Name)
		}
	}
}
