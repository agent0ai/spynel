package instructions

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/agent0ai/spynel/internal/fsx"
)

func stateRoot(t *testing.T) string {
	root := filepath.Join(t.TempDir(), ".spynel")
	if err := os.MkdirAll(filepath.Join(root, "instructions"), 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestAppendLoadsFreshRoleSpecificUTF8InstructionsAtEnd(t *testing.T) {
	root := stateRoot(t)
	for _, role := range roles {
		path := filepath.Join(root, "instructions", "agent-"+string(role)+".md")
		if err := os.WriteFile(path, []byte("Prefer fresh "+string(role)+" behavior. ✓\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		prompt, err := Append("rendered task evidence", root, role)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(prompt, "Prefer fresh "+string(role)+" behavior. ✓") || !strings.Contains(prompt, "the "+string(role)+" agent from "+role.RelativePath()) || !strings.HasSuffix(prompt, "The precedence stated above still applies to every imported rule.") {
			t.Fatalf("%s prompt did not end with its scoped instructions:\n%s", role, prompt)
		}
		for _, other := range roles {
			if other != role && strings.Contains(prompt, "fresh "+string(other)+" behavior") {
				t.Fatalf("%s prompt leaked %s instructions", role, other)
			}
		}
		if err := os.WriteFile(path, []byte("Manually updated."), 0o600); err != nil {
			t.Fatal(err)
		}
		updated, err := Append("next turn", root, role)
		if err != nil || !strings.Contains(updated, "\nManually updated.\n</workspace_owner_persistent_instructions>") {
			t.Fatalf("%s instructions were not live-reloaded: %q, %v", role, updated, err)
		}
	}
}

func TestLoadRejectsUnsafeOrMalformedFilesWithoutPartialContent(t *testing.T) {
	root := stateRoot(t)
	path := filepath.Join(root, "instructions", "agent-chat.md")
	for name, data := range map[string][]byte{
		"invalid UTF-8": {0xff},
		"oversize":      []byte(strings.Repeat("x", MaxBytes+1)),
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if content, _, err := Load(root, Chat); err == nil || content != "" {
			t.Fatalf("%s load = %q, %v", name, content, err)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if content, _, err := Load(root, Chat); err == nil || content != "" {
		t.Fatalf("unsafe symlink load = %q, %v", content, err)
	}
}

func TestLoadRejectsSymlinkedStateAndInstructionsDirectories(t *testing.T) {
	for _, test := range []struct {
		name      string
		linkState bool
	}{
		{name: "state directory", linkState: true},
		{name: "instructions directory"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspaceRoot := t.TempDir()
			outside := t.TempDir()
			outsideInstructions := filepath.Join(outside, "instructions")
			if err := os.Mkdir(outsideInstructions, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(outsideInstructions, "agent-chat.md"), []byte("external rule"), 0o600); err != nil {
				t.Fatal(err)
			}

			root := filepath.Join(workspaceRoot, ".spynel")
			var linkTarget, linkPath string
			if test.linkState {
				linkTarget, linkPath = outside, root
			} else {
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
				linkTarget, linkPath = outsideInstructions, filepath.Join(root, "instructions")
			}
			if err := os.Symlink(linkTarget, linkPath); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			content, status, err := Load(root, Chat)
			if err == nil || content != "" || status.Valid || !strings.Contains(err.Error(), "must not be a symbolic link") {
				t.Fatalf("symlinked boundary load = %q, %#v, %v", content, status, err)
			}
		})
	}
}

func TestLoadRejectsUnknownRolesAndUnsafeWritePermissions(t *testing.T) {
	root := stateRoot(t)
	if _, _, err := Load(root, Role("../history")); err == nil {
		t.Fatal("path traversal role was accepted")
	}
	path := filepath.Join(root, "instructions", "agent-chat.md")
	if err := os.WriteFile(path, []byte("rule"), 0o622); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o622); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(root, Chat); err == nil || !strings.Contains(err.Error(), "group- or world-writable") {
		t.Fatalf("unsafe permission error = %v", err)
	}
}

func TestConcurrentAtomicReplacementNeverReturnsPartialInstructions(t *testing.T) {
	root := stateRoot(t)
	path := filepath.Join(root, "instructions", "agent-developer.md")
	oldContent := strings.Repeat("old", 1000)
	newContent := strings.Repeat("new", 1000)
	if err := fsx.AtomicWriteFile(path, []byte(oldContent), 0o600); err != nil {
		t.Fatal(err)
	}
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for index := 0; index < 100; index++ {
			content := oldContent
			if index%2 == 0 {
				content = newContent
			}
			_ = fsx.AtomicWriteFile(path, []byte(content), 0o600)
		}
	}()
	for index := 0; index < 200; index++ {
		content, _, err := Load(root, Developer)
		if err != nil {
			if strings.Contains(err.Error(), "changed during safety validation") {
				continue
			}
			t.Fatal(err)
		}
		if content != oldContent && content != newContent {
			t.Fatalf("partial concurrent content returned: %d bytes", len(content))
		}
	}
	writers.Wait()
}

func TestMissingFileHasDeterministicEmptySection(t *testing.T) {
	prompt, err := Append("base", stateRoot(t), Developer)
	if err != nil || !strings.Contains(prompt, "No persistent instructions are currently configured for this role.") {
		t.Fatalf("missing-file prompt = %q, %v", prompt, err)
	}
}

func TestEnsureChatGuidanceAppendsCurrentContractOnce(t *testing.T) {
	custom := EnsureChatGuidance("custom chat prompt")
	if !strings.Contains(custom, chatGuidanceMarker) || strings.Count(EnsureChatGuidance(custom), chatGuidanceMarker) != 1 || strings.Count(EnsureChatGuidance(custom), chatTranscriptionGuidanceMarker) != 1 {
		t.Fatalf("chat guidance was missing or duplicated: %q", custom)
	}
}

func TestEnsureChatGuidanceCoversContextualSpynelTranscriptions(t *testing.T) {
	prompt := EnsureChatGuidance("preserved custom chat prompt with " + chatGuidanceMarker)
	for _, test := range []struct {
		input       string
		context     string
		interpretAs string
	}{
		{input: "spinal", context: "the Spynel framework", interpretAs: "Spynel"},
		{input: "spinel", context: "the Spynel framework", interpretAs: "Spynel"},
		{input: "spy nell", context: "the Spynel framework", interpretAs: "Spynel"},
		{input: "spinal", context: "a medical reference", interpretAs: "literal meaning"},
	} {
		if !strings.Contains(prompt, "`"+test.input+"`") || !strings.Contains(prompt, test.context) || !strings.Contains(prompt, test.interpretAs) {
			t.Errorf("guidance does not cover %#v:\n%s", test, prompt)
		}
	}
}

func TestInjectScopeDisciplineIsExactOnceAndPreservesPrecedence(t *testing.T) {
	prompt := InjectScopeDiscipline("Custom prompt with an explicit user request and safety contract.")
	prompt = InjectScopeDiscipline(prompt)
	if strings.Count(prompt, ScopeDisciplineGuidance) != 1 {
		t.Fatalf("scope discipline was missing or duplicated: %q", prompt)
	}
	for _, required := range []string{"explicit user instructions", "safety", "authorization", "lifecycle", "independent-review", "evidence", "data-handling"} {
		if !strings.Contains(ScopeDisciplineGuidance, required) {
			t.Fatalf("scope discipline omitted precedence for %q: %q", required, ScopeDisciplineGuidance)
		}
	}
}
