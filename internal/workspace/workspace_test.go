package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/frdel/spynel/internal/config"
	"github.com/frdel/spynel/internal/harness"
)

func TestInitCreatesDocumentedWorkspace(t *testing.T) {
	previousDetection := detectCodingHarness
	detectCodingHarness = func(func(string) (string, error)) (harness.Definition, string, bool) {
		definition, _ := harness.Lookup("claude-code")
		return definition, "/usr/local/bin/claude", true
	}
	t.Cleanup(func() { detectCodingHarness = previousDetection })
	root := t.TempDir()
	if err := Init(root, false); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"spynel.yaml", ".spynel/AGENTS.md", ".spynel/tasks/AGENTS.md",
		".spynel/prompts/task.md", ".spynel/tasks/todo", ".spynel/attachments", ".spynel/runtime/leases",
	} {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Fatalf("missing initialized path %s: %v", path, err)
		}
	}
	cfg, err := config.Load(filepath.Join(root, "spynel.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Root != root {
		t.Fatalf("config root = %q, want %q", cfg.Root, root)
	}
	if cfg.Harness.Name != "claude-code" {
		t.Fatalf("detected coding harness was not selected: %#v", cfg.Harness)
	}
	if cfg.Harness.Sandbox != "danger-full-access" {
		t.Fatalf("initialized coding harness should be unrestricted: %#v", cfg.Harness)
	}
	if !cfg.Speech.Enabled || cfg.Speech.Model != "small" {
		t.Fatalf("initialized workspace must enable the small Whisper model by default: %#v", cfg.Speech)
	}
	if err := Init(root, false); err == nil {
		t.Fatal("second init should require --force")
	}
}

func TestInitLeavesHarnessSelectionOpenWhenNothingIsDetected(t *testing.T) {
	previousDetection := detectCodingHarness
	detectCodingHarness = func(func(string) (string, error)) (harness.Definition, string, bool) {
		return harness.Definition{}, "", false
	}
	t.Cleanup(func() { detectCodingHarness = previousDetection })
	root := t.TempDir()
	if err := Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(root, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Harness.Name != "" {
		t.Fatalf("unexpected coding harness selection: %#v", cfg.Harness)
	}
}
