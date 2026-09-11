package tui

import (
	"encoding/base64"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/agent0ai/spynel/internal/channel/tui/textarea"
	"github.com/agent0ai/spynel/internal/markdown"
	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/rivo/uniseg"
)

type transcriptRender struct {
	label   string
	layout  markdown.Layout
	spinner string
	view    string
	source  string
}
type textPoint struct{ entry, offset int }
type outputSelection struct {
	active, labels bool
	anchor, caret  textPoint
}
type pane uint8

const (
	noPane pane = iota
	outputPane
	inputPane
	formPane
)

type dragSelection struct {
	pane       pane
	x, y       int
	ticking    bool
	rangeAt    func([]rune, int) (int, int)
	start, end textPoint // Original unit stays intact when reversing the drag.
}
type dragTick struct{ generation uint64 }

type clickSequence struct {
	pane     pane
	x, y     int
	count    int
	at       time.Time
	released bool
}

func (m model) nextClick(which pane, event tea.MouseMsg) clickSequence {
	now := time.Now()
	if m.now != nil {
		now = m.now()
	}
	previous := m.clicks
	count := 1
	if previous.released && previous.pane == which && previous.y == event.Y &&
		event.X >= previous.x-1 && event.X <= previous.x+1 &&
		now.Sub(previous.at) >= 0 && now.Sub(previous.at) <= 500*time.Millisecond && previous.count < 3 {
		count = previous.count + 1
	}
	return clickSequence{pane: which, x: event.X, y: event.Y, count: count, at: now}
}

type clipboardResult struct {
	status   string
	fallback bool
}
type clipboardPaste struct {
	text       string
	err        error
	generation uint64
}

// OSC writes and renderer writes share one lock; control strings can never
// bisect a frame's ANSI sequences. Payloads are base64, never interpolated raw.
// Retain the terminal file methods so Tea can discover size and watch SIGWINCH.
// Embed only term.File: os.File's WriteString/ReadFrom would bypass the lock.
type terminalOutput struct {
	sync.Mutex
	term.File
}

func (w *terminalOutput) Write(p []byte) (int, error) {
	w.Lock()
	defer w.Unlock()
	return w.File.Write(p)
}
func clipboardOSC(text string) string {
	return "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\a"
}

func (m model) transcriptRender(entry transcriptEntry) transcriptRender {
	label := entry.role
	logical := stripUnsafeTerminalControls(entry.text)
	switch entry.role {
	case "user":
		label = userChatLabel
	case "assistant":
		label = agentChatLabel
		logical = renderAgentMarkdownText(logical, m.chatMarkdownWidth(), m.activeTheme)
	case "error":
		label = errorChatLabel
	}
	entryRender := transcriptRender{source: entry.text, label: label, layout: markdown.WrapLogical(logical, m.chatContentWidth())}
	entryRender.view = m.transcriptView(entryRender)
	return entryRender
}

func (m model) transcriptView(entry transcriptRender) string {
	if entry.view != "" {
		return entry.view
	}
	content := entry.layout.View() + entry.spinner
	if entry.label == "" {
		return content
	}
	style := m.styles.agent
	if entry.label == userChatLabel {
		style = m.styles.user
	} else if entry.label == errorChatLabel {
		style = m.styles.error
	}
	return m.renderChatMessageContent(entry.label, style, content, false)
}

func pointBefore(a, b textPoint) bool {
	return a.entry < b.entry || a.entry == b.entry && a.offset < b.offset
}
func (s outputSelection) bounds() (textPoint, textPoint) {
	if pointBefore(s.caret, s.anchor) {
		return s.caret, s.anchor
	}
	return s.anchor, s.caret
}

func (m model) selectedOutput() string {
	if !m.selection.active {
		return ""
	}
	a, b := m.selection.bounds()
	if a.entry < 0 || b.entry >= len(m.output) {
		return ""
	}
	var parts []string
	for i := a.entry; i <= b.entry; i++ {
		entry := m.output[i]
		runes := []rune(entry.layout.Text)
		start, end := 0, len(runes)
		if !m.selection.labels {
			if i == a.entry {
				start = min(a.offset, end)
			}
			if i == b.entry {
				end = min(b.offset, end)
			}
		}
		text := string(runes[start:end])
		if m.selection.labels && entry.label != "" {
			text = entry.label + strings.Repeat(" ", max(1, chatContentColumn-uniseg.StringWidth(entry.label))) + strings.ReplaceAll(text, "\n", "\n"+strings.Repeat(" ", chatContentColumn))
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n\n")
}

// selectedHistoryView paints only real selected cells. Borders, scrollbar,
// row-end fill, and the activity spinner have no text offsets.
func (m model) selectedHistoryView() string {
	if !m.selection.active {
		return m.viewport.View()
	}
	a, b := m.selection.bounds()
	lines := strings.Split(m.viewport.View(), "\n")
	rowNumber := 0
	for index, entry := range m.output {
		for rowIndex, row := range entry.layout.Rows {
			visible := rowNumber - m.viewport.YOffset
			rowNumber++
			if visible < 0 || visible >= len(lines) || index < a.entry || index > b.entry {
				continue
			}
			start, end := row.Start, row.End
			if !m.selection.labels {
				if index == a.entry {
					start = max(start, a.offset)
				}
				if index == b.entry {
					end = min(end, b.offset)
				}
			}
			prefix := chatContentColumn
			if entry.label == "" {
				prefix = 0
			}
			original := lines[visible]
			var out strings.Builder
			at := 0
			paint := func(x1, x2 int) {
				if x2 <= x1 {
					return
				}
				out.WriteString(ansi.Cut(original, at, x1))
				out.WriteString(m.styles.selectedCommand.Render(ansi.Strip(ansi.Cut(original, x1, x2))))
				at = x2
			}
			if m.selection.labels {
				paint(0, prefix)
			}
			spanStart, spanEnd := -1, 0
			for _, cell := range row.Cells {
				if cell.Start < cell.End && cell.Start >= start && cell.End <= end {
					if spanStart < 0 {
						spanStart = prefix + cell.X
					}
					spanEnd = prefix + cell.X + cell.Width
				} else if spanStart >= 0 {
					paint(spanStart, spanEnd)
					spanStart = -1
				}
			}
			if spanStart >= 0 {
				paint(spanStart, spanEnd)
			}
			out.WriteString(ansi.Cut(original, at, ansi.StringWidth(original)))
			lines[visible] = out.String()

			_ = rowIndex
		}
		rowNumber++ // the real blank row between messages
	}
	return strings.Join(lines, "\n")
}

type paneBounds struct{ x, y, width, height int }

func (b paneBounds) contains(x, y int) bool {
	return x >= b.x && x < b.x+b.width && y >= b.y && y < b.y+b.height
}
func (m model) bounds(which pane) paneBounds {
	if which == outputPane {
		if m.height > 0 && m.height < layoutOverhead+minComposerHeight+1 {
			return paneBounds{}
		}
		return paneBounds{0, 2, max(0, m.width-1), m.viewport.Height}
	}
	_, inset := borderedContentGeometry(m.width)
	y := m.viewport.Height + 4 + m.inlineMenuHeight()
	if m.height > 0 && m.height < layoutOverhead+minComposerHeight+1 {
		y = 2
	}
	return paneBounds{1 + inset, y, m.inputWidth, m.composerRows}
}

func (m model) outputHit(x, y int) textPoint {
	at, _ := m.outputPosition(x, y, false)
	return at
}

// Character hits exclude labels, padding, inter-message gaps and row-end fill;
// insertion hits retain the existing nearest-boundary behavior for dragging.
func (m model) outputPosition(x, y int, character bool) (textPoint, bool) {
	row := bounded(y-m.bounds(outputPane).y, 0, max(0, m.viewport.Height-1)) + m.viewport.YOffset
	for i, entry := range m.output {
		if row < len(entry.layout.Rows) {
			if character && row < 0 {
				return textPoint{}, false
			}
			r := entry.layout.Rows[max(0, row)]
			cell := 1
			if entry.label != "" {
				cell += chatContentColumn
			}
			for _, c := range r.Cells {
				if character {
					if x >= cell+c.X && x < cell+c.X+c.Width && c.Start < c.End {
						return textPoint{i, c.Start}, true
					}
				} else if x < cell+c.X+(c.Width+1)/2 {
					return textPoint{i, c.Start}, true
				}
			}

			return textPoint{i, r.End}, false
		}
		row -= len(entry.layout.Rows) + 1
	}
	if len(m.output) == 0 {
		return textPoint{}, false
	}
	last := len(m.output) - 1
	return textPoint{last, len([]rune(m.output[last].layout.Text))}, false
}

func (m *model) cancelDrag() {
	m.drag = dragSelection{}
	m.screenDrag = nil
	m.dragGeneration++
	m.clicks = clickSequence{}
}
func (m *model) clearSelections() {
	m.cancelDrag()
	m.selection = outputSelection{}
	m.input.ClearSelection()
	m.outputFocus = false
	m.input.Focus()
}

func (m *model) handleMouse(event tea.MouseMsg) tea.Cmd {
	m.editorNotice = ""
	if m.dialog != nil {
		m.cancelDrag()
		return m.handleDialogMouse(event)
	}
	if m.screen != nil {
		return m.handleFormMouse(event)
	}
	if event.Action == tea.MouseActionRelease {
		clicks := m.clicks
		if m.drag.pane != noPane && (event.X != m.drag.x || event.Y != m.drag.y) {
			m.drag.x, m.drag.y = event.X, event.Y
			m.extendDrag()
		}
		m.cancelDrag()
		if (event.Button == tea.MouseButtonLeft || event.Button == tea.MouseButtonNone) &&
			clicks.count > 0 && event.X == clicks.x && event.Y == clicks.y {
			clicks.released = true
			m.clicks = clicks
		}
		return nil
	}
	if event.Button == tea.MouseButtonNone && event.Action == tea.MouseActionMotion {
		m.cancelDrag()
		return nil
	}
	which := noPane
	if m.bounds(outputPane).contains(event.X, event.Y) {
		which = outputPane
	} else if m.bounds(inputPane).contains(event.X, event.Y) {
		which = inputPane
	}
	if event.Button == tea.MouseButtonWheelUp || event.Button == tea.MouseButtonWheelDown {
		m.clicks = clickSequence{}
		direction := 3
		if event.Button == tea.MouseButtonWheelUp {
			direction = -3
		}
		if which == inputPane {
			m.input.ScrollBy(direction)
		} else if which == outputPane {
			m.viewport.SetYOffset(m.viewport.YOffset + direction)
			if direction < 0 {
				m.noteManualScrollUp()
			} else {
				m.resumeTailFollowAtBottom()
			}
		}
		if m.drag.pane != noPane {
			m.extendDrag()
		}
		return nil
	}
	if event.Button != tea.MouseButtonLeft {
		m.clicks = clickSequence{}
		return nil
	}
	if event.Action == tea.MouseActionPress {
		if which == noPane {
			m.clearSelections()
			return nil
		}
		if event.Shift && (which == inputPane && !m.outputFocus || which == outputPane && m.selection.active) {
			if which == inputPane && !m.input.HasSelection() {
				at := m.input.CursorOffset()
				m.input.Select(at, at)
			}
			m.cancelDrag()
			m.drag = dragSelection{pane: which, x: event.X, y: event.Y}
			m.extendDrag()
			return m.scheduleDrag()
		}
		// Resolve actual characters before focus/selection can reveal the caret
		// and change a deliberately scrolled composer viewport.
		var hit textPoint
		var content bool
		if which == inputPane {
			b := m.bounds(inputPane)
			hit.offset, content = m.input.CharacterAt(event.X-b.x, event.Y-b.y)
		} else {
			hit, content = m.outputPosition(event.X, event.Y, true)
		}
		clicks := clickSequence{}
		if content && !event.Shift && !event.Alt && !event.Ctrl {
			clicks = m.nextClick(which, event)
		}
		m.clearSelections()
		m.clicks = clicks
		m.drag = dragSelection{pane: which, x: event.X, y: event.Y}
		if which == outputPane {
			m.outputFocus = true
			m.input.Blur()
			hit := m.outputHit(event.X, event.Y)
			m.selection = outputSelection{active: true, labels: event.X < 1+chatContentColumn, anchor: hit, caret: hit}
		} else {
			b := m.bounds(inputPane)
			at := m.input.Hit(event.X-b.x, event.Y-b.y)
			m.input.Select(at, at)
		}
		if clicks.count >= 2 {
			var text string
			if which == outputPane {
				text = m.output[hit.entry].layout.Text
			} else {
				text = m.input.Value()
			}
			rangeAt := textarea.WordRange
			if clicks.count == 3 {
				rangeAt = textarea.LineRange
			}
			start, end := rangeAt([]rune(text), hit.offset)
			m.drag.rangeAt = rangeAt
			m.drag.start, m.drag.end = textPoint{hit.entry, start}, textPoint{hit.entry, end}
			if which == outputPane {
				m.selection = outputSelection{active: true, anchor: textPoint{hit.entry, start}, caret: textPoint{hit.entry, end}}
			} else {
				m.input.Select(start, end)
			}
		}
	} else if m.drag.pane != noPane {
		if event.X == m.drag.x && event.Y == m.drag.y {
			return nil
		}
		m.clicks = clickSequence{}
		m.drag.x, m.drag.y = event.X, event.Y
		m.extendDrag()
	}
	if which == inputPane && event.Action == tea.MouseActionPress {
		m.refreshPreservingHistory()
	}
	return m.scheduleDrag()
}

func (m *model) extendDrag() {
	if m.drag.rangeAt != nil {
		b := m.bounds(m.drag.pane)
		x, y := m.drag.x, bounded(m.drag.y, b.y, b.y+b.height-1)
		var at textPoint
		var text string
		var character bool
		left := b.x
		if m.drag.pane == outputPane {
			at, character = m.outputPosition(x, y, true)
			if !character {
				at = m.outputHit(x, y)
			}
			text = m.output[at.entry].layout.Text
			left = 1
			if m.output[at.entry].label != "" {
				left += chatContentColumn
			}
		} else {
			at.offset, character = m.input.CharacterAt(x-b.x, y-b.y)
			if !character {
				at.offset = m.input.Hit(x-b.x, y-b.y)
			}
			text = m.input.Value()
		}
		runes := []rune(text)
		// Row-end insertion hits belong to the preceding unit, including a
		// word spanning a soft wrap. Left-edge hits belong to the next unit.
		if !character && x > left && at.offset > 0 && runes[at.offset-1] != '\n' {
			at.offset--
		}
		start, end := m.drag.rangeAt(runes, at.offset)
		a, c := m.drag.start, textPoint{at.entry, end}
		if pointBefore(textPoint{at.entry, start}, a) {
			a, c = m.drag.end, textPoint{at.entry, start}
		} else if pointBefore(c, m.drag.end) {
			c = m.drag.end
		}
		if m.drag.pane == outputPane {
			m.selection.anchor, m.selection.caret = a, c
		} else {
			m.input.Select(a.offset, c.offset)
		}
		return
	}
	if m.drag.pane == outputPane {
		m.selection.caret = m.outputHit(m.drag.x, m.drag.y)
	} else if m.drag.pane == inputPane {
		b := m.bounds(inputPane)
		at := m.input.Hit(m.drag.x-b.x, bounded(m.drag.y-b.y, 0, b.height-1))
		a, _ := m.input.SelectionRange()
		if m.input.CursorOffset() == a {
			_, a = m.input.SelectionRange()
		}
		m.input.Select(a, at)
	}
}
func (m *model) scheduleDrag() tea.Cmd {
	if m.drag.pane == noPane {
		return nil
	}
	b := m.bounds(m.drag.pane)
	if m.drag.y >= b.y && m.drag.y < b.y+b.height {
		if m.drag.ticking {
			m.dragGeneration++
			m.drag.ticking = false
		}
		return nil
	}
	if m.drag.ticking {
		return nil
	}
	m.drag.ticking = true
	generation := m.dragGeneration
	return tea.Tick(50*time.Millisecond, func(time.Time) tea.Msg { return dragTick{generation} })
}
func (m *model) autoScroll(tick dragTick) tea.Cmd {
	if tick.generation != m.dragGeneration || m.drag.pane == noPane || !m.drag.ticking {
		return nil
	}
	m.drag.ticking = false
	b := m.bounds(m.drag.pane)
	direction := 0
	if m.drag.y < b.y {
		direction = -min(3, b.y-m.drag.y)
	} else if m.drag.y >= b.y+b.height {
		direction = min(3, m.drag.y-b.y-b.height+1)
	}
	if direction == 0 {
		return nil
	}
	changed := false
	if m.drag.pane == inputPane {
		changed = m.input.ScrollBy(direction)
	} else {
		before := m.viewport.YOffset
		m.viewport.SetYOffset(before + direction)
		changed = before != m.viewport.YOffset
	}
	m.extendDrag()
	if !changed {
		return nil
	}
	return m.scheduleDrag()
}

func (m *model) copySelection(text string) tea.Cmd {
	m.clipboardText = text
	m.clipboardFallback = true
	write := m.writeClipboard
	return func() tea.Msg {
		// The terminal may live on a different host even when a native helper
		// succeeds (docker exec, SSH forwarding). Always offer OSC 52 to it.
		transportOK := false
		if write != nil {
			_, err := io.WriteString(write, clipboardOSC(text))
			transportOK = err == nil
		}
		if os.Getenv("SSH_CONNECTION") == "" && os.Getenv("SSH_TTY") == "" {
			if err := clipboard.WriteAll(text); err == nil {
				return clipboardResult{}
			}
		}
		if transportOK {
			return clipboardResult{fallback: true} // OSC 52 has no acknowledgement.
		}
		return clipboardResult{status: "Clipboard unavailable; F6 opens terminal copy, Ctrl+V pastes here", fallback: true}
	}
}

func (m *model) handleSelectionKey(k tea.KeyMsg) (bool, tea.Cmd) {
	selected := m.input.SelectedText()
	if m.outputFocus {
		selected = m.selectedOutput()
	}
	if k.Type == tea.KeyCtrlC && selected != "" {
		m.cancelDrag()
		return true, m.copySelection(selected)
	}
	if k.Type == tea.KeyF6 {
		m.cancelDrag()
		if selected == "" {
			m.editorNotice = "Select text first, then F6 for terminal copy"
			return true, nil
		}
		return true, tea.Exec(&terminalCopy{ctx: m.ctx, text: selected}, func(err error) tea.Msg { return terminalCopyResult{err} })
	}
	if k.Type == tea.KeyCtrlX {
		if !m.outputFocus && selected != "" {
			cmd := m.copySelection(selected)
			m.input.DeleteSelection()
			m.pruneTokens()
			m.syncCommandMenu()
			return true, cmd
		}
		return true, nil // transcript is always read-only; no selection is a no-op
	}
	if k.Type == tea.KeyCtrlV {
		m.cancelDrag()
		m.outputFocus = false
		m.selection = outputSelection{}
		m.input.Focus()
		if m.clipboardFallback {
			return true, m.enqueuePaste(m.clipboardText)
		}
		generation := m.input.HistoryGeneration()
		return true, func() tea.Msg { value, err := clipboard.ReadAll(); return clipboardPaste{value, err, generation} }
	}
	if k.Type == tea.KeyEsc && (m.selection.active || m.input.HasSelection() || m.drag.pane != noPane) {
		m.clearSelections()
		m.refreshPreservingHistory()
		return true, nil
	}
	if !k.Paste && (k.Type == tea.KeyCtrlZ || k.Type == tea.KeyCtrlY) {
		m.cancelDrag()
		if !m.outputFocus && !m.themeMenu {
			m.cancelPasteWork()
			if k.Type == tea.KeyCtrlZ {
				m.input.Undo()
			} else {
				m.input.Redo()
			}
			m.pruneTokens()
			m.syncCommandMenu()
		}
		return true, nil
	}
	if !m.outputFocus && k.Type == tea.KeyCtrlA {
		m.input.SelectAll()
		return true, nil
	}
	if m.outputFocus && k.Type == tea.KeyCtrlA {
		if len(m.output) > 0 {
			m.selection = outputSelection{active: true, anchor: textPoint{}, caret: textPoint{len(m.output) - 1, len([]rune(m.output[len(m.output)-1].layout.Text))}}
		}
		return true, nil
	}
	if m.outputFocus && k.Type != tea.KeyPgUp && k.Type != tea.KeyPgDown && !(k.Alt && (k.Type == tea.KeyUp || k.Type == tea.KeyDown)) {
		m.clearSelections()
		m.refreshPreservingHistory()
	}
	return false, nil
}
