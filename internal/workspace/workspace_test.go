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
		".spynel/config.yaml", ".spynel/AGENTS.md", ".spynel/tasks/AGENTS.md",
		".spynel/prompts/create-task.md", ".spynel/prompts/create-goal.md", ".spynel/prompts/task.md", ".spynel/prompts/review.md", ".spynel/prompts/goal-review.md", ".spynel/prompts/heartbeat.md", ".spynel/prompts/notification.md",
		".spynel/instructions/agent-chat.md", ".spynel/instructions/agent-developer.md", ".spynel/instructions/agent-reviewer.md", ".spynel/instructions/agent-notification.md", ".spynel/instructions/agent-heartbeat.md",
		".spynel/tasks/todo", ".spynel/tasks/working", ".spynel/tasks/review", ".spynel/tasks/reviewing", ".spynel/tasks/cancelled", ".spynel/tasks/archive",
		".spynel/goals/proposed", ".spynel/goals/planning", ".spynel/goals/active", ".spynel/goals/review", ".spynel/goals/reviewing", ".spynel/goals/abandoned",
		".spynel/attachments", ".spynel/jobs", ".spynel/runtime/leases",
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
	instructionEntries, err := os.ReadDir(filepath.Join(root, ".spynel", "instructions"))
	if err != nil || len(instructionEntries) != 5 {
		t.Fatalf("initialized instruction files = %d, %v", len(instructionEntries), err)
	}
	for _, entry := range instructionEntries {
		info, statErr := entry.Info()
		if statErr != nil {
			t.Fatalf("instruction file %s metadata: %v", entry.Name(), statErr)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("instruction file %s mode = %v", entry.Name(), info.Mode())
		}
	}
	cfg, err := config.Load(config.PathForRoot(root))
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

func TestInitAndUpgradeRejectSymlinkedInstructionBoundaries(t *testing.T) {
	previousDetection := detectCodingHarness
	detectCodingHarness = func(func(string) (string, error)) (harness.Definition, string, bool) {
		return harness.Definition{}, "", false
	}
	t.Cleanup(func() { detectCodingHarness = previousDetection })

	for _, operation := range []struct {
		name string
		run  func(string) error
	}{
		{name: "init", run: func(root string) error { return Init(root, false) }},
		{name: "upgrade", run: Upgrade},
	} {
		t.Run(operation.name, func(t *testing.T) {
			root := t.TempDir()
			stateRoot := filepath.Join(root, ".spynel")
			if err := os.Mkdir(stateRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if err := os.Symlink(outside, filepath.Join(stateRoot, "instructions")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			if err := operation.run(root); err == nil || !strings.Contains(err.Error(), "must not be a symbolic link") {
				t.Fatalf("%s error = %v", operation.name, err)
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatalf("%s wrote through symlinked instructions directory: %#v, %v", operation.name, entries, err)
			}
		})
	}
}

func TestWorkspaceTemplatesExcludeRepositoryDeveloperPolicy(t *testing.T) {
	generatedRoot := t.TempDir()
	previousDetection := detectCodingHarness
	detectCodingHarness = func(func(string) (string, error)) (harness.Definition, string, bool) {
		return harness.Definition{}, "", false
	}
	t.Cleanup(func() { detectCodingHarness = previousDetection })
	if err := Init(generatedRoot, false); err != nil {
		t.Fatal(err)
	}

	for _, spec := range files {
		if !strings.HasPrefix(spec.Path, ".spynel/prompts/") && !strings.HasSuffix(spec.Path, "AGENTS.md") {
			continue
		}
		name := strings.TrimPrefix(spec.Template, "templates/")
		embedded, err := Template(name)
		if err != nil {
			t.Fatal(err)
		}
		generated, err := os.ReadFile(filepath.Join(generatedRoot, filepath.FromSlash(spec.Path)))
		if err != nil {
			t.Fatal(err)
		}
		for source, text := range map[string]string{"embedded": string(embedded), "generated": string(generated)} {
			lower := strings.ToLower(text)
			for _, forbidden := range []string{"cache", "gocache", "cold-cache", "shared user cache", "go test", "go build", "build artifact", ".tmp-bin", ".tmp-toolchains", ".tmp-artifacts", ".tmp-tui-captures"} {
				if strings.Contains(lower, forbidden) {
					t.Errorf("%s %s contains repository development guidance %q", source, name, forbidden)
				}
			}
		}
	}

}

func TestFrameworkPromptsEncodeRiskProportionateReview(t *testing.T) {
	chat, err := Template("chat.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"expected value", "minor, localized, easily reversible changes", "explicit request for speed or no review", "Goal derivation alone never decides"} {
		if !strings.Contains(string(chat), required) {
			t.Errorf("communication prompt is missing review policy %q", required)
		}
	}

	goal, err := Template("goal.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"Follow the injected configured task-review mode", "`always` forces `review_required: true`", "allow direct completion", "mandatory goal outcome review"} {
		if !strings.Contains(string(goal), required) {
			t.Errorf("goal prompt is missing review policy %q", required)
		}
	}

	review, err := Template("review.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"If the workspace uses Git", "trivial, localized, low-risk corrections", "rerun the relevant verification", "requires design judgment"} {
		if !strings.Contains(string(review), required) {
			t.Errorf("review prompt is missing correction boundary %q", required)
		}
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
	heartbeatPrompt := filepath.Join(root, ".spynel", "prompts", "heartbeat.md")
	heartbeatCustom := []byte("custom heartbeat prompt\n")
	if err := os.WriteFile(heartbeatPrompt, heartbeatCustom, 0o600); err != nil {
		t.Fatal(err)
	}
	notificationPrompt := filepath.Join(root, ".spynel", "prompts", "notification.md")
	notificationCustom := []byte("custom notification prompt\n")
	if err := os.WriteFile(notificationPrompt, notificationCustom, 0o600); err != nil {
		t.Fatal(err)
	}
	instructionPath := filepath.Join(root, ".spynel", "instructions", "agent-developer.md")
	instructionCustom := []byte("Keep this developer preference.\n")
	if err := os.WriteFile(instructionPath, instructionCustom, 0o600); err != nil {
		t.Fatal(err)
	}
	missingInstruction := filepath.Join(root, ".spynel", "instructions", "agent-reviewer.md")
	if err := os.Remove(missingInstruction); err != nil {
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
	if got, _ := os.ReadFile(heartbeatPrompt); string(got) != string(heartbeatCustom) {
		t.Fatal("upgrade overwrote a user heartbeat prompt")
	}
	if got, _ := os.ReadFile(notificationPrompt); string(got) != string(notificationCustom) {
		t.Fatal("upgrade overwrote a user notification prompt")
	}
	if got, _ := os.ReadFile(instructionPath); string(got) != string(instructionCustom) {
		t.Fatal("upgrade overwrote user persistent instructions")
	}
	if _, err := os.Stat(missingInstruction); err != nil {
		t.Fatalf("upgrade did not restore missing persistent instructions: %v", err)
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
	customExtraPath := filepath.Join(root, ".spynel", "themes", "tokyo-night.yaml")
	if err := os.WriteFile(customExtraPath, []byte("user-owned custom theme\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Init(root, true); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(customPath); err != nil || string(got) != string(custom) {
		t.Fatalf("force init replaced existing stock-named file: %q, %v", got, err)
	}
	if got, err := os.ReadFile(customExtraPath); err != nil || string(got) != "user-owned custom theme\n" {
		t.Fatalf("force init removed custom theme: %q, %v", got, err)
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
		"trusted assistant, not as an orchestration console",
		"Understood—this is being worked on.",
		"Do not expose task or goal filenames",
		"explicitly asks for technical details",
		"/status`, `/jobs`, `/tasks`, `/goals`, `/job info`, `/log`",
		"Telegram and WhatsApp, never include a local-path Markdown link",
		"security implication, destructive effect, or failure",
		"obtain the environment's current UTC time",
		"If a new message arrives while you are responding",
		"Do not silently abandon earlier commitments",
	} {
		if !strings.Contains(prompt, contract) {
			t.Fatalf("communication prompt is missing %q:\n%s", contract, prompt)
		}
	}
	if strings.Contains(prompt, "provide the durable path") {
		t.Fatalf("communication prompt still requires a durable path in routine confirmations:\n%s", prompt)
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
		if strings.Count(prompt, "{{SPYNEL_DOCS_GUIDANCE}}") != 1 {
			t.Fatalf("%s does not have exactly one docs guidance insertion point", name)
		}
		if (name == "task.md" || name == "review.md") && !strings.Contains(prompt, "completion_summary") {
			t.Fatalf("%s does not require a durable notification summary", name)
		}
	}
}

func TestCreationPromptsDefineTaskAndGoalContracts(t *testing.T) {
	for _, test := range []struct {
		name string
		want []string
	}{
		{"create-task.md", []string{"{{USER_MESSAGE}}", "{{TASK_SOURCE}}", "one finite, independently verifiable objective", "[done, failed, waiting, cancelled]", "Do not implement", "Understood—this is being worked on.", "explicitly requested technical details"}},
		{"create-goal.md", []string{"{{USER_MESSAGE}}", "{{GOAL_SOURCE}}", "long-lived, recurring, or multi-round outcome", "success_criteria", "round: 0", "Do not create implementation tasks", "summarizes the intended outcome", "planning or work has begun", "explicitly requested technical details"}},
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
		if strings.Contains(string(data), "durable path") {
			t.Fatalf("%s still requires a durable path in its routine confirmation", test.name)
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
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Harness.Name != "" {
		t.Fatalf("unexpected coding harness selection: %#v", cfg.Harness)
	}
}

func TestUpgradeRemovesRetiredTUILaunchPreference(t *testing.T) {
	root := t.TempDir()
	if err := Init(root, false); err != nil {
		t.Fatal(err)
	}
	path := config.PathForRoot(root)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "\n    tui:\n", "\n    tui:\n        enabled: false\n", 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Upgrade(root); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), "tui:\n        enabled:") || strings.Contains(string(after), "tui:\n    enabled:") {
		t.Fatalf("upgrade retained retired TUI launch preference:\n%s", after)
	}
}
