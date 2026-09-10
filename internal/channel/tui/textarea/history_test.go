package textarea

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestUndoRedoAtomicEdits(t *testing.T) {
	for _, action := range []string{"typing", "paste", "insert", "rune", "cut", "backspace", "delete", "backspace-grapheme", "delete-grapheme", "join", "word-left", "word-right", "line-left", "line-right", "newline", "range", "transpose", "case"} {
		t.Run(action, func(t *testing.T) {
			m := New()
			m.Focus()
			m.SetValue("界é👩🏽‍💻 OLD\n\tlast word")
			m.Select(10, 8) // reverse selection of OL
			if action == "word-left" || action == "line-left" {
				m.Select(len([]rune(m.Value())), len([]rune(m.Value())))
			} else if action == "word-right" || action == "line-right" || action == "case" || action == "transpose" {
				m.Select(8, 8)
			} else if action == "backspace-grapheme" {
				m.Select(7, 7)
			} else if action == "delete-grapheme" {
				m.Select(3, 3)
			} else if action == "join" {
				m.Select(12, 12)
			}
			before := m.editState()
			k := tea.KeyMsg{}
			switch action {
			case "typing":
				k = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("X")}
			case "paste":
				k = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("界\n  é👩🏽‍💻\t"), Paste: true}
			case "insert":
				m.InsertString("界\n  é👩🏽‍💻\t")
			case "rune":
				m.InsertRune('界')
			case "cut":
				m.DeleteSelection()
			case "backspace", "backspace-grapheme", "join":
				k.Type = tea.KeyBackspace
			case "delete", "delete-grapheme":
				k.Type = tea.KeyDelete
			case "word-left":
				k.Type = tea.KeyCtrlW
			case "word-right":
				k = tea.KeyMsg{Type: tea.KeyRunes, Alt: true, Runes: []rune("d")}
			case "line-left":
				k.Type = tea.KeyCtrlU
			case "line-right":
				k.Type = tea.KeyCtrlK
			case "newline":
				k.Type = tea.KeyEnter
			case "range":
				m.ReplaceRange(8, 11, "replacement\n界")
			case "transpose":
				k.Type = tea.KeyCtrlT
			case "case":
				k = tea.KeyMsg{Type: tea.KeyRunes, Alt: true, Runes: []rune("l")}
			}
			if k.Type != 0 {
				m, _ = m.Update(k)
			}
			after := m.editState()
			if after.text == before.text || len(m.history.undo) != 1 {
				t.Fatalf("edit was not one action: %q, steps=%d", after.text, len(m.history.undo))
			}
			if !m.Undo() || m.editState() != before || m.Undo() {
				t.Fatalf("undo did not restore the complete original state: %+v", m.editState())
			}
			if !m.Redo() || m.editState() != after || m.Redo() {
				t.Fatalf("redo did not restore the complete edited state: %+v", m.editState())
			}
		})
	}
}

func TestUndoGroupingBranchAndNoOpEdits(t *testing.T) {
	m := New()
	m.Focus()
	m.Select(0, 0) // an empty click anchor must not split subsequent typing
	typeText := func(text string) {
		for _, r := range text {
			m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		}
	}
	typeText("hello")
	m.history.editedAt = m.history.editedAt.Add(-2 * time.Second)
	typeText("界é")
	if !m.Undo() || m.Value() != "hello" || !m.Undo() || m.Value() != "" {
		t.Fatal("typing did not group or respect a pause")
	}
	m.Redo()
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m.SelectAll()
	m.ScrollBy(-1)
	m.InsertString("")
	m.InsertString("\x00")
	m.CharLimit = 1
	m.InsertString("rejected")
	if len(m.history.undo) != 1 || len(m.history.redo) != 1 || m.SelectedText() != "hello" {
		t.Fatal("navigation or rejected edit changed history/selection")
	}
	m.CharLimit = 0
	typeText("X")
	if m.Redo() || m.Value() != "X" {
		t.Fatal("new edit retained redo branch")
	}
	m.Undo()
	if m.Value() != "hello" || m.SelectedText() != "hello" {
		t.Fatal("replacement lost selection")
	}
	m.SetValue("")
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlW})
	if m.Undo() || m.Redo() {
		t.Fatal("edge deletion created history")
	}
}

func TestUndoRestoresVisibleCaretAfterReflow(t *testing.T) {
	m := New()
	m.Prompt, m.ShowLineNumbers = "", false
	m.SetWidth(12)
	m.SetHeight(3)
	m.SetValue(strings.Repeat("界é👩🏽‍💻 row\n", 20) + "sentinel")
	end := len([]rune(m.Value()))
	m.Select(end-8, end)
	m.InsertString("new\n tail")
	m.SetWidth(7)
	m.SetHeight(2)
	m.ScrollBy(-1000)
	if !m.Undo() || m.SelectedText() != "sentinel" {
		t.Fatal("undo lost the logical selection")
	}
	for range 2 {
		row := m.cursorLineNumber()
		if row < m.ScrollOffset() || row >= m.ScrollOffset()+m.Height() {
			t.Fatal("restored caret is hidden")
		}
		m.Redo()
	}
}

func TestUndoBoundsAndDraftReset(t *testing.T) {
	m := New()
	for range maxUndoSteps + 10 {
		m.InsertRune('x')
	}
	if len(m.history.undo) != maxUndoSteps {
		t.Fatal("step bound not enforced")
	}
	m.SetValue(strings.Repeat("x", 65536))
	for range 100 {
		m.InsertRune('y')
	}
	bytes := 0
	for _, s := range m.history.undo {
		bytes += len(s.text) + 64
	}
	if bytes > maxUndoBytes || len(m.history.undo) >= maxUndoSteps {
		t.Fatal("byte bound not enforced")
	}
	for _, reset := range []func(){m.Reset, func() { m.SetValue("replacement") }} {
		m.InsertString("old draft")
		reset()
		if m.Undo() || m.Redo() || m.RetainsText("old draft") {
			t.Fatal("history crossed draft replacement")
		}
	}
}

func TestUndoFencesDelayedWidgetClipboard(t *testing.T) {
	m := New()
	m.Focus()
	m.InsertString("draft")
	result := clipboardHistoryMsg{m.HistoryGeneration(), pasteMsg("late")}
	m.Undo()
	m.Redo()
	m, _ = m.Update(result)
	if m.Value() != "draft" {
		t.Fatal("late clipboard changed redone draft")
	}
	result.generation = m.HistoryGeneration()
	m.SetValue("replacement")
	m, _ = m.Update(result)
	if m.Value() != "replacement" {
		t.Fatal("late clipboard crossed reset")
	}
}

func TestUndoCompatibleDeletionRuns(t *testing.T) {
	for _, k := range []tea.KeyMsg{
		{Type: tea.KeyBackspace}, {Type: tea.KeyDelete},
		{Type: tea.KeyCtrlW}, {Type: tea.KeyRunes, Alt: true, Runes: []rune("d")},
	} {
		t.Run(k.String(), func(t *testing.T) {
			m := New()
			m.Focus()
			m.SetValue("one 界é👩🏽‍💻 three four")
			if m.groupingKind(k) == forwardDelete || m.groupingKind(k) == forwardWordDelete {
				m.CursorStart()
			}
			before := m.editState()
			for range 3 {
				m, _ = m.Update(k)
			}
			after := m.editState()
			if before.text == after.text || !m.Undo() || m.editState() != before {
				t.Fatal("adjacent deletions did not undo as one complete state")
			}
			if !m.Redo() || m.editState() != after || m.Redo() {
				t.Fatal("redo did not preserve deletion grouping")
			}
		})
	}
}

func TestUndoGroupBoundaries(t *testing.T) {
	for _, boundary := range []string{"pause", "navigation", "selection", "scroll", "space", "tab", "newline", "nbsp", "ideographic-space", "size", "delete-pause", "delete-navigation", "delete-direction", "delete-kind", "typing-delete"} {
		t.Run(boundary, func(t *testing.T) {
			m := New()
			m.Focus()
			press := func(k tea.KeyMsg) { m, _ = m.Update(k) }
			typeText := func(text string) {
				for _, r := range text {
					press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
				}
			}
			typeText("hello")
			switch boundary {
			case "pause":
				m.history.editedAt = m.history.editedAt.Add(-undoGroupDelay)
			case "navigation":
				press(tea.KeyMsg{Type: tea.KeyLeft})
				press(tea.KeyMsg{Type: tea.KeyRight})
			case "selection":
				m.Select(4, 5)
				m.ClearSelection()
			case "scroll":
				m.ScrollBy(-1)
			case "space", "tab", "nbsp", "ideographic-space":
				typeText(map[string]string{"space": " ", "tab": "\t", "nbsp": "\u00a0", "ideographic-space": "\u3000"}[boundary])
			case "newline":
				press(tea.KeyMsg{Type: tea.KeyEnter})
			case "size":
				typeText(strings.Repeat("x", maxUndoGroupRunes-len("hello")))
			case "delete-pause", "delete-navigation", "delete-direction", "delete-kind", "typing-delete":
				m.SetValue("one two three four")
				m.SetCursor(13)
				press(tea.KeyMsg{Type: tea.KeyBackspace})
				before := m.Value()
				k := tea.KeyMsg{Type: tea.KeyBackspace}
				switch boundary {
				case "delete-pause":
					m.history.editedAt = m.history.editedAt.Add(-undoGroupDelay)
				case "delete-navigation":
					press(tea.KeyMsg{Type: tea.KeyLeft})
					press(tea.KeyMsg{Type: tea.KeyRight})
				case "delete-direction":
					k.Type = tea.KeyDelete
				case "delete-kind":
					k.Type = tea.KeyCtrlW
				case "typing-delete":
					k = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}
				}
				press(k)
				m.Undo()
				if m.Value() != before {
					t.Fatalf("deletion action boundary lost: %q, want %q", m.Value(), before)
				}
				return
			}
			before := m.Value()
			typeText("next")
			m.Undo()
			if m.Value() != before {
				t.Fatalf("typing boundary lost: %q, want %q", m.Value(), before)
			}
			m.Redo()
			if m.Value() != before+"next" {
				t.Fatal("redo split the typing run")
			}
		})
	}
}

func TestUndoNoOpHistoryCapture(t *testing.T) {
	m := New()
	m.SetValue(strings.Repeat("x", 65536))
	// Transactions keep only positions until an edit is accepted. In particular,
	// empty and rejected direct edits must not serialize a full draft for history.
	if allocations := testing.AllocsPerRun(10, func() { defer m.beginEdit()() }); allocations > 1 {
		t.Fatalf("no-op history transaction allocated text: %g allocations", allocations)
	}
	m.startEdit(typingEdit)
	m.recordEdit(1)
	m.finishEdit()
	if allocations := testing.AllocsPerRun(10, func() {
		m.startEdit(typingEdit)
		m.recordEdit(1)
		m.finishEdit()
	}); allocations != 0 {
		t.Fatalf("joined history capture allocated an unused snapshot: %g allocations", allocations)
	}
	m.Focus()
	m.SetValue("same")
	m.InsertString("!")
	m.Undo()
	m.SelectAll()
	m.InsertString("same") // accepted equal replacement only moves the caret
	m.ReplaceRange(0, 4, "same")
	m.ReplaceRange(0, 0, "")
	m.SetCursor(0)
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Alt: true, Runes: []rune("l")})
	m.SetWidth(8)
	m.SetHeight(2)
	if len(m.history.undo) != 0 || !m.Redo() || m.Value() != "same!" {
		t.Fatal("equal replacement, case, navigation or resize discarded redo or added an edit")
	}
}
