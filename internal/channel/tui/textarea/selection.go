package textarea

import (
	"strings"
	"unicode"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/rivo/uniseg"
)

// VisualRow links displayed cells to logical rune offsets. End excludes the
// real newline and the synthetic cursor cell; soft wraps add no source text.
type VisualRow struct {
	Start, End int
	Text       string
}

func (m Model) VisualRows() []VisualRow {
	var rows []VisualRow
	offset := 0
	for _, line := range m.value {
		col := 0
		for _, wrapped := range m.memoizedWrap(line, m.width) {
			end := min(len(line), col+len(wrapped))
			rows = append(rows, VisualRow{offset + col, offset + end, string(line[col:end])})
			col = end
		}
		offset += len(line) + 1
	}
	return rows
}

func (m Model) ScrollOffset() int { return m.viewport.YOffset }
func (m *Model) ScrollBy(rows int) bool {
	if rows != 0 {
		m.BreakUndoGroup()
	}
	before := m.viewport.YOffset
	// Use actual logical layout, not View's decorative end-of-buffer rows.
	m.viewport.YOffset = clamp(before+rows, 0, max(0, len(m.VisualRows())-m.height))
	return before != m.viewport.YOffset
}

func (m Model) CursorOffset() int {
	offset := m.col
	for _, line := range m.value[:m.row] {
		offset += len(line) + 1
	}
	return offset
}

func (m *Model) setOffset(offset int) {
	offset = max(0, offset)
	for row, line := range m.value {
		if offset <= len(line) || row == len(m.value)-1 {
			m.row = row
			m.SetCursor(offset)
			return
		}
		offset -= len(line) + 1
	}
}

func (m *Model) ClearSelection() {
	if !m.editing {
		m.BreakUndoGroup()
	}
	m.selecting = false
}
func (m Model) HasSelection() bool { return m.selecting && m.anchor != m.CursorOffset() }
func (m Model) SelectionRange() (int, int) {
	end := m.CursorOffset()
	if !m.selecting {
		return end, end
	}
	return min(m.anchor, end), max(m.anchor, end)
}
func (m Model) SelectedText() string {
	start, end := m.SelectionRange()
	text := []rune(m.Value())
	return string(text[min(start, len(text)):min(end, len(text))])
}
func (m *Model) Select(anchor, caret int) {
	m.BreakUndoGroup()
	m.setOffset(anchor)
	m.anchor = m.CursorOffset()
	m.selecting = true
	m.setOffset(caret)
	m.repositionView()
}
func (m *Model) SelectAll() { m.Select(0, len([]rune(m.Value()))) }

// ReplaceRange atomically replaces a logical range and maps the existing caret
// and selection through the edit. Async preparation must not steal navigation.
func (m *Model) ReplaceRange(start, end int, replacement string) bool {
	defer m.beginEdit()()
	text := []rune(m.Value())
	start = graphemeBoundaryAtOrBefore(text, start)
	end = graphemeBoundaryAtOrBefore(text, max(start, end))
	next := *m
	next.SetValue(string(text[:start]) + replacement + string(text[end:]))
	if next.Err != nil {
		m.Err = next.Err
		return false
	}
	if next.Value() == string(text) {
		return true
	}
	m.recordEdit(end - start + len([]rune(replacement)))
	next.history = m.history
	next.editRecorded = m.editRecorded
	next.historyGeneration = m.historyGeneration
	delta := len([]rune(next.Value())) - len(text)
	mapOffset := func(at int) int {
		if at <= start {
			return at
		}
		if at <= end {
			return end + delta
		}
		return at + delta
	}
	next.setOffset(mapOffset(m.anchor))
	next.anchor = next.CursorOffset()
	next.selecting = m.selecting
	next.setOffset(mapOffset(m.CursorOffset()))
	next.viewport.YOffset = m.viewport.YOffset
	row := m.cursorLineNumber()
	if row >= m.viewport.YOffset && row < m.viewport.YOffset+m.height {
		next.repositionView()
	} else {
		// Preserve deliberate wheel scrolling during asynchronous preparation.
		next.ScrollBy(0)
	}
	*m = next
	return true
}

// Hit returns the nearest insertion boundary in a visible row. Wide emoji,
// combining sequences, and tabs are indivisible for mouse and keyboard alike.
func (m Model) Hit(x, y int) int {
	at, _ := m.hit(x, y, false)
	return at
}

// CharacterAt returns the grapheme under the pointer, excluding row-end fill
// and the synthetic cursor cell. Both halves of a wide character hit its start.
func (m Model) CharacterAt(x, y int) (int, bool) { return m.hit(x, y, true) }

func (m Model) hit(x, y int, character bool) (int, bool) {
	rows := m.VisualRows()
	if character && (x < 0 || x >= m.width || y < 0 || y >= m.height || y+m.viewport.YOffset >= len(rows)) {
		return 0, false
	}
	row := rows[clamp(y+m.viewport.YOffset, 0, len(rows)-1)]
	offset, cell := row.Start, 0
	clusters := uniseg.NewGraphemes(row.Text)
	for clusters.Next() {
		w := textWidth(clusters.Str())
		edge := (w + 1) / 2
		if character {
			edge = w
		}
		if x < cell+edge {
			return offset, true
		}
		offset += len(clusters.Runes())
		cell += w
	}
	return row.End, false
}

// LineRange returns a logical line's rune bounds, excluding its newline.
func LineRange(text []rune, at int) (int, int) {
	at = clamp(at, 0, len(text))
	start, end := at, at
	for start > 0 && text[start-1] != '\n' {
		start--
	}
	for end < len(text) && text[end] != '\n' {
		end++
	}
	return start, end
}

// WordRange uses the editor's whitespace-delimited grapheme words: punctuation
// stays with its word; whitespace selects its run within the logical line.
func WordRange(text []rune, at int) (int, int) {
	start, end := LineRange(text, at)
	if at < start || at >= end {
		return end, end
	}
	// Scan once: a word may span many wrapped rows. Repeatedly looking up a
	// grapheme from the beginning would make long unbroken text quadratic.
	clusters := uniseg.NewGraphemes(string(text[start:end]))
	offset, previousSpace := start, false
	for clusters.Next() {
		space := unicode.IsSpace(clusters.Runes()[0])
		if offset > start && space != previousSpace {
			if at < offset {
				return start, offset
			}
			start = offset
		}
		offset += len(clusters.Runes())
		previousSpace = space
	}
	return start, end
}

func (m *Model) DeleteSelection() bool {
	defer m.beginEdit()()
	if !m.HasSelection() {
		// A click/Shift round trip can leave an empty anchor. An edit consumes
		// that anchor too, or its inserted text becomes the next selection.
		m.ClearSelection()
		return false
	}
	start, end := m.SelectionRange()
	m.recordEdit(end - start)
	text := []rune(m.Value())
	text = append(text[:start], text[end:]...)
	lines := strings.Split(string(text), "\n")
	m.value = make([][]rune, len(lines))
	for i, line := range lines {
		m.value[i] = []rune(line)
	}
	m.selecting = false
	m.setOffset(start)
	m.repositionView()
	return true
}

func (m *Model) selectionKey(k tea.KeyMsg) (tea.KeyMsg, bool) {
	if k.Paste {
		return k, false
	}
	shift := map[tea.KeyType]tea.KeyType{
		tea.KeyShiftLeft: tea.KeyLeft, tea.KeyShiftRight: tea.KeyRight,
		tea.KeyShiftUp: tea.KeyUp, tea.KeyShiftDown: tea.KeyDown,
		tea.KeyShiftHome: tea.KeyHome, tea.KeyShiftEnd: tea.KeyEnd,
		tea.KeyCtrlShiftLeft: tea.KeyCtrlLeft, tea.KeyCtrlShiftRight: tea.KeyCtrlRight,
		tea.KeyCtrlShiftHome: tea.KeyCtrlHome, tea.KeyCtrlShiftEnd: tea.KeyCtrlEnd,
	}
	if base, ok := shift[k.Type]; ok {
		if !m.selecting {
			m.anchor = m.CursorOffset()
			m.selecting = true
		}
		k.Type = base
		return k, false
	}
	if key.Matches(k, m.KeyMap.DeleteCharacterBackward, m.KeyMap.DeleteCharacterForward, m.KeyMap.DeleteWordBackward, m.KeyMap.DeleteWordForward, m.KeyMap.DeleteBeforeCursor, m.KeyMap.DeleteAfterCursor) && m.DeleteSelection() {
		return k, true
	}
	if key.Matches(k, m.KeyMap.TransposeCharacterBackward, m.KeyMap.LowercaseWordForward, m.KeyMap.UppercaseWordForward, m.KeyMap.CapitalizeWordForward) {
		m.ClearSelection()
	}
	if key.Matches(k, m.KeyMap.CharacterBackward, m.KeyMap.CharacterForward, m.KeyMap.LinePrevious, m.KeyMap.LineNext, m.KeyMap.LineStart, m.KeyMap.LineEnd, m.KeyMap.InputBegin, m.KeyMap.InputEnd, m.KeyMap.WordBackward, m.KeyMap.WordForward) {
		start, end := m.SelectionRange()
		selected := m.HasSelection()
		m.ClearSelection()
		if selected && (k.Type == tea.KeyLeft || k.Type == tea.KeyRight) {
			if k.Type == tea.KeyLeft {
				m.setOffset(start)
			} else {
				m.setOffset(end)
			}
			return k, true
		}
	}
	if k.Type == tea.KeyEsc {
		m.ClearSelection()
		return k, true
	}
	return k, false
}

func (m *Model) moveWord(direction int) {
	// Word boundaries are whitespace-delimited extended grapheme clusters,
	// including real newlines. This also terminates at empty buffer edges.
	text := []rune(m.Value())
	offset := m.CursorOffset()
	space := func(i int) bool { return unicode.IsSpace(text[i]) }
	if direction < 0 {
		for offset > 0 && space(offset-1) {
			offset = graphemeBoundaryAtOrBefore(text, offset-1)
		}
		for offset > 0 && !space(offset-1) {
			offset = graphemeBoundaryAtOrBefore(text, offset-1)
		}
	} else {
		for offset < len(text) && space(offset) {
			_, offset = graphemeRangeAt(text, offset)
		}
		for offset < len(text) && !space(offset) {
			_, offset = graphemeRangeAt(text, offset)
		}
	}
	m.setOffset(offset)
}

func displayText(text string) string { return strings.ReplaceAll(text, "\t", "    ") }
func textWidth(text string) int      { return uniseg.StringWidth(displayText(text)) }

func (m Model) renderSelected(text []rune, offset int, style lipgloss.Style, cursorAt int) string {
	start, end := m.SelectionRange()
	selected := m.HasSelection() && start < offset+len(text) && end > offset
	if !m.Mask && !selected && cursorAt < 0 {
		return style.Render(displayText(string(text)))
	}
	var out, run strings.Builder
	runSelected := false
	flush := func() {
		if run.Len() == 0 {
			return
		}
		if runSelected {
			out.WriteString(m.SelectionStyle.Render(run.String()))
		} else {
			out.WriteString(style.Render(run.String()))
		}
		run.Reset()
	}
	clusters := uniseg.NewGraphemes(string(text))
	col := 0
	for clusters.Next() {
		value := displayText(clusters.Str())
		if m.Mask {
			value = strings.Repeat("*", textWidth(clusters.Str()))
		}
		count := len(clusters.Runes())
		inSelection := selected && offset+col >= start && offset+col < end
		if col == cursorAt && !inSelection {
			flush()
			m.Cursor.SetChar(value)
			out.WriteString(m.Cursor.View())
		} else {
			if inSelection != runSelected {
				flush()
				runSelected = inSelection
			}
			run.WriteString(value)
		}
		col += count
	}
	flush()
	return out.String()
}

func (m *Model) moveVertical(direction int) {
	rows := m.VisualRows()
	row := m.cursorLineNumber()
	if row+direction < 0 {
		m.moveToBegin()
		return
	}
	if row+direction >= len(rows) {
		m.moveToEnd()
		return
	}
	column := max(m.lastCharOffset, m.LineInfo().CharOffset)
	at := m.Hit(column, row+direction-m.viewport.YOffset)
	m.setOffset(at)
	m.lastCharOffset = column
}
