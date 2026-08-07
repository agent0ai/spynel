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
