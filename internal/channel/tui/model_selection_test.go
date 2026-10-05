package tui

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/workspace"
	tea "github.com/charmbracelet/bubbletea"
)

type selectionHarness struct {
	harness.Harness
	selection harness.InferenceSelection
}

func TestDirectModelCompletionReturnsToChat(t *testing.T) {
	m := testModel()
	m.openScreen(core.Screen{ID: "model-service:fixture", SaveDisabled: true, Controls: []core.ScreenControl{
		{Key: "select:default", Kind: "action", Value: "Normal"},
	}})
	saved := core.ScreenControl{Key: "model", Kind: "action", Value: "Model · model-a"}
	next, _ := m.Update(screenActionResult{screenID: m.screen.ID, generation: m.screenGeneration, action: "select:default", screen: &core.Screen{SavedControl: &saved, ActionMessage: "Saved selection"}})
	m = next.(model)
	if m.screen != nil || len(m.screenStack) != 0 || len(m.transcript) != 1 {
		t.Fatal("direct model completion did not return to chat with its confirmation")
	}
}

func (h *selectionHarness) Models(context.Context) ([]harness.Model, error) {
	return []harness.Model{{ID: "model-a", DisplayName: "Model A", Default: true, Efforts: []string{"low", "high"}, ServiceModes: []harness.ModelPropertyOption{
		{ID: "default", DisplayName: "Normal"}, {ID: "priority", DisplayName: "Fast"},
	}}}, nil
}

func (h *selectionHarness) CommitInference(selection harness.InferenceSelection, save func() error) error {
	if err := save(); err != nil {
		return err
	}
	h.selection = selection
	return nil
}

func TestModelSelectionSaveReturnsToCanonicalConfiguration(t *testing.T) {
	for _, mode := range []string{"", "default", "priority"} {
		t.Run("speed="+mode, func(t *testing.T) {
			root := t.TempDir()
			if err := workspace.Init(root, false); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(config.PathForRoot(root))
			if err != nil {
				t.Fatal(err)
			}
			cfg.Harness.Name, cfg.Harness.Model = "codex", "old-model"
			cfg.Harness.ReasoningEffort, cfg.Harness.ServiceMode = "low", "priority"
			if err := config.Save(cfg); err != nil {
				t.Fatal(err)
			}
			target := &selectionHarness{}
			service := app.New(cfg, target)
			t.Cleanup(service.Runtime.Close)
			m := testModel()
			next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 34})
			m = next.(model)
			m.screenAction = service.ScreenAction
			screen, err := service.Screen("config")
			if err != nil {
				t.Fatal(err)
			}
			m.openScreen(screen)
			for index := range m.screen.Controls {
				if m.screen.Controls[index].Key == "workspace.history_char_limit" {
					m.screen.Controls[index].Value = "unsaved invalid edit"
				}
			}
			apply := func(action string) {
				t.Helper()
				command := m.runScreenAction(action)
				result := command().(screenActionResult)
				if result.err != nil {
					t.Fatal(result.err)
				}
				next, _ := m.Update(result)
				m = next.(model)
			}
			apply("model")
			apply("select:model-a")
			apply("select:high")
			capture := func(name string) {
				t.Helper()
				if directory := os.Getenv("SPYNEL_CAPTURE_DIR"); directory != "" && mode == "default" {
					if err := os.MkdirAll(directory, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(directory, name+".ansi"), []byte(m.View()), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			capture("model-speed-normal")
			if !strings.HasPrefix(m.screen.ID, "model-service:") || len(m.screenStack) != 1 {
				t.Fatalf("lost dependent selector: %#v", m.screen)
			}
			for _, key := range []string{"select:", "select:default", "select:priority"} {
				found := false
				for _, control := range m.screen.Controls {
					found = found || control.Key == key
				}
				if !found {
					t.Fatalf("speed choice %q missing", key)
				}
			}
			apply("select:" + mode)
			capture("config-after-model-save")
			if m.screen == nil || m.screen.ID != "config" || len(m.screenStack) != 0 {
				t.Fatalf("speed save did not return to configuration: %#v", m.screen)
			}
			if m.screenChanges()["workspace.history_char_limit"] != "unsaved invalid edit" {
				t.Fatal("selection lost unrelated unsaved configuration edits")
			}
			got := service.Settings.Snapshot().Harness
			want := harness.InferenceSelection{Model: "model-a", Effort: "high", ServiceMode: mode}
			if target.selection != want || got.Model != want.Model || got.ReasoningEffort != want.Effort || got.ServiceMode != want.ServiceMode {
				t.Fatalf("saved/runtime selection diverged: %#v, %#v", got, target.selection)
			}
			reloaded, err := config.Load(config.PathForRoot(root))
			if err != nil || !reflect.DeepEqual(reloaded.Harness, got) {
				t.Fatalf("canonical reload differs: %#v, %v", reloaded.Harness, err)
			}
			canonical, _ := service.Screen("config")
			if !reflect.DeepEqual(m.screen.Controls[1], canonical.Controls[1]) {
				t.Fatalf("stale model row: %#v, want %#v", m.screen.Controls[1], canonical.Controls[1])
			}
			// Reproduce clicking Model after save, then Escape without another save.
			apply("model")
			if m.screen.ID != "model" || m.screen.InitialControl != "select:model-a" {
				t.Fatalf("Model button routed into consumed speed context: %#v", m.screen)
			}
			next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
			m = next.(model)
			if m.screen.ID != "config" || !reflect.DeepEqual(m.screen.Controls[1], canonical.Controls[1]) {
				t.Fatal("Escape restored a stale model summary")
			}
			// Discard only the unrelated edit, then close/reopen via canonical state.
			for index := range m.screen.Controls {
				if m.screen.Controls[index].Key == "workspace.history_char_limit" {
					m.screen.Controls[index].Value = m.screenOriginal["workspace.history_char_limit"]
				}
			}
			next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
			m = next.(model)
			if m.screen != nil {
				t.Fatal("unchanged configuration did not close on Escape")
			}
			m.openScreen(canonical)
			apply("model")
			apply("select:model-a")
			if m.screen.InitialControl != "select:high" {
				t.Fatal("reopened effort picker is stale")
			}
			apply("select:high")
			if m.screen.InitialControl != "select:"+mode {
				t.Fatal("reopened speed picker is stale")
			}
		})
	}
}
