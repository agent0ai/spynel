package textarea

import (
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

const (
	maxUndoSteps      = 100
	maxUndoBytes      = 4 << 20
	maxUndoGroupRunes = 64
	undoGroupDelay    = 500 * time.Millisecond
)

type editKind uint8

const (
	isolatedEdit editKind = iota
	typingEdit
	backwardDelete
	forwardDelete
	backwardWordDelete
	forwardWordDelete
)

type editState struct {
	text                          string
	caret, anchor, column, scroll int
	selecting                     bool
}

type editHistory struct {
	undo, redo   []editState
	editedAt     time.Time
	caret, runes int
	kind         editKind
}

type clipboardHistoryMsg struct {
	generation uint64
	message    tea.Msg
}

// HistoryGeneration fences asynchronous edits across draft resets and undo/redo.
// Ordinary edits still allow callers to validate and replace an unchanged range.
func (m Model) HistoryGeneration() uint64 { return m.historyGeneration }

func (m Model) editState() editState {
	return editState{m.Value(), m.CursorOffset(), m.anchor, m.lastCharOffset, m.ScrollOffset(), m.selecting}
}

// beginEdit groups nested mutations (notably selection replacement) into one
// action. Immutable strings keep snapshots independent of the mutable rune grid.
func (m *Model) beginEdit() func() {
	if m.editing {
		return func() {}
	}
	m.startEdit(isolatedEdit)
	return m.finishEdit
}

func (m *Model) startEdit(kind editKind) {
	m.editing, m.editRecorded = true, false
	// Save only positions until a mutation is validated. Navigation and rejected
	// edits never serialize the draft, and nested edits retain the original range.
	m.editBefore = editState{"", m.CursorOffset(), m.anchor, m.lastCharOffset, m.ScrollOffset(), m.selecting}
	if m.HasSelection() {
		kind = isolatedEdit
	}
	m.editKind = kind
	if kind == isolatedEdit || kind != m.history.kind {
		m.BreakUndoGroup()
	}
}

// recordEdit is called immediately before the first accepted text mutation.
// A joined group needs no text snapshot and no after-edit serialization.
func (m *Model) recordEdit(runes int) {
	if m.editRecorded {
		return
	}
	now := time.Now()
	before := m.editBefore
	group := m.editKind != isolatedEdit && !m.history.editedAt.IsZero() &&
		now.Sub(m.history.editedAt) < undoGroupDelay && before.caret == m.history.caret &&
		m.history.runes+runes <= maxUndoGroupRunes
	if !group {
		before.text = m.Value()
		m.history.undo = append(m.history.undo, before)
		m.history.runes = 0
	}
	m.history.redo = nil
	m.history.editedAt, m.history.kind = now, m.editKind
	m.history.runes += runes
	m.editRecorded = true
	m.trimHistory()
}

func (m *Model) finishEdit() {
	if m.editRecorded {
		m.history.caret = m.CursorOffset()
	}
	m.editing, m.editRecorded = false, false
	m.editBefore = editState{}
}

func (m *Model) trimHistory() {
	// ponytail: bounded full-text snapshots; use deltas only if large drafts
	// need deeper history. Include state overhead and release evicted strings.
	bytes := 0
	for _, stack := range [][]editState{m.history.undo, m.history.redo} {
		for _, state := range stack {
			bytes += len(state.text) + 64
		}
	}
	for len(m.history.undo)+len(m.history.redo) > maxUndoSteps || bytes > maxUndoBytes {
		stack := &m.history.undo
		if len(*stack) == 0 {
			stack = &m.history.redo
		}
		bytes -= len((*stack)[0].text) + 64
		(*stack)[0] = editState{}
		*stack = (*stack)[1:]
	}
}

func (m *Model) restoreEdit(state editState) {
	lines := strings.Split(state.text, "\n")
	m.value = make([][]rune, len(lines))
	for i, line := range lines {
		m.value[i] = []rune(line)
	}
	m.setOffset(state.caret)
	m.anchor, m.selecting, m.lastCharOffset = state.anchor, state.selecting, state.column
	m.viewport.YOffset = state.scroll
	m.repositionView()
	m.Err = nil
	m.Cursor.Blink = false
}

// Undo restores an edit in this draft only. Empty history is a quiet no-op.
func (m *Model) Undo() bool { return m.restoreHistory(false) }

// Redo reapplies an undone edit without repeating any external side effects.
func (m *Model) Redo() bool { return m.restoreHistory(true) }

func (m *Model) restoreHistory(redo bool) bool {
	m.historyGeneration++
	m.BreakUndoGroup()
	from, to := &m.history.undo, &m.history.redo
	if redo {
		from, to = to, from
	}
	if len(*from) == 0 {
		return false
	}
	state := (*from)[len(*from)-1]
	(*from)[len(*from)-1] = editState{}
	*from = (*from)[:len(*from)-1]
	*to = append(*to, m.editState())
	m.restoreEdit(state)
	m.trimHistory()
	return true
}

// BreakUndoGroup separates edits across navigation and focus/selection changes.
func (m *Model) BreakUndoGroup() { m.history.editedAt = time.Time{} }

// BreakUndoGroupForKey also covers keys consumed by a surrounding UI. Editing
// compatibility belongs here so callers don't split repeated deletion runs.
func (m *Model) BreakUndoGroupForKey(k tea.KeyMsg) {
	if kind := m.groupingKind(k); kind == isolatedEdit || kind != m.history.kind {
		m.BreakUndoGroup()
	}
}

// RetainsText includes undo/redo states so callers can retain attachment metadata
// exactly as long as a label can be restored, without owning another edit stack.
func (m Model) RetainsText(text string) bool {
	if strings.Contains(m.Value(), text) {
		return true
	}
	for _, stack := range [][]editState{m.history.undo, m.history.redo} {
		for _, state := range stack {
			if strings.Contains(state.text, text) {
				return true
			}
		}
	}
	return false
}

func (m Model) groupingKind(k tea.KeyMsg) editKind {
	if k.Paste {
		return isolatedEdit
	}
	switch {
	case key.Matches(k, m.KeyMap.DeleteCharacterBackward):
		return backwardDelete
	case key.Matches(k, m.KeyMap.DeleteCharacterForward):
		return forwardDelete
	case key.Matches(k, m.KeyMap.DeleteWordBackward):
		return backwardWordDelete
	case key.Matches(k, m.KeyMap.DeleteWordForward):
		return forwardWordDelete
	}
	if k.Type == tea.KeyRunes && !k.Alt && len(k.Runes) > 0 {
		for _, r := range k.Runes {
			if unicode.IsSpace(r) {
				return isolatedEdit
			}
		}
		return typingEdit
	}
	return isolatedEdit
}
