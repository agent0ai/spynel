package config

import (
	"os"
	"strings"
	"testing"
)

func TestNotificationDeadlineSettingPersistsAndRejectsInvalidValues(t *testing.T) {
	cfg := Default()
	cfg.Root = t.TempDir()
	cfg.Path = PathForRoot(cfg.Root)
	if setting, ok := SettingByKey(cfg, "orchestrator.notification_timeout_seconds"); !ok || setting.Value != "120" || setting.Restart || !setting.Advanced {
		t.Fatalf("default live setting: %+v, %t", setting, ok)
	}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	store := NewStore(cfg)
	for _, value := range []string{"30", "600", "1800"} {
		updated, err := store.Update(func(next *Config) error {
			_, err := SetSetting(next, "orchestrator.notification_timeout_seconds", value)
			return err
		})
		if err != nil {
			t.Fatal(value, err)
		}
		saved, err := Load(cfg.Path)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []Config{updated, saved, store.Snapshot()} {
			s, _ := SettingByKey(c, "orchestrator.notification_timeout_seconds")
			if s.Value != value {
				t.Fatal("stale persisted selection", s.Value, value)
			}
		}
	}
	before, _ := os.ReadFile(cfg.Path)
	for _, value := range []string{"0", "29", "1801", "3.5", "9999999999999999999999"} {
		if _, err := store.Update(func(next *Config) error {
			_, err := SetSetting(next, "orchestrator.notification_timeout_seconds", value)
			return err
		}); err == nil {
			t.Fatal("invalid deadline accepted", value)
		}
		after, _ := os.ReadFile(cfg.Path)
		if string(after) != string(before) {
			t.Fatal("invalid deadline changed canonical configuration")
		}
	}
	// Omission defaults without rewriting the input; recognized bad YAML types fail closed.
	for _, value := range []string{"", "true", "1.5", "0", "1801"} {
		raw := "version: 1\n"
		if value != "" {
			raw += "orchestrator:\n  notification_timeout_seconds: " + value + "\n"
		}
		if err := os.WriteFile(cfg.Path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		loaded, err := Load(cfg.Path)
		if value != "" {
			if err == nil {
				t.Fatal("bad YAML accepted", value)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		s, _ := SettingByKey(loaded, "orchestrator.notification_timeout_seconds")
		if s.Value != "120" {
			t.Fatal("omitted deadline", s.Value)
		}
		after, _ := os.ReadFile(cfg.Path)
		if strings.TrimSpace(string(after)) != "version: 1" {
			t.Fatal("load rewrote configuration")
		}
	}
}
