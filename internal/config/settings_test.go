package config

import (
	"strings"
	"testing"
)

func TestHeartbeatSettingsAreLiveWhileOtherOrchestratorControlsRequireRestart(t *testing.T) {
	cfg := Default()
	for _, key := range []string{"orchestrator.enabled", "orchestrator.semantic_heartbeat_minutes"} {
		setting, ok := SettingByKey(cfg, key)
		if !ok || setting.Restart {
			t.Fatalf("live setting %q = %#v, present %t", key, setting, ok)
		}
	}
	for _, key := range []string{"orchestrator.interval_seconds", "orchestrator.max_parallel"} {
		setting, ok := SettingByKey(cfg, key)
		if !ok || !setting.Restart {
			t.Fatalf("restart-bound setting %q = %#v, present %t", key, setting, ok)
		}
	}
}

func TestSetSettingParsesSharedCommandValues(t *testing.T) {
	cfg := Default()
	for _, test := range []struct {
		key   string
		value string
	}{
		{"workspace.history_max_messages", "24"},
		{"harness.name", "claude"},
		{"harness.sandbox", "unrestricted"},
		{"channels.tui.theme", "catppuccin-latte"},
		{"channels.telegram.allowed_users", "@one, 42"},
		{"channels.whatsapp.mode", "dedicated"},
		{"speech.language", "fr"},
		{"orchestrator.interval_seconds", "15"},
		{"orchestrator.semantic_heartbeat_minutes", "30"},
		{"extensions.hook_timeout", "45s"},
	} {
		if _, err := SetSetting(&cfg, test.key, test.value); err != nil {
			t.Fatalf("set %s: %v", test.key, err)
		}
	}
	if cfg.Workspace.HistoryMaxMessages != 24 || cfg.Harness.Name != "claude-code" || cfg.Harness.Sandbox != "danger-full-access" || cfg.Channels.TUI.Theme != "catppuccin-latte" || len(cfg.Channels.Telegram.AllowedUsers) != 2 || cfg.Channels.WhatsApp.Mode != "dedicated" || cfg.Speech.Language != "fr" || cfg.Orchestrator.IntervalSec != 15 || cfg.Orchestrator.SemanticHeartbeatMinutes != 30 || cfg.Extensions.HookTimeout != "45s" {
		t.Fatalf("unexpected config after settings: %#v", cfg)
	}
}

func TestSpeechSettingsExposeParakeetLanguagesWithoutModelSize(t *testing.T) {
	cfg := Default()
	language, ok := SettingByKey(cfg, "speech.language")
	if !ok {
		t.Fatal("speech.language setting is missing")
	}
	if strings.Join(language.Choices, ",") != strings.Join(SpeechLanguages(), ",") {
		t.Fatalf("speech language choices = %#v", language.Choices)
	}
	for _, removed := range []string{"speech.model", "speech.command", "speech.ffmpeg_command", "speech.model_path"} {
		if _, ok := SettingByKey(cfg, removed); ok {
			t.Fatalf("obsolete Whisper setting %q is still exposed", removed)
		}
	}
}

func TestChannelSettingsPutEssentialsFirstAndRemovePromptOverrides(t *testing.T) {
	cfg := Default()
	wantEssential := map[string][]string{
		"telegram": {"channels.telegram.token", "channels.telegram.allowed_users", "channels.telegram.enabled"},
		"whatsapp": {"channels.whatsapp.mode", "channels.whatsapp.allowed_numbers", "channels.whatsapp.enabled"},
	}
	for section, want := range wantEssential {
		var essential []string
		advancedStarted := false
		for _, setting := range Settings(cfg) {
			if setting.Section != section {
				continue
			}
			if setting.Advanced {
				advancedStarted = true
				continue
			}
			if advancedStarted {
				t.Fatalf("%s essential setting %q follows advanced settings", section, setting.Key)
			}
			essential = append(essential, setting.Key)
		}
		if len(essential) != len(want) {
			t.Fatalf("%s essentials = %#v, want %#v", section, essential, want)
		}
		for index := range want {
			if essential[index] != want[index] {
				t.Fatalf("%s essentials = %#v, want %#v", section, essential, want)
			}
		}
	}
	for _, removed := range []string{
		"channels.telegram.default_project",
		"channels.telegram.user_projects",
		"channels.telegram.agent_instructions",
		"channels.whatsapp.project",
		"channels.whatsapp.agent_instructions",
	} {
		if _, ok := SettingByKey(cfg, removed); ok {
			t.Fatalf("removed channel setting %q is still exposed", removed)
		}
		if _, err := SetSetting(&cfg, removed, "stale"); err == nil {
			t.Fatalf("removed channel setting %q can still be changed", removed)
		}
	}
}

func TestWhatsAppAllowedNumbersAreDescribedAsRequired(t *testing.T) {
	setting, ok := SettingByKey(Default(), "channels.whatsapp.allowed_numbers")
	if !ok {
		t.Fatal("WhatsApp allowed-number setting is missing")
	}
	if !strings.Contains(strings.ToLower(setting.Description), "required") || strings.Contains(strings.ToLower(setting.Description), "empty allows") {
		t.Fatalf("WhatsApp allowed-number description = %q", setting.Description)
	}
}

func TestMainSettingsExposeOnlySimpleHarnessChoicesAndPutAdvancedLast(t *testing.T) {
	cfg := Default()
	var essential []string
	advancedStarted := false
	for _, setting := range Settings(cfg) {
		if setting.Section != "config" {
			continue
		}
		if setting.Advanced {
			advancedStarted = true
			continue
		}
		if advancedStarted {
			t.Fatalf("essential setting %q follows advanced settings", setting.Key)
		}
		essential = append(essential, setting.Key)
	}
	want := []string{"harness.sandbox", "workspace.history_max_messages", "workspace.history_char_limit", "startup.enabled"}
	if len(essential) != len(want) {
		t.Fatalf("main essentials = %#v, want %#v", essential, want)
	}
	for index := range want {
		if essential[index] != want[index] {
			t.Fatalf("main essentials = %#v, want %#v", essential, want)
		}
	}
	for _, key := range []string{
		"harness.command", "harness.cwd", "harness.effort", "harness.approval_policy", "harness.network",
		"recipient.command", "recipient.cwd", "recipient.effort", "recipient.approval_policy", "recipient.sandbox", "recipient.network",
	} {
		if _, ok := SettingByKey(cfg, key); ok {
			t.Fatalf("implementation setting %q is still exposed", key)
		}
		if _, err := SetSetting(&cfg, key, "unsafe"); err == nil {
			t.Fatalf("implementation setting %q is still configurable", key)
		}
	}
	for _, key := range []string{"harness.name", "harness.model"} {
		setting, ok := SettingByKey(cfg, key)
		if !ok || setting.Section != "harness" {
			t.Fatalf("simple harness setting %q = %#v, %t", key, setting, ok)
		}
	}
	sandbox, ok := SettingByKey(cfg, "harness.sandbox")
	if !ok || sandbox.Section != "config" || len(sandbox.Choices) != 3 || sandbox.Value != "danger-full-access" {
		t.Fatalf("sandbox setting = %#v, %t", sandbox, ok)
	}
}

func TestSetSettingRejectsInvalidAndMasksSecrets(t *testing.T) {
	cfg := Default()
	if _, err := SetSetting(&cfg, "channels.whatsapp.poll_interval_seconds", "1"); err == nil {
		t.Fatal("invalid polling interval succeeded")
	}
	if _, err := SetSetting(&cfg, "not.real", "value"); err == nil {
		t.Fatal("unknown setting succeeded")
	}
	setting, err := SetSetting(&cfg, "channels.telegram.token", "123:secret")
	if err != nil {
		t.Fatal(err)
	}
	if !setting.Secret || setting.Value != "set" || !IsSecretSetting(setting.Key) {
		t.Fatalf("secret setting was exposed: %#v", setting)
	}
}

func TestSetSettingsValidatesRelatedFormFieldsTogether(t *testing.T) {
	cfg := Default()
	cfg.Channels.Telegram.Enabled = true
	cfg.Channels.Telegram.AllowedUsers = []string{"123456789"}
	changed, err := SetSettings(&cfg, map[string]string{
		"channels.telegram.mode":           "webhook",
		"channels.telegram.webhook_url":    "https://spynel.example",
		"channels.telegram.webhook_secret": "verification-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 3 || cfg.Channels.Telegram.Mode != "webhook" || cfg.Channels.Telegram.WebhookURL == "" || cfg.Channels.Telegram.WebhookSecret == "" {
		t.Fatalf("related settings were not applied: %#v, %#v", changed, cfg.Channels.Telegram)
	}
}

func TestTelegramWhitelistAndEnabledStateValidateAtomically(t *testing.T) {
	cfg := Default()
	if _, err := SetSetting(&cfg, "channels.telegram.enabled", "on"); err == nil || !strings.Contains(err.Error(), "allowed_users requires at least one user") {
		t.Fatalf("Telegram enabled without a whitelist: %v", err)
	}
	if cfg.Channels.Telegram.Enabled {
		t.Fatal("failed Telegram enable changed the configuration")
	}
	if _, err := SetSettings(&cfg, map[string]string{
		"channels.telegram.allowed_users": "123456789",
		"channels.telegram.enabled":       "on",
	}); err != nil {
		t.Fatalf("Telegram whitelist and enable transaction failed: %v", err)
	}
	if _, err := SetSetting(&cfg, "channels.telegram.allowed_users", ""); err == nil || !strings.Contains(err.Error(), "allowed_users requires at least one user") {
		t.Fatalf("enabled Telegram whitelist was cleared: %v", err)
	}
	if len(cfg.Channels.Telegram.AllowedUsers) != 1 {
		t.Fatal("failed whitelist clear changed the configuration")
	}
}

func TestWhatsAppWhitelistAndEnabledStateValidateAtomically(t *testing.T) {
	cfg := Default()
	if _, err := SetSetting(&cfg, "channels.whatsapp.enabled", "on"); err == nil || !strings.Contains(err.Error(), "allowed_numbers requires at least one number") {
		t.Fatalf("WhatsApp enabled without an allow-list: %v", err)
	}
	if cfg.Channels.WhatsApp.Enabled {
		t.Fatal("failed WhatsApp enable changed the configuration")
	}
	if _, err := SetSettings(&cfg, map[string]string{
		"channels.whatsapp.allowed_numbers": "+1 (555) 123-4567",
		"channels.whatsapp.enabled":         "on",
	}); err != nil {
		t.Fatalf("WhatsApp allow-list and enable transaction failed: %v", err)
	}
	if _, err := SetSetting(&cfg, "channels.whatsapp.allowed_numbers", ""); err == nil || !strings.Contains(err.Error(), "allowed_numbers requires at least one number") {
		t.Fatalf("enabled WhatsApp allow-list was cleared: %v", err)
	}
	if len(cfg.Channels.WhatsApp.AllowedNumbers) != 1 || cfg.Channels.WhatsApp.AllowedNumbers[0] != "+1 (555) 123-4567" {
		t.Fatalf("failed allow-list clear changed the configuration: %#v", cfg.Channels.WhatsApp.AllowedNumbers)
	}
}
