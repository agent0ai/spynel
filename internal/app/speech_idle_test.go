package app

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/workspace"
)

func TestSpeechIdleSettingSharedScreenCommandAndLiveCommit(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	service := New(cfg, newServiceHarness())
	defer service.Close()
	const key = "speech.idle_timeout_seconds"
	found := false
	for _, control := range settingsScreen(cfg, "config").Controls {
		if control.Key == key {
			found = true
			if control.Value != "120" || !control.Advanced {
				t.Fatalf("speech setting = %#v", control)
			}
		}
	}
	if !found {
		t.Fatal("advanced speech setting missing")
	}
	refreshes := 0
	service.RefreshSpeechIdleTimeout = func() {
		refreshes++
		saved, err := config.Load(cfg.Path)
		if err != nil || saved.Speech.IdleTimeoutSec != 300 || service.Settings.Snapshot().Speech != saved.Speech {
			t.Fatal("refresh preceded canonical save/reload")
		}
	}
	var response core.Event
	err = service.Handle(context.Background(), core.Message{Channel: "cli", Conversation: "idle-test", Text: "/config set " + key + " 300"}, func(event core.Event) { response = event })
	if err != nil || refreshes != 1 || !strings.Contains(response.Text, "300") {
		t.Fatalf("shared command: %#v, %v; refreshes=%d", response, err, refreshes)
	}
	before, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ApplySettings(map[string]string{key: "9223372036854775807"}); err == nil {
		t.Fatal("invalid timeout accepted")
	}
	after, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if refreshes != 1 || !bytes.Equal(before, after) || service.Settings.Snapshot().Speech.IdleTimeoutSec != 300 {
		t.Fatal("invalid setting changed persisted or live state")
	}
}
