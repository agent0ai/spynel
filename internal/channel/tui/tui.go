package tui

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	bubblespinner "github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/frdel/spynel/internal/channel"
	"github.com/frdel/spynel/internal/core"
	"github.com/frdel/spynel/internal/history"
	markdownfmt "github.com/frdel/spynel/internal/markdown"
)

type uiEvent struct{ event core.Event }
type connectionEvent struct{ status channel.ConnectionStatus }
type pairingEvent struct{ event channel.PairingEvent }
type noticeEvent struct{ notice channel.Notice }
type titleEvent struct{ title string }
type runtimeEvent struct{ status core.RuntimeStatus }
type redrawTickMsg struct{}
type screenSaveResult struct{ err error }
type screenActionResult struct {
	action string
	screen *core.Screen
	err    error
}

type Options struct {
	Attachments        string
	TitlePath          string
	ConnectionEvents   <-chan channel.ConnectionStatus
	PairingEvents      <-chan channel.PairingEvent
	NoticeEvents       <-chan channel.Notice
	InitialConnections []channel.ConnectionStatus
	TitleEvents        <-chan string
	RuntimeEvents      <-chan core.RuntimeStatus
	InitialRuntime     core.RuntimeStatus
	SaveSettings       func(map[string]string) error
	InitialScreen      *core.Screen
	ScreenAction       func(context.Context, string, string, map[string]string) (*core.Screen, error)
}

type transcriptEntry struct {
	role string
	text string
}

type composerToken struct {
	label     string
	expansion string
}

type screenFrame struct {
	screen   *core.Screen
	original map[string]string
	cursors  map[int]int
	index    int
	advanced bool
	scroll   int
	manual   bool
}

type model struct {
	ctx            context.Context
	handler        channel.Handler
	title          string
	input          textarea.Model
	inputWidth     int
	composerRows   int
	viewport       viewport.Model
	events         chan core.Event
	transcript     []transcriptEntry
	streaming      string
	responseText   string
	responseCommit int
	working        bool
	logoSpinner    bubblespinner.Model
	workingSpinner bubblespinner.Model
	commands       []core.SlashCommand
	commandMenu    bool
	commandIndex   int
	tokens         []composerToken
	attachments    string
	connections    <-chan channel.ConnectionStatus
	pairings       <-chan channel.PairingEvent
	notices        <-chan channel.Notice
	titles         <-chan string
	runtimeEvents  <-chan core.RuntimeStatus
	runtimeStatus  core.RuntimeStatus
	connection     map[string]channel.ConnectionStatus
	ignoreNextLF   bool
	pendingMouse   string
	status         string
	width          int
	height         int
	conversation   string
	welcome        *core.Screen
	welcomeFocus   bool
	screen         *core.Screen
	screenOriginal map[string]string
	screenCursors  map[int]int
	screenIndex    int
	screenAdvanced bool
	screenScroll   int
	screenManual   bool
	screenSaving   bool
	screenStack    []screenFrame
	saveSettings   func(map[string]string) error
	screenAction   func(context.Context, string, string, map[string]string) (*core.Screen, error)
	screenResult   string
}

const (
	minComposerHeight  = 1
	maxComposerHeight  = 10
	userChatLabel      = "You"
	agentChatLabel     = "Spy"
	chatContentColumn  = 4
	maxCommandRows     = 7
	compactPasteChars  = 1000
	maxTitleChars      = 80
	layoutOverhead     = 6
	redrawInterval     = 10 * time.Second
	maxTranscriptRows  = 500
	maxTranscriptRunes = 500000
)

const transcriptOmitted = "Older messages are omitted from the live display; use /history for the complete conversation file."

var (
	accent               = lipgloss.Color("#A78BFA")
	userAccent           = lipgloss.Color("#60A5FA")
	muted                = lipgloss.Color("#64748B")
	borderColor          = lipgloss.Color("#334155")
	standardBorderStyle  = lipgloss.NewStyle().Foreground(borderColor)
	scrollThumbStyle     = lipgloss.NewStyle().Foreground(accent)
	panel                = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(borderColor).Padding(0, 1)
	titleStyle           = lipgloss.NewStyle().Bold(true).Foreground(accent)
	userStyle            = lipgloss.NewStyle().Bold(true).Foreground(userAccent)
	agentStyle           = lipgloss.NewStyle().Bold(true).Foreground(accent)
	statusStyle          = lipgloss.NewStyle().Foreground(muted)
	commandStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("#CBD5E1"))
	selectedCommandStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#0F172A")).Background(accent)
	tokenStyle           = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FBBF24"))
)

var unsafeAttachmentName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

var mouseReportEscape = regexp.MustCompile(`\[[<>][0-9]+;[0-9]+;[0-9]+[Mm]`)

func Run(ctx context.Context, title string, handler channel.Handler, commands []core.SlashCommand, initialHistory []history.Entry, options Options) error {
	resolvedTitle, err := loadTitle(title, options.TitlePath)
	if err != nil {
		return fmt.Errorf("load TUI title: %w", err)
	}
	input := textarea.New()
	input.Placeholder = "Message Spynel or type /help…"
	input.Prompt = ""
	styleComposer(&input)
	// Keep the textarea's internal viewport at the maximum height so it does
	// not scroll a newly wrapped row away before the outer layout expands.
	// composerRows controls how many of those rows are actually rendered.
	input.SetHeight(maxComposerHeight)
	input.Focus()
	input.ShowLineNumbers = false
	input.CharLimit = 64 * 1024
	m := model{
		ctx: ctx, handler: handler, title: resolvedTitle, input: input,
		viewport: viewport.New(80, 20), events: make(chan core.Event, 256), composerRows: minComposerHeight,
		logoSpinner:    newLogoSpinner(),
		workingSpinner: newWorkingSpinner(),
		commands:       append([]core.SlashCommand(nil), commands...),
		transcript:     transcriptFromHistory(initialHistory),
		attachments:    options.Attachments,
		connections:    options.ConnectionEvents,
		pairings:       options.PairingEvents,
		notices:        options.NoticeEvents,
		titles:         options.TitleEvents,
		runtimeEvents:  options.RuntimeEvents,
		runtimeStatus:  options.InitialRuntime,
		saveSettings:   options.SaveSettings,
		screenAction:   options.ScreenAction,
		connection:     connectionMap(options.InitialConnections),
		status:         "Ready", conversation: "local",
	}
	if options.InitialScreen != nil {
		m.openScreen(*options.InitialScreen)
	}
	program := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	_, err = program.Run()
	return err
}

func styleComposer(input *textarea.Model) {
	placeholder := lipgloss.NewStyle().Foreground(muted).Italic(true).UnsetBackground()
	input.FocusedStyle.CursorLine = input.FocusedStyle.Text
	input.FocusedStyle.Placeholder = placeholder
	input.BlurredStyle.CursorLine = input.BlurredStyle.Text
	input.BlurredStyle.Placeholder = placeholder
}

func (m *model) filterMouseEscape(key tea.KeyMsg) (tea.KeyMsg, bool) {
	if key.Paste {
		m.pendingMouse = ""
		return key, false
	}
	if key.Type != tea.KeyRunes {
		m.pendingMouse = ""
		return key, false
	}
	text := string(key.Runes)
	if m.pendingMouse != "" {
		text = m.pendingMouse + text
		m.pendingMouse = ""
		if cleaned := mouseReportEscape.ReplaceAllString(text, ""); cleaned != text {
			if cleaned == "" {
				return tea.KeyMsg{}, true
			}
			key.Runes = []rune(cleaned)
			key.Alt = false
			return key, false
		}
		if possibleMouseReport(text) {
			m.pendingMouse = text
			return tea.KeyMsg{}, true
		}
		key.Runes = []rune(text)
		key.Alt = false
		return key, false
	}
	if key.Alt && text == "[" {
		m.pendingMouse = "["
		return tea.KeyMsg{}, true
	}
	if cleaned := mouseReportEscape.ReplaceAllString(text, ""); cleaned != text {
		if cleaned == "" {
			return tea.KeyMsg{}, true
		}
		key.Runes = []rune(cleaned)
		key.Alt = false
	}
	if possibleMouseReport(string(key.Runes)) {
		m.pendingMouse = string(key.Runes)
		return tea.KeyMsg{}, true
	}
	return key, false
}

func possibleMouseReport(value string) bool {
	if len(value) > 64 || (!strings.HasPrefix(value, "[<") && !strings.HasPrefix(value, "[>")) {
		return false
	}
	for _, character := range value[2:] {
		if (character < '0' || character > '9') && character != ';' {
			return false
		}
	}
	return true
}

func newLogoSpinner() bubblespinner.Model {
	return bubblespinner.New(
		bubblespinner.WithSpinner(bubblespinner.Spinner{
			Frames: []string{"◉◉", "◑◉", "○◉", "○◑", "○○", "◐○", "◉○", "◉◐", "◉◉"},
			FPS:    time.Second / 10,
		}),
	)
}

func newWorkingSpinner() bubblespinner.Model {
	return bubblespinner.New(
		bubblespinner.WithSpinner(bubblespinner.Spinner{
			Frames: []string{"⠋", "⠙", "⠸", "⠴", "⠦", "⠇"},
			FPS:    time.Second / 12,
		}),
	)
}

func (m model) Init() tea.Cmd {
	// Keep terminal-native drag selection available and clear mouse reporting
	// if a previous abnormal exit left the terminal mode enabled.
	return tea.Batch(textarea.Blink, m.waitEvent(), m.redrawTick(), tea.DisableMouse)
}

func (m model) redrawTick() tea.Cmd {
	return tea.Tick(redrawInterval, func(time.Time) tea.Msg { return redrawTickMsg{} })
}

func (m model) repaint() tea.Cmd {
	if m.width <= 0 || m.height <= 0 {
		return nil
	}
	// Bubble Tea invalidates its differential-render cache for every window
	// size message. Sending the current size repaints the complete frame without
	// clearing it first, which repairs terminal-owned screen clears without a
	// visible erase/repaint flash.
	return func() tea.Msg { return tea.WindowSizeMsg{Width: m.width, Height: m.height} }
}

func (m model) waitEvent() tea.Cmd {
	return func() tea.Msg {
		select {
		case event := <-m.events:
			return uiEvent{event: event}
		case status := <-m.connections:
			return connectionEvent{status: status}
		case event := <-m.pairings:
			return pairingEvent{event: event}
		case notice := <-m.notices:
			return noticeEvent{notice: notice}
		case title := <-m.titles:
			return titleEvent{title: title}
		case status := <-m.runtimeEvents:
			return runtimeEvent{status: status}
		case <-m.ctx.Done():
			return tea.Quit()
		}
	}
}

func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	var commands []tea.Cmd
	updateInput := true
	updateViewport := true
	inputBefore := m.input.Value()
	inputWasAtEnd := m.inputCursorAtEnd()
	switch value := message.(type) {
	case tea.WindowSizeMsg:
		if m.width == value.Width && m.height == value.Height {
			// A same-size message is an intentional renderer-cache invalidation.
			// Leave the model, especially the user's history offset, untouched.
			break
		}
		m.width, m.height = value.Width, value.Height
		m.viewport.Width = max(20, value.Width-5)
		m.inputWidth = max(20, value.Width-7)
		m.input.SetWidth(m.inputWidth)
		m.resizeComposer()
		m.refresh()
	case tea.KeyMsg:
		updateViewport = false
		filtered, consumed := m.filterMouseEscape(value)
		if consumed {
			updateInput = false
			break
		}
		value = filtered
		message = filtered
		if m.screen != nil {
			updateInput = false
			updateViewport = false
			return m, m.handleScreenKey(value)
		}
		if value.Type == tea.KeyCtrlL {
			updateInput = false
			commands = append(commands, m.repaint())
			break
		}
		if value.Type != tea.KeyCtrlJ {
			m.ignoreNextLF = false
		}
		if value.Type == tea.KeyCtrlC {
			if m.input.Value() != "" || m.commandMenu {
				m.ignoreNextLF = false
				m.resetComposer()
				m.resizeComposer()
				return m, nil
			}
			return m, tea.Quit
		}
		if value.Paste && len(value.Runes) > 0 {
			handled, err := m.handlePaste(string(value.Runes))
			if handled {
				updateInput = false
				if err != nil {
					m.status = "Paste failed: " + err.Error()
				}
				break
			}
		}
		if m.handleCommandMenuKey(value) {
			updateInput = false
			updateViewport = false
			break
		}
		if value.Type == tea.KeyPgUp || value.Type == tea.KeyPgDown {
			updateInput = false
			updateViewport = true
			break
		}
		if m.handleTokenKey(value) {
			updateInput = false
			break
		}
		switch value.Type {
		case tea.KeyCtrlJ:
			// Warp can pass Enter through docker exec as CRLF. The CR sends
			// the message; ignore only its immediate bare LF companion. A
			// standalone LF is Shift+Enter and inserts a composer newline.
			if m.ignoreNextLF && !value.Paste {
				m.ignoreNextLF = false
				updateInput = false
				break
			}
			m.ignoreNextLF = false
			m.expandComposerForNewline()
			message = tea.KeyMsg{Type: tea.KeyEnter}
		case tea.KeyEnter:
			commands = append(commands, m.repaint())
			if value.Paste {
				m.expandComposerForNewline()
				message = tea.KeyMsg{Type: tea.KeyEnter}
				break
			}
			displayText := strings.TrimSpace(m.input.Value())
			if displayText == "" {
				m.ignoreNextLF = true
				m.input.Reset()
				m.tokens = nil
				m.commandMenu = false
				m.commandIndex = 0
				m.resizeComposer()
				return m, tea.Batch(commands...)
			}
			if displayText == "/quit" || displayText == "/exit" {
				return m, tea.Quit
			}
			messageText := m.expandTokens(displayText)
			wasWorking := m.working
			isCommand := strings.HasPrefix(displayText, "/")
			updateInput = false
			m.ignoreNextLF = true
			m.resetComposer()
			if wasWorking {
				m.commitStreamingResponse()
			} else {
				m.resetResponse()
				m.working = !isCommand
			}
			m.appendTranscript(transcriptEntry{role: "user", text: displayText})
			m.status = "Sending…"
			m.resizeComposer()
			m.refresh()
			msg := core.Message{Channel: "tui", Conversation: m.conversation, Sender: "local", Text: messageText, ReceivedAt: time.Now().UTC()}
			handler := m.handler
			events := m.events
			ctx := m.ctx
			commands = append(commands, func() tea.Msg {
				go func() {
					err := handler(ctx, msg, func(event core.Event) { events <- event })
					if err != nil {
						events <- core.Event{Kind: core.EventError, Text: err.Error(), Done: true, Local: isCommand}
					}
				}()
				return nil
			})
			if m.working && !wasWorking {
				commands = append(commands, m.logoSpinner.Tick, m.workingSpinner.Tick)
			}
		}
	case tea.MouseMsg:
		// Application mouse reporting is disabled so terminal-native drag
		// selection and copying remain available. Ignore any queued stale event.
		m.pendingMouse = ""
		updateInput = false
		updateViewport = false
	case uiEvent:
		event := value.event
		switch event.Kind {
		case core.EventDelta:
			wasWorking := m.working
			m.streaming += event.Text
			m.responseText += event.Text
			m.working = true
			if !wasWorking {
				commands = append(commands, m.logoSpinner.Tick, m.workingSpinner.Tick)
			}
		case core.EventFinal:
			if event.Clear {
				m.transcript = nil
				m.welcome = nil
				m.welcomeFocus = false
				m.resetResponse()
				m.working = false
				m.status = event.Text
				break
			}
			if event.Local {
				m.appendTranscript(transcriptEntry{role: "assistant", text: event.Text})
				if m.working {
					m.status = "Harness working"
				} else {
					m.status = "Ready"
				}
				break
			}
			m.finishRecipientResponse(event.Text)
			m.working = false
			m.status = "Ready"
		case core.EventError:
			if !event.Local {
				m.working = false
				m.resetResponse()
			}
			m.appendTranscript(transcriptEntry{role: "error", text: event.Text})
			if event.Local && m.working {
				m.status = "Harness working"
			} else {
				m.status = "Harness error"
			}
		case core.EventStatus:
			m.status = event.Text
		case core.EventScreen:
			if event.Screen != nil {
				m.openScreen(*event.Screen)
			}
		}
		m.refresh()
		commands = append(commands, m.waitEvent())
	case bubblespinner.TickMsg:
		updateInput = false
		updateViewport = false
		switch value.ID {
		case m.logoSpinner.ID():
			if !m.working {
				break
			}
			var tick tea.Cmd
			m.logoSpinner, tick = m.logoSpinner.Update(value)
			commands = append(commands, tick)
		case m.workingSpinner.ID():
			if !m.working {
				break
			}
			var tick tea.Cmd
			m.workingSpinner, tick = m.workingSpinner.Update(value)
			commands = append(commands, tick)
			m.refreshPreservingHistory()
		}
	case connectionEvent:
		if m.connection == nil {
			m.connection = map[string]channel.ConnectionStatus{}
		}
		m.connection[value.status.Name] = value.status
		commands = append(commands, m.waitEvent())
	case pairingEvent:
		if m.screen != nil && (m.screen.ID == value.event.Name || strings.HasPrefix(m.screen.ID, "wizard:"+value.event.Name+":")) {
			m.screen.Banner = value.event.Rendered
			m.screen.Status = value.event.Detail
			m.refresh()
		}
		commands = append(commands, m.waitEvent())
	case noticeEvent:
		m.status = strings.Title(value.notice.Channel) + " · " + value.notice.Sender + ": " + value.notice.Text //nolint:staticcheck
		commands = append(commands, m.waitEvent())
	case titleEvent:
		m.title = value.title
		m.status = "Title changed to " + value.title
		m.refresh()
		commands = append(commands, m.waitEvent())
	case runtimeEvent:
		m.runtimeStatus = value.status
		commands = append(commands, m.waitEvent())
	case screenSaveResult:
		m.screenSaving = false
		if value.err != nil {
			m.status = "Save failed: " + value.err.Error()
		} else {
			m.captureScreenOriginal()
			m.status = "Configuration saved"
		}
	case screenActionResult:
		m.screenSaving = false
		if value.err != nil {
			m.status = "Action failed: " + value.err.Error()
			break
		}
		m.screenResult = value.action
		selectionScreen := m.screen != nil && (m.screen.ID == "harness" || m.screen.ID == "model")
		if selectionScreen && len(m.screenStack) > 0 && (value.screen == nil || value.screen.ID != m.screen.ID) {
			m.restoreParentScreen()
			selection := strings.TrimPrefix(value.action, "select:")
			if selection == "" {
				selection = "harness default"
			}
			m.status = "Selected " + selection
			break
		}
		if value.screen != nil {
			m.openScreen(*value.screen)
			break
		}
		if m.screen != nil && m.screen.ExitOnAction {
			return m, tea.Quit
		}
		m.clearScreen()
		m.status = "Ready"
		if selectionScreen {
			selection := strings.TrimPrefix(value.action, "select:")
			if selection == "" {
				selection = "harness default"
			}
			m.status = "Selected " + selection
		}
	case redrawTickMsg:
		updateInput = false
		updateViewport = false
		commands = append(commands, m.repaint(), m.redrawTick())
	}
	var cmd tea.Cmd
	if updateInput {
		m.input, cmd = m.input.Update(message)
		commands = append(commands, cmd)
		if len(m.input.Value()) < len(inputBefore) && inputWasAtEnd && composerVisualRows(m.input.Value(), m.inputWidth) > maxComposerHeight {
			commands = append(commands, m.reanchorComposerEnd())
		}
		if m.input.Value() != inputBefore {
			m.pruneTokens()
			m.syncCommandMenu()
		}
		m.snapCursorOutsideToken(message)
	}
	m.resizeComposer()
	if updateViewport {
		m.viewport, cmd = m.viewport.Update(message)
		commands = append(commands, cmd)
	}
	return m, tea.Batch(commands...)
}

func (m *model) commitStreamingResponse() {
	if m.streaming != "" {
		m.appendTranscript(transcriptEntry{role: "assistant", text: m.streaming})
	}
	m.responseCommit = len(m.responseText)
	m.streaming = ""
}

func (m *model) finishRecipientResponse(final string) {
	text := completedResponse(m.responseText, final)
	if m.responseCommit > 0 {
		committed := ""
		if m.responseCommit <= len(m.responseText) {
			committed = m.responseText[:m.responseCommit]
		}
		if committed != "" && strings.HasPrefix(text, committed) {
			text = strings.TrimPrefix(text, committed)
		} else {
			text = completedResponse(m.streaming, final)
		}
	}
	if text != "" {
		m.appendTranscript(transcriptEntry{role: "assistant", text: text})
	}
	m.resetResponse()
}

func (m *model) resetResponse() {
	m.streaming = ""
	m.responseText = ""
	m.responseCommit = 0
}

func completedResponse(streaming, final string) string {
	if streaming == "" {
		return final
	}
	if final == "" {
		return streaming
	}
	if final == streaming || strings.HasPrefix(final, streaming) || strings.HasSuffix(final, streaming) {
		return final
	}
	if strings.HasSuffix(streaming, final) {
		return streaming
	}
	return streaming + "\n" + final
}

func (m *model) expandComposerForNewline() {
	m.composerRows = min(maxComposerHeight, m.composerRows+1)
}

func (m *model) resetComposer() {
	m.input.Reset()
	m.composerRows = minComposerHeight
	m.commandMenu = false
	m.commandIndex = 0
	m.tokens = nil
}

func (m model) inputCursorAtEnd() bool {
	lines := strings.Split(m.input.Value(), "\n")
	if m.input.Line() != len(lines)-1 {
		return false
	}
	info := m.input.LineInfo()
	return info.StartColumn+info.ColumnOffset == len([]rune(lines[len(lines)-1]))
}

func (m *model) reanchorComposerEnd() tea.Cmd {
	value := m.input.Value()
	m.input.SetValue(value)
	// SetValue resets the textarea's private viewport. Prime its content before
	// Update asks it to reposition, otherwise the viewport still appears empty
	// and clamps the requested bottom offset back to zero.
	_ = m.input.View()
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(tea.KeyMsg{Type: tea.KeyNull})
	return cmd
}

func loadTitle(fallback, path string) (string, error) {
	if path == "" {
		return fallback, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return fallback, nil
	}
	if err != nil {
		return "", err
	}
	title := strings.Join(strings.Fields(string(data)), " ")
	if title == "" {
		return fallback, nil
	}
	if runes := []rune(title); len(runes) > maxTitleChars {
		title = string(runes[:maxTitleChars])
	}
	return title, nil
}

func connectionMap(statuses []channel.ConnectionStatus) map[string]channel.ConnectionStatus {
	result := make(map[string]channel.ConnectionStatus, len(statuses))
	for _, status := range statuses {
		result[status.Name] = status
	}
	return result
}

func (m *model) handlePaste(value string) (bool, error) {
	paths := pastedFilePaths(value)
	if len(paths) > 0 {
		var labels []string
		for _, path := range paths {
			token, err := m.copyAttachment(path)
			if err != nil {
				return true, err
			}
			m.tokens = append(m.tokens, token)
			labels = append(labels, token.label)
		}
		m.input.InsertString(strings.Join(labels, " "))
		m.commandMenu = false
		m.commandIndex = 0
		m.status = fmt.Sprintf("Attached %d file(s)", len(labels))
		return true, nil
	}
	characters := len([]rune(value))
	if characters < compactPasteChars {
		return false, nil
	}
	token := composerToken{
		label:     fmt.Sprintf("[Pasted %d chars]", characters),
		expansion: value,
	}
	m.tokens = append(m.tokens, token)
	m.input.InsertString(token.label)
	m.commandMenu = false
	m.commandIndex = 0
	m.status = token.label
	return true, nil
}

func (m *model) copyAttachment(source string) (composerToken, error) {
	if m.attachments == "" {
		return composerToken{}, fmt.Errorf("attachments directory is not configured")
	}
	if err := os.MkdirAll(m.attachments, 0o700); err != nil {
		return composerToken{}, err
	}
	input, err := os.Open(source)
	if err != nil {
		return composerToken{}, err
	}
	defer input.Close()
	originalName := filepath.Base(source)
	safeName := strings.Trim(unsafeAttachmentName.ReplaceAllString(originalName, "_"), "._")
	if safeName == "" {
		safeName = "attachment"
	}
	extension := filepath.Ext(safeName)
	stem := strings.TrimSuffix(safeName, extension)
	var destination string
	var output *os.File
	for index := 0; ; index++ {
		name := safeName
		if index > 0 {
			name = fmt.Sprintf("%s-%d%s", stem, index+1, extension)
		}
		destination = filepath.Join(m.attachments, name)
		output, err = os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if !os.IsExist(err) {
			break
		}
	}
	if err != nil {
		return composerToken{}, err
	}
	removeIncomplete := true
	defer func() {
		_ = output.Close()
		if removeIncomplete {
			_ = os.Remove(destination)
		}
	}()
	if _, err := io.Copy(output, input); err != nil {
		return composerToken{}, err
	}
	if err := output.Sync(); err != nil {
		return composerToken{}, err
	}
	if err := output.Close(); err != nil {
		return composerToken{}, err
	}
	removeIncomplete = false
	labelName := strings.ReplaceAll(originalName, "]", "_")
	label := "[Attachment " + labelName + "]"
	return composerToken{label: label, expansion: label + "(<" + filepath.ToSlash(destination) + ">)"}, nil
}

func pastedFilePaths(value string) []string {
	words := splitShellWords(strings.TrimSpace(value))
	if len(words) == 0 {
		return nil
	}
	paths := make([]string, 0, len(words))
	for _, word := range words {
		path := normalizePastedPath(word)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil
		}
		paths = append(paths, absolute)
	}
	return paths
}

func normalizePastedPath(value string) string {
	value = strings.TrimSpace(value)
	if parsed, err := url.Parse(value); err == nil && parsed.Scheme == "file" {
		value = parsed.Path
	}
	if unquoted, err := strconv.Unquote(value); err == nil {
		value = unquoted
	} else if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		value = value[1 : len(value)-1]
	}
	return value
}

func splitShellWords(value string) []string {
	var words []string
	var current strings.Builder
	var quote rune
	escaped := false
	flush := func() {
		if current.Len() > 0 {
			words = append(words, current.String())
			current.Reset()
		}
	}
	for _, character := range value {
		switch {
		case escaped:
			current.WriteRune(character)
			escaped = false
		case character == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if character == quote {
				quote = 0
			} else {
				current.WriteRune(character)
			}
		case character == '\'' || character == '"':
			quote = character
		case character == ' ' || character == '\n' || character == '\t' || character == '\r':
			flush()
		default:
			current.WriteRune(character)
		}
	}
	if escaped {
		current.WriteRune('\\')
	}
	flush()
	return words
}

type composerTokenRange struct {
	start int
	end   int
}

func (m *model) handleTokenKey(key tea.KeyMsg) bool {
	row, column := m.inputCursorPosition()
	lines := strings.Split(m.input.Value(), "\n")
	if row < 0 || row >= len(lines) {
		return false
	}
	for _, tokenRange := range m.tokenRanges(lines[row]) {
		switch {
		case key.Type == tea.KeyLeft && column == tokenRange.end:
			m.input.SetCursor(tokenRange.start)
			return true
		case key.Type == tea.KeyRight && column == tokenRange.start:
			m.input.SetCursor(tokenRange.end)
			return true
		case (key.Type == tea.KeyBackspace || key.Type == tea.KeyCtrlH) && column == tokenRange.end:
			m.deleteInputRange(row, tokenRange.start, tokenRange.end)
			return true
		case (key.Type == tea.KeyDelete || key.Type == tea.KeyCtrlD) && column == tokenRange.start:
			m.deleteInputRange(row, tokenRange.start, tokenRange.end)
			return true
		}
	}
	return false
}

func (m *model) snapCursorOutsideToken(message tea.Msg) {
	key, ok := message.(tea.KeyMsg)
	if !ok {
		return
	}
	row, column := m.inputCursorPosition()
	lines := strings.Split(m.input.Value(), "\n")
	if row < 0 || row >= len(lines) {
		return
	}
	for _, tokenRange := range m.tokenRanges(lines[row]) {
		if column <= tokenRange.start || column >= tokenRange.end {
			continue
		}
		if key.Type == tea.KeyRight || key.Type == tea.KeyDown || column-tokenRange.start > tokenRange.end-column {
			m.input.SetCursor(tokenRange.end)
		} else {
			m.input.SetCursor(tokenRange.start)
		}
		return
	}
}

func (m model) inputCursorPosition() (int, int) {
	lineInfo := m.input.LineInfo()
	return m.input.Line(), lineInfo.StartColumn + lineInfo.ColumnOffset
}

func (m model) tokenRanges(line string) []composerTokenRange {
	var ranges []composerTokenRange
	lineRunes := []rune(line)
	for _, token := range m.tokens {
		labelRunes := []rune(token.label)
		searchAt := 0
		for searchAt <= len(lineRunes) {
			suffix := string(lineRunes[searchAt:])
			byteOffset := strings.Index(suffix, token.label)
			if byteOffset < 0 {
				break
			}
			// strings.Index returns a byte offset; convert the matched prefix to runes.
			start := searchAt + len([]rune(suffix[:byteOffset]))
			ranges = append(ranges, composerTokenRange{start: start, end: start + len(labelRunes)})
			searchAt = start + len(labelRunes)
		}
	}
	return ranges
}

func (m *model) deleteInputRange(row, start, end int) {
	lines := strings.Split(m.input.Value(), "\n")
	if row < 0 || row >= len(lines) {
		return
	}
	runes := []rune(lines[row])
	if start < 0 || end > len(runes) || start >= end {
		return
	}
	lines[row] = string(append(runes[:start], runes[end:]...))
	m.input.SetValue(strings.Join(lines, "\n"))
	for steps := 0; m.input.Line() > row && steps < 10000; steps++ {
		m.input.CursorUp()
	}
	m.input.SetCursor(start)
	m.pruneTokens()
	m.syncCommandMenu()
}

func (m *model) pruneTokens() {
	value := m.input.Value()
	remaining := make([]composerToken, 0, len(m.tokens))
	for _, token := range m.tokens {
		if strings.Contains(value, token.label) {
			remaining = append(remaining, token)
		}
	}
	m.tokens = remaining
}

func (m model) expandTokens(value string) string {
	for _, token := range m.tokens {
		value = strings.Replace(value, token.label, token.expansion, 1)
	}
	return value
}

func transcriptFromHistory(entries []history.Entry) []transcriptEntry {
	transcript := make([]transcriptEntry, 0, len(entries))
	for _, entry := range entries {
		transcript = append(transcript, transcriptEntry{role: entry.Role, text: entry.Content})
	}
	return boundTranscript(transcript)
}

func (m *model) appendTranscript(entries ...transcriptEntry) {
	m.transcript = append(m.transcript, entries...)
	m.transcript = boundTranscript(m.transcript)
}

func boundTranscript(entries []transcriptEntry) []transcriptEntry {
	alreadyTrimmed := len(entries) > 0 && entries[0].role == "status" && entries[0].text == transcriptOmitted
	if alreadyTrimmed {
		entries = entries[1:]
	}
	for index := range entries {
		runes := []rune(entries[index].text)
		if len(runes) <= maxTranscriptRunes {
			continue
		}
		prefix := "[Earlier content omitted from the live display]\n"
		keep := max(0, maxTranscriptRunes-len([]rune(prefix)))
		entries[index].text = prefix + string(runes[len(runes)-keep:])
		alreadyTrimmed = true
	}
	start := len(entries)
	used := 0
	rowLimit := maxTranscriptRows
	if alreadyTrimmed {
		rowLimit--
	}
	for start > 0 && len(entries)-start < rowLimit {
		characters := len([]rune(entries[start-1].text))
		if start < len(entries) {
			characters++
		}
		if used > 0 && used+characters > maxTranscriptRunes {
			break
		}
		start--
		used += characters
	}
	trimmed := alreadyTrimmed || start > 0
	result := append([]transcriptEntry(nil), entries[start:]...)
	if trimmed {
		if len(result) >= maxTranscriptRows {
			result = result[len(result)-maxTranscriptRows+1:]
		}
		result = append([]transcriptEntry{{role: "status", text: transcriptOmitted}}, result...)
	}
	return result
}

func (m *model) handleCommandMenuKey(key tea.KeyMsg) bool {
	if !m.commandMenu {
		return false
	}
	matches := m.commandMatches()
	if len(matches) == 0 {
		m.commandMenu = false
		m.commandIndex = 0
		return false
	}
	switch key.Type {
	case tea.KeyUp:
		m.commandIndex = (m.commandIndex - 1 + len(matches)) % len(matches)
		return true
	case tea.KeyDown:
		m.commandIndex = (m.commandIndex + 1) % len(matches)
		return true
	case tea.KeyTab:
		m.input.SetValue(matches[m.commandIndex].Value)
		m.commandMenu = false
		m.commandIndex = 0
		return true
	case tea.KeyEnter:
		// Insert the selection, then let the ordinary Enter path dispatch it.
		m.input.SetValue(matches[m.commandIndex].Value)
		m.commandMenu = false
		m.commandIndex = 0
		return false
	case tea.KeyEsc:
		m.commandMenu = false
		m.commandIndex = 0
		return true
	default:
		return false
	}
}

func (m *model) syncCommandMenu() {
	matches := m.commandMatches()
	m.commandMenu = len(matches) > 0
	m.commandIndex = 0
}

func (m model) commandMatches() []core.SlashCommand {
	value := strings.ToLower(m.input.Value())
	if !strings.HasPrefix(value, "/") || strings.Contains(value, "\n") {
		return nil
	}
	var matches []core.SlashCommand
	for _, command := range m.commands {
		if strings.HasPrefix(strings.ToLower(command.Value), value) {
			matches = append(matches, command)
		}
	}
	return matches
}

func (m *model) resizeComposer() bool {
	height := composerHeight(m.input.Value(), m.inputWidth)
	oldViewportHeight := m.viewport.Height
	changed := height != m.composerRows
	m.composerRows = height
	if m.input.Height() != maxComposerHeight {
		m.input.SetHeight(maxComposerHeight)
	}
	if m.height > 0 {
		m.viewport.Height = max(5, m.height-layoutOverhead-height-m.commandMenuHeight())
	}
	return changed || oldViewportHeight != m.viewport.Height
}

func (m model) commandMenuHeight() int {
	if !m.commandMenu {
		return 0
	}
	return min(maxCommandRows, len(m.commandMatches())) + 2
}

func composerHeight(value string, width int) int {
	return min(maxComposerHeight, max(minComposerHeight, composerVisualRows(value, width)))
}

func composerVisualRows(value string, width int) int {
	width = max(1, width)
	height := 0
	for _, line := range strings.Split(value, "\n") {
		lineWidth := lipgloss.Width(line)
		// The textarea moves the cursor onto a fresh visual row when the
		// current line exactly fills its width, so reserve that row too.
		height += lineWidth/width + 1
	}
	return max(minComposerHeight, height)
}

func (m *model) refresh() {
	m.renderHistory()
	if m.welcomeFocus {
		m.viewport.GotoTop()
		m.welcomeFocus = false
		return
	}
	m.viewport.GotoBottom()
}

func (m *model) refreshPreservingHistory() {
	offset := m.viewport.YOffset
	m.renderHistory()
	m.viewport.SetYOffset(offset)
}

func (m *model) renderHistory() {
	entries := make([]string, 0, len(m.transcript)+2)
	if m.welcome != nil {
		entries = append(entries, m.renderWelcome(*m.welcome))
	}
	for _, entry := range m.transcript {
		entries = append(entries, m.renderTranscriptEntry(entry))
	}
	if m.streaming != "" {
		content := m.renderAgentMarkdown(m.streaming)
		if m.working {
			content += m.workingSpinner.View()
		}
		entries = append(entries, m.renderChatMessage(agentChatLabel, agentStyle, content))
	} else if m.working {
		entries = append(entries, m.renderChatMessage(agentChatLabel, agentStyle, m.workingSpinner.View()))
	}
	content := strings.Join(entries, "\n\n")
	if content == "" {
		content = statusStyle.Render("A fresh conversation is ready. Try /help or ask Spynel to create a task.")
	}
	content = lipgloss.NewStyle().Width(max(10, m.viewport.Width-1)).Render(content)
	m.viewport.SetContent(content)
}

func (m model) renderWelcome(welcome core.Screen) string {
	parts := make([]string, 0, 2)
	if welcome.Banner != "" {
		parts = append(parts, titleStyle.Render(welcome.Banner))
	}
	if welcome.Subtitle != "" {
		contentWidth := max(20, m.viewport.Width-1)
		if welcome.Markdown {
			parts = append(parts, trimRenderedPadding(markdownfmt.Terminal(welcome.Subtitle, contentWidth)))
		} else {
			parts = append(parts, statusStyle.Render(ansi.Hardwrap(welcome.Subtitle, contentWidth, true)))
		}
	}
	return strings.Join(parts, "\n\n")
}

func (m model) renderTranscriptEntry(entry transcriptEntry) string {
	switch entry.role {
	case "user":
		return m.renderChatMessage(userChatLabel, userStyle, entry.text)
	case "assistant":
		return m.renderChatMessage(agentChatLabel, agentStyle, m.renderAgentMarkdown(entry.text))
	case "error":
		return m.renderChatMessage("Error", lipgloss.NewStyle().Foreground(lipgloss.Color("#FB7185")), entry.text)
	default:
		return m.renderChatMessage(entry.role, statusStyle, entry.text)
	}
}

func (m model) renderChatMessage(label string, style lipgloss.Style, content string) string {
	contentWidth := m.chatContentWidth()
	content = ansi.Hardwrap(content, contentWidth, true)
	padding := strings.Repeat(" ", max(1, chatContentColumn-lipgloss.Width(label)))
	continuation := strings.Repeat(" ", chatContentColumn)
	return style.Render(label) + padding + strings.ReplaceAll(content, "\n", "\n"+continuation)
}

func (m model) chatContentWidth() int {
	return max(1, m.viewport.Width-chatContentColumn-1)
}

func (m model) renderAgentMarkdown(text string) string {
	return trimRenderedPadding(markdownfmt.Terminal(text, m.chatContentWidth()))
}

func trimRenderedPadding(content string) string {
	lines := strings.Split(content, "\n")
	for index, line := range lines {
		trimmed := strings.TrimRight(ansi.Strip(line), " \t")
		lines[index] = ansi.Truncate(line, lipgloss.Width(trimmed), "")
	}
	return strings.Join(lines, "\n")
}

func (m model) View() string {
	title := titleStyle.Render(m.spynelLogo() + " " + m.title)
	status := statusStyle.Render(m.connectionBadge("telegram", "TG") + "  " + m.connectionBadge("whatsapp", "WA") + "  " + runtimeCount(m.runtimeStatus.Jobs, "jobs") + "  " + runtimeCount(m.runtimeStatus.Logs, "logs") + "  " + m.status)
	barWidth := max(20, m.width)
	header := " " + ansi.Truncate(title+"  "+status, barWidth-1, "…")
	if m.screen != nil {
		screenHeight := max(5, m.height-2)
		content, offset, total := m.screenContent(screenHeight, max(20, m.width))
		content = fitContent(content, max(1, screenHeight-2), max(1, m.width-2))
		form := titledPanel(m.screen.Title, content, max(20, m.width), offset, total)
		footerText := " " + ansi.Truncate(m.screenFooterHint(), barWidth-1, "…")
		return lipgloss.JoinVertical(lipgloss.Left, header, form, statusStyle.Render(footerText))
	}
	historyView := fitContent(m.viewport.View(), m.viewport.Height, m.viewport.Width)
	chat := panel.Width(max(20, m.width-2)).Height(max(5, m.viewport.Height)).Render(historyView)
	chat = replaceRightBorder(chat, m.viewport.Height, m.viewport.YOffset, m.viewport.TotalLineCount())
	inputOffset, inputRows := m.inputScrollMetrics()
	inputView := fitContent(m.renderInput(), m.composerRows, m.inputWidth)
	input := panel.Width(max(20, m.width-2)).Render(inputView)
	input = replaceRightBorder(input, m.composerRows, inputOffset, inputRows)
	sections := []string{header, chat}
	if commandMenu := m.commandMenuView(); commandMenu != "" {
		sections = append(sections, commandMenu)
	}
	footerText := " " + ansi.Truncate(m.footerHint(), barWidth-1, "…")
	sections = append(sections, input, statusStyle.Render(footerText))
	return lipgloss.JoinVertical(lipgloss.Left, sections...)
}

func (m *model) openScreen(screen core.Screen) {
	if screen.ID == "welcome" {
		copyScreen := screen
		copyScreen.Controls = nil
		m.welcome = &copyScreen
		m.welcomeFocus = true
		m.screen = nil
		m.screenOriginal = nil
		m.screenCursors = nil
		m.screenIndex = 0
		m.screenAdvanced = false
		m.screenScroll = 0
		m.screenManual = false
		m.screenSaving = false
		m.screenStack = nil
		m.status = "Ready"
		return
	}
	if screen.ID == "chat" && screen.Conversation != "" {
		m.conversation = screen.Conversation
		m.welcome = nil
		m.welcomeFocus = false
		m.transcript = make([]transcriptEntry, 0, len(screen.Transcript))
		for _, entry := range screen.Transcript {
			m.transcript = append(m.transcript, transcriptEntry{role: entry.Role, text: entry.Text})
		}
		m.transcript = boundTranscript(m.transcript)
		m.screen = nil
		m.screenOriginal = nil
		m.screenCursors = nil
		m.screenIndex = 0
		m.screenAdvanced = false
		m.screenScroll = 0
		m.screenManual = false
		m.screenSaving = false
		m.screenStack = nil
		m.resetResponse()
		m.working = false
		m.status = "Resumed as " + screen.Conversation
		m.refresh()
		return
	}
	if screen.ParentID != "" && m.screen != nil && screen.ParentID == m.screen.ID {
		m.screenStack = append(m.screenStack, m.currentScreenFrame())
	}
	copyScreen := screen
	copyScreen.Controls = append([]core.ScreenControl(nil), screen.Controls...)
	for index := range copyScreen.Controls {
		copyScreen.Controls[index].Options = append([]string(nil), screen.Controls[index].Options...)
	}
	m.screen = &copyScreen
	m.screenAdvanced = false
	m.screenCursors = map[int]int{}
	for index, control := range copyScreen.Controls {
		if control.Kind == "text" || control.Kind == "password" {
			m.screenCursors[index] = len([]rune(control.Value))
		}
	}
	m.screenIndex = 0
	if visible := m.visibleScreenControlIndices(); len(visible) > 0 {
		m.screenIndex = visible[0]
	}
	if screen.InitialControl != "" {
		for _, index := range m.visibleScreenControlIndices() {
			if copyScreen.Controls[index].Key == screen.InitialControl {
				m.screenIndex = index
				break
			}
		}
	}
	m.screenScroll = 0
	m.screenManual = screen.StartAtTop
	m.screenSaving = false
	m.captureScreenOriginal()
	m.status = "Editing " + screen.Title
}

func (m *model) captureScreenOriginal() {
	m.screenOriginal = map[string]string{}
	if m.screen == nil {
		return
	}
	for _, control := range m.screen.Controls {
		if control.Kind == "action" || control.Kind == "disclosure" || control.Kind == "hidden" {
			continue
		}
		m.screenOriginal[control.Key] = control.Value
	}
}

func (m *model) handleScreenKey(key tea.KeyMsg) tea.Cmd {
	if m.screen == nil {
		return nil
	}
	if key.Type == tea.KeyCtrlL {
		return m.repaint()
	}
	if key.Type == tea.KeyEsc || key.Type == tea.KeyCtrlC {
		if m.screen.Required {
			m.screenResult = "exit"
			return tea.Quit
		}
		if m.restoreParentScreen() {
			m.status = "Editing " + m.screen.Title
			return nil
		}
		m.clearScreen()
		m.status = "Ready"
		return nil
	}
	visible := m.visibleScreenControlIndices()
	if len(visible) == 0 || m.screenSaving {
		return nil
	}
	switch key.Type {
	case tea.KeyUp, tea.KeyShiftTab:
		m.screenManual = false
		m.moveScreenSelection(-1)
		return nil
	case tea.KeyDown, tea.KeyTab:
		m.screenManual = false
		m.moveScreenSelection(1)
		return nil
	case tea.KeyPgUp:
		m.screenManual = true
		m.screenScroll = max(0, m.screenScroll-max(1, (m.height-4)/2))
		return nil
	case tea.KeyPgDown:
		m.screenManual = true
		m.screenScroll += max(1, (m.height-4)/2)
		return nil
	case tea.KeyCtrlS:
		if m.screen.SaveDisabled {
			return nil
		}
		return m.saveScreen()
	}
	control := &m.screen.Controls[m.screenIndex]
	switch control.Kind {
	case "action":
		if key.Type == tea.KeyEnter || key.Type == tea.KeySpace {
			return m.runScreenAction(control.Key)
		}
	case "disclosure":
		if key.Type == tea.KeyEnter || key.Type == tea.KeySpace {
			m.screenAdvanced = !m.screenAdvanced
		}
	case "toggle", "select":
		if key.Type == tea.KeyEnter || key.Type == tea.KeySpace || key.Type == tea.KeyLeft || key.Type == tea.KeyRight {
			direction := 1
			if key.Type == tea.KeyLeft {
				direction = -1
			}
			cycleControl(control, direction)
		}
	case "text", "password":
		m.editScreenText(control, key)
	}
	return nil
}

func (m *model) currentScreenFrame() screenFrame {
	return screenFrame{
		screen:   cloneScreen(m.screen),
		original: cloneStringMap(m.screenOriginal),
		cursors:  cloneIntMap(m.screenCursors),
		index:    m.screenIndex,
		advanced: m.screenAdvanced,
		scroll:   m.screenScroll,
		manual:   m.screenManual,
	}
}

func (m *model) restoreParentScreen() bool {
	if len(m.screenStack) == 0 {
		return false
	}
	last := len(m.screenStack) - 1
	frame := m.screenStack[last]
	m.screenStack = m.screenStack[:last]
	m.screen = cloneScreen(frame.screen)
	m.screenOriginal = cloneStringMap(frame.original)
	m.screenCursors = cloneIntMap(frame.cursors)
	m.screenIndex = frame.index
	m.screenAdvanced = frame.advanced
	m.screenScroll = frame.scroll
	m.screenManual = frame.manual
	m.screenSaving = false
	return true
}

func (m *model) clearScreen() {
	m.screen = nil
	m.screenOriginal = nil
	m.screenCursors = nil
	m.screenIndex = 0
	m.screenAdvanced = false
	m.screenScroll = 0
	m.screenManual = false
	m.screenSaving = false
	m.screenStack = nil
}

func cloneScreen(screen *core.Screen) *core.Screen {
	if screen == nil {
		return nil
	}
	copyScreen := *screen
	copyScreen.Controls = append([]core.ScreenControl(nil), screen.Controls...)
	for index := range copyScreen.Controls {
		copyScreen.Controls[index].Options = append([]string(nil), screen.Controls[index].Options...)
	}
	copyScreen.Transcript = append([]core.ChatEntry(nil), screen.Transcript...)
	return &copyScreen
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneIntMap(values map[int]int) map[int]int {
	if values == nil {
		return nil
	}
	result := make(map[int]int, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func (m *model) editScreenText(control *core.ScreenControl, key tea.KeyMsg) {
	runes := []rune(control.Value)
	if m.screenCursors == nil {
		m.screenCursors = map[int]int{}
	}
	cursor, ok := m.screenCursors[m.screenIndex]
	if !ok {
		cursor = len(runes)
	}
	cursor = bounded(cursor, 0, len(runes))
	insert := func(characters []rune) {
		if len(characters) == 0 {
			return
		}
		updated := make([]rune, 0, len(runes)+len(characters))
		updated = append(updated, runes[:cursor]...)
		updated = append(updated, characters...)
		updated = append(updated, runes[cursor:]...)
		runes = updated
		cursor += len(characters)
	}
	switch key.Type {
	case tea.KeyLeft:
		cursor = max(0, cursor-1)
	case tea.KeyRight:
		cursor = min(len(runes), cursor+1)
	case tea.KeyHome, tea.KeyCtrlA:
		cursor = 0
	case tea.KeyEnd, tea.KeyCtrlE:
		cursor = len(runes)
	case tea.KeyBackspace, tea.KeyCtrlH:
		if cursor > 0 {
			runes = append(runes[:cursor-1], runes[cursor:]...)
			cursor--
		}
	case tea.KeyDelete:
		if cursor < len(runes) {
			runes = append(runes[:cursor], runes[cursor+1:]...)
		}
	case tea.KeyCtrlU:
		runes = nil
		cursor = 0
	case tea.KeySpace:
		insert([]rune{' '})
	case tea.KeyRunes:
		insert(key.Runes)
	}
	control.Value = string(runes)
	m.screenCursors[m.screenIndex] = cursor
}

func (m model) visibleScreenControlIndices() []int {
	if m.screen == nil {
		return nil
	}
	indices := make([]int, 0, len(m.screen.Controls))
	for index, control := range m.screen.Controls {
		if control.Kind == "hidden" || (control.Advanced && !m.screenAdvanced) {
			continue
		}
		indices = append(indices, index)
	}
	return indices
}

func (m *model) moveScreenSelection(direction int) {
	visible := m.visibleScreenControlIndices()
	if len(visible) == 0 {
		return
	}
	position := 0
	for index, controlIndex := range visible {
		if controlIndex == m.screenIndex {
			position = index
			break
		}
	}
	position = (position + direction + len(visible)) % len(visible)
	m.screenIndex = visible[position]
}

func (m *model) runScreenAction(action string) tea.Cmd {
	if m.screen == nil {
		return nil
	}
	if action == "exit" && m.screen.ExitOnAction {
		m.screenResult = action
		return tea.Quit
	}
	callback := m.screenAction
	if callback == nil {
		m.status = "Screen action is unavailable"
		return nil
	}
	screenID := m.screen.ID
	values := m.screenValues()
	m.screenSaving = true
	ctx := m.ctx
	return func() tea.Msg {
		next, err := callback(ctx, screenID, action, values)
		return screenActionResult{action: action, screen: next, err: err}
	}
}

func (m model) screenValues() map[string]string {
	values := map[string]string{}
	if m.screen == nil {
		return values
	}
	for _, control := range m.screen.Controls {
		if control.Kind == "action" || control.Kind == "disclosure" {
			continue
		}
		values[control.Key] = control.Value
	}
	return values
}

func cycleControl(control *core.ScreenControl, direction int) {
	if len(control.Options) == 0 {
		return
	}
	index := 0
	for current, option := range control.Options {
		if option == control.Value {
			index = current
			break
		}
	}
	index = (index + direction + len(control.Options)) % len(control.Options)
	control.Value = control.Options[index]
}

func (m *model) saveScreen() tea.Cmd {
	if m.screen == nil || m.saveSettings == nil {
		m.status = "Configuration saving is unavailable"
		return nil
	}
	changes := map[string]string{}
	for _, control := range m.screen.Controls {
		if control.Kind == "action" || control.Kind == "disclosure" || control.Kind == "hidden" {
			continue
		}
		if original, ok := m.screenOriginal[control.Key]; !ok || original != control.Value {
			changes[control.Key] = control.Value
		}
	}
	if len(changes) == 0 {
		m.status = "No configuration changes"
		return nil
	}
	m.screenSaving = true
	save := m.saveSettings
	return func() tea.Msg { return screenSaveResult{err: save(changes)} }
}

func (m model) screenContent(height, width int) (string, int, int) {
	if m.screen == nil {
		return "", 0, 0
	}
	innerWidth := max(12, width-4)
	lines := m.screenConnectionSection(innerWidth)
	if m.screen.Banner != "" {
		for _, line := range strings.Split(m.screen.Banner, "\n") {
			lines = append(lines, agentStyle.Render(line))
		}
		lines = append(lines, "")
	}
	if m.screen.Status != "" {
		lines = append(lines, agentStyle.Render(ansi.Hardwrap(m.screen.Status, innerWidth, true)), "")
	}
	if m.screen.Markdown {
		rendered := strings.Trim(trimRenderedPadding(markdownfmt.Terminal(m.screen.Subtitle, innerWidth)), "\n")
		lines = append(lines, strings.Split(rendered, "\n")...)
	} else {
		for _, line := range strings.Split(m.screen.Subtitle, "\n") {
			lines = append(lines, statusStyle.Render(line))
		}
	}
	lines = append(lines, "")
	selectedLine := 0
	previousControlKind := ""
	previousControlKey := ""
	for index, control := range m.screen.Controls {
		if control.Kind == "hidden" || (control.Advanced && !m.screenAdvanced) {
			continue
		}
		separateWizardLauncher := (m.screen.ID == "telegram" || m.screen.ID == "whatsapp") && previousControlKey == "wizard"
		separateWizardActions := strings.HasPrefix(m.screen.ID, "wizard:") && control.Kind == "action" && previousControlKind != "" && previousControlKind != "action"
		separateDisclosure := control.Kind == "disclosure" && previousControlKind != ""
		separateAdvancedControls := control.Advanced && previousControlKind == "disclosure"
		if separateWizardLauncher || separateWizardActions || separateDisclosure || separateAdvancedControls {
			lines = append(lines, "")
		}
		value := control.Value
		if control.Kind == "action" {
			value = "[ " + control.Value + " ]"
		} else if control.Kind == "disclosure" {
			value = "[ Show advanced settings ]"
			if m.screenAdvanced {
				value = "[ Hide advanced settings ]"
			}
		}
		if control.Secret {
			if value != "" {
				value = strings.Repeat("•", len([]rune(value)))
			} else if control.Configured {
				value = "(configured; type to replace)"
			}
		}
		if index == m.screenIndex && (control.Kind == "text" || control.Kind == "password") {
			value = m.renderScreenTextCursor(index, control, value)
		}
		label := strings.Title(control.Label) //nolint:staticcheck
		line := fmt.Sprintf("%-28s %s", label, value)
		if control.Kind == "action" || control.Kind == "disclosure" {
			line = value
		}
		if index == m.screenIndex {
			selectedLine = len(lines)
			line = agentStyle.Render(ansi.Truncate(line, innerWidth, "…"))
		} else {
			line = ansi.Truncate(line, innerWidth, "…")
		}
		lines = append(lines, line)
		if control.DescriptionMarkdown {
			rendered := strings.Trim(trimRenderedPadding(markdownfmt.Terminal(control.Description, max(1, innerWidth-2))), "\n")
			for _, descriptionLine := range strings.Split(rendered, "\n") {
				lines = append(lines, statusStyle.Render("  "+ansi.Truncate(descriptionLine, max(1, innerWidth-2), "…")))
			}
		} else {
			lines = append(lines, statusStyle.Render("  "+ansi.Truncate(control.Description, max(1, innerWidth-2), "…")))
		}
		previousControlKind = control.Kind
		previousControlKey = control.Key
	}
	visible := max(1, height-2)
	offset := bounded(selectedLine-visible/2, 0, max(0, len(lines)-visible))
	if m.screenManual {
		offset = bounded(m.screenScroll, 0, max(0, len(lines)-visible))
	}
	end := min(len(lines), offset+visible)
	return strings.Join(lines[offset:end], "\n"), offset, len(lines)
}

func (m model) screenConnectionSection(innerWidth int) []string {
	name := ""
	for _, candidate := range []string{"telegram", "whatsapp"} {
		if m.screen.ID == candidate || strings.HasPrefix(m.screen.ID, "wizard:"+candidate+":") {
			name = candidate
			break
		}
	}
	if name == "" {
		return nil
	}
	status, ok := m.connection[name]
	if !ok {
		status = channel.ConnectionStatus{Name: name, State: channel.ConnectionUnconfigured}
	}
	indicator := "○ Not configured"
	switch status.State {
	case channel.ConnectionConnected:
		indicator = "● Connected"
	case channel.ConnectionConnecting:
		indicator = "◐ Connecting"
	case channel.ConnectionError:
		indicator = "▲ Error"
	}
	lines := []string{agentStyle.Render("Status"), statusStyle.Render("  " + indicator)}
	if name == "telegram" && status.Identity != "" {
		identity := status.Identity
		if status.Link != "" {
			identity = "[" + status.Identity + "](" + status.Link + ")"
		}
		rendered := strings.Trim(trimRenderedPadding(markdownfmt.Terminal("Bot: "+identity, max(1, innerWidth-2))), "\n")
		for _, line := range strings.Split(rendered, "\n") {
			lines = append(lines, statusStyle.Render("  "+line))
		}
	}
	if detail := strings.Join(strings.Fields(status.Detail), " "); detail != "" {
		wrapped := ansi.Hardwrap("Detail: "+detail, max(1, innerWidth-2), true)
		for _, line := range strings.Split(wrapped, "\n") {
			lines = append(lines, statusStyle.Render("  "+line))
		}
	}
	return append(lines, "")
}

func (m model) renderScreenTextCursor(index int, control core.ScreenControl, display string) string {
	valueRunes := []rune(control.Value)
	if len(valueRunes) == 0 && control.Secret && control.Configured {
		return display + "█"
	}
	cursor := len(valueRunes)
	if position, ok := m.screenCursors[index]; ok {
		cursor = bounded(position, 0, len(valueRunes))
	}
	displayRunes := []rune(display)
	if cursor > len(displayRunes) {
		cursor = len(displayRunes)
	}
	return string(displayRunes[:cursor]) + "█" + string(displayRunes[cursor:])
}

func (m model) screenFooterHint() string {
	if m.screenSaving {
		if m.screen != nil && (m.screen.ID == "harness" || m.screen.ID == "model") {
			return "Applying selection…"
		}
		if m.screen != nil && m.screen.SaveDisabled {
			return "Applying setup…"
		}
		return "Saving configuration…"
	}
	if m.screen != nil && (m.screen.ID == "harness" || m.screen.ID == "model") {
		if len(m.screenStack) > 0 {
			return "↑/↓ or Tab/Shift+Tab navigate · Space/Enter select · Esc back"
		}
		return "↑/↓ or Tab/Shift+Tab navigate · Space/Enter select · Esc cancel"
	}
	if m.screen != nil && m.screen.SaveDisabled {
		return "↑/↓ or Tab/Shift+Tab navigate · PgUp/PgDown scroll · type edit · Space/Enter choose · Esc cancel"
	}
	return "↑/↓ or Tab/Shift+Tab navigate · type edit · Space/Enter choose · Ctrl+S save · Esc chat"
}

func (m model) footerHint() string {
	if m.commandMenu {
		return "↑/↓ choose · Tab insert · Enter send · Esc close"
	}
	return "Enter send · Shift+Enter newline · PgUp/PgDown history · Ctrl+C clear/quit"
}

func (m model) spynelLogo() string {
	if m.working || m.streaming != "" {
		return m.logoSpinner.View()
	}
	if len(m.transcript) > 0 && m.transcript[len(m.transcript)-1].role == "assistant" {
		return "◉◉"
	}
	return "○○"
}

func runtimeCount(count int, label string) string {
	return fmt.Sprintf("%d %s", count, label)
}

func (m model) connectionBadge(name, short string) string {
	status, ok := m.connection[name]
	if !ok {
		status = channel.ConnectionStatus{Name: name, State: channel.ConnectionUnconfigured}
	}
	switch status.State {
	case channel.ConnectionConnected:
		return "● " + short
	case channel.ConnectionConnecting:
		return "◐ " + short
	case channel.ConnectionError:
		return "▲ " + short
	default:
		return "○ " + short
	}
}

func (m model) commandMenuView() string {
	if !m.commandMenu {
		return ""
	}
	matches := m.commandMatches()
	if len(matches) == 0 {
		return ""
	}
	start := 0
	if m.commandIndex >= maxCommandRows {
		start = m.commandIndex - maxCommandRows + 1
	}
	end := min(len(matches), start+maxCommandRows)
	lineWidth := max(10, m.width-3)
	lines := make([]string, 0, end-start)
	for index := start; index < end; index++ {
		command := matches[index]
		line := fmt.Sprintf("%-28s %s", command.Usage, command.Description)
		line = truncateWidth(line, lineWidth)
		style := commandStyle
		if index == m.commandIndex {
			style = selectedCommandStyle
		}
		lines = append(lines, style.Width(lineWidth).Render(line))
	}
	content := strings.Join(lines, "\n")
	return titledPanel("Commands", content, max(20, m.width), start, len(matches))
}

func (m model) renderInput() string {
	view := m.input.View()
	for _, token := range m.tokens {
		view = strings.ReplaceAll(view, token.label, tokenStyle.Render(token.label))
	}
	return view
}

func (m model) inputScrollMetrics() (int, int) {
	total := composerVisualRows(m.input.Value(), m.inputWidth)
	cursorRow := 0
	lines := strings.Split(m.input.Value(), "\n")
	for index := 0; index < m.input.Line() && index < len(lines); index++ {
		cursorRow += composerVisualRows(lines[index], m.inputWidth)
	}
	cursorRow += m.input.LineInfo().RowOffset
	offset := bounded(cursorRow-m.composerRows+1, 0, max(0, total-m.composerRows))
	return offset, total
}

func fitContent(content string, height, width int) string {
	height = max(1, height)
	width = max(1, width)
	lines := strings.Split(content, "\n")
	rendered := make([]string, height)
	for row := range rendered {
		line := ""
		if row < len(lines) {
			line = ansi.Truncate(lines[row], width, "")
		}
		rendered[row] = lipgloss.NewStyle().Width(width).Render(line)
	}
	return strings.Join(rendered, "\n")
}

func scrollbar(height, offset, total int) []string {
	height = max(1, height)
	total = max(height, total)
	thumbHeight := height
	if total > height {
		thumbHeight = max(1, (height*height+total-1)/total)
	}
	maxOffset := max(0, total-height)
	maxStart := height - thumbHeight
	start := 0
	if maxOffset > 0 {
		start = bounded((bounded(offset, 0, maxOffset)*maxStart+maxOffset/2)/maxOffset, 0, maxStart)
	}
	result := make([]string, height)
	for row := range result {
		if row >= start && row < start+thumbHeight {
			result[row] = scrollThumbStyle.Render("┃")
		} else {
			result[row] = standardBorderStyle.Render("│")
		}
	}
	return result
}

func titledPanel(title, content string, width, offset, total int) string {
	width = max(20, width)
	innerWidth := max(1, width-2)
	title = " " + title + " "
	title = ansi.Truncate(title, max(1, innerWidth-1), "…")
	top := "╭─" + title + strings.Repeat("─", max(0, innerWidth-1-lipgloss.Width(title))) + "╮"
	bottom := "╰" + strings.Repeat("─", innerWidth) + "╯"
	lines := strings.Split(content, "\n")
	for index, line := range lines {
		line = ansi.Truncate(line, innerWidth, "")
		lines[index] = standardBorderStyle.Render("│") + lipgloss.NewStyle().Width(innerWidth).Render(line) + standardBorderStyle.Render("│")
	}
	box := standardBorderStyle.Render(top) + "\n" + strings.Join(lines, "\n") + "\n" + standardBorderStyle.Render(bottom)
	return replaceRightBorder(box, len(lines), offset, total)
}

func replaceRightBorder(box string, visible, offset, total int) string {
	lines := strings.Split(box, "\n")
	if len(lines) < 3 {
		return box
	}
	height := len(lines) - 2
	edge := make([]string, height)
	if total > max(1, visible) {
		edge = scrollbar(height, offset, total)
	} else {
		border := standardBorderStyle.Render("│")
		for row := range edge {
			edge[row] = border
		}
	}
	for row := 0; row < height; row++ {
		lineIndex := row + 1
		width := lipgloss.Width(lines[lineIndex])
		if width < 1 {
			continue
		}
		lines[lineIndex] = ansi.Truncate(lines[lineIndex], width-1, "") + edge[row]
	}
	return strings.Join(lines, "\n")
}

func bounded(value, lower, upper int) int {
	return min(upper, max(lower, value))
}

func truncateWidth(value string, width int) string {
	if lipgloss.Width(value) <= width {
		return value
	}
	width = max(1, width-1)
	var result strings.Builder
	for _, character := range value {
		if lipgloss.Width(result.String()+string(character)) > width {
			break
		}
		result.WriteRune(character)
	}
	return result.String() + "…"
}
