package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent0ai/spynel/internal/agentdocs"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/extensions"
	"github.com/agent0ai/spynel/internal/instructions"
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
	for _, role := range []string{"chat", "developer", "reviewer", "notification", "heartbeat"} {
		data, err := os.ReadFile(cfg.StatePath("instructions", "agent-"+role+".md"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), instructions.ScopeDisciplineGuidance) || strings.Contains(string(data), instructions.DeveloperScopeDisciplineGuidance) || strings.Contains(string(data), instructions.ReviewerScopeDisciplineGuidance) {
			t.Fatalf("new workspace stored framework scope discipline as a local %s rule", role)
		}
	}
	for _, route := range cfg.Orchestrator.Routes {
		paths := []string{route.Prompt, route.RecoveryPrompt}
		if route.ReviewPrompt != "" {
			paths = append(paths, route.ReviewPrompt)
		}
		for _, path := range paths {
			prompt, err := manager.renderPrompt(route, Lease{File: filepath.Join(root, ".spynel", route.Name, "working", "example.md")}, path)
			if err != nil {
				t.Fatalf("render %s: %v", path, err)
			}
			if strings.Count(prompt, " docs <topic>") != 1 || strings.Contains(prompt, agentdocs.PromptPlaceholder) || !strings.Contains(prompt, "AGENTS.md") {
				t.Fatalf("%s guidance is missing, duplicated, or unresolved:\n%s", path, prompt)
			}
			if strings.Count(prompt, instructions.ScopeDisciplineGuidance) != 1 {
				t.Fatalf("%s scope discipline is missing or duplicated:\n%s", path, prompt)
			}
		}
	}

	custom := filepath.Join(root, ".spynel", "prompts", "custom.md")
	if err := os.WriteFile(custom, []byte("custom recovery prompt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	route := cfg.Orchestrator.Routes[0]
	prompt, err := manager.renderPrompt(route, Lease{File: filepath.Join(root, "example.md")}, custom)
	if err != nil || strings.Count(prompt, " docs <topic>") != 1 || strings.Count(prompt, instructions.ScopeDisciplineGuidance) != 1 {
		t.Fatalf("custom prompt guidance = %q, %v", prompt, err)
	}
	prompt, err = manager.renderPrompt(route, Lease{File: filepath.Join(root, agentdocs.PromptPlaceholder+".md")}, route.Prompt)
	if err != nil || strings.Count(prompt, " docs <topic>") != 1 {
		t.Fatalf("placeholder-like replacement data duplicated guidance = %q, %v", prompt, err)
	}
	for _, test := range []struct {
		name         string
		route        config.Route
		file         string
		prompt       string
		roleGuidance string
	}{
		{name: "task implementation", route: cfg.Orchestrator.Routes[0], file: filepath.Join(root, ".spynel", "tasks", "working", "task.md"), prompt: cfg.Orchestrator.Routes[0].Prompt, roleGuidance: instructions.DeveloperScopeDisciplineGuidance},
		{name: "task recovery", route: cfg.Orchestrator.Routes[0], file: filepath.Join(root, ".spynel", "tasks", "working", "task.md"), prompt: cfg.Orchestrator.Routes[0].RecoveryPrompt, roleGuidance: instructions.DeveloperScopeDisciplineGuidance},
		{name: "task review", route: cfg.Orchestrator.Routes[0], file: filepath.Join(root, ".spynel", "tasks", "reviewing", "task.md"), prompt: cfg.Orchestrator.Routes[0].ReviewPrompt, roleGuidance: instructions.ReviewerScopeDisciplineGuidance},
		{name: "goal planning", route: cfg.Orchestrator.Routes[1], file: filepath.Join(root, ".spynel", "goals", "planning", "goal.md"), prompt: cfg.Orchestrator.Routes[1].Prompt, roleGuidance: instructions.DeveloperScopeDisciplineGuidance},
		{name: "goal recovery", route: cfg.Orchestrator.Routes[1], file: filepath.Join(root, ".spynel", "goals", "planning", "goal.md"), prompt: cfg.Orchestrator.Routes[1].RecoveryPrompt, roleGuidance: instructions.DeveloperScopeDisciplineGuidance},
		{name: "goal review", route: cfg.Orchestrator.Routes[1], file: filepath.Join(root, ".spynel", "goals", "reviewing", "goal.md"), prompt: cfg.Orchestrator.Routes[1].ReviewPrompt, roleGuidance: instructions.ReviewerScopeDisciplineGuidance},
	} {
		t.Run(test.name, func(t *testing.T) {
			stock, err := manager.renderPrompt(test.route, Lease{File: test.file}, test.prompt)
			if err != nil || strings.Count(stock, instructions.ScopeDisciplineGuidance) != 1 || strings.Count(stock, test.roleGuidance) != 1 {
				t.Fatalf("stock prompt scope discipline = %q, %v", stock, err)
			}
			overridden, err := manager.renderPrompt(test.route, Lease{File: test.file}, custom)
			if err != nil || strings.Count(overridden, instructions.ScopeDisciplineGuidance) != 1 || strings.Count(overridden, test.roleGuidance) != 1 {
				t.Fatalf("custom prompt scope discipline = %q, %v", overridden, err)
			}
			if !strings.Contains(stock, "explicit user instructions") || !strings.Contains(stock, "safety") || !strings.Contains(overridden, "custom recovery prompt") {
				t.Fatalf("prompt lost explicit user/safety precedence or the workspace override: stock=%q custom=%q", stock, overridden)
			}
		})
	}
}
