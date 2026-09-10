package textarea

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestSemanticEditing(t *testing.T) {
	m := New()
	m.Prompt = ""
	m.ShowLineNumbers = false
	m.SetWidth(9)
	m.SetHeight(2)
	m.Focus()
	source := "  界e\u0301👨‍👩‍👧‍👦\n\talpha beta\n\nlast  "
	m.SetValue(source)
	m.SelectAll()
	if m.SelectedText() != source {
		t.Fatalf("selection lost source: %q", m.SelectedText())
	}
	rows := m.VisualRows()
	for _, row := range rows {
		if row.Text != string([]rune(source)[row.Start:row.End]) {
			t.Fatalf("row lacks source range: %#v", row)
		}
	}
	// Reverse range, selected newline, replacement, and exact whitespace.
	m.Select(len([]rune(source)), 0)
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("new\n  text"), Paste: true})
	if m.Value() != "new\n  text" || m.HasSelection() {
		t.Fatalf("replacement = %q", m.Value())
	}
	m.Select(0, 4)
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDelete})
	if m.Value() != "  text" {
		t.Fatalf("multiline delete = %q", m.Value())
	}
	m.SetValue("👩🏽‍💻 e\u0301\n界 word")
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlLeft})
	if m.SelectedText() != "" || m.CursorOffset() != len([]rune("👩🏽‍💻 e\u0301\n界 ")) {
		t.Fatalf("word left = %d", m.CursorOffset())
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlShiftLeft})
	if m.SelectedText() != "界 " {
		t.Fatalf("shift word left = %q", m.SelectedText())
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if m.Value() != "👩🏽‍💻 e\u0301\nword" {
		t.Fatalf("selected word delete = %q", m.Value())
	}
	m.SetValue("e\u0301👩🏽‍💻")
	m.Select(0, 2)
	if !strings.Contains(ansi.Strip(m.View()), "e\u0301👩🏽‍💻") {
		t.Fatalf("grapheme render = %q", m.View())
	}
	m.Select(0, 0)
	for range 3 {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlLeft})
	}
	if m.CursorOffset() != 0 {
		t.Fatal("word movement escaped beginning")
	}
	m.SetValue("\n  \n")
	m.Select(0, 0)
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlLeft})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlRight})
	if m.CursorOffset() != 4 {
		t.Fatal("blank word navigation did not terminate at end")
	}
}

func TestMouseAndKeyboardShareGraphemeBoundaries(t *testing.T) {
	m := New()
	m.Prompt = ""
	m.ShowLineNumbers = false
	m.SetWidth(12)
	m.SetHeight(3)
	m.Focus()
	m.SetValue("界e\u0301👩🏽‍💻x\n  second\nthird")
	m.ScrollBy(-100)
	for x, want := range []int{0, 1, 1, 3, 7, 7, 8} {
		if got := m.Hit(x, 0); got != want {
			t.Errorf("hit %d=%d want %d", x, got, want)
		}
	}
	m.Select(1, 7)
	m.SetWidth(5)
	if got := m.SelectedText(); got != "e\u0301👩🏽‍💻" {
		t.Fatalf("resize selection = %q", got)
	}
	m.Select(0, 0)
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftRight})
	if got := m.SelectedText(); got != "界" {
		t.Fatalf("wide shift selection=%q", got)
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftDown})
	if !m.HasSelection() {
		t.Fatal("vertical shift lost anchor")
	}
	m.SelectAll()
	m.InsertString("one\ntwo")
	m.Select(3, 3)
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDelete})
	if m.Value() != "onetwo" {
		t.Fatalf("delete newline=%q", m.Value())
	}
	m.SetValue("a\nb")
	m.Select(0, 0)
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDelete})
	if m.Value() != "\nb" {
		t.Fatalf("delete character also deleted newline: %q", m.Value())
	}
}

func TestRejectedPastePreservesSelection(t *testing.T) {
	m := New()
	m.CharLimit = 6
	m.SetValue("ab界é")
	m.Select(0, 2)
	m.InsertString("too long 👩🏽‍💻")
	if m.Err == nil || m.Value() != "ab界é" || m.SelectedText() != "ab" {
		t.Fatal("rejected paste destroyed source or selection")
	}
	m.Err = nil
	m.InsertString("XY")
	if m.Value() != "XY界é" || m.HasSelection() {
		t.Fatal("fitting replacement failed")
	}
}

func TestCollapsedSelectionEditsConsumeAnchor(t *testing.T) {
	for _, edit := range []string{"string", "rune", "key", "paste", "newline", "backspace", "delete", "word", "transpose", "uppercase"} {
		t.Run(edit, func(t *testing.T) {
			m := New()
			m.Focus()
			m.SetValue("ab word")
			m.Select(2, 2) // A plain click or Shift movement back to the anchor.
			switch edit {
			case "string":
				m.InsertString("界é")
			case "rune":
				m.InsertRune('界')
			case "key":
				m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("X")})
			case "paste":
				m, _ = m.Update(pasteMsg("PASTED\n  text"))
			case "newline":
				m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
			case "backspace":
				m, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
			case "delete":
				m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDelete})
			case "word":
				m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlW})
			case "transpose":
				m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
			case "uppercase":
				m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("u"), Alt: true})
			}
			if m.selecting || m.HasSelection() {
				t.Fatalf("edit left a selection anchor: %q", m.SelectedText())
			}
			before, at := []rune(m.Value()), m.CursorOffset()
			m.InsertString("X")
			m.InsertString("Y")
			want := string(before[:at]) + "XY" + string(before[at:])
			if m.Value() != want || m.HasSelection() {
				t.Fatalf("continued typing=%q selected=%q want=%q", m.Value(), m.SelectedText(), want)
			}
			m, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftLeft})
			if m.SelectedText() != "Y" {
				t.Fatalf("Shift used a stale anchor: %q", m.SelectedText())
			}
		})
	}
	// Empty and rejected insertions must retain the anchor for Shift extension.
	m := New()
	m.Focus()
	m.CharLimit = 4
	m.SetValue("abcd")
	m.Select(2, 2)
	m.InsertString("")
	m.InsertString("X")
	if m.Err == nil || !m.selecting || m.anchor != 2 || m.Value() != "abcd" {
		t.Fatal("rejection changed the collapsed selection or draft")
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.Err == nil || !m.selecting || m.Value() != "abcd" || m.CursorOffset() != 2 {
		t.Fatal("newline bypassed transactional insertion")
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftRight})
	if m.SelectedText() != "c" {
		t.Fatal("rejected edit lost Shift extension")
	}
}

func TestWholeValueAndPreparedRangeAreTransactional(t *testing.T) {
	m := New()
	m.CharLimit = 24
	m.SetValue("abcDEFghi\nsentinel")
	m.Select(6, 3)
	for _, value := range []string{strings.Repeat("x", 25), strings.Repeat("\n", maxLines)} {
		m.SetValue(value)
		if m.Err == nil || m.Value() != "abcDEFghi\nsentinel" || m.SelectedText() != "DEF" || m.CursorOffset() != 3 {
			t.Fatal("rejected SetValue changed draft, selection, or caret")
		}
	}
	if m.ReplaceRange(3, 6, strings.Repeat("x", 25)) || m.SelectedText() != "DEF" {
		t.Fatal("rejected range replacement changed selection")
	}
	if !m.ReplaceRange(3, 6, "界é") || m.SelectedText() != "界é" || m.CursorOffset() != 3 {
		t.Fatal("prepared replacement lost reverse selection")
	}
	m.Select(10, 18)
	if !m.ReplaceRange(3, 6, "XY") || m.SelectedText() != "sentinel" || m.CursorOffset() != 17 {
		t.Fatal("prepared replacement moved unrelated selection")
	}
	m.Select(5, 5)
	m.InsertString("Z")
	if m.Value() != "abcXYZghi\nsentinel" {
		t.Fatalf("typing after prepared replacement = %q", m.Value())
	}

	m.CharLimit = 0
	m.SetValue("keep\nme")
	m.SelectAll()
	m.SetValue(strings.Repeat("\n", maxLines))
	if m.Err == nil || m.SelectedText() != "keep\nme" {
		t.Fatal("line-limit rejection erased input")
	}
}

func TestEditsOwnViewportReconciliation(t *testing.T) {
	newDraft := func() Model {
		m := New()
		m.Prompt, m.ShowLineNumbers = "", false
		m.SetWidth(12)
		m.SetHeight(3)
		m.CharLimit = 0
		m.SetValue(strings.Repeat("row\n", 20) + "tail")
		return m
	}
	visibleCaret := func(t *testing.T, m Model) {
		t.Helper()
		row := m.cursorLineNumber()
		if row < m.ScrollOffset() || row >= m.ScrollOffset()+m.Height() {
			t.Fatalf("caret row %d outside viewport %d+%d", row, m.ScrollOffset(), m.Height())
		}
	}
	for _, edit := range []string{"set", "string", "rune", "delete", "range"} {
		t.Run(edit, func(t *testing.T) {
			m := newDraft()
			switch edit {
			case "set":
				// Whole-value replacement itself must reveal the final caret.
			case "string":
				m.InsertString("\nPASTED-END")
			case "rune":
				m.InsertRune('\n')
			case "delete":
				m.Select(8, m.CursorOffset())
				m.DeleteSelection()
			case "range":
				end := m.CursorOffset()
				if !m.ReplaceRange(end-4, end, strings.Repeat("x", 24)+"END") {
					t.Fatal(m.Err)
				}
			}
			visibleCaret(t, m)
			_ = m.View()
			visibleCaret(t, m)
		})
	}
	for _, edit := range []string{"set", "range", "empty", "sanitize-empty", "async-scrolled"} {
		t.Run(edit+"/preserve-view", func(t *testing.T) {
			m := newDraft()
			m.Select(m.CursorOffset()-4, m.CursorOffset())
			m.ScrollBy(-5)
			before, offset := m.Value(), m.ScrollOffset()
			selected, caret := m.SelectedText(), m.CursorOffset()
			switch edit {
			case "set", "range":
				m.CharLimit = len([]rune(before))
				if edit == "set" {
					m.SetValue(before + "x")
				} else {
					m.ReplaceRange(caret-4, caret, "oversized")
				}
				if m.Err == nil {
					t.Fatal("oversized edit accepted")
				}
			case "empty":
				m.InsertString("")
			case "sanitize-empty":
				m.InsertString("\x00")
			case "async-scrolled":
				if !m.ReplaceRange(caret-4, caret, "TAIL") {
					t.Fatal(m.Err)
				}
				before, selected = strings.TrimSuffix(before, "tail")+"TAIL", "TAIL"
			}
			_ = m.View()
			if m.ScrollOffset() != offset || m.Value() != before || m.SelectedText() != selected || m.CursorOffset() != caret {
				t.Fatal("transaction changed the preserved view, draft, selection, or caret")
			}
		})
	}
}

func TestGeometryPreservesCaretVisibilityAndSelection(t *testing.T) {
	for _, dimension := range []string{"width", "height"} {
		for _, detached := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/detached=%t", dimension, detached), func(t *testing.T) {
				m := New()
				m.Prompt, m.ShowLineNumbers = "", false
				m.SetWidth(38)
				m.SetHeight(10)
				m.SetValue(strings.Repeat("0123456789 ", 60) + "\nVISIBLE-TAIL 界é👩🏽‍💻")
				end := m.CursorOffset()
				m.Select(end-len([]rune("VISIBLE-TAIL 界é👩🏽‍💻")), end)
				if detached {
					m.ScrollBy(-1000)
					m.ScrollBy(2)
				}
				selected, value := m.SelectedText(), m.Value()
				for _, size := range []int{20, 60, 38} {
					if dimension == "width" {
						m.SetWidth(size)
					} else {
						m.SetHeight(size / 10)
					}
					if m.Value() != value || m.CursorOffset() != end || m.SelectedText() != selected {
						t.Fatal("geometry changed logical text, caret, or selection")
					}
					if detached {
						if m.ScrollOffset() != 2 {
							t.Fatal("geometry undid intentional scrolling")
						}
					} else if row := m.cursorLineNumber(); row < m.ScrollOffset() || row >= m.ScrollOffset()+m.Height() {
						t.Fatalf("caret row %d outside viewport %d+%d", row, m.ScrollOffset(), m.Height())
					}
					_ = m.View()
				}
			})
		}
	}
}

func TestWordAndLogicalLineRanges(t *testing.T) {
	text := []rune("previous\n  alpha,beta 界é👩🏽‍💻\t \nlast")
	for _, want := range []string{"  ", "alpha,beta", "界é👩🏽‍💻", "\t "} {
		start := len([]rune(strings.SplitN(string(text), want, 2)[0]))
		for at := start; at < start+len([]rune(want)); at++ {
			a, b := WordRange(text, at)
			if got := string(text[a:b]); got != want {
				t.Fatalf("word at %d=%q want=%q", at, got, want)
			}
			a, b = LineRange(text, at)
			if got := string(text[a:b]); got != "  alpha,beta 界é👩🏽‍💻\t " {
				t.Fatalf("line=%q", got)
			}
		}
	}
	for _, text := range []string{"", "\n"} {
		for _, at := range []int{-1, 0, len([]rune(text)), 100} {
			a, b := WordRange([]rune(text), at)
			if a != b {
				t.Fatalf("empty word %q at %d: %d:%d", text, at, a, b)
			}
		}
	}
	m := New()
	m.Prompt = ""
	m.ShowLineNumbers = false
	m.SetWidth(8)
	m.SetHeight(2)
	m.SetValue("界é👩🏽‍💻\nnext")
	m.ScrollBy(-100)
	for _, tc := range []struct{ x, at int }{{0, 0}, {1, 0}, {2, 1}, {3, 3}, {4, 3}} {
		if at, ok := m.CharacterAt(tc.x, 0); !ok || at != tc.at {
			t.Fatalf("character hit %d: %d/%t", tc.x, at, ok)
		}
	}
	for _, pos := range [][2]int{{-1, 0}, {5, 0}, {8, 0}, {0, 2}} {
		if _, ok := m.CharacterAt(pos[0], pos[1]); ok {
			t.Fatalf("fill/outside hit at %v", pos)
		}
	}
}
