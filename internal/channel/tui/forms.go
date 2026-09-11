package tui

import (
	"strings"

	"github.com/agent0ai/spynel/internal/channel/tui/textarea"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/cursor"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type formHit struct{ index, row, width, valueX int }
type formLayout struct {
	content       string
	offset, total int
	hits          []formHit
}
type formDrag struct {
	index, anchor, start, end int
	rangeAt                   func([]rune, int) (int, int)
}
type formPaste struct {
	text                         string
	err                          error
	generation, screenGeneration uint64
	index                        int
}

func (m model) formBounds() paneBounds {
	fixed := 0
	if m.screen != nil {
		if strings.TrimSpace(m.screen.Title) != "" {
			fixed += 2
		}
		if len(m.screen.Tabs) > 0 {
			fixed += 3
		}
	}
	return paneBounds{1, 2 + fixed, max(1, m.width-3), max(1, max(5, m.height-2)-2-fixed)}
}

func (m model) formEditor(index, width int) textarea.Model {
	control := m.screen.Controls[index]
	editor, exists := m.screenEditors[index]
	if !exists {
		editor = textarea.New()
		editor.Prompt, editor.Placeholder = "", ""
		editor.ShowLineNumbers = false
		editor.CharLimit = 64 * 1024
		editor.MaxWidth = 0
		editor.Cursor.SetMode(cursor.CursorStatic)
		editor.KeyMap.Paste.SetEnabled(false) // Clipboard completion belongs to the current form generation.
	}
	if !exists || editor.Value() != control.Value {
		editor.SetValue(control.Value)
	}
	editor.Mask = control.Secret || control.Kind == "password"
	if editor.Mask && control.Configured && control.Value == "" {
		editor.Placeholder = "(configured)"
	}
	fieldWidth := min(max(1, width/2), max(1, ansi.StringWidth(control.Value)+1))
	if editor.Placeholder != "" {
		fieldWidth = min(max(1, width/2), len(editor.Placeholder)+1)
	}
	editor.SetWidth(fieldWidth)
	editor.SetHeight(1)
	if index == m.screenIndex {
		editor.Focus()
	} else {
		editor.Blur()
	}
	styleComposer(&editor, m.styles)
	return editor
}

func (m *model) storeFormEditor(index int, editor textarea.Model) {
	if m.screenEditors == nil {
		m.screenEditors = map[int]textarea.Model{}
	}
	m.screenEditors[index] = editor
	m.screen.Controls[index].Value = editor.Value()
	if editor.Err != nil {
		m.screen.Status = editor.Err.Error()
	}
}

func (m model) formHasSelection() bool {
	if m.screen == nil || m.screenIndex < 0 || m.screenIndex >= len(m.screen.Controls) {
		return false
	}
	editor, ok := m.screenEditors[m.screenIndex]
	return ok && editor.HasSelection()
}

func (m *model) editScreenText(key tea.KeyMsg) tea.Cmd {
	editor := m.formEditor(m.screenIndex, max(1, m.width-3))
	if key.Type == tea.KeySpace {
		key.Type, key.Runes = tea.KeyRunes, []rune{' '}
	}
	editor.BreakUndoGroupForKey(key)
	m.screenManual = false
	m.screenDrag = nil
	var command tea.Cmd
	switch key.Type {
	case tea.KeyCtrlA:
		editor.SelectAll()
	case tea.KeyCtrlC, tea.KeyCtrlX:
		if editor.HasSelection() {
			command = m.copySelection(editor.SelectedText())
			if key.Type == tea.KeyCtrlX {
				editor.DeleteSelection()
			}
		}
	case tea.KeyF6:
		if editor.HasSelection() {
			text := editor.SelectedText()
			command = tea.Exec(&terminalCopy{ctx: m.ctx, text: text}, func(err error) tea.Msg { return terminalCopyResult{err} })
		}
	case tea.KeyCtrlV:
		if m.clipboardFallback {
			editor.InsertString(m.clipboardText)
		} else {
			generation, screenGeneration, index := editor.HistoryGeneration(), m.screenGeneration, m.screenIndex
			command = func() tea.Msg {
				text, err := clipboard.ReadAll()
				return formPaste{text, err, generation, screenGeneration, index}
			}
		}
	case tea.KeyEnter: // Single-line form controls leave Enter to navigation/actions.
	default:
		editor, _ = editor.Update(key)
	}
	m.storeFormEditor(m.screenIndex, editor)
	return command
}

func (m *model) handleFormMouse(event tea.MouseMsg) tea.Cmd {
	if m.screenSaving || m.screen.ID == core.ScreenWhatsAppQR {
		m.cancelDrag()
		return nil
	}
	bounds := m.formBounds()
	layout := m.layoutScreen(bounds.height, bounds.width)
	if event.Button == tea.MouseButtonWheelUp || event.Button == tea.MouseButtonWheelDown {
		m.cancelDrag()
		if bounds.contains(event.X, event.Y) || event.X == m.width-1 && event.Y >= bounds.y && event.Y < bounds.y+bounds.height {
			delta := 3
			if event.Button == tea.MouseButtonWheelUp {
				delta = -3
			}
			m.screenScroll = bounded(layout.offset+delta, 0, max(0, layout.total-bounds.height))
			m.screenManual = true
		}
		return nil
	}
	if event.Action == tea.MouseActionRelease || event.Action == tea.MouseActionMotion {
		if m.screenDrag != nil && (event.Button == tea.MouseButtonLeft || event.Action == tea.MouseActionRelease) {
			for _, hit := range layout.hits {
				if hit.index == m.screenDrag.index {
					editor := m.formEditor(hit.index, bounds.width)
					at := editor.Hit(event.X-bounds.x-hit.valueX, 0)
					drag := m.screenDrag
					if drag.rangeAt != nil {
						start, end := drag.rangeAt([]rune(editor.Value()), at)
						if at < drag.start {
							editor.Select(drag.end, start)
						} else {
							editor.Select(drag.start, max(drag.end, end))
						}
					} else {
						editor.Select(drag.anchor, at)
					}
					m.storeFormEditor(hit.index, editor)
					break
				}
			}
		}
		if event.Action == tea.MouseActionRelease {
			m.screenDrag = nil
			if event.X == m.clicks.x && event.Y == m.clicks.y {
				m.clicks.released = true
			} else {
				m.clicks = clickSequence{}
			}
		} else if event.X != m.clicks.x || event.Y != m.clicks.y {
			m.clicks = clickSequence{}
		}
		return nil
	}
	if event.Action != tea.MouseActionPress || event.Button != tea.MouseButtonLeft || !bounds.contains(event.X, event.Y) {
		return nil
	}
	row := event.Y - bounds.y + layout.offset
	for _, hit := range layout.hits {
		if hit.row != row || event.X-bounds.x >= hit.width {
			continue
		}
		if m.screenIndex != hit.index {
			m.clicks = clickSequence{}
		}
		m.screenIndex = hit.index
		m.screenManual, m.screenScroll = true, layout.offset
		control := &m.screen.Controls[hit.index]
		switch control.Kind {
		case "action":
			return m.runScreenAction(control.Key)
		case "disclosure":
			m.screenAdvanced = !m.screenAdvanced
		case "select", "toggle":
			direction := 1
			if event.X-bounds.x == hit.valueX {
				direction = -1
			}
			cycleControl(control, direction)
		case "text", "password":
			editor := m.formEditor(hit.index, bounds.width)
			// Resolve geometry before revealing a caret changes the editor viewport.
			x := event.X - bounds.x - hit.valueX
			at := editor.Hit(x, 0)
			anchor := at
			if event.Shift {
				anchor = editor.CursorOffset()
				if previous, ok := m.screenEditors[hit.index]; ok {
					start, end := previous.SelectionRange()
					anchor = start
					if previous.CursorOffset() == start {
						anchor = end
					}
				}
			}
			clicks := m.nextClick(formPane, event)
			if _, character := editor.CharacterAt(x, 0); !character || event.Shift {
				clicks = clickSequence{}
			}
			m.clicks = clicks
			editor.Select(anchor, at)
			m.screenDrag = &formDrag{index: hit.index, anchor: anchor}
			if clicks.count >= 2 {
				rangeAt := textarea.WordRange
				if clicks.count == 3 {
					rangeAt = textarea.LineRange
				}
				start, end := rangeAt([]rune(editor.Value()), at)
				editor.Select(start, end)
				m.screenDrag.start, m.screenDrag.end, m.screenDrag.rangeAt = start, end, rangeAt
			}
			m.storeFormEditor(hit.index, editor)
		}
		return nil
	}
	m.cancelDrag()
	return nil
}

func (m *model) handleDialogMouse(event tea.MouseMsg) tea.Cmd {
	if event.Action != tea.MouseActionPress || event.Button != tea.MouseButtonLeft {
		return nil
	}
	popup := m.dialogView(max(1, m.width))
	rows := strings.Split(popup, "\n")
	width := lipgloss.Width(popup)
	top := max(0, (m.height-min(len(rows), m.height))/2)
	if event.Y != top+len(rows)-3 {
		return nil
	}
	// Match the same centered, clipped button row rendered by dialogView.
	var widths []int
	total := 0
	for _, option := range m.dialog.options {
		w := lipgloss.Width(" " + option.label + " ")
		widths = append(widths, w)
		total += w
	}
	total += max(0, len(widths)-1) * 2
	contentWidth := max(1, width-6)
	shown := min(total, contentWidth)
	x := max(0, (m.width-width)/2) + 1 + max(0, (width-2-shown)/2)
	end := x + shown
	for index, w := range widths {
		if event.X >= x && event.X < min(x+w, end) {
			return m.resolveDialog(m.dialog.options[index].value)
		}
		x += w + 2
	}
	return nil
}
