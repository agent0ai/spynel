package config

import (
	"bytes"
	"os"
	"reflect"
	"testing"
)

func TestVerboseDefaultValidationAndRestart(t *testing.T) {
	path := writeTestConfig(t, t.TempDir(), []byte("version: 1\nchannels: {tui: {title: Keep}}\n"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Channels.Verbose != "on" {
		t.Fatal("missing configuration did not default to on")
	}
	for _, setting := range Settings(cfg) {
		if setting.Key == "channels.verbose" && !reflect.DeepEqual(setting.Choices, []string{"on", "off"}) {
			t.Fatal("obsolete mode remains selectable", setting.Choices)
		}
	}
	store := NewStore(cfg)
	for _, mode := range []string{"off", "on"} {
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
	for _, invalid := range []string{"auto", "", "ON", "yes", "true", "on off"} {
		if _, err := store.Update(func(c *Config) error { _, err := SetSetting(c, "channels.verbose", invalid); return err }); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) || store.Snapshot().Channels.Verbose != "on" {
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

func TestVerboseLegacyAutoNormalizesOnlyOnRead(t *testing.T) {
	for _, input := range []string{"verbose: auto", "value: &mode auto\n  verbose: *mode", "<<: {verbose: auto}"} {
		t.Run(input, func(t *testing.T) {
			before := []byte("version: 1\n# keep on read\nchannels:\n  " + input + "\n  tui: {title: Keep}\nworkspace: {history_char_limit: 1234}\n")
			path := writeTestConfig(t, t.TempDir(), before)
			cfg, err := Load(path)
			if err != nil || cfg.Channels.Verbose != "on" || cfg.Channels.TUI.Title != "Keep" || cfg.Workspace.HistoryCharLimit != 1234 {
				t.Fatal("legacy normalization lost configuration", err)
			}
			if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
				t.Fatal("loading rewrote configuration")
			}
			if err := Save(cfg); err != nil {
				t.Fatal(err)
			}
			reloaded, err := Load(path)
			expected, _ := cfg.Encode()
			actual, encodeErr := reloaded.Encode()
			if err != nil || encodeErr != nil || !bytes.Equal(expected, actual) {
				t.Fatal("canonical save lost normalized settings", err)
			}
			cfg.Channels.Verbose = "auto"
			if err := Save(cfg); err == nil {
				t.Fatal("obsolete mode can still be saved")
			}
		})
	}
}

func TestVerboseYAMLRejectsMergedNull(t *testing.T) {
	for _, text := range []string{"value: &mode null\nchannels: {verbose: *mode}", "values: &channels {verbose: null}\nchannels: {<<: *channels}", "values: &config {channels: {verbose: null}}\n<<: *config"} {
		if _, err := Load(writeTestConfig(t, t.TempDir(), []byte(text))); err == nil {
			t.Fatal("accepted null mode")
		}
	}
}
