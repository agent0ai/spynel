package config

import (
	"bytes"
	"os"
	"testing"
)

func TestSpeechIdleTimeoutDefaultsRoundTripAndValidation(t *testing.T) {
	path := writeTestConfig(t, t.TempDir(), []byte("version: 1\n"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Speech.IdleTimeoutSec != 120 || Default().Speech.IdleTimeoutSec != 120 {
		t.Fatal("missing idle timeout did not default to 120 seconds")
	}
	setting, ok := SettingByKey(cfg, "speech.idle_timeout_seconds")
	if !ok || !setting.Advanced || setting.Restart || setting.Section != "config" || setting.Value != "120" {
		t.Fatalf("setting = %#v", setting)
	}
	store := NewStore(cfg)
	for _, valid := range []string{"1", "300", "86400"} {
		if _, err := store.Update(func(next *Config) error { _, err := SetSetting(next, setting.Key, valid); return err }); err != nil {
			t.Fatal(err)
		}
		reloaded, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := SettingByKey(reloaded, setting.Key)
		if got.Value != valid || reloaded.Speech != store.Snapshot().Speech {
			t.Fatalf("round trip = %#v", reloaded.Speech)
		}
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"0", "-1", "86401", "9223372036854775807", "18446744073709551616", "1.5", "2m", "false"} {
		if _, err := store.Update(func(next *Config) error { _, err := SetSetting(next, setting.Key, invalid); return err }); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) || store.Snapshot().Speech.IdleTimeoutSec != 86400 {
			t.Fatalf("invalid %q changed disk or runtime", invalid)
		}
		invalidPath := writeTestConfig(t, t.TempDir(), []byte("speech:\n  idle_timeout_seconds: "+invalid+"\n"))
		if _, err := Load(invalidPath); err == nil {
			t.Fatalf("YAML accepted %q", invalid)
		}
	}
}

func TestSpeechIdleTimeoutYAMLRejectsFractionalAliasesAndMerges(t *testing.T) {
	for _, data := range []string{
		"speech: {idle_timeout_seconds: null}",
		"speech: {idle_timeout_seconds: '120'}",
		"value: &seconds 1.5\nspeech: {idle_timeout_seconds: *seconds}",
		"values: &speech {idle_timeout_seconds: 1.5}\nspeech: {<<: *speech}",
		"values: &config {speech: {idle_timeout_seconds: 1.5}}\n<<: *config",
	} {
		if _, err := Load(writeTestConfig(t, t.TempDir(), []byte(data))); err == nil {
			t.Fatalf("accepted invalid YAML %q", data)
		}
	}
}
