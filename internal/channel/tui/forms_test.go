package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agent0ai/spynel/internal/core"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func formTextPosition(t *testing.T, m model, text string) (int, int) {
	t.Helper()
	for y, line := range strings.Split(m.View(), "\n") {
		plain := ansi.Strip(line)
		if at := strings.Index(plain, text); at >= 0 {
			return ansi.StringWidth(plain[:at]), y
		}
	}
	t.Fatalf("%q is not visible:\n%s", text, ansi.Strip(m.View()))
	return 0, 0
}

func formMouse(m *model, x, y int, action tea.MouseAction, button tea.MouseButton) tea.Cmd {
	next, command := m.Update(tea.MouseMsg{X: x, Y: y, Action: action, Button: button})
	*m = next.(model)
	return command
}

func TestFormsUseMouseForScrollingActionsAndEditing(t *testing.T) {
	for _, id := range []string{"config", "telegram", "whatsapp", "wizard:telegram:token", "wizard:whatsapp:access", "model"} {
		t.Run(id, func(t *testing.T) {
			m := testModel()
			next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 22})
			m = next.(model)
			calls := 0
			m.screenAction = func(_ context.Context, screenID, action string, values map[string]string) (*core.Screen, error) {
				if screenID != id || action != "apply" || values["field"] != "hello planet" {
					t.Fatalf("action received %q %q %#v", screenID, action, values)
				}
				calls++
				return nil, nil
			}
			controls := []core.ScreenControl{
				{Key: "field", Label: "Editable", Kind: "text", Value: "hello world"},
				{Key: "mode", Label: "Mode", Kind: "select", Value: "on", Options: []string{"on", "off"}},
				{Key: "apply", Kind: "action", Value: "Apply now"},
				{Key: "advanced", Kind: "disclosure", Value: "Advanced"},
			}
			for range 15 {
				controls = append(controls, core.ScreenControl{Key: "extra", Label: "Extra", Kind: "text", Value: "extra value", Advanced: true})
			}
			m.openScreen(core.Screen{ID: id, Title: "Configuration", StartAtTop: true, Controls: controls})
			x, y := formTextPosition(t, m, "hello world")
			formMouse(&m, x+6, y, tea.MouseActionPress, tea.MouseButtonLeft)
			formMouse(&m, x+11, y, tea.MouseActionMotion, tea.MouseButtonLeft)
			formMouse(&m, x+11, y, tea.MouseActionRelease, tea.MouseButtonLeft)
			if got := m.screenEditors[0].SelectedText(); got != "world" {
				t.Fatalf("drag selected %q", got)
			}
			next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("planet")})
			m = next.(model)
			if got := m.screen.Controls[0].Value; got != "hello planet" {
				t.Fatalf("replace selection: %q", got)
			}
			next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlZ})
			m = next.(model)
			if got := m.screen.Controls[0].Value; got != "hello world" {
				t.Fatalf("undo: %q", got)
			}
			next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlY})
			m = next.(model)
			x, y = formTextPosition(t, m, "‹ on ›")
			formMouse(&m, x+3, y, tea.MouseActionPress, tea.MouseButtonLeft)
			if m.screen.Controls[1].Value != "off" {
				t.Fatal("click did not change choice")
			}
			x, y = formTextPosition(t, m, "Apply now")
			command := formMouse(&m, x, y, tea.MouseActionPress, tea.MouseButtonLeft)
			if command == nil {
				t.Fatal("button did not activate")
			}
			_ = command()
			m.screenSaving = false
			if calls != 1 {
				t.Fatalf("button calls: %d", calls)
			}
			x, y = formTextPosition(t, m, "Show Advanced Settings")
			formMouse(&m, x, y, tea.MouseActionPress, tea.MouseButtonLeft)
			if !m.screenAdvanced {
				t.Fatal("disclosure click ignored")
			}
			index := m.screenIndex
			for range 20 {
				formMouse(&m, 40, 12, tea.MouseActionPress, tea.MouseButtonWheelDown)
			}
			if m.screenScroll == 0 || m.screenIndex != index {
				t.Fatal("wheel failed or changed selection")
			}
			down := m.screenScroll
			for range 40 {
				formMouse(&m, 40, 12, tea.MouseActionPress, tea.MouseButtonWheelUp)
			}
			if m.screenScroll != 0 || down <= 0 {
				t.Fatal("wheel did not return to top")
			}
			if strings.Contains(strings.ToLower(ansi.Strip(m.View())), "mouse") {
				t.Fatal("form includes unsolicited mouse instructions")
			}
		})
	}
}

func TestAutostartResultFlipsOneButtonWithoutLosingEdits(t *testing.T) {
	m := testModel()
	m.openScreen(core.Screen{ID: "config", Controls: []core.ScreenControl{
		{Key: "field", Kind: "text", Value: "original"},
		{Key: "autostart:disable", Kind: "action", Value: "Disable autostart", Description: "Autostart enabled. Registration verified."},
	}})
	m.screen.Controls[0].Value = "unsaved"
	m.screenIndex = 1
	actions := []string{}
	m.screenAction = func(_ context.Context, _, action string, _ map[string]string) (*core.Screen, error) {
		actions = append(actions, action)
		control := core.ScreenControl{Key: "autostart:enable", Kind: "action", Value: "Enable autostart"}
		if action == "autostart:enable" {
			control.Key, control.Value = "autostart:disable", "Disable autostart"
		}
		return &core.Screen{SavedControl: &control, ActionMessage: "Registration checked."}, nil
	}
	for range 2 {
		next, command := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		m = next.(model)
		if command == nil {
			t.Fatal("Enter did not activate autostart")
		}
		next, _ = m.Update(command())
		m = next.(model)
	}
	if strings.Join(actions, ",") != "autostart:disable,autostart:enable" || len(m.screen.Controls) != 2 || m.screenIndex != 1 || m.screen.Controls[0].Value != "unsaved" || !m.screenDirty() {
		t.Fatalf("repair sequence: %#v, %#v", actions, m.screen)
	}
	control := core.ScreenControl{Key: "autostart:check", Kind: "action", Value: "Check autostart", Description: "State unknown"}
	next, _ := m.Update(screenActionResult{screenID: "config", action: "autostart:disable", screen: &core.Screen{SavedControl: &control}, err: errors.New("permission denied")})
	m = next.(model)
	if m.screen.Controls[1].Key != "autostart:check" || !strings.Contains(m.screen.Status, "permission denied") {
		t.Fatal("failed action left stale button or hid error")
	}
}

func TestFormClipboardCannotWriteIntoReopenedScreen(t *testing.T) {
	m := testModel()
	screen := core.Screen{ID: "config", Controls: []core.ScreenControl{{Key: "field", Kind: "text", Value: "original"}}}
	m.openScreen(screen)
	editor := m.formEditor(0, 80)
	late := formPaste{text: "late", generation: editor.HistoryGeneration(), screenGeneration: m.screenGeneration, index: 0}
	m.clearScreen()
	m.openScreen(screen)
	next, _ := m.Update(late)
	m = next.(model)
	if m.screen.Controls[0].Value != "original" {
		t.Fatal("stale clipboard changed reopened form")
	}
}

func TestDialogButtonsReceiveClicksWithoutEditingCoveredForm(t *testing.T) {
	m := testModel()
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = next.(model)
	m.openScreen(core.Screen{ID: "config", Controls: []core.ScreenControl{{Key: "field", Kind: "text", Value: "original"}}})
	m.screen.Controls[0].Value = "unsaved"
	m.confirmDiscardScreenChanges()
	x, y := formTextPosition(t, m, "Keep editing")
	formMouse(&m, x, y, tea.MouseActionPress, tea.MouseButtonLeft)
	if m.dialog != nil || m.screen == nil || m.screen.Controls[0].Value != "unsaved" {
		t.Fatal("modal button click failed")
	}
}

func TestReopenedFormRejectsOldActionAndSaveResults(t *testing.T) {
	m := testModel()
	screen := core.Screen{ID: "config", Controls: []core.ScreenControl{{Key: "autostart:enable", Kind: "action", Value: "Enable autostart"}}}
	m.openScreen(screen)
	generation := m.screenGeneration
	m.clearScreen()
	m.openScreen(screen)
	control := core.ScreenControl{Key: "autostart:disable", Kind: "action", Value: "Disable autostart"}
	next, _ := m.Update(screenActionResult{screenID: "config", generation: generation, action: "autostart:enable", screen: &core.Screen{SavedControl: &control}})
	m = next.(model)
	next, _ = m.Update(screenSaveResult{screenID: "config", generation: generation, closeOnSuccess: true})
	m = next.(model)
	if m.screen == nil || m.screen.Controls[0].Key != "autostart:enable" {
		t.Fatal("stale result changed reopened form")
	}
}
