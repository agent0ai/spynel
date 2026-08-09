package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type dialogOption struct {
	label string
	value string
}

// dialogModel is a small modal interaction owned by the TUI. While it is
// present, keyboard input is routed here before the screen or composer. The
// resolver makes the primitive reusable without teaching core.Screen about
// terminal-specific overlays.
type dialogModel struct {
	title       string
	message     string
	options     []dialogOption
	selected    int
	cancelValue string
	resolve     func(*model, string) tea.Cmd
}

func (m *model) openDialog(dialog dialogModel) {
	dialog.options = append([]dialogOption(nil), dialog.options...)
	dialog.selected = bounded(dialog.selected, 0, max(0, len(dialog.options)-1))
	m.dialog = &dialog
}

func (m *model) handleDialogKey(key tea.KeyMsg) tea.Cmd {
	if m.dialog == nil {
		return nil
	}
	switch key.Type {
	case tea.KeyCtrlL:
		return m.repaint()
	case tea.KeyEsc:
		return m.resolveDialog(m.dialog.cancelValue)
	case tea.KeyLeft, tea.KeyUp, tea.KeyShiftTab:
		m.moveDialogSelection(-1)
	case tea.KeyRight, tea.KeyDown, tea.KeyTab:
		m.moveDialogSelection(1)
	case tea.KeyEnter, tea.KeySpace:
		if len(m.dialog.options) > 0 {
			return m.resolveDialog(m.dialog.options[m.dialog.selected].value)
		}
	}
	return nil
}

func (m *model) moveDialogSelection(direction int) {
	if m.dialog == nil || len(m.dialog.options) == 0 {
		return
	}
	m.dialog.selected = (m.dialog.selected + direction + len(m.dialog.options)) % len(m.dialog.options)
}

func (m *model) resolveDialog(value string) tea.Cmd {
	if m.dialog == nil {
		return nil
	}
	resolve := m.dialog.resolve
	m.dialog = nil
	if resolve == nil {
		return nil
	}
	return resolve(m, value)
}

func (m model) overlayDialog(base string) string {
	if m.dialog == nil {
		return base
	}
	terminalWidth := max(1, m.width)
	terminalHeight := max(1, m.height)
	popup := m.dialogView(terminalWidth)
	popupLines := strings.Split(popup, "\n")
	baseLines := strings.Split(base, "\n")
	for len(baseLines) < terminalHeight {
		baseLines = append(baseLines, fillLine(m.styles.base, "", terminalWidth))
	}
	if len(baseLines) > terminalHeight {
		baseLines = baseLines[:terminalHeight]
	}
	if len(popupLines) > terminalHeight {
		popupLines = popupLines[:terminalHeight]
	}
	popupWidth := 0
	for _, line := range popupLines {
		popupWidth = max(popupWidth, lipgloss.Width(line))
	}
	left := max(0, (terminalWidth-popupWidth)/2)
	top := max(0, (terminalHeight-len(popupLines))/2)
	for row, popupLine := range popupLines {
		baseRow := top + row
		line := baseLines[baseRow]
		prefix := ansi.Cut(line, 0, left)
		suffix := ansi.Cut(line, min(terminalWidth, left+popupWidth), terminalWidth)
		baseLines[baseRow] = prefix + popupLine + suffix
	}
	return strings.Join(baseLines, "\n")
}

func (m model) dialogView(terminalWidth int) string {
	if m.dialog == nil {
		return ""
	}
	width := min(56, max(4, terminalWidth-2))
	innerWidth := max(2, width-2)
	contentWidth := max(1, innerWidth-4)
	border := m.styles.title.Background(m.styles.elevated.GetBackground())
	topLabel := "─ " + strings.TrimSpace(m.dialog.title) + " "
	topLabel = ansi.Truncate(topLabel, innerWidth, "…")
	top := border.Render("╭" + topLabel + strings.Repeat("─", max(0, innerWidth-lipgloss.Width(topLabel))) + "╮")
	row := func(content string) string {
		content = ansi.Truncate(content, innerWidth, "")
		return border.Render("│") + fillLine(m.styles.elevated, content, innerWidth) + border.Render("│")
	}

	lines := []string{top, row("")}
	message := ansi.Hardwrap(strings.TrimSpace(m.dialog.message), contentWidth, true)
	for _, messageLine := range strings.Split(message, "\n") {
		lines = append(lines, row("  "+messageLine))
	}
	lines = append(lines, row(""))

	buttons := make([]string, 0, len(m.dialog.options))
	for index, option := range m.dialog.options {
		style := m.styles.surface
		if index == m.dialog.selected {
			style = m.styles.selected.Foreground(m.styles.title.GetForeground())
		}
		buttons = append(buttons, style.Render(" "+option.label+" "))
	}
	buttonRow := strings.Join(buttons, m.styles.elevated.Render("  "))
	if lipgloss.Width(buttonRow) > contentWidth {
		buttonRow = ansi.Truncate(buttonRow, contentWidth, "…")
	}
	padding := strings.Repeat(" ", max(0, (innerWidth-lipgloss.Width(buttonRow))/2))
	lines = append(lines, row(padding+buttonRow), row(""), m.dialogHintBorder(innerWidth))
	return strings.Join(lines, "\n")
}

func (m model) dialogHintBorder(innerWidth int) string {
	border := m.styles.title.Background(m.styles.elevated.GetBackground())
	hint := m.styles.footer.Background(m.styles.elevated.GetBackground())
	parts := []string{"←→ nav", "␠/↵ choose", "␛ cancel"}
	content := border.Render("─ ")
	for index, part := range parts {
		if index > 0 {
			content += border.Render(" ─ ")
		}
		content += hint.Render(part)
	}
	contentWidth := lipgloss.Width(content)
	if contentWidth > innerWidth {
		content = ansi.Truncate(content, innerWidth, "")
		contentWidth = lipgloss.Width(content)
	}
	content += border.Render(strings.Repeat("─", max(0, innerWidth-contentWidth)))
	return border.Render("╰") + content + border.Render("╯")
}
