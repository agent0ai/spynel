package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindReportsUninitializedDirectory(t *testing.T) {
	_, err := Find(t.TempDir())
	if !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Find error = %v, want ErrNotInitialized", err)
	}
}

func TestFindDiscoversCanonicalConfigFromChildDirectory(t *testing.T) {
	root := t.TempDir()
	path := writeTestConfig(t, root, []byte("version: 1\n"))
	child := filepath.Join(root, "nested", "project")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	found, err := Find(child)
	if err != nil {
		t.Fatal(err)
	}
	if found != path {
		t.Fatalf("Find() = %q, want %q", found, path)
	}
}

func TestLoadMigratesLegacyRootConfig(t *testing.T) {
	root := t.TempDir()
	legacyPath := filepath.Join(root, LegacyFileName)
	data := []byte("version: 1\nworkspace:\n  state_dir: .spynel\n  history_char_limit: 321\n")
	if err := os.WriteFile(legacyPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	canonicalPath := PathForRoot(root)
	if cfg.Path != canonicalPath || cfg.Root != root || cfg.StatePath() != filepath.Join(root, StateDirectoryName) {
		t.Fatalf("migrated paths = path %q root %q state %q", cfg.Path, cfg.Root, cfg.StatePath())
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy config still exists: %v", err)
	}
	saved, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "state_dir") {
		t.Fatalf("canonical config retained workspace.state_dir:\n%s", saved)
	}
	if cfg.Workspace.HistoryCharLimit != 321 {
		t.Fatalf("migrated workspace values = %#v", cfg.Workspace)
	}
}

func TestLoadCanonicalPathMigratesLegacyRootConfig(t *testing.T) {
	root := t.TempDir()
	legacyPath := filepath.Join(root, LegacyFileName)
	if err := os.WriteFile(legacyPath, []byte("version: 1\nworkspace:\n  state_dir: .spynel\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Path != PathForRoot(root) || cfg.Root != root {
		t.Fatalf("migrated config = path %q root %q", cfg.Path, cfg.Root)
	}
}

func TestLoadRejectsLegacyCustomStateDirectory(t *testing.T) {
	root := t.TempDir()
	legacyPath := filepath.Join(root, LegacyFileName)
	if err := os.WriteFile(legacyPath, []byte("version: 1\nworkspace:\n  state_dir: private-state\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(legacyPath); err == nil || !strings.Contains(err.Error(), "cannot automatically migrate") {
		t.Fatalf("custom legacy state migration error = %v", err)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("rejected migration changed the legacy config: %v", err)
	}
	if _, err := os.Stat(PathForRoot(root)); !os.IsNotExist(err) {
		t.Fatalf("rejected migration created a canonical config: %v", err)
	}
}

func writeTestConfig(t *testing.T, root string, data []byte) string {
	t.Helper()
	path := PathForRoot(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaultIsValidAndRoutesAreExtensible(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Orchestrator.Routes) != 2 {
		t.Fatalf("expected task and goal routes, got %d", len(cfg.Orchestrator.Routes))
	}
	if cfg.Harness.Name != "" {
		t.Fatalf("default coding harness should await detection, got %q", cfg.Harness.Name)
	}
	if cfg.Harness.Sandbox != "danger-full-access" {
		t.Fatalf("default coding harness should be unrestricted, got %q", cfg.Harness.Sandbox)
	}
	if cfg.Harness.ChatAgentPrefix != "" || cfg.Harness.DeveloperAgentPrefix != "/goal" || cfg.Harness.ReviewerAgentPrefix != "/goal" || cfg.Harness.HeartbeatAgentPrefix != "/goal" || cfg.Harness.Reviews != TaskReviewsSkipTrivial {
		t.Fatalf("unexpected harness agent defaults: %#v", cfg.Harness)
	}
	if !cfg.Speech.Enabled || cfg.Speech.Language != "en" || cfg.Speech.NumThreads != 2 {
		t.Fatalf("unexpected speech defaults: %#v", cfg.Speech)
	}
	if cfg.Channels.TUI.Theme != "spynel" {
		t.Fatalf("unexpected default TUI theme: %#v", cfg.Channels.TUI)
	}
	if cfg.Orchestrator.SemanticHeartbeatMinutes != 15 {
		t.Fatalf("semantic heartbeat default = %d, want 15", cfg.Orchestrator.SemanticHeartbeatMinutes)
	}
	if got := strings.Join(cfg.Orchestrator.Routes[0].AllowedNext, ","); got != "todo,working,review,reviewing,waiting,done,failed,cancelled" {
		t.Fatalf("task workflow statuses = %q", got)
	}
	goals := cfg.Orchestrator.Routes[1]
	if filepath.Base(goals.Source) != "proposed" || filepath.Base(goals.Working) != "planning" || filepath.Base(goals.ReviewPrompt) != "goal-review.md" || strings.Join(goals.AllowedNext, ",") != "proposed,planning,active,review,reviewing,waiting,done,abandoned" {
		t.Fatalf("goal workflow defaults = %#v", goals)
	}
}

func TestHarnessAgentPrefixesAndReviewModeValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		prefix string
		prompt string
		want   string
	}{
		{name: "command", prefix: "/goal", prompt: "Do the work", want: "/goal Do the work"},
		{name: "outer whitespace", prefix: "  /goal   ", prompt: "Do the work", want: "/goal Do the work"},
		{name: "multi-token", prefix: "  /goal keep   this  ", prompt: "Do the work", want: "/goal keep   this Do the work"},
		{name: "empty prefix", prefix: "", prompt: "prompt", want: "prompt"},
		{name: "whitespace-only prefix", prefix: " \t ", prompt: "prompt", want: "prompt"},
		{name: "empty prompt", prefix: " /goal ", prompt: "", want: "/goal "},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := PrependAgentPrefix(test.prefix, test.prompt); got != test.want {
				t.Fatalf("PrependAgentPrefix(%q, %q) = %q, want %q", test.prefix, test.prompt, got, test.want)
			}
		})
	}
	for _, mode := range []string{TaskReviewsSkipTrivial, TaskReviewsAlways, TaskReviewsNever} {
		cfg := Default()
		cfg.Harness.Reviews = mode
		if err := cfg.Validate(); err != nil {
			t.Fatalf("review mode %q: %v", mode, err)
		}
	}
	cfg := Default()
	cfg.Harness.Reviews = "sometimes"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "harness.reviews") {
		t.Fatalf("invalid review mode validation = %v", err)
	}
	cfg = Default()
	cfg.Harness.DeveloperAgentPrefix = "/goal\nunsafe"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "developer_agent_prefix") {
		t.Fatalf("multiline prefix validation = %v", err)
	}
}

func TestMinimalConfigRetainsHarnessAgentDefaults(t *testing.T) {
	root := t.TempDir()
	path := writeTestConfig(t, root, []byte("version: 1\n"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Harness.DeveloperAgentPrefix != "/goal" || cfg.Harness.ReviewerAgentPrefix != "/goal" || cfg.Harness.HeartbeatAgentPrefix != "/goal" || cfg.Harness.Reviews != TaskReviewsSkipTrivial {
		t.Fatalf("legacy defaults were not retained: %#v", cfg.Harness)
	}
}

func TestSemanticHeartbeatValidationSupportsExplicitDisable(t *testing.T) {
	cfg := Default()
	cfg.Orchestrator.SemanticHeartbeatMinutes = 0
	if err := cfg.Validate(); err != nil {
		t.Fatalf("disabled semantic heartbeat was rejected: %v", err)
	}
	for _, invalid := range []int{-1, 1, 4, 1441} {
		cfg.Orchestrator.SemanticHeartbeatMinutes = invalid
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "semantic_heartbeat_minutes") {
			t.Fatalf("invalid semantic heartbeat %d produced %v", invalid, err)
		}
	}
}

func TestLoadUpgradesLegacyGoalRouteToTypedLifecycle(t *testing.T) {
	root := t.TempDir()
	data := []byte("version: 1\norchestrator:\n  enabled: true\n  interval_seconds: 10\n  max_parallel: 1\n  routes:\n    - name: goals\n      source: .spynel/goals/active\n      working: .spynel/goals/working\n      prompt: .spynel/prompts/goal.md\n      recovery_prompt: .spynel/prompts/recovery.md\n      stale_after: 2h\n      allowed_next: [active, working, waiting, done]\n")
	path := writeTestConfig(t, root, data)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	route := cfg.Orchestrator.Routes[0]
	if filepath.Base(route.Source) != "proposed" || filepath.Base(route.Working) != "planning" || filepath.Base(route.ReviewPrompt) != "goal-review.md" || !strings.Contains(strings.Join(route.AllowedNext, ","), "reviewing") {
		t.Fatalf("legacy goal route was not upgraded: %#v", route)
	}
}

func TestSpeechLanguagesContainEveryParakeetLanguage(t *testing.T) {
	want := []string{"auto", "bg", "hr", "cs", "da", "nl", "en", "et", "fi", "fr", "de", "el", "hu", "it", "lv", "lt", "mt", "pl", "pt", "ro", "sk", "sl", "es", "sv", "ru", "uk"}
	for _, language := range want {
		if !IsSpeechLanguage(language) {
			t.Fatalf("supported language %q is missing", language)
		}
	}
	if IsSpeechLanguage("ja") {
		t.Fatal("unsupported language was accepted")
	}
}

func TestTelegramWhitelistIsRequiredWhenEnabled(t *testing.T) {
	cfg := Default()
	cfg.Channels.Telegram.Enabled = true
	for _, invalid := range [][]string{nil, {}, {"  "}, {"@"}, {"..."}, {"-7"}, {"bad user"}, {strings.Repeat("a", 33)}} {
		cfg.Channels.Telegram.AllowedUsers = invalid
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "allowed_users requires at least one user") {
			t.Fatalf("enabled Telegram accepted invalid whitelist %#v: %v", invalid, err)
		}
	}
	cfg.Channels.Telegram.AllowedUsers = []string{"123456789"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("enabled Telegram rejected a configured whitelist: %v", err)
	}
}

func TestTelegramWebhookRequiresSecretWhenEnabled(t *testing.T) {
	cfg := Default()
	cfg.Channels.Telegram.Enabled = true
	cfg.Channels.Telegram.Mode = "webhook"
	cfg.Channels.Telegram.WebhookURL = "https://public.example"
	cfg.Channels.Telegram.AllowedUsers = []string{"123456789"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "webhook_secret is required") {
		t.Fatalf("enabled Telegram webhook accepted an empty secret: %v", err)
	}
	cfg.Channels.Telegram.WebhookSecret = "verification-secret"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("enabled Telegram webhook rejected a configured secret: %v", err)
	}
}

func TestWhatsAppWhitelistIsRequiredWhenEnabled(t *testing.T) {
	cfg := Default()
	cfg.Channels.WhatsApp.Enabled = true
	for _, invalid := range [][]string{nil, {}, {"  "}, {" + "}, {"phone"}, {"12x34"}, {"1234567890123456"}} {
		cfg.Channels.WhatsApp.AllowedNumbers = invalid
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "allowed_numbers requires at least one number") {
			t.Fatalf("enabled WhatsApp accepted invalid whitelist %#v: %v", invalid, err)
		}
	}
	cfg.Channels.WhatsApp.AllowedNumbers = []string{"+1 (555) 123-4567"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("enabled WhatsApp rejected a configured whitelist: %v", err)
	}
}

func TestNormalizeWhatsAppNumber(t *testing.T) {
	tests := map[string]string{
		"+420 123 456 789":  "420123456789",
		"00420-123-456-789": "420123456789",
		"(420) 123.456.789": "420123456789",
		"0123 456 789":      "0123456789",
		"phone":             "",
	}
	for input, want := range tests {
		if got := NormalizeWhatsAppNumber(input); got != want {
			t.Errorf("NormalizeWhatsAppNumber(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestLoadMergesUserValuesWithDefaults(t *testing.T) {
	root := t.TempDir()
	data := []byte("version: 1\nworkspace:\n  history_char_limit: 321\nrecipient:\n  name: codex\nchannels:\n  whatsapp:\n    mode: dedicated\n")
	path := writeTestConfig(t, root, data)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workspace.HistoryCharLimit != 321 || cfg.Workspace.HistoryMaxMessages != 50 {
		t.Fatalf("defaults were not retained: %#v", cfg.Workspace)
	}
	if cfg.Harness.Sandbox != "danger-full-access" {
		t.Fatalf("missing sandbox did not inherit the unrestricted default: %#v", cfg.Harness)
	}
	if cfg.Channels.WhatsApp.Mode != "dedicated" || cfg.Channels.Telegram.PollTimeoutSec != 30 {
		t.Fatalf("nested defaults were not retained: %#v", cfg.Channels)
	}
	if cfg.Channels.TUI.Theme != "spynel" {
		t.Fatalf("missing TUI theme did not inherit the default: %#v", cfg.Channels.TUI)
	}
	if cfg.Resolve("relative") != filepath.Join(root, "relative") {
		t.Fatalf("relative path did not resolve against config root")
	}
}

func TestLoadIgnoresLegacyWhisperFields(t *testing.T) {
	root := t.TempDir()
	data := []byte("version: 1\nspeech:\n  enabled: true\n  command: whisper-cli\n  ffmpeg_command: ffmpeg\n  model: small\n  model_path: old.bin\n  language: auto\n")
	path := writeTestConfig(t, root, data)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Speech.Language != "auto" || cfg.Speech.NumThreads != 2 || cfg.Speech.ModelDir != "" {
		t.Fatalf("legacy speech config did not migrate to Parakeet defaults: %#v", cfg.Speech)
	}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(saved)
	for _, obsolete := range []string{"command:", "ffmpeg_command:", "model:", "model_path:"} {
		if strings.Contains(text, obsolete) {
			t.Fatalf("saved Parakeet config retained %q:\n%s", obsolete, text)
		}
	}
}

func TestHarnessSandboxValidationAcceptsCanonicalModes(t *testing.T) {
	for _, mode := range []string{"read-only", "workspace-write", "danger-full-access"} {
		cfg := Default()
		cfg.Harness.Sandbox = mode
		if err := cfg.Validate(); err != nil {
			t.Fatalf("sandbox %q: %v", mode, err)
		}
	}
	cfg := Default()
	cfg.Harness.Sandbox = "unknown"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "harness.sandbox") {
		t.Fatalf("invalid sandbox validation = %v", err)
	}
}

func TestCustomACPRequiresCommandAndPreservesShellFreeArguments(t *testing.T) {
	cfg := Default()
	cfg.Harness.Name = "acp"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "harness.acp_command") {
		t.Fatalf("custom ACP without command = %v", err)
	}
	cfg.Harness.ACPCommand = "/tools/custom agent"
	cfg.Harness.ACPArgs = []string{"--stdio", "value with spaces"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid custom ACP rejected: %v", err)
	}
	arguments := cfg.HarnessArgs()
	if len(arguments) != 2 || arguments[1] != "value with spaces" {
		t.Fatalf("custom ACP args = %#v", arguments)
	}
	arguments[0] = "changed"
	if cfg.Harness.ACPArgs[0] != "--stdio" {
		t.Fatal("custom ACP arguments were returned by reference")
	}
	cfg.Harness.ACPArgs = []string{"bad\x00argument"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Fatalf("custom ACP NUL argument = %v", err)
	}
}

func TestLoadNormalizesACPAndHarnessAliases(t *testing.T) {
	root := t.TempDir()
	data := []byte("version: 1\nharness:\n  name: custom-acp\n  sandbox: danger-full-access\n  acp_command: fixture\n")
	path := writeTestConfig(t, root, data)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Harness.Name != "acp" {
		t.Fatalf("custom ACP alias normalized to %q", cfg.Harness.Name)
	}
}

func TestLoadMigratesLegacyRecipientSectionAndSaveUsesHarness(t *testing.T) {
	root := t.TempDir()
	data := []byte("version: 1\nrecipient:\n  name: claude-code\n  command: /old/manual/path\n  model: sonnet\n")
	path := writeTestConfig(t, root, data)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Harness.Name != "claude-code" || cfg.Harness.Model != "sonnet" {
		t.Fatalf("legacy harness values were not migrated: %#v", cfg.Harness)
	}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(saved)
	if !strings.Contains(text, "harness:\n") || strings.Contains(text, "recipient:\n") || strings.Contains(text, "manual/path") {
		t.Fatalf("saved config did not use the simplified harness contract:\n%s", text)
	}
}

func TestStorePersistsValidatedUpdatesAndPublishesSnapshot(t *testing.T) {
	root := t.TempDir()
	path := PathForRoot(root)
	cfg := Default()
	cfg.Path = path
	cfg.Root = root
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	store := NewStore(cfg)
	updated, err := store.Update(func(next *Config) error {
		next.Workspace.HistoryMaxMessages = 25
		next.Workspace.HistoryCharLimit = 9000
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Workspace.HistoryMaxMessages != 25 || store.Snapshot().Workspace.HistoryCharLimit != 9000 {
		t.Fatalf("unexpected stored config: %#v", store.Snapshot().Workspace)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Workspace.HistoryMaxMessages != 25 || reloaded.Workspace.HistoryCharLimit != 9000 {
		t.Fatalf("update was not persisted: %#v", reloaded.Workspace)
	}
	select {
	case event := <-store.Updates():
		if event.Workspace.HistoryMaxMessages != 25 {
			t.Fatalf("unexpected update event: %#v", event.Workspace)
		}
	default:
		t.Fatal("configuration update was not published")
	}
	if _, err := store.Update(func(next *Config) error {
		next.Workspace.HistoryMaxMessages = -1
		return nil
	}); err == nil {
		t.Fatal("invalid configuration update succeeded")
	}
	if store.Snapshot().Workspace.HistoryMaxMessages != 25 {
		t.Fatal("invalid update changed the in-memory snapshot")
	}
}

func TestLegacyNotificationWorkflowConfigurationIsDroppedOnSave(t *testing.T) {
	root := t.TempDir()
	path := writeTestConfig(t, root, []byte("version: 1\nnotifications:\n  contact_bindings:\n    - principal: owner\n      contacts: [tui/local]\n  quiet_hours:\n    enabled: true\n    start: 22:00\n    end: 07:00\n"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "\nnotifications:") || strings.Contains(string(data), "contact_bindings") || strings.Contains(string(data), "quiet_hours") {
		t.Fatalf("retired notification workflow configuration survived save:\n%s", data)
	}
}
