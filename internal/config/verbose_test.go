package config

import (
	"bytes"
	"os"
	"testing"
)

func TestVerboseDefaultValidationAndRestart(t *testing.T) {
	path := writeTestConfig(t, t.TempDir(), []byte("version: 1\nchannels: {tui: {title: Keep}}\n"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Channels.Verbose != "auto" {
		t.Fatal("legacy default changed")
	}
	store := NewStore(cfg)
	for _, mode := range []string{"on", "off", "auto"} {
		_, err := store.Update(func(c *Config) error { _, err := SetSetting(c, "channels.verbose", mode); return err })
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := Load(path)
		if err != nil || loaded.Channels.Verbose != mode || loaded.Channels.TUI.Title != "Keep" {
			t.Fatalf("restart: %#v %v", loaded.Channels, err)
		}
	}
	before, _ := os.ReadFile(path)
	for _, invalid := range []string{"", "ON", "yes", "true", "on off"} {
		if _, err := store.Update(func(c *Config) error { _, err := SetSetting(c, "channels.verbose", invalid); return err }); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) || store.Snapshot().Channels.Verbose != "auto" {
			t.Fatal("invalid changed state")
		}
	}
	for _, invalid := range []string{"null", "true", "12", "[]", "ON", "''"} {
		path := writeTestConfig(t, t.TempDir(), []byte("channels: {verbose: "+invalid+"}"))
		if _, err := Load(path); err == nil {
			t.Fatalf("YAML accepted %q", invalid)
		}
	}
}

func TestVerboseYAMLRejectsMergedNull(t *testing.T) {
	for _, text := range []string{"value: &mode null\nchannels: {verbose: *mode}", "values: &channels {verbose: null}\nchannels: {<<: *channels}", "values: &config {channels: {verbose: null}}\n<<: *config"} {
		if _, err := Load(writeTestConfig(t, t.TempDir(), []byte(text))); err == nil {
			t.Fatal("accepted null mode")
		}
	}
}
