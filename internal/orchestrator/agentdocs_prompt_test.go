package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent0ai/spynel/internal/agentdocs"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/extensions"
	"github.com/agent0ai/spynel/internal/workspace"
)

func TestEveryOrchestrationPhaseGetsOneCallableDocsGuidance(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	manager := New(cfg, nil, extensions.Runner{})
	for _, route := range cfg.Orchestrator.Routes {
		paths := []string{route.Prompt, route.RecoveryPrompt}
		if route.ReviewPrompt != "" {
			paths = append(paths, route.ReviewPrompt)
		}
		for _, path := range paths {
			prompt, err := manager.renderPrompt(route, filepath.Join(root, ".spynel", route.Name, "working", "example.md"), path)
			if err != nil {
				t.Fatalf("render %s: %v", path, err)
			}
			if strings.Count(prompt, " docs <topic>") != 1 || strings.Contains(prompt, agentdocs.PromptPlaceholder) || !strings.Contains(prompt, "AGENTS.md") {
				t.Fatalf("%s guidance is missing, duplicated, or unresolved:\n%s", path, prompt)
			}
		}
	}

	legacy := filepath.Join(root, ".spynel", "prompts", "legacy.md")
	if err := os.WriteFile(legacy, []byte("custom recovery prompt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	route := cfg.Orchestrator.Routes[0]
	prompt, err := manager.renderPrompt(route, filepath.Join(root, "example.md"), legacy)
	if err != nil || strings.Count(prompt, " docs <topic>") != 1 {
		t.Fatalf("legacy prompt upgrade = %q, %v", prompt, err)
	}
	prompt, err = manager.renderPrompt(route, filepath.Join(root, agentdocs.PromptPlaceholder+".md"), route.Prompt)
	if err != nil || strings.Count(prompt, " docs <topic>") != 1 {
		t.Fatalf("placeholder-like replacement data duplicated guidance = %q, %v", prompt, err)
	}
}
