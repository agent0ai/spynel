package harness

import (
	"errors"
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
