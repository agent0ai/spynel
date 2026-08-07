package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/theme"
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
		".spynel/prompts/create-task.md", ".spynel/prompts/create-goal.md", ".spynel/prompts/task.md", ".spynel/prompts/review.md", ".spynel/prompts/goal-review.md",
		".spynel/tasks/todo", ".spynel/tasks/working", ".spynel/tasks/review", ".spynel/tasks/reviewing", ".spynel/tasks/cancelled",
		".spynel/goals/proposed", ".spynel/goals/planning", ".spynel/goals/active", ".spynel/goals/review", ".spynel/goals/reviewing", ".spynel/goals/abandoned",
		".spynel/attachments", ".spynel/runtime/leases",
		".spynel/themes/spynel.yaml", ".spynel/themes/hack-the-box.yaml", ".spynel/themes/github-colorblind-dark.yaml",
		".spynel/themes/gruvbox-dark.yaml", ".spynel/themes/nord.yaml", ".spynel/themes/okabe-ito-dark.yaml",
		".spynel/themes/gruvbox-light.yaml", ".spynel/themes/rose-pine-dawn.yaml", ".spynel/themes/tol-muted-light.yaml",
		".spynel/themes/catppuccin-latte.yaml",
		".spynel/themes/okabe-ito-light.yaml", ".spynel/themes/solarized-light.yaml",
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
	if cfg.Channels.TUI.Theme != "spynel" {
		t.Fatalf("initialized TUI theme = %q", cfg.Channels.TUI.Theme)
	}
	themes, err := theme.LoadDir(cfg.StatePath("themes"))
	if err != nil || len(themes) != 12 {
		t.Fatalf("initialized themes = %#v, %v", themes, err)
	}
	for index, builtin := range theme.Builtins() {
		if themes[index].Name != builtin.Name {
			t.Fatalf("initialized theme %d = %q, want %q", index, themes[index].Name, builtin.Name)
		}
		loaded, ok := theme.Find(themes, builtin.Name)
		if !ok || loaded != builtin {
			t.Fatalf("initialized theme %q differs from built-in: loaded=%#v builtin=%#v", builtin.Name, loaded, builtin)
		}
	}
	if !cfg.Speech.Enabled || cfg.Speech.Language != "en" || cfg.Speech.NumThreads != 2 {
		t.Fatalf("initialized workspace must enable English Parakeet by default: %#v", cfg.Speech)
	}
	if err := Init(root, false); err == nil {
		t.Fatal("second init should require --force")
	}
}

func TestUpgradeAddsReviewAssetsWithoutOverwritingPrompts(t *testing.T) {
	root := t.TempDir()
	if err := Init(root, false); err != nil {
		t.Fatal(err)
	}
	taskPrompt := filepath.Join(root, ".spynel", "prompts", "task.md")
	custom := []byte("custom task prompt\n")
	if err := os.WriteFile(taskPrompt, custom, 0o600); err != nil {
		t.Fatal(err)
	}
	reviewPrompt := filepath.Join(root, ".spynel", "prompts", "review.md")
	if err := os.Remove(reviewPrompt); err != nil {
		t.Fatal(err)
	}
	createTaskPrompt := filepath.Join(root, ".spynel", "prompts", "create-task.md")
	if err := os.Remove(createTaskPrompt); err != nil {
		t.Fatal(err)
	}
	goalReviewPrompt := filepath.Join(root, ".spynel", "prompts", "goal-review.md")
	if err := os.Remove(goalReviewPrompt); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, ".spynel", "tasks", "review")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, ".spynel", "goals", "reviewing")); err != nil {
		t.Fatal(err)
	}
	if err := Upgrade(root); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(taskPrompt); string(got) != string(custom) {
		t.Fatal("upgrade overwrote a user prompt")
	}
	if _, err := os.Stat(reviewPrompt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".spynel", "tasks", "review")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{createTaskPrompt, goalReviewPrompt, filepath.Join(root, ".spynel", "goals", "reviewing")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("upgrade did not restore %s: %v", path, err)
		}
	}
}

func TestUpgradePreservesCustomThemeCollection(t *testing.T) {
	root := t.TempDir()
	themeDir := filepath.Join(root, ".spynel", "themes")
	if err := os.MkdirAll(themeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	customPath := filepath.Join(themeDir, "custom.yaml")
	custom := []byte("user-owned custom theme\n")
	if err := os.WriteFile(customPath, custom, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Upgrade(root); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(themeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "custom.yaml" {
		t.Fatalf("upgrade changed custom theme collection: %#v", entries)
	}
	if got, err := os.ReadFile(customPath); err != nil || string(got) != string(custom) {
		t.Fatalf("upgrade changed custom theme: %q, %v", got, err)
	}
}

func TestForceInitAddsRevisedThemesWithoutReplacingExistingFiles(t *testing.T) {
	previousDetection := detectCodingHarness
	detectCodingHarness = func(func(string) (string, error)) (harness.Definition, string, bool) {
		return harness.Definition{}, "", false
	}
	t.Cleanup(func() { detectCodingHarness = previousDetection })
	root := t.TempDir()
	if err := Init(root, false); err != nil {
		t.Fatal(err)
	}
	customPath := filepath.Join(root, ".spynel", "themes", "spynel.yaml")
	custom := []byte("user-owned theme file\n")
	if err := os.WriteFile(customPath, custom, 0o600); err != nil {
		t.Fatal(err)
	}
	newPath := filepath.Join(root, ".spynel", "themes", "tol-muted-light.yaml")
	if err := os.Remove(newPath); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(root, ".spynel", "themes", "tokyo-night.yaml")
	if err := os.WriteFile(legacyPath, []byte("user-owned legacy theme\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Init(root, true); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(customPath); err != nil || string(got) != string(custom) {
		t.Fatalf("force init replaced existing stock-named file: %q, %v", got, err)
	}
	if got, err := os.ReadFile(legacyPath); err != nil || string(got) != "user-owned legacy theme\n" {
		t.Fatalf("force init removed legacy theme: %q, %v", got, err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("force init did not materialize missing revised theme: %v", err)
	}
}

func TestCommunicationPromptDispatchesWorkAndStaysResponsive(t *testing.T) {
	data, err := Template("chat.md")
	if err != nil {
		t.Fatal(err)
	}
	prompt := string(data)
	for _, contract := range []string{
		"responsive control plane",
		"Questions, conversation, and status requests: answer promptly",
		"create or update a durable task",
		"create or update a durable goal",
		"Do not implement requested work",
		"Perform routine bounded inspection silently",
		"send exactly one concise consolidated user-facing response",
		"Do not send a preamble or progress message",
		"obtain the environment's current UTC time",
		"If a new message arrives while you are responding",
		"Do not silently abandon earlier commitments",
	} {
		if !strings.Contains(prompt, contract) {
			t.Fatalf("communication prompt is missing %q:\n%s", contract, prompt)
		}
	}
}

func TestOrchestrationPromptsRequireClockDerivedTimestamps(t *testing.T) {
	for _, name := range []string{"task.md", "review.md", "goal.md", "goal-review.md", "recovery.md"} {
		data, err := Template(name)
		if err != nil {
			t.Fatal(err)
		}
		prompt := string(data)
		if !strings.Contains(prompt, "current UTC time") || !strings.Contains(prompt, "estimate") {
			t.Fatalf("%s does not require a clock-derived timestamp:\n%s", name, prompt)
		}
		if !strings.Contains(prompt, ".spynel/AGENTS.md") || !strings.Contains(prompt, "hidden") {
			t.Fatalf("%s does not require the hidden workspace DOX chain:\n%s", name, prompt)
		}
	}
}

func TestCreationPromptsDefineTaskAndGoalContracts(t *testing.T) {
	for _, test := range []struct {
		name string
		want []string
	}{
		{"create-task.md", []string{"{{USER_MESSAGE}}", "{{TASK_SOURCE}}", "one finite, independently verifiable objective", "[done, failed, waiting, cancelled]", "Do not implement"}},
		{"create-goal.md", []string{"{{USER_MESSAGE}}", "{{GOAL_SOURCE}}", "long-lived, recurring, or multi-round outcome", "success_criteria", "round: 0", "Do not create implementation tasks"}},
	} {
		data, err := Template(test.name)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range test.want {
			if !strings.Contains(string(data), want) {
				t.Fatalf("%s is missing %q", test.name, want)
			}
		}
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
