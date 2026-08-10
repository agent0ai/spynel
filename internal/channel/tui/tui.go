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
	"unicode"
	"unicode/utf8"

	"github.com/agent0ai/spynel/internal/channel/tui/textarea"
	bubblespinner "github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/rivo/uniseg"

	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/history"
	markdownfmt "github.com/agent0ai/spynel/internal/markdown"
	"github.com/agent0ai/spynel/internal/theme"
)

type uiEvent struct {
	event  core.Event
	replay bool
}
type connectionEvent struct{ status channel.ConnectionStatus }
type pairingEvent struct{ event channel.PairingEvent }
type noticeEvent struct{ notice channel.Notice }
type titleEvent struct{ title string }
type runtimeEvent struct{ status core.RuntimeStatus }
type durableWorkEvent struct{ counts core.DurableWorkCounts }
type themeEvent struct{ theme theme.Theme }
type taskNotificationEvent struct{ notification channel.Notification }
type notificationPauseMsg struct{ sequence uint64 }
type notificationAckMsg struct {
	ids   []string
	after int
	err   error
}
type notificationAckRetryMsg struct{}
type notificationBoundaryMsg struct {
	sequence uint64
	safe     bool
	after    int
}
type redrawTickMsg struct{}
type logoAnimationTickMsg struct{ generation uint64 }

type logoAnimationMode uint8

const (
	logoStopped logoAnimationMode = iota
	logoBackground
	logoForeground
)

type screenSaveResult struct {
	screenID       string
	closeOnSuccess bool
	err            error
}
type themeSaveResult struct {
	theme theme.Theme
	err   error
}
type themeLoadResult struct {
	themes  []theme.Theme
	err     error
	elapsed time.Duration
}
type streamRefreshMsg struct{}
type streamRenderCooldownMsg struct{}
type screenActionResult struct {
	action        string
	selectedIndex int
	screen        *core.Screen
	err           error
}
type pendingPaste struct {
	placeholder string
	value       string
}
type pastePreparedMsg struct {
	paste   pendingPaste
	tokens  []composerToken
	handled bool
	err     error
	elapsed time.Duration
}
type streamRenderResult struct {
	version  uint64
	text     string
	width    int
	theme    theme.Theme
	rendered string
	elapsed  time.Duration
}
type historyRenderResult struct {
	version uint64
	width   int
	theme   theme.Theme
	entries []string
	elapsed time.Duration
}
type diagnosticResultMsg struct{}

type Options struct {
	Conversation       string
	Attachments        string
	TitlePath          string
	ConnectionEvents   <-chan channel.ConnectionStatus
	PairingEvents      <-chan channel.PairingEvent
	NoticeEvents       <-chan channel.Notice
	NotificationEvents <-chan channel.Notification
	AckNotification    func(string, int) error
	InitialConnections []channel.ConnectionStatus
	TitleEvents        <-chan string
	Themes             []theme.Theme
	InitialTheme       theme.Theme
	ThemeEvents        <-chan theme.Theme
	SaveTheme          func(string) error
	LoadThemes         func() ([]theme.Theme, error)
	RuntimeEvents      <-chan core.RuntimeStatus
	InitialRuntime     core.RuntimeStatus
	DurableWorkEvents  <-chan core.DurableWorkCounts
	InitialDurableWork core.DurableWorkCounts
	SaveSettings       func(map[string]string) error
	InitialScreen      *core.Screen
	ScreenAction       func(context.Context, string, string, map[string]string) (*core.Screen, error)
	Diagnostic         func(context.Context, string, string) error
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
	ctx                      context.Context
	handler                  channel.Handler
	title                    string
	input                    textarea.Model
	inputWidth               int
	composerRows             int
	viewport                 viewport.Model
	events                   chan core.Event
	transcript               []transcriptEntry
	streaming                string
	responseText             string
	responseCommit           int
	working                  bool
	mainAgentActivity        int
	logoSpinner              bubblespinner.Model
	logoAnimation            logoAnimationMode
	logoGeneration           uint64
	logoStoppedFrame         string
	logoTick                 func(time.Duration, uint64) tea.Cmd
	workingSpinner           bubblespinner.Model
	commands                 []core.SlashCommand
	commandMenu              bool
	commandIndex             int
	themeMenu                bool
	themeIndex               int
	themeOriginal            theme.Theme
	themeLoading             bool
	themes                   []theme.Theme
	activeTheme              theme.Theme
	styles                   uiStyles
	themeEvents              <-chan theme.Theme
	saveTheme                func(string) error
	loadThemes               func() ([]theme.Theme, error)
	tokens                   []composerToken
	attachments              string
	connections              <-chan channel.ConnectionStatus
	pairings                 <-chan channel.PairingEvent
	notices                  <-chan channel.Notice
	notifications            <-chan channel.Notification
	ackNotification          func(string, int) error
	titles                   <-chan string
	runtimeEvents            <-chan core.RuntimeStatus
	runtimeStatus            core.RuntimeStatus
	durableWorkEvents        <-chan core.DurableWorkCounts
	durableWork              core.DurableWorkCounts
	connection               map[string]channel.ConnectionStatus
	ignoreNextLF             bool
	pendingMouse             string
	status                   string
	width                    int
	height                   int
	conversation             string
	welcome                  *core.Screen
	welcomeFocus             bool
	historyCache             []string
	historyWidth             int
	historyTheme             theme.Theme
	historyValid             bool
	streamCache              string
	streamRendered           string
	streamWidth              int
	streamTheme              theme.Theme
	screen                   *core.Screen
	screenOriginal           map[string]string
	screenCursors            map[int]int
	screenIndex              int
	screenAdvanced           bool
	screenScroll             int
	screenManual             bool
	screenSaving             bool
	screenStack              []screenFrame
	dialog                   *dialogModel
	saveSettings             func(map[string]string) error
	screenAction             func(context.Context, string, string, map[string]string) (*core.Screen, error)
	screenResult             string
	pendingNotifications     []channel.Notification
	deltaSequence            uint64
	notificationAckIDs       []string
	notificationAckAfter     int
	notificationAckBusy      bool
	notificationBoundaryBusy bool
	deferredUIEvents         []core.Event
	pasteQueue               []pendingPaste
	pasteBusy                bool
	pasteCancelled           bool
	pasteCancel              context.CancelFunc
	pasteSequence            uint64
	preparePaste             func(context.Context, string, string) ([]composerToken, bool, error)
	streamVersion            uint64
	streamRefreshPending     bool
	streamRenderBusy         bool
	streamRenderCooling      bool
	streamRenderText         string
	renderStream             func(string, int, theme.Theme) string
	historyVersion           uint64
	historyRenderBusy        bool
	renderHistoryEntries     func(model) []string
	initialHistoryScroll     bool
	now                      func() time.Time
	manualScrollUpUntil      time.Time
	diagnostic               func(context.Context, string, string) error
	diagnosticBusy           bool
	lastDiagnostic           time.Time
}

const (
	minComposerHeight = 1
	maxComposerHeight = 10
	userChatLabel     = "You"
	agentChatLabel    = "Spy"
	errorChatLabel    = "Err"
	chatContentColumn = 4
	maxCommandRows    = 7
	compactPasteChars = 1000
	maxTitleChars     = 80
	// Header, footer, history top/bottom insets, and composer borders.
	layoutOverhead         = 6
	redrawInterval         = 10 * time.Second
	maxTranscriptRows      = 500
	maxTranscriptRunes     = 500000
	maxPendingPastes       = 8
	maxPendingRawRunes     = 8192
	slowWorkerThreshold    = 100 * time.Millisecond
	diagnosticInterval     = 5 * time.Second
	asyncHistoryRunes      = 16 * 1024
	asyncHistoryEntries    = 50
	streamRefreshInterval  = time.Second / 60
	manualScrollGrace      = 2 * time.Second
	logoFullInterval       = time.Second / 10
	logoBackgroundInterval = 2 * logoFullInterval
)

const transcriptOmitted = "Older messages are omitted from the live display; use /history for the complete conversation file."

const (
	composerPlaceholder = "Message Spynel, / for commands"
	emptyConversation   = "A fresh start."
)

type uiStyles struct {
	base            lipgloss.Style
	header          lipgloss.Style
	headerFill      lipgloss.Style
	footerFill      lipgloss.Style
	footer          lipgloss.Style
	surface         lipgloss.Style
	elevated        lipgloss.Style
	selected        lipgloss.Style
	title           lipgloss.Style
	user            lipgloss.Style
	agent           lipgloss.Style
	status          lipgloss.Style
	command         lipgloss.Style
	selectedCommand lipgloss.Style
	token           lipgloss.Style
	error           lipgloss.Style
	success         lipgloss.Style
	warning         lipgloss.Style
	track           lipgloss.Style
	thumb           lipgloss.Style
}

func stylesFor(active theme.Theme) uiStyles {
	c := active.Colors
	background := lipgloss.Color(c.Background)
	surface := lipgloss.Color(c.Surface)
	elevated := lipgloss.Color(c.SurfaceElevated)
	selected := lipgloss.Color(c.SurfaceSelected)
	text := lipgloss.Color(c.Text)
	muted := lipgloss.Color(c.TextMuted)
	primary := lipgloss.Color(c.Primary)
	ribbon := ribbonThemeColor(c.Background, c.User)
	return uiStyles{
		base:            lipgloss.NewStyle().Foreground(text).Background(background),
		header:          lipgloss.NewStyle().Foreground(muted).Background(background),
		headerFill:      lipgloss.NewStyle().Foreground(ribbon).Background(background),
		footerFill:      lipgloss.NewStyle().Foreground(ribbon).Background(background),
		footer:          lipgloss.NewStyle().Foreground(muted).Background(background),
		surface:         lipgloss.NewStyle().Foreground(text).Background(surface),
		elevated:        lipgloss.NewStyle().Foreground(text).Background(elevated),
		selected:        lipgloss.NewStyle().Foreground(text).Background(selected).Bold(true),
		title:           lipgloss.NewStyle().Bold(true).Foreground(primary),
		user:            lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(c.User)),
		agent:           lipgloss.NewStyle().Bold(true).Foreground(primary),
		status:          lipgloss.NewStyle().Foreground(muted),
		command:         lipgloss.NewStyle().Foreground(text).Background(elevated),
		selectedCommand: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(c.Background)).Background(primary),
		token:           lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(c.Warning)),
		error:           lipgloss.NewStyle().Foreground(lipgloss.Color(c.Error)),
		success:         lipgloss.NewStyle().Foreground(lipgloss.Color(c.Success)),
		warning:         lipgloss.NewStyle().Foreground(lipgloss.Color(c.Warning)),
		track:           lipgloss.NewStyle().Foreground(lipgloss.Color(c.Border)),
		thumb:           lipgloss.NewStyle().Foreground(primary),
	}
}

func ribbonThemeColor(background, user string) lipgloss.Color {
	backgroundValue, backgroundErr := strconv.ParseUint(strings.TrimPrefix(background, "#"), 16, 24)
	userValue, userErr := strconv.ParseUint(strings.TrimPrefix(user, "#"), 16, 24)
	if backgroundErr != nil || userErr != nil {
		return lipgloss.Color(user)
	}
	// Retain the user accent's hue while giving the page color equal weight so
	// the full-width ribbons stay visibly behind the content.
	blend := func(backgroundComponent, userComponent uint64) uint64 {
		return (backgroundComponent*50 + userComponent*50 + 50) / 100
	}
	red := blend((backgroundValue>>16)&0xff, (userValue>>16)&0xff)
	green := blend((backgroundValue>>8)&0xff, (userValue>>8)&0xff)
	blue := blend(backgroundValue&0xff, userValue&0xff)
	return lipgloss.Color(fmt.Sprintf("#%02X%02X%02X", red, green, blue))
}

var unsafeAttachmentName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

var mouseReportEscape = regexp.MustCompile(`\[[<>][0-9]+;[0-9]+;[0-9]+[Mm]`)

func Run(ctx context.Context, title string, handler channel.Handler, commands []core.SlashCommand, initialHistory []history.Entry, options Options) error {
	// Capability probing is unreliable across docker exec and nested PTYs.
	// Spynel's supported terminals understand 24-bit SGR; selecting it
	// explicitly keeps semantic theme colors from degrading into gray blocks.
	lipgloss.SetColorProfile(termenv.TrueColor)
	resolvedTitle, err := loadTitle(title, options.TitlePath)
	if err != nil {
		return fmt.Errorf("load TUI title: %w", err)
	}
	input := textarea.New()
	input.Placeholder = composerPlaceholder
	input.Prompt = ""
	activeTheme := options.InitialTheme
	if err := activeTheme.Validate(); err != nil {
		activeTheme = theme.Default()
	}
	styles := stylesFor(activeTheme)
	styleComposer(&input, styles)
	// Keep the textarea's internal viewport at the maximum height so it does
	// not scroll a newly wrapped row away before the outer layout expands.
	// composerRows controls how many of those rows are actually rendered.
	input.SetHeight(maxComposerHeight)
	input.Focus()
	input.ShowLineNumbers = false
	input.CharLimit = 64 * 1024
	conversation := strings.TrimSpace(options.Conversation)
	if conversation == "" {
		conversation = "local"
	}
	initialTranscript := transcriptFromHistory(initialHistory)
	m := model{
		ctx: ctx, handler: handler, title: resolvedTitle, input: input,
		viewport: viewport.New(80, 20), events: make(chan core.Event, 256), composerRows: minComposerHeight,
		logoSpinner:          newLogoSpinner(),
		logoTick:             scheduleLogoTick,
		workingSpinner:       newWorkingSpinner(),
		commands:             append([]core.SlashCommand(nil), commands...),
		themes:               append([]theme.Theme(nil), options.Themes...),
		activeTheme:          activeTheme,
		styles:               styles,
		themeEvents:          options.ThemeEvents,
		saveTheme:            options.SaveTheme,
		loadThemes:           options.LoadThemes,
		transcript:           initialTranscript,
		attachments:          options.Attachments,
		connections:          options.ConnectionEvents,
		pairings:             options.PairingEvents,
		notices:              options.NoticeEvents,
		notifications:        options.NotificationEvents,
		ackNotification:      options.AckNotification,
		titles:               options.TitleEvents,
		runtimeEvents:        options.RuntimeEvents,
		runtimeStatus:        options.InitialRuntime,
		durableWorkEvents:    options.DurableWorkEvents,
		durableWork:          options.InitialDurableWork,
		saveSettings:         options.SaveSettings,
		screenAction:         options.ScreenAction,
		preparePaste:         preparePaste,
		renderStream:         renderAgentMarkdownText,
		renderHistoryEntries: renderHistoryEntries,
		now:                  time.Now,
		diagnostic:           options.Diagnostic,
		connection:           connectionMap(options.InitialConnections),
		status:               "Ready", conversation: conversation,
		initialHistoryScroll: len(initialTranscript) > 0,
	}
	if options.InitialScreen != nil {
		m.openScreen(*options.InitialScreen)
	}
	m.logoAnimation = m.desiredLogoAnimation()
	if m.logoAnimation != logoStopped {
		m.logoGeneration = 1
	}
	program := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	_, err = program.Run()
	return err
}

func styleComposer(input *textarea.Model, styles uiStyles) {
	focused := input.Focused()
	// Give textarea-owned cells an explicit semantic foreground. The bordered
	// surface supplies the background, but nested cursor/style resets otherwise
	// fall back to the terminal's default foreground and become unreadable on
	// light themes.
	plain := lipgloss.NewStyle().Foreground(styles.base.GetForeground())
	placeholder := lipgloss.NewStyle().Foreground(styles.status.GetForeground()).Italic(true)
	// bubbles/cursor renders the active cell with Reverse(true). Put the
	// accent in the foreground here so reversal produces a visible accent
	// background block instead of swapping it away into an invisible glyph.
	input.Cursor.Style = lipgloss.NewStyle().Foreground(styles.selectedCommand.GetBackground())
	input.Cursor.TextStyle = plain
	input.FocusedStyle.Base = plain
	input.FocusedStyle.Text = plain
	input.FocusedStyle.CursorLine = plain
	input.FocusedStyle.Placeholder = placeholder
	input.BlurredStyle.Base = plain
	input.BlurredStyle.Text = plain
	input.BlurredStyle.CursorLine = plain
	input.BlurredStyle.Placeholder = placeholder
	// textarea caches a pointer to the active style. Models are copied by the
	// Bubble Tea update loop, so rebind it after every palette change instead
	// of leaving a pointer to a stale pre-copy Spynel style.
	if focused {
		input.Focus()
	} else {
		input.Blur()
	}
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
			FPS:    logoFullInterval,
		}),
	)
}

func scheduleLogoTick(after time.Duration, generation uint64) tea.Cmd {
	return tea.Tick(after, func(time.Time) tea.Msg { return logoAnimationTickMsg{generation: generation} })
}

func (m *model) desiredLogoAnimation() logoAnimationMode {
	if m.mainAgentActivity > 0 {
		return logoForeground
	}
	if m.runtimeStatus.LiveBackgroundJobs > 0 {
		return logoBackground
	}
	return logoStopped
}

func (m *model) syncLogoAnimation() tea.Cmd {
	next := m.desiredLogoAnimation()
	if next == m.logoAnimation {
		return nil
	}
	previous := m.logoAnimation
	m.logoAnimation = next
	m.logoGeneration++
	if next == logoStopped {
		m.logoStoppedFrame = m.logoSpinner.View()
		return nil
	}
	delay := next.interval()
	if previous == logoStopped {
		delay = 0
	}
	if m.logoTick == nil {
		m.logoTick = scheduleLogoTick
	}
	return m.logoTick(delay, m.logoGeneration)
}

func (mode logoAnimationMode) interval() time.Duration {
	if mode == logoBackground {
		return logoBackgroundInterval
	}
	return logoFullInterval
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
	var logo tea.Cmd
	if m.logoAnimation != logoStopped {
		tick := m.logoTick
		if tick == nil {
			tick = scheduleLogoTick
		}
		logo = tick(0, m.logoGeneration)
	}
	return tea.Batch(textarea.Blink, m.waitEvent(), m.redrawTick(), tea.DisableMouse, logo)
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
		case notification := <-m.notifications:
			return taskNotificationEvent{notification: notification}
		case title := <-m.titles:
			return titleEvent{title: title}
		case value := <-m.themeEvents:
			return themeEvent{theme: value}
		case status := <-m.runtimeEvents:
			return runtimeEvent{status: status}
		case counts := <-m.durableWorkEvents:
			return durableWorkEvent{counts: counts}
		case <-m.ctx.Done():
			return tea.Quit()
		}
	}
}

func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	started := time.Now()
	next, command := m.update(message)
	elapsed := time.Since(started)
	if elapsed < streamRefreshInterval {
		return next, command
	}
	updated, ok := next.(model)
	if !ok {
		return next, command
	}
	diagnostic := updated.reportDiagnostic("slow_update", fmt.Sprintf("Update %T took %s; events=%d; paste_queue=%d", message, elapsed.Round(time.Millisecond), len(updated.events), len(updated.pasteQueue)))
	return updated, tea.Batch(command, diagnostic)
}

func (m model) update(message tea.Msg) (tea.Model, tea.Cmd) {
	var commands []tea.Cmd
	updateInput := true
	updateViewport := true
	inputBefore := m.input.Value()
	inputWasAtEnd := m.inputCursorAtEnd()
	inputLineRowsBefore := m.input.LineInfo().Height
	manualScrollDirection := 0
	manualScrollOffset := m.viewport.YOffset
	switch value := message.(type) {
	case tea.WindowSizeMsg:
		if m.width == value.Width && m.height == value.Height {
			// A same-size message is an intentional renderer-cache invalidation.
			// Leave the model, especially the user's history offset, untouched.
			break
		}
		m.width, m.height = value.Width, value.Height
		terminalWidth := max(1, value.Width)
		// Chat owns one left inset, one inset before the right-edge scrollbar,
		// and the scrollbar column. The bordered composer normally owns four fewer
		// cells, but its cosmetic insets yield at the narrow feasible boundary.
		// Use the same geometry as borderedSurface so the textarea never wraps or
		// clips against a narrower width than the panel actually exposes.
		m.viewport.Width = max(1, terminalWidth-3)
		m.inputWidth, _ = borderedContentGeometry(terminalWidth)
		m.input.SetWidth(m.inputWidth)
		m.resizeComposerForViewport(false)
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
		if value.Type == tea.KeyCtrlC && (m.screen != nil || m.dialog != nil) {
			updateInput = false
			updateViewport = false
			if m.screen != nil && m.screen.Required {
				m.screenResult = "exit"
				return m, tea.Quit
			}
			m.clearScreen()
			m.status = "Ready"
			return m, nil
		}
		if m.dialog != nil {
			updateInput = false
			updateViewport = false
			return m, m.handleDialogKey(value)
		}
		if m.screen != nil && m.screen.ID == core.ScreenWhatsAppQR {
			updateInput = false
			updateViewport = false
			if m.restoreParentScreen() {
				m.status = "Editing " + m.screen.Title
			} else {
				m.clearScreen()
				m.status = "Ready"
			}
			return m, nil
		}
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
			if m.input.Value() != "" {
				m.ignoreNextLF = false
				m.cancelThemeMenu()
				m.cancelPasteWork()
				m.resetComposer()
				m.resizeComposer()
				return m, nil
			}
			if m.working {
				m.ignoreNextLF = false
				m.cancelThemeMenu()
				commands = append(commands, m.repaint())
				commands = append(commands, m.dispatchMessage("/stop", "/stop")...)
				m.resizeComposer()
				return m, tea.Batch(commands...)
			}
			return m, tea.Quit
		}
		if value.Paste && len(value.Runes) > 0 {
			updateInput = false
			if command := m.enqueuePaste(string(value.Runes)); command != nil {
				commands = append(commands, command)
			}
			break
		}
		if handled, command := m.handleThemeMenuKey(value); handled {
			updateInput = false
			updateViewport = false
			if command != nil {
				commands = append(commands, command)
			}
			break
		}
		if m.handleCommandMenuKey(value) {
			updateInput = false
			updateViewport = false
			break
		}
		if value.Type == tea.KeyPgUp || value.Type == tea.KeyPgDown {
			updateInput = false
			updateViewport = true
			if value.Type == tea.KeyPgUp {
				manualScrollDirection = -1
			} else {
				manualScrollDirection = 1
			}
			break
		}
		if value.Type == tea.KeyShiftUp || value.Type == tea.KeyShiftDown {
			updateInput = false
			updateViewport = false
			if value.Type == tea.KeyShiftUp {
				m.viewport.ScrollUp(1)
				m.noteManualScrollUp()
			} else {
				m.viewport.ScrollDown(1)
				m.resumeTailFollowAtBottom()
			}
			break
		}
		if boundaryKey, ok := m.composerBoundaryArrow(value); ok {
			message = boundaryKey
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
			if m.pasteBusy && !m.pasteCancelled || len(m.pasteQueue) > 0 {
				m.status = "Waiting for pasted files"
				updateInput = false
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
			updateInput = false
			m.ignoreNextLF = true
			commands = append(commands, m.dispatchMessage(displayText, messageText)...)
		}
	case tea.MouseMsg:
		// Application mouse reporting is disabled so terminal-native drag
		// selection and copying remain available. Ignore any queued stale event.
		m.pendingMouse = ""
		updateInput = false
		updateViewport = false
	case uiEvent:
		event := value.event
		refreshEvent := true
		terminal := !event.Local && (event.Kind == core.EventFinal || event.Kind == core.EventError)
		if m.notificationAckBusy && (terminal || len(m.deferredUIEvents) > 0) {
			m.deferredUIEvents = append(m.deferredUIEvents, event)
			m.refresh()
			if !value.replay {
				commands = append(commands, m.waitEvent())
			}
			break
		}
		switch event.Kind {
		case core.EventDelta:
			refreshEvent = false
			m.deltaSequence++
			m.streamVersion++
			wasWorking := m.working
			streamWasEmpty := m.streaming == ""
			m.streaming += event.Text
			m.responseText += event.Text
			m.working = true
			if !wasWorking {
				commands = append(commands, m.workingSpinner.Tick)
			}
			if len(m.pendingNotifications) > 0 && !m.notificationAckBusy {
				commands = append(commands, notificationPause(m.deltaSequence))
			}
			if command := m.requestStreamRender(); command != nil {
				commands = append(commands, command)
			}
			if streamWasEmpty {
				m.refresh()
			} else if command := m.queueStreamRefresh(); command != nil {
				commands = append(commands, command)
			}
		case core.EventFinal:
			if event.Clear {
				m.transcript = nil
				m.welcome = nil
				m.invalidateHistoryRender()
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
			m.flushNotifications()
			m.working = event.Continues || m.mainAgentActivity > 0
			if m.working {
				m.status = "Harness working"
			} else {
				m.status = "Ready"
			}
		case core.EventError:
			if !event.Local {
				m.working = event.Continues || m.mainAgentActivity > 0
				if m.streaming != "" {
					m.commitStreamingResponse()
				} else {
					m.resetResponse()
				}
			}
			m.appendTranscript(transcriptEntry{role: "error", text: event.Text})
			if !event.Continues {
				m.flushNotifications()
			}
			if m.working {
				m.status = "Harness working"
			} else {
				m.status = "Harness error"
			}
		case core.EventStatus:
			m.status = event.Text
		case core.EventActivity:
			wasWorking := m.working
			if event.Active {
				m.mainAgentActivity++
			} else if m.mainAgentActivity > 0 {
				m.mainAgentActivity--
			}
			m.working = m.mainAgentActivity > 0
			if event.Active {
				m.status = "Harness working"
				if !wasWorking {
					commands = append(commands, m.workingSpinner.Tick)
				}
			}
		case core.EventScreen:
			if event.Screen != nil {
				m.openScreen(*event.Screen)
			}
		case core.EventThemePicker:
			refreshEvent = false
			if command := m.requestThemeMenu(); command != nil {
				commands = append(commands, command)
			}
		}
		if command := m.syncLogoAnimation(); command != nil {
			commands = append(commands, command)
		}
		if refreshEvent {
			m.refresh()
		}
		if !value.replay {
			commands = append(commands, m.waitEvent())
		}
		if !m.notificationAckBusy && len(m.deferredUIEvents) > 0 {
			event := m.deferredUIEvents[0]
			m.deferredUIEvents = m.deferredUIEvents[1:]
			commands = append(commands, func() tea.Msg { return uiEvent{event: event, replay: true} })
		}
	case taskNotificationEvent:
		m.pendingNotifications = append(m.pendingNotifications, value.notification)
		if !m.working {
			m.flushNotifications()
		} else if !m.notificationAckBusy {
			commands = append(commands, notificationPause(m.deltaSequence))
		}
		m.refresh()
		commands = append(commands, m.waitEvent())
	case notificationPauseMsg:
		if m.working && !m.notificationAckBusy && !m.notificationBoundaryBusy && m.ackNotification != nil && value.sequence == m.deltaSequence {
			if len(m.streaming) > asyncHistoryRunes {
				m.notificationBoundaryBusy = true
				text, response, sequence := m.streaming, m.responseText, value.sequence
				commands = append(commands, func() tea.Msg {
					return notificationBoundaryMsg{sequence: sequence, safe: safeNotificationBoundary(text), after: len([]rune(response))}
				})
			} else if safeNotificationBoundary(m.streaming) {
				commands = append(commands, m.beginNotificationAck())
			}
		}
	case notificationBoundaryMsg:
		m.notificationBoundaryBusy = false
		if value.safe && value.sequence == m.deltaSequence && m.working && !m.notificationAckBusy && m.ackNotification != nil {
			commands = append(commands, m.beginNotificationAckAt(value.after))
		}
	case notificationAckMsg:
		if !m.notificationAckBusy {
			break
		}
		if value.err != nil {
			commands = append(commands, tea.Tick(200*time.Millisecond, func(time.Time) tea.Msg { return notificationAckRetryMsg{} }))
			break
		}
		m.commitStreamingPrefix(value.after)
		m.flushNotificationPrefix(len(value.ids))
		m.notificationAckBusy = false
		m.notificationAckIDs = nil
		m.notificationAckAfter = 0
		m.refresh()
		if len(m.deferredUIEvents) > 0 {
			event := m.deferredUIEvents[0]
			m.deferredUIEvents = m.deferredUIEvents[1:]
			commands = append(commands, func() tea.Msg { return uiEvent{event: event, replay: true} })
		} else if len(m.pendingNotifications) > 0 && m.working {
			commands = append(commands, notificationPause(m.deltaSequence))
		}
	case notificationAckRetryMsg:
		if m.notificationAckBusy {
			commands = append(commands, m.ackPendingNotifications())
		}
	case bubblespinner.TickMsg:
		updateInput = false
		updateViewport = false
		switch value.ID {
		case m.workingSpinner.ID():
			if !m.working {
				break
			}
			var tick tea.Cmd
			m.workingSpinner, tick = m.workingSpinner.Update(value)
			commands = append(commands, tick)
			m.refreshPreservingHistory()
		}
	case logoAnimationTickMsg:
		updateInput = false
		updateViewport = false
		if value.generation != m.logoGeneration || m.logoAnimation == logoStopped {
			break
		}
		m.logoSpinner, _ = m.logoSpinner.Update(m.logoSpinner.Tick())
		if m.logoTick == nil {
			m.logoTick = scheduleLogoTick
		}
		commands = append(commands, m.logoTick(m.logoAnimation.interval(), m.logoGeneration))
	case connectionEvent:
		if m.connection == nil {
			m.connection = map[string]channel.ConnectionStatus{}
		}
		m.connection[value.status.Name] = value.status
		commands = append(commands, m.waitEvent())
	case pairingEvent:
		for index := range m.screenStack {
			applyPairingEvent(m.screenStack[index].screen, value.event)
		}
		if m.screen != nil && m.screen.ID == core.ScreenWhatsAppQR {
			if value.event.State == "code" && strings.TrimSpace(value.event.Rendered) != "" {
				m.screen.Banner = value.event.Rendered
			} else if m.restoreParentScreen() {
				applyPairingEvent(m.screen, value.event)
				m.status = value.event.Detail
			}
			m.refresh()
		} else if applyPairingEvent(m.screen, value.event) {
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
	case themeEvent:
		m.themeMenu = false
		m.applyTheme(value.theme)
		m.status = "Theme changed to " + value.theme.Name
		commands = append(commands, m.waitEvent())
	case themeSaveResult:
		if value.err != nil {
			m.applyTheme(m.themeOriginal)
			m.status = "Theme save failed: " + value.err.Error()
		} else {
			m.applyTheme(value.theme)
			m.status = "Theme changed to " + value.theme.Name
		}
		m.themeMenu = false
		m.resizeComposer()
	case themeLoadResult:
		m.themeLoading = false
		if value.err != nil {
			m.status = "Theme load failed: " + value.err.Error()
		} else {
			m.beginThemeMenu(value.themes)
		}
		if value.elapsed >= slowWorkerThreshold {
			if command := m.reportDiagnostic("slow_theme_load", fmt.Sprintf("theme discovery took %s", value.elapsed.Round(time.Millisecond))); command != nil {
				commands = append(commands, command)
			}
		}
	case runtimeEvent:
		m.runtimeStatus = value.status
		if command := m.syncLogoAnimation(); command != nil {
			commands = append(commands, command)
		}
		commands = append(commands, m.waitEvent())
	case durableWorkEvent:
		m.durableWork = value.counts
		commands = append(commands, m.waitEvent())
	case screenSaveResult:
		if m.screen == nil || m.screen.ID != value.screenID {
			break
		}
		m.screenSaving = false
		if value.err != nil {
			m.status = "Save failed: " + value.err.Error()
		} else {
			if value.closeOnSuccess {
				m.clearScreen()
			} else {
				m.captureScreenOriginal()
			}
			m.status = "Configuration saved"
		}
	case screenActionResult:
		m.screenSaving = false
		if value.err != nil {
			m.status = "Action failed: " + value.err.Error()
			break
		}
		actionMessage := ""
		if value.screen != nil {
			actionMessage = strings.TrimSpace(value.screen.ActionMessage)
			value.screen.ActionMessage = ""
			if value.screen.ID == "" {
				value.screen = nil
			}
		}
		if actionMessage != "" {
			m.appendTranscript(transcriptEntry{role: "assistant", text: actionMessage})
		}
		m.screenResult = value.action
		if value.screen == nil && value.action == "cancel" && len(m.screenStack) > 0 {
			m.restoreParentScreen()
			title := m.screen.Title
			if title == "" {
				title = strings.Title(m.screen.ID) //nolint:staticcheck
			}
			m.status = "Editing " + title
			break
		}
		selectionScreen := m.screen != nil && (m.screen.ID == "harness" || m.screen.ID == "model")
		if selectionScreen && len(m.screenStack) > 0 && (value.screen == nil || value.screen.ID != m.screen.ID) {
			selectionScreenID := m.screen.ID
			m.restoreParentScreen()
			selection := strings.TrimPrefix(value.action, "select:")
			if selection == "" {
				selection = "harness default"
			}
			m.refreshRestoredSelection(selectionScreenID, selection)
			m.status = "Selected " + selection
			break
		}
		if value.screen != nil {
			m.openScreen(*value.screen)
			if strings.HasPrefix(value.action, "delete:") && value.screen.ID == "resume" {
				visible := m.visibleScreenControlIndices()
				if len(visible) > 0 {
					m.screenIndex = visible[min(value.selectedIndex, len(visible)-1)]
				}
			}
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
	case pastePreparedMsg:
		cancelled := m.pasteCancelled
		m.pasteBusy = false
		m.pasteCancelled = false
		m.pasteCancel = nil
		if !cancelled {
			m.applyPreparedPaste(value)
		}
		if value.elapsed >= slowWorkerThreshold {
			if command := m.reportDiagnostic("slow_paste", fmt.Sprintf("paste preparation took %s; queued=%d", value.elapsed.Round(time.Millisecond), len(m.pasteQueue))); command != nil {
				commands = append(commands, command)
			}
		}
		if command := m.startNextPaste(); command != nil {
			commands = append(commands, command)
		}
	case streamRenderResult:
		m.streamRenderBusy = false
		if value.width == m.chatMarkdownWidth() && value.theme == m.activeTheme && strings.HasPrefix(m.streaming, value.text) {
			m.streamRenderText = value.text
			m.streamRendered = value.rendered
			m.streamWidth = m.viewport.Width
			m.streamTheme = m.activeTheme
			if command := m.queueStreamRefresh(); command != nil {
				commands = append(commands, command)
			}
		}
		if value.version != m.streamVersion || value.text != m.streaming {
			if command := m.queueStreamRenderCooldown(); command != nil {
				commands = append(commands, command)
			}
		}
		if value.elapsed >= slowWorkerThreshold {
			if command := m.reportDiagnostic("slow_markdown", fmt.Sprintf("stream Markdown render took %s; stale=%t; runes=%d", value.elapsed.Round(time.Millisecond), value.version != m.streamVersion, len([]rune(value.text)))); command != nil {
				commands = append(commands, command)
			}
		}
	case historyRenderResult:
		m.historyRenderBusy = false
		if value.version == m.historyVersion && value.width == m.viewport.Width && value.theme == m.activeTheme {
			m.historyCache = value.entries
			m.historyWidth = value.width
			m.historyTheme = value.theme
			m.historyValid = true
			m.refresh()
		}
		if !m.historyValid {
			if command := m.requestHistoryRender(); command != nil {
				commands = append(commands, command)
			}
		}
		if value.elapsed >= slowWorkerThreshold {
			if command := m.reportDiagnostic("slow_history_render", fmt.Sprintf("history render took %s; stale=%t; entries=%d", value.elapsed.Round(time.Millisecond), value.version != m.historyVersion, len(value.entries))); command != nil {
				commands = append(commands, command)
			}
		}
	case diagnosticResultMsg:
		m.diagnosticBusy = false
	case redrawTickMsg:
		updateInput = false
		updateViewport = false
		commands = append(commands, m.repaint(), m.redrawTick())
	case streamRefreshMsg:
		m.streamRefreshPending = false
		if m.streaming != "" {
			m.refreshStreaming()
		}
	case streamRenderCooldownMsg:
		m.streamRenderCooling = false
		if command := m.requestStreamRender(); command != nil {
			commands = append(commands, command)
		}
	}
	var cmd tea.Cmd
	if updateInput {
		m.input, cmd = m.input.Update(message)
		commands = append(commands, cmd)
		inputAfter := m.input.Value()
		visualRowsAfter := composerTextareaVisualRows(m.input)
		// Bubbles can leave its private viewport on row ten when an unfinished
		// word creates row eleven, then correct itself only on the next delimiter.
		// Reanchor on that visual-row transition so the first overflow character
		// is visible in the same update.
		wrappedPastCap := m.composerRows == maxComposerHeight && m.input.LineInfo().Height > inputLineRowsBefore
		shrunkOverCap := len(inputAfter) < len(inputBefore) && visualRowsAfter > maxComposerHeight
		if inputAfter != inputBefore && inputWasAtEnd && m.inputCursorAtEnd() && (wrappedPastCap || shrunkOverCap) {
			commands = append(commands, m.reanchorComposerEnd())
		}
		if inputAfter != inputBefore {
			m.pruneTokens()
			m.syncCommandMenu()
		}
		m.snapCursorOutsideToken(message)
	}
	m.resizeComposer()
	if updateViewport {
		m.viewport, cmd = m.viewport.Update(message)
		commands = append(commands, cmd)
		if manualScrollDirection < 0 && m.viewport.YOffset < manualScrollOffset {
			m.noteManualScrollUp()
		} else if manualScrollDirection > 0 {
			m.resumeTailFollowAtBottom()
		}
	}
	if command := m.requestStreamRender(); command != nil {
		commands = append(commands, command)
	}
	if command := m.requestHistoryRender(); command != nil {
		commands = append(commands, command)
	}
	return m, tea.Batch(commands...)
}

func safeNotificationBoundary(text string) bool {
	if text == "" {
		return false
	}
	if markdownConstructOpen(text) {
		return false
	}
	if markdownBlockLineOpen(text) {
		return false
	}
	runes := []rune(text)
	last := runes[len(runes)-1]
	if unicode.IsSpace(last) {
		return true
	}
	if !strings.ContainsRune(".!?;", last) {
		return false
	}
	start := len(runes) - 2
	for start >= 0 && !unicode.IsSpace(runes[start]) {
		start--
	}
	word := runes[start+1 : len(runes)-1]
	if len(word) == 0 {
		return false
	}
	for _, character := range word {
		if !unicode.IsLetter(character) && !unicode.IsDigit(character) && character != '\'' && character != '-' {
			return false
		}
	}
	return true
}

func markdownBlockLineOpen(text string) bool {
	if strings.HasSuffix(text, "\n") || strings.HasSuffix(text, "\r") {
		return false
	}
	line := text
	if newline := strings.LastIndexByte(line, '\n'); newline >= 0 {
		line = line[newline+1:]
	}
	indent := 0
	for indent < len(line) && indent < 4 && line[indent] == ' ' {
		indent++
	}
	if indent >= 4 || (len(line) > indent && line[indent] == '\t') {
		return true
	}
	line = line[indent:]
	if line == "" {
		return false
	}
	if line[0] == '>' {
		return true
	}
	if line[0] == '#' {
		markerEnd := 0
		for markerEnd < len(line) && markerEnd < 6 && line[markerEnd] == '#' {
			markerEnd++
		}
		return markerEnd == len(line) || (markerEnd > 0 && unicode.IsSpace(rune(line[markerEnd])))
	}
	if strings.ContainsRune("-*+", rune(line[0])) {
		return len(line) > 1 && unicode.IsSpace(rune(line[1]))
	}
	digitEnd := 0
	for digitEnd < len(line) && digitEnd < 9 && line[digitEnd] >= '0' && line[digitEnd] <= '9' {
		digitEnd++
	}
	return digitEnd > 0 && digitEnd+1 < len(line) && (line[digitEnd] == '.' || line[digitEnd] == ')') && unicode.IsSpace(rune(line[digitEnd+1]))
}

func markdownConstructOpen(text string) bool {
	runes := []rune(text)
	type delimiter struct {
		character rune
		length    int
	}
	var emphasis []delimiter
	codeTicks := 0
	brackets := 0
	parentheses := 0
	angles := 0
	escaped := false
	lineStart := true
	for index := 0; index < len(runes); {
		character := runes[index]
		if escaped {
			escaped = false
			lineStart = character == '\n'
			index++
			continue
		}
		if character == '\\' {
			escaped = true
			index++
			continue
		}
		if character == '`' {
			end := index + 1
			for end < len(runes) && runes[end] == '`' {
				end++
			}
			length := end - index
			if codeTicks == 0 {
				codeTicks = length
			} else if codeTicks == length {
				codeTicks = 0
			}
			lineStart = false
			index = end
			continue
		}
		if codeTicks > 0 {
			lineStart = character == '\n'
			index++
			continue
		}
		switch character {
		case '[':
			brackets++
		case ']':
			brackets = max(0, brackets-1)
		case '(':
			parentheses++
		case ')':
			parentheses = max(0, parentheses-1)
		case '<':
			angles++
		case '>':
			angles = max(0, angles-1)
		case '*', '_', '~':
			end := index + 1
			for end < len(runes) && runes[end] == character {
				end++
			}
			length := end - index
			listMarker := lineStart && (character == '*' || character == '_') && length == 1 && end < len(runes) && unicode.IsSpace(runes[end])
			if !listMarker {
				if len(emphasis) > 0 && emphasis[len(emphasis)-1] == (delimiter{character: character, length: length}) {
					emphasis = emphasis[:len(emphasis)-1]
				} else {
					emphasis = append(emphasis, delimiter{character: character, length: length})
				}
			}
			lineStart = false
			index = end
			continue
		}
		if character == '\n' {
			lineStart = true
		} else if !unicode.IsSpace(character) {
			lineStart = false
		}
		index++
	}
	return escaped || codeTicks > 0 || brackets > 0 || parentheses > 0 || angles > 0 || len(emphasis) > 0
}

func notificationPause(sequence uint64) tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(time.Time) tea.Msg { return notificationPauseMsg{sequence: sequence} })
}

func notificationIDs(notifications []channel.Notification) []string {
	ids := make([]string, len(notifications))
	for index, notification := range notifications {
		ids[index] = notification.ID
	}
	return ids
}

func (m *model) ackPendingNotifications() tea.Cmd {
	ids := append([]string(nil), m.notificationAckIDs...)
	after := m.notificationAckAfter
	ack := m.ackNotification
	return func() tea.Msg {
		for _, id := range ids {
			if err := ack(id, after); err != nil {
				return notificationAckMsg{ids: ids, after: after, err: err}
			}
		}
		return notificationAckMsg{ids: ids, after: after}
	}
}

func (m *model) beginNotificationAck() tea.Cmd {
	return m.beginNotificationAckAt(len([]rune(m.responseText)))
}

func (m *model) beginNotificationAckAt(after int) tea.Cmd {
	m.notificationAckAfter = after
	m.notificationAckIDs = notificationIDs(m.pendingNotifications)
	m.notificationAckBusy = true
	return m.ackPendingNotifications()
}

func (m *model) flushNotifications() {
	m.flushNotificationPrefix(len(m.pendingNotifications))
}

func (m *model) flushNotificationPrefix(count int) {
	count = min(count, len(m.pendingNotifications))
	for _, notification := range m.pendingNotifications[:count] {
		m.appendTranscript(transcriptEntry{role: "assistant", text: notification.Text})
	}
	m.pendingNotifications = m.pendingNotifications[count:]
}

func (m *model) dispatchMessage(displayText, messageText string) []tea.Cmd {
	wasWorking := m.working
	isCommand := strings.HasPrefix(displayText, "/")
	m.resetComposer()
	if wasWorking {
		m.commitStreamingResponse()
	} else {
		m.resetResponse()
		m.working = !isCommand
	}
	m.appendTranscript(transcriptEntry{role: "user", text: displayText})
	// Sending is explicit navigation to the active conversation tail.
	m.manualScrollUpUntil = time.Time{}
	m.viewport.GotoBottom()
	m.status = "Sending…"
	m.resizeComposer()
	m.refresh()
	msg := core.Message{Channel: "tui", Conversation: m.conversation, Sender: "local", Text: messageText, ReceivedAt: time.Now().UTC()}
	handler := m.handler
	events := m.events
	ctx := m.ctx
	commands := []tea.Cmd{func() tea.Msg {
		emit := func(event core.Event) {
			select {
			case events <- event:
			case <-ctx.Done():
			}
		}
		if err := handler(ctx, msg, emit); err != nil {
			emit(core.Event{Kind: core.EventError, Text: err.Error(), Done: true, Local: isCommand})
		}
		return nil
	}}
	if m.working && !wasWorking {
		commands = append(commands, m.workingSpinner.Tick)
	}
	if command := m.syncLogoAnimation(); command != nil {
		commands = append(commands, command)
	}
	return commands
}

func (m *model) commitStreamingResponse() {
	if m.streaming != "" {
		m.appendTranscript(transcriptEntry{role: "assistant", text: m.streaming})
	}
	m.responseCommit = len(m.responseText)
	m.streaming = ""
}

func (m *model) commitStreamingPrefix(afterChars int) {
	response := []rune(m.responseText)
	afterChars = min(max(afterChars, 0), len(response))
	committedChars := len([]rune(m.responseText[:m.responseCommit]))
	if afterChars > committedChars {
		m.appendTranscript(transcriptEntry{role: "assistant", text: string(response[committedChars:afterChars])})
	}
	m.responseCommit = len(string(response[:afterChars]))
	m.streaming = string(response[afterChars:])
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
	characters := len([]rune(value))
	if characters >= compactPasteChars {
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
	tokens, handled, err := preparePaste(context.Background(), m.attachments, value)
	if err != nil || !handled {
		return handled, err
	}
	m.tokens = append(m.tokens, tokens...)
	m.input.InsertString(tokenLabels(tokens))
	m.commandMenu = false
	m.commandIndex = 0
	m.status = fmt.Sprintf("Attached %d file(s)", len(tokens))
	return true, nil
}

func (m *model) enqueuePaste(value string) tea.Cmd {
	if len([]rune(value)) >= compactPasteChars {
		_, _ = m.handlePaste(value)
		return nil
	}
	if len(m.pasteQueue) >= maxPendingPastes {
		// Pasted input is never dropped. Under pathological admission pressure it
		// remains literal text and deliberately skips filesystem interpretation.
		m.input.InsertString(value)
		m.status = "Paste queue full; inserted as text"
		return nil
	}
	m.pasteSequence++
	paste := pendingPaste{placeholder: fmt.Sprintf("[Preparing paste %d]", m.pasteSequence), value: value}
	m.tokens = append(m.tokens, composerToken{label: paste.placeholder})
	m.input.InsertString(paste.placeholder)
	m.pasteQueue = append(m.pasteQueue, paste)
	m.status = "Preparing paste"
	command := m.startNextPaste()
	if len(m.pasteQueue) >= maxPendingPastes/2 {
		if diagnostic := m.reportDiagnostic("paste_queue_pressure", fmt.Sprintf("paste queue depth reached %d", len(m.pasteQueue))); diagnostic != nil {
			return tea.Batch(command, diagnostic)
		}
	}
	return command
}

func (m *model) startNextPaste() tea.Cmd {
	if m.pasteBusy || len(m.pasteQueue) == 0 {
		return nil
	}
	paste := m.pasteQueue[0]
	m.pasteQueue = m.pasteQueue[1:]
	m.pasteBusy = true
	m.pasteCancelled = false
	prepare := m.preparePaste
	if prepare == nil {
		prepare = preparePaste
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.pasteCancel = cancel
	attachments := m.attachments
	return func() tea.Msg {
		defer cancel()
		started := time.Now()
		tokens, handled, err := prepare(ctx, attachments, paste.value)
		return pastePreparedMsg{paste: paste, tokens: tokens, handled: handled, err: err, elapsed: time.Since(started)}
	}
}

func (m *model) cancelPasteWork() {
	if m.pasteCancel != nil {
		m.pasteCancel()
		m.pasteCancelled = true
	}
	m.pasteQueue = nil
}

func (m *model) reportDiagnostic(event, message string) tea.Cmd {
	if m.diagnostic == nil || m.diagnosticBusy || time.Since(m.lastDiagnostic) < diagnosticInterval {
		return nil
	}
	m.diagnosticBusy = true
	m.lastDiagnostic = time.Now()
	report, parent := m.diagnostic, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, 2*time.Second)
		defer cancel()
		_ = report(ctx, event, message)
		return diagnosticResultMsg{}
	}
}

func (m *model) applyPreparedPaste(result pastePreparedMsg) {
	value := m.input.Value()
	if !strings.Contains(value, result.paste.placeholder) {
		m.removeComposerToken(result.paste.placeholder)
		return
	}
	replacement := result.paste.value
	status := "Pasted text"
	if result.err != nil {
		status = "Paste failed; inserted as text: " + result.err.Error()
	} else if result.handled {
		replacement = tokenLabels(result.tokens)
		status = fmt.Sprintf("Attached %d file(s)", len(result.tokens))
	}
	m.input.SetValue(strings.Replace(value, result.paste.placeholder, replacement, 1))
	m.replaceComposerToken(result.paste.placeholder, result.tokens)
	m.commandMenu = false
	m.commandIndex = 0
	m.status = status
	m.resizeComposer()
}

func (m *model) removeComposerToken(label string) {
	m.replaceComposerToken(label, nil)
}

func (m *model) replaceComposerToken(label string, replacements []composerToken) {
	for index, token := range m.tokens {
		if token.label != label {
			continue
		}
		updated := make([]composerToken, 0, len(m.tokens)-1+len(replacements))
		updated = append(updated, m.tokens[:index]...)
		updated = append(updated, replacements...)
		updated = append(updated, m.tokens[index+1:]...)
		m.tokens = updated
		return
	}
}

func tokenLabels(tokens []composerToken) string {
	labels := make([]string, len(tokens))
	for index, token := range tokens {
		labels[index] = token.label
	}
	return strings.Join(labels, " ")
}

func preparePaste(ctx context.Context, attachments, value string) ([]composerToken, bool, error) {
	paths := pastedFilePaths(value)
	if len(paths) == 0 {
		return nil, false, nil
	}
	tokens := make([]composerToken, 0, len(paths))
	for _, path := range paths {
		token, err := copyAttachment(ctx, attachments, path)
		if err != nil {
			return nil, true, err
		}
		tokens = append(tokens, token)
	}
	return tokens, true, nil
}

func copyAttachment(ctx context.Context, attachments, source string) (composerToken, error) {
	if attachments == "" {
		return composerToken{}, fmt.Errorf("attachments directory is not configured")
	}
	if err := os.MkdirAll(attachments, 0o700); err != nil {
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
		destination = filepath.Join(attachments, name)
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
	if _, err := io.Copy(output, contextReader{ctx: ctx, reader: input}); err != nil {
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

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(buffer)
	}
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

func (m model) composerBoundaryArrow(key tea.KeyMsg) (tea.KeyMsg, bool) {
	if key.Alt {
		return tea.KeyMsg{}, false
	}
	lineInfo := m.input.LineInfo()
	switch key.Type {
	case tea.KeyUp:
		if m.input.Line() == 0 && lineInfo.RowOffset == 0 {
			return tea.KeyMsg{Type: tea.KeyCtrlHome}, true
		}
	case tea.KeyDown:
		if m.input.Line() == m.input.LineCount()-1 && lineInfo.RowOffset >= lineInfo.Height-1 {
			return tea.KeyMsg{Type: tea.KeyCtrlEnd}, true
		}
	}
	return tea.KeyMsg{}, false
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
	oldLength := len(m.transcript)
	cacheReusable := m.historyValid && m.historyWidth == m.viewport.Width && m.historyTheme == m.activeTheme
	entryTrimmed := false
	for index := range entries {
		var trimmed bool
		entries[index], trimmed = boundTranscriptEntry(entries[index])
		entryTrimmed = entryTrimmed || trimmed
	}
	combined := append(m.transcript, entries...)
	if entryTrimmed && (len(combined) == 0 || combined[0].role != "status" || combined[0].text != transcriptOmitted) {
		combined = append([]transcriptEntry{{role: "status", text: transcriptOmitted}}, combined...)
	}
	m.transcript = trimBoundedTranscript(combined)
	if !cacheReusable || len(m.transcript) != oldLength+len(entries) || transcriptWorkLarge(entries) {
		m.invalidateHistoryRender()
		return
	}
	for _, entry := range entries {
		m.historyCache = append(m.historyCache, m.renderTranscriptEntry(entry))
	}
}

func boundTranscript(entries []transcriptEntry) []transcriptEntry {
	alreadyTrimmed := len(entries) > 0 && entries[0].role == "status" && entries[0].text == transcriptOmitted
	if alreadyTrimmed {
		entries = entries[1:]
	}
	for index := range entries {
		var trimmed bool
		entries[index], trimmed = boundTranscriptEntry(entries[index])
		alreadyTrimmed = alreadyTrimmed || trimmed
	}
	if alreadyTrimmed {
		entries = append([]transcriptEntry{{role: "status", text: transcriptOmitted}}, entries...)
	}
	return trimBoundedTranscript(entries)
}

func boundTranscriptEntry(entry transcriptEntry) (transcriptEntry, bool) {
	runes := []rune(entry.text)
	if len(runes) <= maxTranscriptRunes {
		return entry, false
	}
	prefix := "[Earlier content omitted from the live display]\n"
	keep := max(0, maxTranscriptRunes-len([]rune(prefix)))
	entry.text = prefix + string(runes[len(runes)-keep:])
	return entry, true
}

func trimBoundedTranscript(entries []transcriptEntry) []transcriptEntry {
	alreadyTrimmed := len(entries) > 0 && entries[0].role == "status" && entries[0].text == transcriptOmitted
	if alreadyTrimmed {
		entries = entries[1:]
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

func (m *model) openThemeMenu() {
	values := m.themes
	if m.loadThemes != nil {
		loaded, err := m.loadThemes()
		if err != nil {
			m.status = "Theme load failed: " + err.Error()
			return
		}
		values = loaded
	}
	m.beginThemeMenu(values)
}

func (m *model) requestThemeMenu() tea.Cmd {
	if m.themeLoading {
		return nil
	}
	if m.loadThemes == nil {
		m.beginThemeMenu(m.themes)
		return nil
	}
	m.themeLoading = true
	load := m.loadThemes
	return func() tea.Msg {
		started := time.Now()
		values, err := load()
		return themeLoadResult{themes: values, err: err, elapsed: time.Since(started)}
	}
}

func (m *model) beginThemeMenu(values []theme.Theme) {
	m.themeOriginal = m.activeTheme
	m.themes = append([]theme.Theme(nil), values...)
	if len(m.themes) == 0 {
		m.themes = []theme.Theme{theme.Default()}
	}
	m.commandMenu = false
	m.themeMenu = true
	m.themeIndex = 0
	for index, value := range m.themes {
		if strings.EqualFold(value.Name, m.activeTheme.Name) {
			m.themeIndex = index
			break
		}
	}
	m.applyTheme(m.themes[m.themeIndex])
	m.status = "Previewing themes"
	m.resizeComposer()
}

func (m *model) cancelThemeMenu() {
	if !m.themeMenu {
		return
	}
	m.applyTheme(m.themeOriginal)
	m.themeMenu = false
	m.themeIndex = 0
	m.status = "Theme preview cancelled"
	m.resizeComposer()
}

func (m *model) handleThemeMenuKey(key tea.KeyMsg) (bool, tea.Cmd) {
	if !m.themeMenu || len(m.themes) == 0 {
		return false, nil
	}
	switch key.Type {
	case tea.KeyUp, tea.KeyShiftTab:
		m.themeIndex = (m.themeIndex - 1 + len(m.themes)) % len(m.themes)
		m.applyTheme(m.themes[m.themeIndex])
		return true, nil
	case tea.KeyDown, tea.KeyTab:
		m.themeIndex = (m.themeIndex + 1) % len(m.themes)
		m.applyTheme(m.themes[m.themeIndex])
		return true, nil
	case tea.KeyEsc:
		m.cancelThemeMenu()
		return true, nil
	case tea.KeyEnter, tea.KeySpace:
		selected := m.themes[m.themeIndex]
		m.themeMenu = false
		m.resizeComposer()
		if m.saveTheme == nil {
			m.applyTheme(m.themeOriginal)
			m.status = "Theme saving is unavailable"
			return true, nil
		}
		save := m.saveTheme
		return true, func() tea.Msg { return themeSaveResult{theme: selected, err: save(selected.Name)} }
	default:
		return true, nil
	}
}

func (m *model) applyTheme(value theme.Theme) {
	if err := value.Validate(); err != nil {
		return
	}
	offset := m.viewport.YOffset
	m.activeTheme = value
	m.styles = stylesFor(value)
	styleComposer(&m.input, m.styles)
	m.invalidateHistoryRender()
	m.renderHistory()
	m.viewport.SetYOffset(offset)
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
	return m.resizeComposerForViewport(true)
}

func (m *model) resizeComposerForViewport(compensate bool) bool {
	capacity := m.composerCapacity()
	height := min(capacity, composerTextareaVisualRows(m.input))
	oldViewportHeight := m.viewport.Height
	oldViewportOffset := m.viewport.YOffset
	shouldFollow := m.shouldFollowTail()
	changed := height != m.composerRows
	m.composerRows = height
	inputHeightChanged := m.input.Height() != capacity
	if inputHeightChanged {
		m.input.SetHeight(capacity)
		if m.inputCursorAtEnd() {
			// SetHeight alone leaves textarea's private viewport at its previous
			// top row until another edit. Reanchor now so a terminal shrink shows
			// the cursor-bearing tail in this same WindowSize update.
			_ = m.reanchorComposerEnd()
		}
	}
	if m.height >= layoutOverhead+minComposerHeight+1 {
		m.viewport.Height = max(1, m.height-layoutOverhead-height-m.inlineMenuHeight())
	}
	if m.viewport.Height != oldViewportHeight {
		if shouldFollow {
			// Resizing the composer changes the viewport's maximum offset. Keep
			// tail-adjacent history anchored immediately above the composer.
			m.viewport.GotoBottom()
		} else if compensate && changed {
			// Preserve the row immediately above the composer when its visual
			// height changes. Growing the composer moves history up by the same
			// number of rows; shrinking it applies the inverse compensation.
			// SetYOffset performs the required top/tail clamping.
			m.viewport.SetYOffset(oldViewportOffset + oldViewportHeight - m.viewport.Height)
		} else {
			// Terminal and picker/menu changes alter the available canvas rather
			// than the composer's visual-row geometry. Preserve the top-row
			// anchor; refresh/reflow owns any later content-height clamping.
			m.viewport.SetYOffset(oldViewportOffset)
		}
	}
	return changed || oldViewportHeight != m.viewport.Height
}

// composerCapacity is ten rows in an ordinary terminal. Below the minimum
// header/history/composer/footer layout, history yields its canvas entirely so
// the bordered editor and its cursor still fit between the one-row chrome.
func (m model) composerCapacity() int {
	if m.height <= 0 {
		return maxComposerHeight
	}
	if m.height < layoutOverhead+minComposerHeight+1 {
		return bounded(m.height-4, minComposerHeight, maxComposerHeight)
	}
	return bounded(m.height-layoutOverhead-1-m.inlineMenuHeight(), minComposerHeight, maxComposerHeight)
}

func (m model) inlineMenuHeight() int {
	height := 0
	if m.themeMenu {
		height = min(maxCommandRows, len(m.themes)) + 2
	} else if m.commandMenu {
		height = min(maxCommandRows, len(m.commandMatches())) + 2
	} else {
		return 0
	}
	if m.height <= 0 {
		return height
	}
	available := m.height - layoutOverhead - minComposerHeight - 1
	if available < 3 { // A picker needs one content row and two borders.
		return 0
	}
	return min(height, available)
}

func composerHeight(value string, width int) int {
	input := textarea.New()
	input.Prompt = ""
	input.ShowLineNumbers = false
	input.SetWidth(max(1, width))
	input.SetValue(value)
	return min(maxComposerHeight, composerTextareaVisualRows(input))
}

// composerTextareaVisualRows asks the textarea for its own wrapping geometry
// instead of maintaining a second copy of its wrapping algorithm. LineInfo
// describes the active logical line; cloned probes measure the remaining
// explicit lines without disturbing editor state or its cursor.
func composerTextareaVisualRows(input textarea.Model) int {
	lines := strings.Split(input.Value(), "\n")
	activeLine := input.Line()
	height := 0
	probe := newComposerProbe(input.Width())
	for index, line := range lines {
		if index == activeLine {
			height += max(minComposerHeight, input.LineInfo().Height)
			continue
		}
		probe.SetValue(line)
		height += max(minComposerHeight, probe.LineInfo().Height)
	}
	return max(minComposerHeight, height)
}

func newComposerProbe(width int) textarea.Model {
	probe := textarea.New()
	probe.Prompt = ""
	probe.ShowLineNumbers = false
	probe.SetWidth(max(1, width))
	return probe
}

func (m *model) refresh() {
	shouldFollow := m.shouldFollowTail()
	offset := m.viewport.YOffset
	m.renderHistory()
	if m.welcomeFocus {
		m.viewport.GotoTop()
		m.welcomeFocus = false
		return
	}
	if m.initialHistoryScroll {
		m.completeInitialHistoryScroll()
		return
	}
	if shouldFollow {
		m.viewport.GotoBottom()
	} else {
		m.viewport.SetYOffset(offset)
	}
}

func (m *model) refreshPreservingHistory() {
	offset := m.viewport.YOffset
	m.renderHistory()
	if m.completeInitialHistoryScroll() {
		return
	}
	m.viewport.SetYOffset(offset)
}

// completeInitialHistoryScroll publishes an automatically loaded transcript at
// its newest row only after both the real terminal size and rendered history
// are available. Clearing the flag here prevents later renders from forcing a
// user who has deliberately scrolled upward back to the bottom.
func (m *model) completeInitialHistoryScroll() bool {
	if !m.initialHistoryScroll || m.width <= 0 || m.height <= 0 || !m.historyValid {
		return false
	}
	m.viewport.GotoBottom()
	m.initialHistoryScroll = false
	return true
}

func (m *model) queueStreamRefresh() tea.Cmd {
	if m.streamRefreshPending {
		return nil
	}
	m.streamRefreshPending = true
	return tea.Tick(streamRefreshInterval, func(time.Time) tea.Msg { return streamRefreshMsg{} })
}

func (m *model) refreshStreaming() {
	shouldFollow := m.shouldFollowTail()
	offset := m.viewport.YOffset
	m.renderHistory()
	if shouldFollow {
		m.viewport.GotoBottom()
	} else {
		m.viewport.SetYOffset(offset)
	}
}

// shouldFollowTail keeps follow mode stable across content growth and layout
// changes. One current visible page is the deterministic tail-adjacent zone;
// explicit upward scrolling suppresses follow mode for a short reading grace
// period even when that scroll remains inside the zone.
func (m *model) shouldFollowTail() bool {
	if !m.tailAdjacent() {
		return false
	}
	return !m.currentTime().Before(m.manualScrollUpUntil)
}

func (m *model) tailAdjacent() bool {
	maximumOffset := max(0, m.viewport.TotalLineCount()-m.viewport.Height)
	distance := max(0, maximumOffset-m.viewport.YOffset)
	return distance <= max(1, m.viewport.Height)
}

func (m *model) noteManualScrollUp() {
	m.manualScrollUpUntil = m.currentTime().Add(manualScrollGrace)
}

func (m *model) resumeTailFollowAtBottom() {
	if m.viewport.AtBottom() {
		m.manualScrollUpUntil = time.Time{}
	}
}

func (m *model) currentTime() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func (m *model) renderHistory() {
	if m.historyValid && (m.historyWidth != m.viewport.Width || m.historyTheme != m.activeTheme) {
		m.invalidateHistoryRender()
	}
	if !m.historyValid || m.historyWidth != m.viewport.Width || m.historyTheme != m.activeTheme {
		if !m.historyWorkLarge() {
			m.historyCache = renderHistoryEntries(*m)
			m.historyWidth = m.viewport.Width
			m.historyTheme = m.activeTheme
			m.historyValid = true
		}
	}
	entries := append([]string(nil), m.historyCache...)
	if m.streaming != "" {
		content := m.pendingStreamContent()
		if m.working {
			content += m.workingSpinner.View()
		}
		entries = append(entries, m.renderMarkdownChatMessage(agentChatLabel, m.styles.agent, content))
	} else if m.working {
		entries = append(entries, m.renderChatMessage(agentChatLabel, m.styles.agent, m.workingSpinner.View()))
	}
	content := strings.Join(entries, "\n\n")
	if content == "" {
		content = m.styles.status.Render(emptyConversation)
	}
	m.viewport.SetContent(content)
}

func (m *model) invalidateHistoryRender() {
	m.historyValid = false
	m.historyVersion++
	m.streamCache = ""
	m.streamRendered = ""
	m.streamRenderText = ""
}

func (m model) historyWorkLarge() bool {
	return transcriptWorkLarge(m.transcript)
}

func transcriptWorkLarge(entries []transcriptEntry) bool {
	if len(entries) > asyncHistoryEntries {
		return true
	}
	total := 0
	for _, entry := range entries {
		total += len([]rune(entry.text))
		if total > asyncHistoryRunes {
			return true
		}
	}
	return false
}

func renderHistoryEntries(snapshot model) []string {
	entries := make([]string, 0, len(snapshot.transcript)+1)
	if snapshot.welcome != nil {
		entries = append(entries, snapshot.renderWelcome(*snapshot.welcome))
	}
	for _, entry := range snapshot.transcript {
		entries = append(entries, snapshot.renderTranscriptEntry(entry))
	}
	return entries
}

func (m *model) requestHistoryRender() tea.Cmd {
	if m.historyValid || m.historyRenderBusy || !m.historyWorkLarge() || m.viewport.Width <= 0 {
		return nil
	}
	m.historyRenderBusy = true
	snapshot := *m
	snapshot.transcript = append([]transcriptEntry(nil), m.transcript...)
	if m.welcome != nil {
		welcome := *m.welcome
		snapshot.welcome = &welcome
	}
	version, width, active := m.historyVersion, m.viewport.Width, m.activeTheme
	render := m.renderHistoryEntries
	if render == nil {
		render = renderHistoryEntries
	}
	ctx := m.ctx
	return func() tea.Msg {
		select {
		case <-ctx.Done():
			return historyRenderResult{version: version, width: width, theme: active}
		default:
		}
		started := time.Now()
		entries := render(snapshot)
		return historyRenderResult{version: version, width: width, theme: active, entries: entries, elapsed: time.Since(started)}
	}
}

func (m *model) requestStreamRender() tea.Cmd {
	if m.streamRenderBusy || m.streamRenderCooling || m.streaming == "" || m.viewport.Width <= 0 {
		return nil
	}
	if m.streamRenderText == m.streaming && m.streamWidth == m.viewport.Width && m.streamTheme == m.activeTheme {
		return nil
	}
	m.streamRenderBusy = true
	text, width, active, version := m.streaming, m.chatMarkdownWidth(), m.activeTheme, m.streamVersion
	render := m.renderStream
	if render == nil {
		render = renderAgentMarkdownText
	}
	ctx := m.ctx
	return func() tea.Msg {
		select {
		case <-ctx.Done():
			return streamRenderResult{version: version, text: text, width: width, theme: active}
		default:
		}
		started := time.Now()
		rendered := render(text, width, active)
		return streamRenderResult{version: version, text: text, width: width, theme: active, rendered: rendered, elapsed: time.Since(started)}
	}
}

func (m *model) queueStreamRenderCooldown() tea.Cmd {
	if m.streamRenderCooling {
		return nil
	}
	m.streamRenderCooling = true
	return tea.Tick(streamRefreshInterval, func(time.Time) tea.Msg { return streamRenderCooldownMsg{} })
}

func (m model) pendingStreamContent() string {
	if m.streamRenderText != "" && len(m.streaming) >= len(m.streamRenderText) && m.streamWidth == m.viewport.Width && m.streamTheme == m.activeTheme {
		return m.streamRendered + pendingStreamPlain(m.streaming[len(m.streamRenderText):], m.chatMarkdownWidth())
	}
	return pendingStreamPlain(m.streaming, m.chatMarkdownWidth())
}

func pendingStreamPlain(value string, width int) string {
	value, truncated := trailingRunes(value, maxPendingRawRunes)
	value = stripUnsafeTerminalControls(value)
	if truncated {
		value = "…" + value
	}
	return ansi.Hardwrap(value, max(1, width), true)
}

func trailingRunes(value string, limit int) (string, bool) {
	if limit <= 0 {
		return "", value != ""
	}
	index := len(value)
	for count := 0; count < limit && index > 0; count++ {
		_, size := utf8.DecodeLastRuneInString(value[:index])
		index -= size
	}
	return value[index:], index > 0
}

func stripUnsafeTerminalControls(value string) string {
	value = ansi.Strip(value)
	return strings.Map(func(character rune) rune {
		if character == '\n' || character == '\t' || character >= 0x20 && character != 0x7f && !(character >= 0x80 && character <= 0x9f) {
			return character
		}
		return -1
	}, value)
}

func (m model) renderWelcome(welcome core.Screen) string {
	parts := make([]string, 0, 2)
	if welcome.Banner != "" {
		parts = append(parts, m.styles.title.Render(welcome.Banner))
	}
	if welcome.Subtitle != "" {
		contentWidth := max(1, m.viewport.Width-1)
		if welcome.Markdown {
			parts = append(parts, trimRenderedPadding(markdownfmt.TerminalWithTheme(welcome.Subtitle, contentWidth, m.activeTheme)))
		} else {
			parts = append(parts, m.styles.status.Render(ansi.Hardwrap(welcome.Subtitle, contentWidth, true)))
		}
	}
	return strings.Join(parts, "\n\n")
}

func (m model) renderTranscriptEntry(entry transcriptEntry) string {
	switch entry.role {
	case "user":
		return m.renderChatMessage(userChatLabel, m.styles.user, entry.text)
	case "assistant":
		return m.renderMarkdownChatMessage(agentChatLabel, m.styles.agent, m.renderAgentMarkdown(entry.text))
	case "error":
		return m.renderChatMessage(errorChatLabel, m.styles.error, entry.text)
	default:
		return m.renderChatMessage(entry.role, m.styles.status, entry.text)
	}
}

func (m model) renderChatMessage(label string, style lipgloss.Style, content string) string {
	return m.renderChatMessageContent(label, style, content, true)
}

func (m model) renderMarkdownChatMessage(label string, style lipgloss.Style, content string) string {
	width := m.chatContentWidth()
	rows := strings.Split(content, "\n")
	for index, row := range rows {
		if lipgloss.Width(row) > width {
			rows[index] = ansi.Hardwrap(row, width, true)
		}
	}
	return m.renderChatMessageContent(label, style, strings.Join(rows, "\n"), false)
}

func (m model) renderChatMessageContent(label string, style lipgloss.Style, content string, wrap bool) string {
	contentWidth := m.chatContentWidth()
	if wrap {
		// Word-wrap first so a fitting prose token moves intact to the fresh
		// row. Hard wrapping is reserved for a token wider than the row; keeping
		// spaces here preserves authored indentation on explicit source lines.
		content = wordWrapPreservingSpaces(content, contentWidth)
	}
	padding := strings.Repeat(" ", max(1, chatContentColumn-lipgloss.Width(label)))
	continuation := strings.Repeat(" ", chatContentColumn)
	body := padding + strings.ReplaceAll(content, "\n", "\n"+continuation)
	// Style rows independently so Lip Gloss preserves explicit newlines without
	// padding every row to the widest one. Those hidden cells can exceed the
	// viewport and soft-wrap an otherwise short Markdown heading.
	rows := strings.Split(body, "\n")
	for index, row := range rows {
		rows[index] = m.styles.base.Inline(true).Render(row)
	}
	return style.Render(label) + strings.Join(rows, "\n")
}

// wordWrapPreservingSpaces wraps source prose without normalizing authored
// whitespace. Fitting words move intact to a fresh row; only genuinely
// overwide tokens are split, and those splits occur at grapheme boundaries.
func wordWrapPreservingSpaces(content string, width int) string {
	width = max(1, width)
	lines := strings.Split(content, "\n")
	for index, line := range lines {
		lines[index] = wrapSourceLine(line, width)
	}
	return strings.Join(lines, "\n")
}

func wrapSourceLine(line string, width int) string {
	type segment struct {
		text       string
		whitespace bool
	}
	var segments []segment
	graphemes := uniseg.NewGraphemes(line)
	for graphemes.Next() {
		value := graphemes.Str()
		space := true
		for _, r := range value {
			if !unicode.IsSpace(r) {
				space = false
				break
			}
		}
		if len(segments) == 0 || segments[len(segments)-1].whitespace != space {
			segments = append(segments, segment{whitespace: space})
		}
		segments[len(segments)-1].text += value
	}

	rows := []string{""}
	rowWidth := 0
	appendGraphemes := func(value string) {
		items := uniseg.NewGraphemes(value)
		for items.Next() {
			item := items.Str()
			itemWidth := uniseg.StringWidth(item)
			if rowWidth > 0 && rowWidth+itemWidth > width {
				rows = append(rows, "")
				rowWidth = 0
			}
			rows[len(rows)-1] += item
			rowWidth += itemWidth
		}
	}
	for index, part := range segments {
		partWidth := uniseg.StringWidth(part.text)
		if part.whitespace && rowWidth > 0 && index+1 < len(segments) && !segments[index+1].whitespace {
			nextWidth := uniseg.StringWidth(segments[index+1].text)
			if nextWidth <= width && rowWidth+partWidth+nextWidth > width {
				spaces := splitGraphemes(part.text)
				for len(spaces) > 0 && rowWidth+uniseg.StringWidth(spaces[0]) <= width {
					appendGraphemes(spaces[0])
					spaces = spaces[1:]
				}
				rows = append(rows, "")
				rowWidth = 0
				// A soft line boundary represents one ordinary separator cell
				// that had no physical room on the prior row. Any additional
				// authored whitespace remains visible and byte-for-byte intact.
				if len(spaces) > 0 {
					spaces = spaces[1:]
				}
				appendGraphemes(strings.Join(spaces, ""))
				continue
			}
		}
		if !part.whitespace && partWidth <= width && rowWidth > 0 && rowWidth+partWidth > width {
			rows = append(rows, "")
			rowWidth = 0
		}
		appendGraphemes(part.text)
	}
	return strings.Join(rows, "\n")
}

func splitGraphemes(value string) []string {
	var result []string
	graphemes := uniseg.NewGraphemes(value)
	for graphemes.Next() {
		result = append(result, graphemes.Str())
	}
	return result
}

func (m model) chatContentWidth() int {
	return max(1, m.viewport.Width-chatContentColumn)
}

func (m model) renderAgentMarkdown(text string) string {
	return renderAgentMarkdownText(text, m.chatMarkdownWidth(), m.activeTheme)
}

func renderAgentMarkdownText(text string, width int, active theme.Theme) string {
	if remainder, found := cutSpynelLogoMarkdown(text); found {
		parts := []string{stylesFor(active).title.Render(core.SpynelASCII)}
		if remainder = strings.TrimSpace(remainder); remainder != "" {
			parts = append(parts, trimRenderedPadding(markdownfmt.TerminalWithTheme(remainder, width, active)))
		}
		return strings.Join(parts, "\n\n")
	}
	return trimRenderedPadding(markdownfmt.TerminalWithTheme(text, width, active))
}

func (m model) chatMarkdownWidth() int {
	// TerminalWithTheme converts its inclusive public width to Glamour's
	// exclusive word-wrap boundary by adding one. Pass the physical content
	// boundary minus one so padded inline-code spans wrap at the real edge.
	return max(1, m.chatContentWidth()-1)
}

// cutSpynelLogoMarkdown treats the fenced logo body as a semantic marker, not
// immutable transcript art. Older persisted /welcome messages therefore pick
// up the current canonical logo instead of preserving a superseded design.
func cutSpynelLogoMarkdown(text string) (string, bool) {
	const prefix = "```spynel-logo\n"
	remainder, found := strings.CutPrefix(text, prefix)
	if !found {
		return "", false
	}
	_, remainder, found = strings.Cut(remainder, "\n```")
	return remainder, found
}

func trimRenderedPadding(content string) string {
	return markdownfmt.TrimTerminalLinePadding(content)
}

func (m model) View() string {
	return m.overlayDialog(m.viewWithoutDialog())
}

func (m model) viewWithoutDialog() string {
	if m.screen != nil && m.screen.ID == core.ScreenWhatsAppQR {
		return m.fullscreenWhatsAppQR()
	}
	barWidth := max(1, m.width)
	header := m.headerView(barWidth)
	if m.screen != nil {
		screenHeight := max(5, m.height-2)
		fixedRows := 0
		if strings.TrimSpace(m.screen.Title) != "" {
			fixedRows += 2 // screen title plus one separating row
		}
		if len(m.screen.Tabs) > 0 {
			fixedRows += 3 // labels, underline, and one separating row
		}
		contentHeight := max(1, screenHeight-2-fixedRows)
		contentWidth := max(1, m.width-3)
		content, offset, total := m.screenContent(contentHeight, contentWidth)
		content = fitContent(content, contentHeight, contentWidth)
		form := m.screenPanel(m.screen.Title, content, screenHeight, barWidth, offset, total)
		return lipgloss.JoinVertical(lipgloss.Left, header, form, m.footerView(m.screenFooterHint(), barWidth))
	}
	if m.height > 0 && m.height < layoutOverhead+minComposerHeight+1 {
		inputOffset, inputRows := m.inputScrollMetrics()
		inputView := fitContent(m.renderInput(), m.composerRows, m.inputWidth)
		input := m.borderedSurface("", inputView, m.composerRows, barWidth, inputOffset, inputRows, m.styles.surface)
		return lipgloss.JoinVertical(lipgloss.Left, header, input, m.footerView(m.footerHint(), barWidth))
	}
	historyView := fitContent(m.viewport.View(), m.viewport.Height, m.viewport.Width)
	chat := m.historySurface(historyView, m.viewport.Height, barWidth, m.viewport.YOffset, m.viewport.TotalLineCount())
	inputOffset, inputRows := m.inputScrollMetrics()
	inputView := fitContent(m.renderInput(), m.composerRows, m.inputWidth)
	input := m.borderedSurface("", inputView, m.composerRows, barWidth, inputOffset, inputRows, m.styles.surface)
	sections := []string{header, chat}
	if picker := m.inlineMenuView(); picker != "" {
		sections = append(sections, picker)
	}
	sections = append(sections, input, m.footerView(m.footerHint(), barWidth))
	return lipgloss.JoinVertical(lipgloss.Left, sections...)
}

func (m model) fullscreenWhatsAppQR() string {
	if m.screen == nil {
		return ""
	}
	return lipgloss.Place(
		max(1, m.width), max(1, m.height), lipgloss.Center, lipgloss.Center,
		m.screen.Banner,
		lipgloss.WithWhitespaceBackground(m.styles.base.GetBackground()),
	)
}

func applyPairingEvent(screen *core.Screen, event channel.PairingEvent) bool {
	if screen == nil || (screen.ID != event.Name && !strings.HasPrefix(screen.ID, "wizard:"+event.Name+":")) {
		return false
	}
	if screen.ID == "whatsapp" {
		screen.Banner = ""
	}
	screen.Status = event.Detail
	return true
}

func (m model) headerView(width int) string {
	ribbon := m.styles.headerFill.Render("▀▀")
	identity := m.styles.title.Background(m.styles.header.GetBackground()).Render(m.spynelLogo() + " " + m.title)
	left := ribbon + identity
	segments := []string{
		m.connectionSegment("telegram", "TG"),
		m.connectionSegment("whatsapp", "WA"),
		m.styles.status.Background(m.styles.header.GetBackground()).Render(runtimeCount(m.durableWork.Goals, "goal")),
		m.styles.status.Background(m.styles.header.GetBackground()).Render(runtimeCount(m.durableWork.Tasks, "task")),
		m.styles.status.Background(m.styles.header.GetBackground()).Render(runtimeCount(m.runtimeStatus.Jobs, "job")),
		m.styles.status.Background(m.styles.header.GetBackground()).Render(runtimeCount(m.runtimeStatus.Logs, "log")),
	}
	right := strings.Join(segments, ribbon) + ribbon
	// Preserve the workspace identity first; compact/truncate volatile status
	// on narrow terminals instead of squeezing every title into an ellipsis.
	leftWidth := min(lipgloss.Width(left), max(12, width*2/5))
	left = ansi.Truncate(left, leftWidth, "…")
	rightWidth := max(1, width-lipgloss.Width(left)-2)
	right = ansi.Truncate(right, rightWidth, "…")
	gapWidth := max(2, width-lipgloss.Width(left)-lipgloss.Width(right))
	gap := m.styles.headerFill.Render(strings.Repeat("▀", gapWidth))
	content := left + gap + right
	return fillLine(m.styles.header, content, width)
}

func (m model) footerView(hint string, width int) string {
	width = max(1, width)
	items := strings.Split(hint, " · ")
	for index, item := range items {
		items[index] = m.styles.footer.Render(item)
	}
	ribbon := m.styles.footerFill.Render("▄▄")
	middle := strings.Join(items, ribbon)
	rightWidth := max(2, width-lipgloss.Width(middle)-2)
	content := ribbon + middle + m.styles.footerFill.Render(strings.Repeat("▄", rightWidth))
	return ansi.Truncate(content, width, "")
}

func (m model) screenPanel(title, content string, height, width, offset, total int) string {
	parts := make([]string, 0, 5)
	panelTotal := total
	if strings.TrimSpace(title) != "" {
		parts = append(parts, m.styles.title.Render(title), "")
		panelTotal += 2
	}
	if m.screen != nil && len(m.screen.Tabs) > 0 {
		parts = append(parts, m.tabsView(max(1, width-3)))
		parts = append(parts, "")
		panelTotal += 3
	}
	parts = append(parts, content)
	body := fitContent(lipgloss.JoinVertical(lipgloss.Left, parts...), max(1, height-2), max(1, width-3))
	return m.screenCanvas(body, height, width, offset, panelTotal)
}

func (m model) tabsView(width int) string {
	if m.screen == nil || len(m.screen.Tabs) == 0 {
		return ""
	}
	width = max(1, width)
	const gapWidth = 3
	labels := make([]string, 0, len(m.screen.Tabs))
	underlines := make([]string, 0, len(m.screen.Tabs))
	for index, tab := range m.screen.Tabs {
		tab = strings.TrimSpace(tab)
		labelStyle := m.styles.status.Bold(true)
		underlineStyle := m.styles.track
		if index == m.screen.ActiveTab {
			labelStyle = m.styles.base.Bold(true)
			underlineStyle = m.styles.thumb
		}
		labels = append(labels, labelStyle.Render(tab))
		underlines = append(underlines, underlineStyle.Render(strings.Repeat("━", max(1, lipgloss.Width(tab)))))
	}
	gap := strings.Repeat(" ", gapWidth)
	trackGap := m.styles.track.Render(strings.Repeat("━", gapWidth))
	labelRow := ansi.Truncate(strings.Join(labels, gap), width, "…")
	underlineRow := ansi.Truncate(strings.Join(underlines, trackGap), width, "")
	if remaining := width - lipgloss.Width(underlineRow); remaining > 0 {
		underlineRow += m.styles.track.Render(strings.Repeat("━", remaining))
	}
	return labelRow + "\n" + underlineRow
}

func (m model) screenCanvas(content string, height, width, offset, total int) string {
	height = max(3, height)
	width = max(4, width)
	bodyHeight := height - 2
	contentWidth := width - 3
	lines := strings.Split(content, "\n")
	edge := make([]string, bodyHeight)
	if total > bodyHeight {
		edge = scrollbar(bodyHeight, offset, total, m.styles, m.styles.base.GetBackground())
	} else {
		for row := range edge {
			edge[row] = m.styles.base.Render(" ")
		}
	}
	result := make([]string, 0, height)
	result = append(result, fillLine(m.styles.base, "", width))
	for row := 0; row < bodyHeight; row++ {
		line := ""
		if row < len(lines) {
			line = ansi.Truncate(lines[row], contentWidth, "")
		}
		line += strings.Repeat(" ", max(0, contentWidth-lipgloss.Width(line)))
		result = append(result, fillLine(m.styles.base, " "+line+" ", width-1)+edge[row])
	}
	result = append(result, fillLine(m.styles.base, "", width))
	return strings.Join(result, "\n")
}

func (m model) borderedSurface(title, content string, height, width, offset, total int, style lipgloss.Style) string {
	height = max(1, height)
	width = max(4, width)
	innerWidth := width - 2
	contentWidth, inset := borderedContentGeometry(width)
	// Borders sit outside the panel surface and therefore belong to the page,
	// not the terminal's default background or the control's inner surface.
	border := m.panelBorderStyle()
	titlePart := ""
	if title != "" {
		titlePart = "─" + title
	}
	topFill := strings.Repeat("─", max(0, innerWidth-lipgloss.Width(titlePart)))
	top := border.Render("╭" + titlePart + topFill + "╮")
	lines := strings.Split(content, "\n")
	edge := make([]string, height)
	if total > height {
		edge = scrollbar(height, offset, total, m.styles, m.styles.base.GetBackground())
	} else {
		for row := range edge {
			edge[row] = border.Render("│")
		}
	}
	result := make([]string, 0, height+2)
	result = append(result, top)
	for row := 0; row < height; row++ {
		line := ""
		if row < len(lines) {
			line = lines[row]
		}
		line = ansi.Truncate(line, contentWidth, "")
		line = strings.Repeat(" ", inset) + line + strings.Repeat(" ", max(0, contentWidth-lipgloss.Width(line))) + strings.Repeat(" ", inset)
		result = append(result, border.Render("│")+fillLine(style, line, innerWidth)+edge[row])
	}
	result = append(result, border.Render("╰"+strings.Repeat("─", innerWidth)+"╯"))
	return strings.Join(result, "\n")
}

func borderedContentGeometry(width int) (contentWidth, inset int) {
	innerWidth := max(4, width) - 2
	if innerWidth < 4 {
		// Preserve the border and all feasible content cells before cosmetic
		// horizontal padding. This lets a two-cell grapheme remain visible at the
		// four- and five-column boundary.
		return innerWidth, 0
	}
	return max(1, innerWidth-2), 1
}

func (m model) panelBorderStyle() lipgloss.Style {
	return m.styles.track.Background(m.styles.base.GetBackground())
}

func (m model) historySurface(content string, height, width, offset, total int) string {
	height = max(1, height)
	width = max(4, width)
	contentWidth := width - 3
	lines := strings.Split(content, "\n")
	edge := make([]string, height)
	if total > height {
		edge = scrollbar(height, offset, total, m.styles, m.styles.base.GetBackground())
	} else {
		for row := range edge {
			edge[row] = m.styles.base.Render(" ")
		}
	}
	result := make([]string, 0, height+2)
	// Keep one fixed main-background row above the scrollable transcript.
	result = append(result, fillLine(m.styles.base, "", width))
	for row := 0; row < height; row++ {
		line := ""
		if row < len(lines) {
			line = ansi.Truncate(lines[row], contentWidth, "")
		}
		line += strings.Repeat(" ", max(0, contentWidth-lipgloss.Width(line)))
		// One main-background cell before content and one before the scrollbar.
		result = append(result, fillLine(m.styles.base, " "+line+" ", width-1)+edge[row])
	}
	// Match the fixed top inset below the transcript before any picker/input.
	result = append(result, fillLine(m.styles.base, "", width))
	return strings.Join(result, "\n")
}

func fillLine(style lipgloss.Style, content string, width int) string {
	width = max(1, width)
	content = ansi.Truncate(content, width, "")
	padding := strings.Repeat(" ", max(0, width-lipgloss.Width(content)))
	rendered := style.Render(content + padding)
	background := style.GetBackground()
	if _, absent := background.(lipgloss.NoColor); absent {
		return rendered
	}
	backgroundSGR := trueColorBackgroundSGR(background)
	// Nested component and Markdown styles reset SGR at their boundaries.
	// Reapply the owning panel background after those resets so the panel is
	// one continuous surface rather than terminal-colored holes around text.
	return backgroundSGR + strings.ReplaceAll(rendered, "\x1b[0m", "\x1b[0m"+backgroundSGR) + "\x1b[0m"
}

func trueColorBackgroundSGR(background lipgloss.TerminalColor) string {
	if color, ok := background.(lipgloss.Color); ok {
		if parsed, err := strconv.ParseUint(strings.TrimPrefix(string(color), "#"), 16, 24); err == nil {
			return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", (parsed>>16)&0xff, (parsed>>8)&0xff, parsed&0xff)
		}
	}
	red, green, blue, _ := background.RGBA()
	return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", red>>8, green>>8, blue>>8)
}

func (m *model) openScreen(screen core.Screen) {
	m.dialog = nil
	if screen.ID == "welcome" {
		copyScreen := screen
		copyScreen.Controls = nil
		m.welcome = &copyScreen
		m.invalidateHistoryRender()
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
		m.initialHistoryScroll = len(m.transcript) > 0
		m.invalidateHistoryRender()
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
	copyScreen.Hints = append([]core.ScreenHint(nil), screen.Hints...)
	copyScreen.Controls = append([]core.ScreenControl(nil), screen.Controls...)
	if copyScreen.ID == "whatsapp" {
		// Do not render QR data on the general WhatsApp settings surface, even
		// if a stale or older server supplied it through the generic banner.
		copyScreen.Banner = ""
	}
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
	if m.screenSaving {
		return nil
	}
	if key.Type == tea.KeyEsc {
		if m.screen.Required {
			m.screenResult = "exit"
			return tea.Quit
		}
		if m.restoreParentScreen() {
			m.status = "Editing " + m.screen.Title
			return nil
		}
		if m.isSettingsScreen() && m.screenDirty() {
			m.confirmDiscardScreenChanges()
			return nil
		}
		m.clearScreen()
		m.status = "Ready"
		return nil
	}
	visible := m.visibleScreenControlIndices()
	if len(visible) == 0 {
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
	if m.screen.ID == "resume" && (key.Type == tea.KeyDelete || key.Type == tea.KeyBackspace) && strings.HasPrefix(control.Key, "resume:") {
		return m.runScreenAction("delete:" + strings.TrimPrefix(control.Key, "resume:"))
	}
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

func (m *model) refreshRestoredSelection(key, selection string) {
	if m.screen == nil || m.screen.ID != "config" {
		return
	}
	for index := range m.screen.Controls {
		control := &m.screen.Controls[index]
		if control.Key != key {
			continue
		}
		label, _, found := strings.Cut(control.Value, " · ")
		if !found {
			label = control.Value
		}
		control.Value = strings.TrimSpace(label) + " · " + selection
		return
	}
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
	m.dialog = nil
}

func cloneScreen(screen *core.Screen) *core.Screen {
	if screen == nil {
		return nil
	}
	copyScreen := *screen
	copyScreen.Hints = append([]core.ScreenHint(nil), screen.Hints...)
	copyScreen.Tabs = append([]string(nil), screen.Tabs...)
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
	selectedIndex := m.screenIndex
	values := m.screenValues()
	m.screenSaving = true
	ctx := m.ctx
	return func() tea.Msg {
		next, err := callback(ctx, screenID, action, values)
		return screenActionResult{action: action, selectedIndex: selectedIndex, screen: next, err: err}
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
	changes := m.screenChanges()
	if len(changes) == 0 {
		if m.isSettingsScreen() {
			m.clearScreen()
		}
		m.status = "No configuration changes"
		return nil
	}
	m.screenSaving = true
	screenID := m.screen.ID
	closeOnSuccess := m.isSettingsScreen()
	save := m.saveSettings
	return func() tea.Msg {
		return screenSaveResult{screenID: screenID, closeOnSuccess: closeOnSuccess, err: save(changes)}
	}
}

func (m model) screenChanges() map[string]string {
	changes := map[string]string{}
	if m.screen == nil {
		return changes
	}
	for _, control := range m.screen.Controls {
		if control.Kind == "action" || control.Kind == "disclosure" || control.Kind == "hidden" {
			continue
		}
		if original, ok := m.screenOriginal[control.Key]; !ok || original != control.Value {
			changes[control.Key] = control.Value
		}
	}
	return changes
}

func (m model) screenDirty() bool {
	return len(m.screenChanges()) > 0
}

func (m *model) confirmDiscardScreenChanges() {
	m.openDialog(dialogModel{
		title:       "Unsaved changes",
		message:     "You have unsaved changes. What do you want to do?",
		selected:    2,
		cancelValue: "keep",
		options: []dialogOption{
			{label: "Save", value: "save"},
			{label: "Discard", value: "discard"},
			{label: "Keep editing", value: "keep"},
		},
		resolve: func(m *model, value string) tea.Cmd {
			if value == "save" {
				return m.saveScreen()
			}
			if value == "discard" {
				m.clearScreen()
				m.status = "Changes discarded"
				return nil
			}
			if m.screen != nil {
				title := strings.TrimSpace(m.screen.Title)
				if title == "" {
					title = strings.Title(m.screen.ID) //nolint:staticcheck
				}
				m.status = "Editing " + title
			}
			return nil
		},
	})
}

func (m model) screenContent(height, width int) (string, int, int) {
	if m.screen == nil {
		return "", 0, 0
	}
	innerWidth := max(12, width)
	// Prose renderers treat their wrap width as an exclusive boundary in a few
	// styled/Unicode cases. Reserve one cell so a complete boundary word wraps
	// before screenCanvas applies its final hard safety truncation.
	textWidth := max(1, innerWidth-1)
	lines := m.screenConnectionSection(innerWidth)
	if m.screen.Banner != "" {
		for _, line := range strings.Split(m.screen.Banner, "\n") {
			lines = append(lines, m.styles.agent.Render(line))
		}
		lines = append(lines, "")
	}
	if m.screen.Status != "" {
		lines = append(lines, m.styles.agent.Render(ansi.Hardwrap(m.screen.Status, textWidth, true)), "")
	}
	if m.screen.Subtitle != "" {
		if m.screen.Markdown {
			rendered := strings.Trim(trimRenderedPadding(markdownfmt.TerminalWithTheme(m.screen.Subtitle, textWidth, m.activeTheme)), "\n")
			lines = append(lines, strings.Split(rendered, "\n")...)
		} else {
			for _, line := range strings.Split(m.screen.Subtitle, "\n") {
				lines = append(lines, m.styles.status.Render(ansi.Hardwrap(line, textWidth, true)))
			}
		}
		lines = appendBlankScreenRow(lines)
	}
	if section := m.formSectionTitle(); section != "" && !m.screenHasControlSections() {
		lines = append(lines, m.sectionRule(section, innerWidth), "")
	}
	selectedLine := 0
	previousControlKind := ""
	previousControlKey := ""
	for index, control := range m.screen.Controls {
		if control.Kind == "hidden" || (control.Advanced && !m.screenAdvanced) {
			continue
		}
		sectionDisclosure := control.Kind == "disclosure" && control.Section != ""
		if control.Section != "" {
			if m.isSettingsScreen() {
				lines = appendBlankScreenRows(lines, 2)
			} else {
				lines = appendBlankScreenRow(lines)
			}
			if !sectionDisclosure {
				lines = append(lines, m.sectionRule(control.Section, innerWidth), "")
			}
		}
		separateWizardLauncher := control.Section == "" && (m.screen.ID == "telegram" || m.screen.ID == "whatsapp") && previousControlKey == "wizard"
		separateWizardActions := strings.HasPrefix(m.screen.ID, "wizard:") && control.Kind == "action" && previousControlKind != "" && previousControlKind != "action"
		separateDisclosure := control.Section == "" && control.Kind == "disclosure" && previousControlKind != ""
		separateAdvancedControls := control.Section == "" && control.Advanced && previousControlKind == "disclosure"
		separateInitializationActions := m.screen.Required && previousControlKind == "action" && control.Kind == "action"
		if separateWizardLauncher || separateWizardActions || separateDisclosure || separateAdvancedControls || separateInitializationActions {
			lines = appendBlankScreenRow(lines)
		}
		value := control.Value
		if control.Kind == "disclosure" {
			value = "Show Advanced Settings"
			if m.screenAdvanced {
				value = "Hide Advanced Settings"
			}
		}
		if control.Secret {
			if value != "" {
				value = strings.Repeat("*", len([]rune(value)))
			} else if control.Configured {
				value = "(configured; type to replace)"
			}
		}
		if index == m.screenIndex && (control.Kind == "text" || control.Kind == "password") {
			value = m.renderScreenTextCursor(index, control, value)
		}
		selected := index == m.screenIndex
		label := strings.Title(control.Label) //nolint:staticcheck
		line := ""
		switch control.Kind {
		case "action":
			line = m.screenButton(value, selected)
		case "disclosure":
			if sectionDisclosure {
				line = m.disclosureSectionRule(value, selected, innerWidth)
			} else {
				line = m.screenButton(value, selected)
			}
		case "toggle", "select":
			line = m.screenFieldLine(label, "‹ "+value+" ›", selected, innerWidth)
		default:
			line = m.screenFieldLine(label, value, selected, innerWidth)
		}
		if index == m.screenIndex {
			selectedLine = len(lines)
		}
		lines = append(lines, ansi.Truncate(line, innerWidth, "…"))
		descriptionWidth := textWidth
		if control.DescriptionMarkdown {
			rendered := strings.Trim(trimRenderedPadding(markdownfmt.TerminalWithTheme(control.Description, descriptionWidth, m.activeTheme)), "\n")
			if rendered != "" {
				for _, descriptionLine := range strings.Split(rendered, "\n") {
					lines = append(lines, m.styles.status.Render(ansi.Truncate(descriptionLine, descriptionWidth, "…")))
				}
			}
		} else if control.Description != "" {
			if m.screen.ID == "resume" {
				const indent = "     "
				description := strings.Join(strings.Fields(control.Description), " ")
				description = ansi.Truncate(description, max(1, descriptionWidth-lipgloss.Width(indent)), "…")
				lines = append(lines, m.styles.status.Render(indent+description))
			} else {
				wrapped := ansi.Hardwrap(control.Description, descriptionWidth, true)
				for _, descriptionLine := range strings.Split(wrapped, "\n") {
					lines = append(lines, m.styles.status.Render(ansi.Truncate(descriptionLine, descriptionWidth, "…")))
				}
			}
		}
		if m.isSettingsScreen() {
			lines = appendBlankScreenRow(lines)
		}
		previousControlKind = control.Kind
		previousControlKey = control.Key
	}
	for len(lines) > 0 && strings.TrimSpace(ansi.Strip(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	visible := max(1, height)
	offset := bounded(selectedLine-visible/2, 0, max(0, len(lines)-visible))
	if m.screenManual {
		offset = bounded(m.screenScroll, 0, max(0, len(lines)-visible))
	}
	end := min(len(lines), offset+visible)
	return strings.Join(lines[offset:end], "\n"), offset, len(lines)
}

func appendBlankScreenRow(lines []string) []string {
	if len(lines) == 0 {
		return lines
	}
	if strings.TrimSpace(ansi.Strip(lines[len(lines)-1])) != "" {
		return append(lines, "")
	}
	return lines
}

func appendBlankScreenRows(lines []string, count int) []string {
	if len(lines) == 0 || count <= 0 {
		return lines
	}
	trailing := 0
	for index := len(lines) - 1; index >= 0 && strings.TrimSpace(ansi.Strip(lines[index])) == ""; index-- {
		trailing++
	}
	for trailing < count {
		lines = append(lines, "")
		trailing++
	}
	return lines
}

func (m model) isSettingsScreen() bool {
	return m.screen != nil && (m.screen.ID == "config" || m.screen.ID == "telegram" || m.screen.ID == "whatsapp")
}

func (m model) screenHasControlSections() bool {
	if m.screen == nil {
		return false
	}
	for _, control := range m.screen.Controls {
		if control.Section != "" {
			return true
		}
	}
	return false
}

func (m model) screenFieldLine(label, value string, selected bool, width int) string {
	width = max(1, width)
	value = ansi.Truncate(value, max(1, width/2), "…")
	valueWidth := lipgloss.Width(value)
	label = ansi.Truncate(label, max(1, width-valueWidth-1), "…")
	gap := strings.Repeat(" ", max(1, width-lipgloss.Width(label)-valueWidth))
	labelStyle := m.styles.base
	valueStyle := m.styles.base
	if selected {
		labelStyle = m.styles.title
		valueStyle = m.styles.title
	}
	return labelStyle.Render(label) + gap + valueStyle.Render(value)
}

func (m model) formSectionTitle() string {
	if m.screen == nil || strings.HasPrefix(m.screen.ID, "wizard:") {
		return ""
	}
	switch m.screen.ID {
	case "config":
		return "Core settings"
	case "telegram", "whatsapp":
		return "Basic settings"
	case "harness", "model":
		return "Choose an option"
	default:
		return ""
	}
}

func (m model) sectionRule(label string, width int) string {
	label = strings.TrimSpace(label)
	left := m.styles.title.Render(label)
	rule := strings.Repeat("─", max(0, width-lipgloss.Width(label)-1))
	return left + " " + m.styles.track.Render(rule)
}

func (m model) screenButton(value string, selected bool) string {
	buttonStyle := m.styles.elevated
	if selected {
		buttonStyle = m.styles.selected.Foreground(m.styles.title.GetForeground())
	}
	return buttonStyle.Render(" " + value + " ↵ ")
}

func (m model) disclosureSectionRule(value string, selected bool, width int) string {
	button := m.screenButton(value, selected)
	rule := strings.Repeat("─", max(0, width-lipgloss.Width(button)))
	if rule == "" {
		return button
	}
	return button + m.styles.track.Render(rule)
}

func (m model) screenConnectionSection(innerWidth int) []string {
	if m.screen.ID != "telegram" && m.screen.ID != "whatsapp" {
		return nil
	}
	name := m.screen.ID
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
	indicatorStyle := m.styles.status
	if status.State == channel.ConnectionConnected {
		indicatorStyle = m.styles.success
	} else if status.State == channel.ConnectionError {
		indicatorStyle = m.styles.error
	}
	heading := "WhatsApp Status"
	if m.screen.ID == "telegram" {
		heading = "Telegram Status"
	}
	lines := []string{m.sectionRule(heading, innerWidth), "", indicatorStyle.Render(indicator)}
	if status.State == channel.ConnectionConnected && status.Identity != "" {
		identity := status.Identity
		if status.Link != "" {
			identity = "[" + status.Identity + "](" + status.Link + ")"
		}
		label := "Account: "
		if name == "telegram" {
			label = "Bot: "
		}
		rendered := strings.Trim(trimRenderedPadding(markdownfmt.TerminalWithTheme(label+identity, max(1, innerWidth-2), m.activeTheme)), "\n")
		for _, line := range strings.Split(rendered, "\n") {
			lines = append(lines, m.styles.status.Render(line))
		}
	}
	if detail := strings.Join(strings.Fields(status.Detail), " "); status.State == channel.ConnectionError && detail != "" {
		wrapped := ansi.Hardwrap(detail, max(1, innerWidth-2), true)
		for _, line := range strings.Split(wrapped, "\n") {
			lines = append(lines, m.styles.status.Render(line))
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
	if m.screen != nil && len(m.screen.Hints) > 0 {
		hints := make([]string, 0, len(m.screen.Hints))
		for _, hint := range m.screen.Hints {
			action := hint.Action
			if hint.Key == "␛" && len(m.screenStack) > 0 {
				action = "back"
			}
			gap := " "
			if hint.Key == "⌦" {
				// This glyph fills its terminal cell visually in several common
				// fonts, so reserve one extra cell to preserve a visible gap.
				gap = "  "
			}
			hints = append(hints, strings.TrimSpace(hint.Key+gap+action))
		}
		return strings.Join(hints, " · ")
	}
	if m.screen != nil && (m.screen.ID == "harness" || m.screen.ID == "model") {
		escape := "cancel"
		if len(m.screenStack) > 0 {
			escape = "back"
		}
		return "↑↓/⇥ nav · ␠/↵ select · ␛ " + escape
	}
	if m.screen != nil && m.screen.SaveDisabled {
		return "↑↓/⇥ nav · ␠/↵ choose · ␛ cancel"
	}
	return "↑↓/⇥ nav · ␠/↵ choose · ⌃S save · ␛ exit"
}

func (m model) footerHint() string {
	if m.themeMenu {
		return "↑↓ preview · ↵ apply · ␛ cancel"
	}
	if m.commandMenu {
		return "↑↓ choose · ⇥ insert · ↵ send · ␛ close"
	}
	return "↵ send · ⇧↵ line · PgUp/⇧+↑↓ scroll · ⌃C clear/stop/quit"
}

func (m model) inlineMenuView() string {
	if m.inlineMenuHeight() == 0 {
		return ""
	}
	if m.themeMenu {
		return m.themeMenuView()
	}
	return m.commandMenuView()
}

func (m model) spynelLogo() string {
	if m.logoAnimation != logoStopped {
		return m.logoSpinner.View()
	}
	if m.logoStoppedFrame != "" {
		return m.logoStoppedFrame
	}
	if len(m.transcript) > 0 && m.transcript[len(m.transcript)-1].role == "assistant" {
		return "◉◉"
	}
	return "○○"
}

func runtimeCount(count int, singular string) string {
	label := singular + "s"
	if count == 1 {
		label = singular
	}
	return core.CompactCount(count) + " " + label
}

func (m model) connectionSegment(name, short string) string {
	status, ok := m.connection[name]
	if !ok {
		status = channel.ConnectionStatus{Name: name, State: channel.ConnectionUnconfigured}
	}
	icon := "○"
	iconStyle := m.styles.status
	switch status.State {
	case channel.ConnectionConnected:
		icon = "●"
		iconStyle = m.styles.success
	case channel.ConnectionConnecting:
		icon = "◐"
		iconStyle = m.styles.warning
	case channel.ConnectionError:
		icon = "▲"
		iconStyle = m.styles.error
	}
	icon = iconStyle.Background(m.styles.header.GetBackground()).Render(icon)
	return icon + m.styles.status.Background(m.styles.header.GetBackground()).Render(" "+short)
}

func (m model) commandMenuView() string {
	if !m.commandMenu {
		return ""
	}
	matches := m.commandMatches()
	if len(matches) == 0 {
		return ""
	}
	visibleRows := max(1, m.inlineMenuHeight()-2)
	start := 0
	if m.commandIndex >= visibleRows {
		start = m.commandIndex - visibleRows + 1
	}
	end := min(len(matches), start+visibleRows)
	lineWidth := max(1, m.width-4)
	lines := make([]string, 0, end-start)
	for index := start; index < end; index++ {
		command := matches[index]
		line := fmt.Sprintf("%-28s %s", command.Usage, command.Description)
		line = truncateWidth(line, lineWidth)
		style := m.styles.command
		if index == m.commandIndex {
			style = m.styles.selectedCommand
		}
		lines = append(lines, style.Width(lineWidth).Render(line))
	}
	content := strings.Join(lines, "\n")
	return m.inlinePicker("Commands", content, end-start, max(1, m.width), start, len(matches))
}

func (m model) themeMenuView() string {
	if !m.themeMenu || len(m.themes) == 0 {
		return ""
	}
	visibleRows := max(1, m.inlineMenuHeight()-2)
	start := 0
	if m.themeIndex >= visibleRows {
		start = m.themeIndex - visibleRows + 1
	}
	end := min(len(m.themes), start+visibleRows)
	lineWidth := max(1, m.width-4)
	lines := make([]string, 0, end-start)
	for index := start; index < end; index++ {
		value := m.themes[index]
		line := fmt.Sprintf("%-22s %s", value.Name, value.Description)
		line = truncateWidth(line, lineWidth)
		style := m.styles.command
		if index == m.themeIndex {
			style = m.styles.selectedCommand
		}
		lines = append(lines, style.Width(lineWidth).Render(line))
	}
	return m.inlinePicker("Themes", strings.Join(lines, "\n"), end-start, max(1, m.width), start, len(m.themes))
}

func (m model) inlinePicker(title, content string, rows, width, offset, total int) string {
	width = max(1, width)
	return m.borderedSurface(" "+title+" ", content, max(1, rows), width, offset, total, m.styles.elevated)
}

func (m model) renderInput() string {
	view := m.input.View()
	for _, token := range m.tokens {
		view = strings.ReplaceAll(view, token.label, m.styles.token.Render(token.label))
	}
	return view
}

func (m model) inputScrollMetrics() (int, int) {
	total := composerTextareaVisualRows(m.input)
	cursorRow := 0
	lines := strings.Split(m.input.Value(), "\n")
	probe := newComposerProbe(m.input.Width())
	for index := 0; index < m.input.Line() && index < len(lines); index++ {
		probe.SetValue(lines[index])
		cursorRow += max(minComposerHeight, probe.LineInfo().Height)
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

func scrollbar(height, offset, total int, styles uiStyles, background lipgloss.TerminalColor) []string {
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
	track := styles.track.Background(background)
	thumb := styles.thumb.Background(background)
	for row := range result {
		if row >= start && row < start+thumbHeight {
			result[row] = thumb.Render("┃")
		} else {
			result[row] = track.Render("│")
		}
	}
	return result
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
