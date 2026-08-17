package harness

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCatalogKeepsLeadingHarnessChoicesInProductOrder(t *testing.T) {
	names := Names()
	want := []string{"codex", "claude-code", "agent-zero"}
	if len(names) < len(want) || !reflect.DeepEqual(names[:len(want)], want) {
		t.Fatalf("leading harness choices = %#v, want %#v", names, want)
	}
	definition, ok := Lookup("agent-zero")
	if !ok || definition.DisplayName != "Agent Zero CLI" || definition.Command != "a0" || !reflect.DeepEqual(definition.Args, []string{"acp"}) {
		t.Fatalf("Agent Zero definition = %#v, %t", definition, ok)
	}
}

func TestAgentZeroRequiresSuccessfulACPCapabilityCheck(t *testing.T) {
	lookPath := func(name string) (string, error) {
		if name == "a0" {
			return "/tools/a0", nil
		}
		return "", errors.New("missing")
	}
	var checkedCommand string
	var checkedArgs []string
	command, err := resolveCommandWithProbe("agent-zero", lookPath, func(command string, args []string) (string, error) {
		checkedCommand = command
		checkedArgs = append([]string(nil), args...)
		return "A0 ACP check OK\n", nil
	})
	if err != nil || command != "/tools/a0" || checkedCommand != command || !reflect.DeepEqual(checkedArgs, []string{"acp", "--check"}) {
		t.Fatalf("Agent Zero capability resolution = %q, %v, probe %q %#v", command, err, checkedCommand, checkedArgs)
	}
	_, err = resolveCommandWithProbe("agent-zero", lookPath, func(string, []string) (string, error) {
		return "usage: a0", errors.New("exit status 2")
	})
	var capabilityErr *CapabilityError
	if !errors.As(err, &capabilityErr) {
		t.Fatalf("pre-ACP Agent Zero error = %T %v", err, err)
	}
}

func TestAgentZeroFactoryBuildsSharedACPCommandWithoutShell(t *testing.T) {
	registry := NewBuiltinRegistry()
	target, err := registry.Create(HarnessConfig{Name: "agent-zero", Command: "/tools/a0", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	adapter, ok := target.(*ACP)
	if !ok || adapter.config.Command != "/tools/a0" || !reflect.DeepEqual(adapter.config.Args, []string{"acp"}) {
		t.Fatalf("Agent Zero adapter = %#v, %t", target, ok)
	}
}

func TestDetectionSkipsPreACPAgentZero(t *testing.T) {
	definition, command, ok := detectWithProbe(func(name string) (string, error) {
		switch name {
		case "a0":
			return "/tools/a0", nil
		case "pi":
			return "/tools/pi", nil
		default:
			return "", errors.New("missing")
		}
	}, func(string, []string) (string, error) {
		return "", errors.New("ACP unsupported")
	})
	if !ok || definition.Name != "pi" || command != "/tools/pi" {
		t.Fatalf("detection after pre-ACP Agent Zero = %#v, %q, %t", definition, command, ok)
	}
}

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
	command, err := ResolveCommand("claude-code", func(name string) (string, error) { return "/bin/" + name, nil })
	if err != nil || command != "/bin/claude" {
		t.Fatalf("Claude command resolution = %q, %v", command, err)
	}
}

func TestACPProfilesResolveFixedAndCustomCommandsWithoutShellParsing(t *testing.T) {
	definition, ok := Lookup("qwen-code")
	if !ok || definition.Name != "qwen-code" || definition.Command != "qwen" {
		t.Fatalf("Qwen ACP profile = %#v, %t", definition, ok)
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
