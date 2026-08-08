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

func TestNotificationContactBindingsRequireExplicitUniqueOrigins(t *testing.T) {
	cfg := Default()
	cfg.Notifications.ContactBindings = []ContactBinding{{Principal: "owner", Contacts: []string{"tui/local", "telegram/TG-7"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid binding rejected: %v", err)
	}
	cfg.Notifications.ContactBindings = []ContactBinding{
		{Principal: "owner", Contacts: []string{"telegram/TG-7"}},
		{Principal: "other", Contacts: []string{"telegram/TG-7"}},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "more than one principal") {
		t.Fatalf("ambiguous binding produced %v", err)
	}
}

func TestLoadUpgradesLegacyGoalRouteToTypedLifecycle(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, FileName)
	data := []byte("version: 1\norchestrator:\n  enabled: true\n  interval_seconds: 10\n  max_parallel: 1\n  routes:\n    - name: goals\n      source: .spynel/goals/active\n      working: .spynel/goals/working\n      prompt: .spynel/prompts/goal.md\n      recovery_prompt: .spynel/prompts/recovery.md\n      stale_after: 2h\n      allowed_next: [active, working, waiting, done]\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
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
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "allowed_users requires at least one user") {
		t.Fatalf("enabled Telegram accepted an empty whitelist: %v", err)
	}
	cfg.Channels.Telegram.AllowedUsers = []string{"  "}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "allowed_users requires at least one user") {
		t.Fatalf("enabled Telegram accepted a whitespace-only whitelist: %v", err)
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
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "allowed_numbers requires at least one number") {
		t.Fatalf("enabled WhatsApp accepted an empty whitelist: %v", err)
	}
	cfg.Channels.WhatsApp.AllowedNumbers = []string{" + "}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "allowed_numbers requires at least one number") {
		t.Fatalf("enabled WhatsApp accepted a whitelist without a number: %v", err)
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
	path := filepath.Join(root, FileName)
	data := []byte("version: 1\nworkspace:\n  history_char_limit: 321\nrecipient:\n  name: codex\nchannels:\n  whatsapp:\n    mode: dedicated\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workspace.StateDir != ".spynel" || cfg.Workspace.HistoryCharLimit != 321 || cfg.Workspace.HistoryMaxMessages != 50 {
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
	path := filepath.Join(root, FileName)
	data := []byte("version: 1\nspeech:\n  enabled: true\n  command: whisper-cli\n  ffmpeg_command: ffmpeg\n  model: small\n  model_path: old.bin\n  language: auto\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
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

func TestLoadMigratesLegacyRecipientSectionAndSaveUsesHarness(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, FileName)
	data := []byte("version: 1\nrecipient:\n  name: claude-code\n  command: /old/manual/path\n  model: sonnet\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
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
	path := filepath.Join(root, FileName)
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

func TestNotificationQuietHoursValidation(t *testing.T) {
	cfg := Default()
	cfg.Notifications.QuietHours = QuietHours{Enabled: true, Start: "22:00", End: "07:00"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid cross-midnight quiet hours: %v", err)
	}
	for _, policy := range []QuietHours{
		{Enabled: true, Start: "night", End: "07:00"},
		{Enabled: true, Start: "22:00", End: "22:00"},
	} {
		cfg.Notifications.QuietHours = policy
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "quiet_hours") {
			t.Fatalf("invalid quiet hours %#v: %v", policy, err)
		}
	}
}
