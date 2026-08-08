package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

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
		".spynel/prompts/create-task.md", ".spynel/prompts/create-goal.md", ".spynel/prompts/task.md", ".spynel/prompts/review.md", ".spynel/prompts/goal-review.md", ".spynel/prompts/heartbeat.md", ".spynel/prompts/notification.md",
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

func TestGoVerificationCacheContracts(t *testing.T) {
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
			for _, forbidden := range []string{"cache", "gocache", "cold-cache", "shared user cache", "go test", "go build", "build artifact", ".spynel-dev"} {
				if strings.Contains(lower, forbidden) {
					t.Errorf("%s %s contains repository development guidance %q", source, name, forbidden)
				}
			}
		}
	}

	rootDOX, err := os.ReadFile("../../AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"shared user cache", "scripts/cold-cache.sh", ".spynel-dev/artifacts/<task-id>/"} {
		if !strings.Contains(string(rootDOX), required) {
			t.Errorf("canonical repository AGENTS.md is missing %q", required)
		}
	}
	scriptDOX, err := os.ReadFile("../../scripts/AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"cold-cache.sh", "dedicated process group", "complete owned process tree"} {
		if !strings.Contains(string(scriptDOX), required) {
			t.Errorf("repository-only cold-cache contract is missing %q", required)
		}
	}
	for path, text := range map[string][]byte{"AGENTS.md": rootDOX, "scripts/AGENTS.md": scriptDOX} {
		if strings.Contains(string(text), "GOCACHE=") {
			t.Errorf("%s contains an ordinary GOCACHE assignment", path)
		}
	}
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"GOCACHE", "cold-cache.sh", ".spynel-dev/artifacts"} {
		if strings.Contains(string(readme), forbidden) {
			t.Errorf("README carries canonical developer policy %q", forbidden)
		}
	}
	ignored, err := os.ReadFile("../../.gitignore")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ignored), "/bin/") || (!strings.Contains(string(ignored), "/.spynel-dev/") && !strings.Contains(string(ignored), ".spynel-dev/")) {
		t.Fatal("verification binary and retained-evidence locations must be ignored")
	}
}

func TestColdCacheHelperCleansCancelledRunOutsideWorkspace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux procfs cancellation contract")
	}
	tempBase := t.TempDir()
	cachePathFile := filepath.Join(tempBase, "cache-path")
	childPIDFile := filepath.Join(tempBase, "child-pid")
	descendantPIDFile := filepath.Join(tempBase, "descendant-pid")
	script := filepath.Join("..", "..", "scripts", "cold-cache.sh")
	command := exec.Command("sh", script, "sh", "-c", `printf '%s' "$GOCACHE" >"$1"; printf '%s' "$$" >"$2"; trap '' TERM; (trap '' TERM; sleep 2; mkdir -p "$GOCACHE/recreated"; while :; do sleep 1; done) & printf '%s' "$!" >"$3"; while :; do sleep 1; done`, "sh", cachePathFile, childPIDFile, descendantPIDFile)
	command.Env = append(os.Environ(), "TMPDIR="+tempBase)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	})

	var cacheDir string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(cachePathFile)
		if err == nil && len(data) != 0 {
			cacheDir = string(data)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if cacheDir == "" {
		t.Fatal("cold-cache child did not publish its cache path")
	}
	if err := exec.Command("kill", "-TERM", strconv.Itoa(command.Process.Pid)).Run(); err != nil {
		t.Fatalf("send TERM to cold-cache helper: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled cold-cache helper unexpectedly succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cold-cache helper did not exit promptly after TERM")
	}
	assertColdCacheRemoved(t, cacheDir, childPIDFile, descendantPIDFile)
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(cacheDir, "recreated")); !os.IsNotExist(err) {
		t.Fatalf("cold-cache descendant recreated the cache after cancellation: %v", err)
	}
}

func TestColdCacheHelperCleansFailedRunOutsideWorkspace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux procfs ownership contract")
	}
	tempBase := t.TempDir()
	script := filepath.Join("..", "..", "scripts", "cold-cache.sh")
	command := exec.Command("sh", script, "sh", "-c", `printf '%s' "$GOCACHE"; exit 7`)
	command.Env = append(os.Environ(), "TMPDIR="+tempBase)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("cold-cache helper unexpectedly succeeded")
	}
	cacheDir := string(output)
	if !strings.HasPrefix(cacheDir, tempBase+string(filepath.Separator)+"spynel-gocache.") {
		t.Fatalf("cold cache path %q is not a unique child of %q", cacheDir, tempBase)
	}
	if _, statErr := os.Stat(cacheDir); !os.IsNotExist(statErr) {
		t.Fatalf("cold cache was not removed after failure: %v", statErr)
	}

	projectRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	rejected := exec.Command("sh", script, "true")
	rejected.Env = append(os.Environ(), "TMPDIR="+projectRoot)
	output, err = rejected.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "must be outside the project workspace") {
		t.Fatalf("cold-cache helper accepted workspace TMPDIR: err=%v output=%q", err, output)
	}
}

func TestColdCacheHelperCleansCompletedRunWithDescendant(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux procfs process-tree contract")
	}
	tempBase := t.TempDir()
	script := filepath.Join("..", "..", "scripts", "cold-cache.sh")
	command := exec.Command("sh", script, "sh", "-c", `printf '%s' "$GOCACHE"; (trap '' TERM; sleep 2; mkdir -p "$GOCACHE/recreated"; while :; do sleep 1; done) &`)
	command.Env = append(os.Environ(), "TMPDIR="+tempBase)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("completed cold-cache diagnostic failed: %v output=%q", err, output)
	}
	cacheDir := string(output)
	if !strings.HasPrefix(cacheDir, tempBase+string(filepath.Separator)+"spynel-gocache.") {
		t.Fatalf("cold cache path %q is not a unique child of %q", cacheDir, tempBase)
	}
	if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
		t.Fatalf("cold cache was not removed after success: %v", err)
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(cacheDir, "recreated")); !os.IsNotExist(err) {
		t.Fatalf("cold-cache descendant recreated the cache after successful command exit: %v", err)
	}
}

func TestColdCacheHelperCleansCompletedRunWithEscapedSession(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux procfs ownership contract")
	}
	tempBase := t.TempDir()
	cachePathFile := filepath.Join(tempBase, "cache-path")
	descendantPIDFile := filepath.Join(tempBase, "descendant-pid")
	script := filepath.Join("..", "..", "scripts", "cold-cache.sh")
	command := exec.Command("sh", script, "sh", "-c", `printf '%s' "$GOCACHE" >"$1"; setsid sh -c 'trap "" TERM; printf "%s" "$$" >"$1"; sleep 2; mkdir -p "$GOCACHE/escaped-recreated"; while :; do sleep 1; done' sh "$2" >/dev/null 2>&1 & while [ ! -s "$2" ]; do sleep 0.01; done`, "sh", cachePathFile, descendantPIDFile)
	command.Env = append(os.Environ(), "TMPDIR="+tempBase)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("completed cold-cache diagnostic failed: %v output=%q", err, output)
	}
	cacheDir, err := os.ReadFile(cachePathFile)
	if err != nil {
		t.Fatal(err)
	}
	assertColdCacheRemoved(t, string(cacheDir), descendantPIDFile)
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(string(cacheDir), "escaped-recreated")); !os.IsNotExist(err) {
		t.Fatalf("escaped cold-cache descendant recreated the cache: %v", err)
	}
}

func assertColdCacheRemoved(t *testing.T, cacheDir string, pidFiles ...string) {
	t.Helper()
	if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
		t.Fatalf("cold cache was not removed: %v", err)
	}
	for _, pidFile := range pidFiles {
		pid, err := os.ReadFile(pidFile)
		if err != nil {
			t.Fatal(err)
		}
		state, err := exec.Command("ps", "-o", "stat=", "-p", string(pid)).Output()
		if err == nil && !strings.HasPrefix(strings.TrimSpace(string(state)), "Z") {
			t.Fatalf("cold-cache process %s survived with state %q", pid, state)
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
		"trusted assistant, not as an orchestration console",
		"Understood—this is being worked on.",
		"Do not expose task or goal filenames",
		"explicitly asks for technical details",
		"/status`, `/jobs`, `/job info`, `/log`",
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
		if (name == "task.md" || name == "review.md") && !strings.Contains(prompt, "notification_summary") {
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
	cfg, err := config.Load(filepath.Join(root, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Harness.Name != "" {
		t.Fatalf("unexpected coding harness selection: %#v", cfg.Harness)
	}
}
