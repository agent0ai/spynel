package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/frdel/spynel/internal/channel"
	"github.com/frdel/spynel/internal/core"
	"github.com/frdel/spynel/internal/history"
)

func TestComposerHeight(t *testing.T) {
	tests := []struct {
		name  string
		value string
		width int
		want  int
	}{
		{name: "empty", width: 20, want: 1},
		{name: "single line", value: "hello", width: 20, want: 1},
		{name: "explicit lines", value: "one\ntwo\nthree", width: 20, want: 3},
		{name: "cursor wraps at exact width", value: "1234", width: 4, want: 2},
		{name: "wrapped line", value: "123456789", width: 4, want: 3},
		{name: "capped", value: "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11", width: 20, want: 10},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := composerHeight(test.value, test.width); got != test.want {
				t.Fatalf("composerHeight(%q, %d) = %d, want %d", test.value, test.width, got, test.want)
			}
		})
	}
}

func TestComposerPlaceholderIsMutedItalicWithoutBackground(t *testing.T) {
	input := textarea.New()
	styleComposer(&input)

	for name, style := range map[string]textarea.Style{
		"focused": input.FocusedStyle,
		"blurred": input.BlurredStyle,
	} {
		if !style.Placeholder.GetItalic() {
			t.Errorf("%s placeholder is not italic", name)
		}
		if style.Placeholder.GetForeground() != muted {
			t.Errorf("%s placeholder foreground = %v, want %v", name, style.Placeholder.GetForeground(), muted)
		}
		_, placeholderHasNoColor := style.Placeholder.GetBackground().(lipgloss.NoColor)
		_, cursorLineHasNoColor := style.CursorLine.GetBackground().(lipgloss.NoColor)
		if !placeholderHasNoColor || !cursorLineHasNoColor {
			t.Errorf("%s placeholder or cursor line retains a background", name)
		}
		if style.CursorLine.Render("selected line") != style.Text.Render("selected line") {
			t.Errorf("%s cursor line differs from ordinary input text", name)
		}
	}
}

func TestYouAndSpyPrefixesAlign(t *testing.T) {
	m := testModel()
	user := ansi.Strip(m.renderTranscriptEntry(transcriptEntry{role: "user", text: "hello"}))
	agent := ansi.Strip(m.renderTranscriptEntry(transcriptEntry{role: "assistant", text: "hello"}))
	if strings.Contains(user, "›") || strings.Contains(agent, "›") {
		t.Fatalf("sender separator remains: user=%q agent=%q", user, agent)
	}
	if user != "You hello" || agent != "Spy hello" {
		t.Fatalf("chat rows = %q and %q, want compact four-character prefixes", user, agent)
	}
	if userContent, agentContent := strings.Index(user, "hello"), strings.Index(agent, "hello"); userContent != chatContentColumn || agentContent != chatContentColumn {
		t.Fatalf("message content is not aligned at column %d: user=%q agent=%q", chatContentColumn, user, agent)
	}
}

func TestMultilineChatMessagesKeepContentIndentation(t *testing.T) {
	m := testModel()
	for _, entry := range []transcriptEntry{
		{role: "user", text: "first line\nsecond line"},
		{role: "user", text: strings.Repeat("wrapped", 5)},
		{role: "assistant", text: "first line\nsecond line"},
	} {
		lines := strings.Split(ansi.Strip(m.renderTranscriptEntry(entry)), "\n")
		if len(lines) < 2 || !strings.HasPrefix(lines[1], strings.Repeat(" ", chatContentColumn)) {
			t.Fatalf("%s continuation is not indented to column %d: %q", entry.role, chatContentColumn, lines)
		}
	}
}

func TestRenderedMarkdownUsesOneBlankLineBetweenParagraphs(t *testing.T) {
	m := testModel()
	result := ansi.Strip(m.renderTranscriptEntry(transcriptEntry{role: "assistant", text: "first paragraph\n\nsecond paragraph"}))
	blankRun := 0
	maxBlankRun := 0
	for _, line := range strings.Split(result, "\n") {
		if strings.TrimSpace(line) == "" {
			blankRun++
			maxBlankRun = max(maxBlankRun, blankRun)
		} else {
			blankRun = 0
		}
	}
	if maxBlankRun != 1 {
		t.Fatalf("rendered paragraph spacing has %d consecutive blank rows, want 1: %q", maxBlankRun, result)
	}
}

func TestUserLabelUsesBlueAccent(t *testing.T) {
	if got := userStyle.GetForeground(); got != userAccent {
		t.Fatalf("user label foreground = %v, want %v", got, userAccent)
	}
	if !userStyle.GetBold() {
		t.Fatal("user label is not bold")
	}
}

func TestWrappedComposerRevealsRowsBeforeScrolling(t *testing.T) {
	m := testModel()
	m.inputWidth = 4
	m.input.SetWidth(4)

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1234")})
	got := next.(model)
	if got.composerRows != 2 || got.input.LineInfo().RowOffset != 1 {
		t.Fatalf("after first wrap: rows=%d row offset=%d", got.composerRows, got.input.LineInfo().RowOffset)
	}
	if first := strings.Split(got.input.View(), "\n")[0]; !strings.Contains(first, "1234") {
		t.Fatalf("first wrapped row disappeared: %q", got.input.View())
	}

	next, _ = got.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("567890123456789012345678901234567890")})
	got = next.(model)
	offset, total := got.inputScrollMetrics()
	if got.composerRows != maxComposerHeight || total != 11 || offset != 1 {
		t.Fatalf("at cap: rows=%d total=%d offset=%d", got.composerRows, total, offset)
	}

	next, _ = got.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1234")})
	got = next.(model)
	nextOffset, nextTotal := got.inputScrollMetrics()
	if nextTotal != 12 || nextOffset != offset+1 {
		t.Fatalf("after one more wrapped row: total=%d offset=%d, previous offset=%d", nextTotal, nextOffset, offset)
	}
}

func TestDeletingTrailingNewlinesKeepsCursorOnBottomComposerRow(t *testing.T) {
	m := testModel()
	lines := make([]string, 21)
	for index := range lines {
		lines[index] = fmt.Sprintf("line%02d", index)
	}
	m.input.SetValue(strings.Join(lines, "\n"))
	m.resizeComposer()

	for deletion := 1; deletion <= 8; deletion++ {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
		m = next.(model)
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
		m = next.(model)
		visible := strings.Split(ansi.Strip(m.input.View()), "\n")
		want := fmt.Sprintf("line%02d", len(lines)-1-deletion)
		if got := strings.TrimSpace(visible[maxComposerHeight-1]); got != want {
			t.Fatalf("after deletion %d bottom row = %q, want %q; view=%q", deletion, got, want, m.input.View())
		}
	}
}

func TestTypingDoesNotMoveChatHistoryOffset(t *testing.T) {
	m := testModel()
	m.height = 24
	m.inputWidth = 4
	m.input.SetWidth(4)
	m.viewport.SetContent(strings.Repeat("history line\n", 100))
	m.viewport.SetYOffset(27)
	wantOffset := m.viewport.YOffset

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1234")})
	got := next.(model)
	if got.composerRows != 2 {
		t.Fatalf("composer rows = %d, want 2", got.composerRows)
	}
	if got.viewport.YOffset != wantOffset {
		t.Fatalf("history offset moved from %d to %d while typing", wantOffset, got.viewport.YOffset)
	}
}

func TestSameSizeRedrawPreservesChatHistoryOffset(t *testing.T) {
	m := testModel()
	m.viewport.SetContent(strings.Repeat("history line\n", 100))
	m.viewport.SetYOffset(27)
	wantOffset := m.viewport.YOffset

	next, _ := m.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
	got := next.(model)
	if got.viewport.YOffset != wantOffset {
		t.Fatalf("same-size redraw moved history offset from %d to %d", wantOffset, got.viewport.YOffset)
	}
}

func TestRedrawTickRequestsSameSizeRendererRepaint(t *testing.T) {
	m := testModel()
	_, cmd := m.Update(redrawTickMsg{})
	if cmd == nil {
		t.Fatal("redraw tick returned no commands")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("redraw tick command = %#v, want repaint and next tick", batch)
	}
	message, ok := batch[0]().(tea.WindowSizeMsg)
	if !ok || message.Width != m.width || message.Height != m.height {
		t.Fatalf("redraw message = %#v, want current size %dx%d", message, m.width, m.height)
	}
}

func TestCtrlLRequestsImmediateRendererRepaint(t *testing.T) {
	m := testModel()
	m.input.SetValue("keep this draft")
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	got := next.(model)
	if got.input.Value() != "keep this draft" {
		t.Fatalf("Ctrl+L changed draft to %q", got.input.Value())
	}
	if cmd == nil {
		t.Fatal("Ctrl+L returned no repaint command")
	}
	message, ok := cmd().(tea.WindowSizeMsg)
	if !ok || message.Width != m.width || message.Height != m.height {
		t.Fatalf("Ctrl+L message = %#v, want current size %dx%d", message, m.width, m.height)
	}
}

func TestEnterSendsTextWithTrailingSpace(t *testing.T) {
	m := testModel()
	m.input.SetValue("hello ")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := next.(model)
	if got.input.Value() != "" {
		t.Fatalf("input value = %q, want empty", got.input.Value())
	}
	if got.composerRows != 1 {
		t.Fatalf("composer rows = %d, want 1", got.composerRows)
	}
	if len(got.transcript) != 1 || !strings.Contains(got.transcript[0].text, "hello") {
		t.Fatalf("transcript = %#v, want sent message", got.transcript)
	}
}

func TestSpaceThenEnterDoesNotInsertNewline(t *testing.T) {
	m := testModel()
	m.input.SetValue(" ")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := next.(model)
	if got.input.Value() != "" || got.composerRows != 1 {
		t.Fatalf("input value = %q, composer rows = %d", got.input.Value(), got.composerRows)
	}
	if !strings.Contains(got.input.View(), got.input.Placeholder) {
		t.Fatalf("placeholder is missing after Space+Enter: %q", got.input.View())
	}
}

func TestExplicitNewlineKeepsPreviousRowVisible(t *testing.T) {
	m := testModel()

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello")})
	typed := next.(model)
	_ = typed.View()
	next, _ = typed.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
	got := next.(model)
	if got.input.Value() != "hello\n" || got.composerRows != 2 {
		t.Fatalf("input value = %q, composer rows = %d", got.input.Value(), got.composerRows)
	}
	if got.input.Line() != 1 {
		t.Fatalf("cursor line = %d, want 1", got.input.Line())
	}
	if view := got.input.View(); !strings.Contains(view, "hello") {
		t.Fatalf("previous row disappeared after newline: %q", view)
	}
}

func TestAltEnterSendsInsteadOfInsertingNewline(t *testing.T) {
	m := testModel()
	m.input.SetValue("hello")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	got := next.(model)
	if got.input.Value() != "" || len(got.transcript) != 1 || got.transcript[0].text != "hello" {
		t.Fatalf("Alt+Enter did not use the send path: input=%q transcript=%#v", got.input.Value(), got.transcript)
	}
}

func TestStandaloneLFInsertsNewlineForWarpShiftEnter(t *testing.T) {
	m := testModel()
	m.input.SetValue("hello")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
	got := next.(model)
	if got.input.Value() != "hello\n" || got.input.Line() != 1 || got.composerRows != 2 {
		t.Fatalf("input value = %q, line = %d, composer rows = %d", got.input.Value(), got.input.Line(), got.composerRows)
	}
}

func TestInputArrowsDoNotScrollHistory(t *testing.T) {
	m := testModel()
	m.input.SetValue("first\nsecond")
	m.input.SetHeight(2)
	m.resizeComposer()
	m.viewport.SetContent(strings.Repeat("history line\n", 100))
	m.viewport.GotoBottom()
	historyOffset := m.viewport.YOffset

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	got := next.(model)
	if got.input.Line() != 0 {
		t.Fatalf("input line = %d, want 0", got.input.Line())
	}
	if got.viewport.YOffset != historyOffset {
		t.Fatalf("history offset = %d, want unchanged %d", got.viewport.YOffset, historyOffset)
	}
}

func TestPageScrollsHistoryAndStaleMouseEventsAreIgnored(t *testing.T) {
	m := testModel()
	m.input.SetValue("first\nsecond")
	m.input.SetHeight(2)
	m.resizeComposer()
	m.viewport.SetContent(strings.Repeat("history line\n", 100))
	m.viewport.GotoBottom()
	inputLine := m.input.Line()

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	got := next.(model)
	if got.viewport.AtBottom() || got.input.Line() != inputLine {
		t.Fatalf("after PageUp: bottom = %t, input line = %d", got.viewport.AtBottom(), got.input.Line())
	}
	pageOffset := got.viewport.YOffset

	next, _ = got.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	got = next.(model)
	if got.viewport.YOffset != pageOffset || got.input.Line() != inputLine {
		t.Fatalf("after stale mouse event: offset = %d (was %d), input line = %d", got.viewport.YOffset, pageOffset, got.input.Line())
	}
}

func TestFragmentedMouseReportsNeverEnterComposer(t *testing.T) {
	m := testModel()
	m.input.SetValue("draft")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'['}, Alt: true})
	got := next.(model)
	if got.input.Value() != "draft" || got.pendingMouse != "[" {
		t.Fatalf("mouse escape prefix was not held: input=%q pending=%q", got.input.Value(), got.pendingMouse)
	}

	next, _ = got.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("<64;131;24M")})
	got = next.(model)
	if got.input.Value() != "draft" || got.pendingMouse != "" {
		t.Fatalf("fragmented wheel report leaked into composer: input=%q pending=%q", got.input.Value(), got.pendingMouse)
	}

	next, _ = got.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("[>65;131;24M")})
	got = next.(model)
	if got.input.Value() != "draft" {
		t.Fatalf("complete wheel report leaked into composer: %q", got.input.Value())
	}

	next, _ = got.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("[<32;41;9M[<0;41;9m")})
	got = next.(model)
	if got.input.Value() != "draft" {
		t.Fatalf("drag reports leaked into composer: %q", got.input.Value())
	}

	next, _ = got.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("[<32;41;")})
	got = next.(model)
	if got.input.Value() != "draft" || got.pendingMouse != "[<32;41;" {
		t.Fatalf("unmodified drag prefix was not held: input=%q pending=%q", got.input.Value(), got.pendingMouse)
	}
	next, _ = got.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("9M")})
	got = next.(model)
	if got.input.Value() != "draft" || got.pendingMouse != "" {
		t.Fatalf("split drag report leaked into composer: input=%q pending=%q", got.input.Value(), got.pendingMouse)
	}
}

func TestMouseEscapeGuardPreservesOrdinaryAndPastedText(t *testing.T) {
	m := testModel()
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'['}, Alt: true})
	next, _ = next.(model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello")})
	got := next.(model)
	if got.input.Value() != "[hello" {
		t.Fatalf("ordinary Alt-bracket text was discarded: %q", got.input.Value())
	}

	key := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("[<64;1;1M"), Paste: true}
	filtered, consumed := got.filterMouseEscape(key)
	if consumed || string(filtered.Runes) != string(key.Runes) {
		t.Fatalf("pasted text was treated as a mouse report: filtered=%q consumed=%t", filtered.Runes, consumed)
	}
}

func TestSlashCommandMenuNavigatesAndInsertsWithTab(t *testing.T) {
	m := testModel()

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	got := next.(model)
	if !got.commandMenu || len(got.commandMatches()) != len(got.commands) {
		t.Fatalf("command menu open = %t, matches = %d, want %d", got.commandMenu, len(got.commandMatches()), len(got.commands))
	}
	if !strings.Contains(got.commandMenuView(), "Commands") || strings.Contains(got.commandMenuView(), "tab=insert") {
		t.Fatalf("command menu title is not concise: %q", got.commandMenuView())
	}
	if got.footerHint() != "↑/↓ choose · Tab insert · Enter send · Esc close" {
		t.Fatalf("command menu controls are missing from footer: %q", got.footerHint())
	}

	next, _ = got.Update(tea.KeyMsg{Type: tea.KeyUp})
	got = next.(model)
	if got.commandIndex != len(got.commands)-1 {
		t.Fatalf("command index = %d, want wrapped index %d", got.commandIndex, len(got.commands)-1)
	}
	next, _ = got.Update(tea.KeyMsg{Type: tea.KeyDown})
	got = next.(model)
	if got.commandIndex != 0 {
		t.Fatalf("command index = %d, want wrapped index 0", got.commandIndex)
	}
	next, _ = got.Update(tea.KeyMsg{Type: tea.KeyDown})
	got = next.(model)
	if got.commandIndex != 1 {
		t.Fatalf("command index = %d, want 1", got.commandIndex)
	}

	next, _ = got.Update(tea.KeyMsg{Type: tea.KeyTab})
	got = next.(model)
	if got.input.Value() != "/status" {
		t.Fatalf("input value = %q, want /status", got.input.Value())
	}
	if got.commandMenu {
		t.Fatal("command menu remained open after selection")
	}
	if len(got.transcript) != 0 {
		t.Fatalf("transcript entries = %d, want 0", len(got.transcript))
	}
}

func TestSlashCommandMenuEnterSendsImmediately(t *testing.T) {
	m := testModel()
	m.input.SetValue("/")
	m.syncCommandMenu()

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := next.(model)
	if got.input.Value() != "" {
		t.Fatalf("input value = %q, want empty after sending", got.input.Value())
	}
	if len(got.transcript) != 1 || got.transcript[0].text != "/help" {
		t.Fatalf("transcript = %#v, want sent /help", got.transcript)
	}
	if got.commandMenu {
		t.Fatal("command menu remained open after sending")
	}
}

func TestCtrlCClearsComposerThenExitsWhenEmpty(t *testing.T) {
	m := testModel()
	m.input.SetValue("/sta")
	m.syncCommandMenu()
	m.composerRows = 3
	m.tokens = []composerToken{{label: "[Pasted 4 chars]", expansion: "body"}}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	got := next.(model)
	if cmd != nil {
		t.Fatal("first Ctrl+C returned a command instead of only clearing input")
	}
	if got.input.Value() != "" || got.commandMenu || got.commandIndex != 0 || got.composerRows != 1 || len(got.tokens) != 0 {
		t.Fatalf("first Ctrl+C did not reset transient input state: value=%q menu=%t index=%d rows=%d tokens=%#v", got.input.Value(), got.commandMenu, got.commandIndex, got.composerRows, got.tokens)
	}

	_, cmd = got.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("second Ctrl+C did not request exit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("second Ctrl+C command returned %T, want tea.QuitMsg", cmd())
	}
}

func TestCtrlCExitsImmediatelyWhenComposerIsAlreadyEmpty(t *testing.T) {
	m := testModel()
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("Ctrl+C on an empty composer did not request exit")
	}
	if message := cmd(); message != (tea.QuitMsg{}) {
		t.Fatalf("Ctrl+C command returned %#v, want tea.QuitMsg", message)
	}
}

func TestCommandMenuBorderHasConciseTitle(t *testing.T) {
	m := testModel()
	m.width = 80
	m.input.SetValue("/")
	m.syncCommandMenu()

	view := m.commandMenuView()
	if !strings.Contains(view, "╭─ Commands ") || strings.Contains(view, "tab=insert") || strings.Contains(view, "enter=send") {
		t.Fatalf("command menu title = %q", view)
	}
	rows := strings.Split(ansi.Strip(view), "\n")
	if len(rows) < 2 || !strings.HasPrefix(rows[1], "│/help") || strings.Contains(rows[1], "›") {
		t.Fatalf("selected command row is not flush-left and marker-free: %q", rows)
	}
}

func TestHeaderShowsRuntimeStatusAndFooterOnlyShowsControls(t *testing.T) {
	m := testModel()
	m.width = 80
	m.title = "Payments"
	m.transcript = make([]transcriptEntry, 26)
	m.connection = connectionMap([]channel.ConnectionStatus{
		{Name: "telegram", State: channel.ConnectionConnected},
		{Name: "whatsapp", State: channel.ConnectionError, Detail: "offline"},
	})
	m.runtimeStatus = core.RuntimeStatus{Logs: 922, Jobs: 3}

	view := ansi.Strip(m.View())
	lines := strings.Split(view, "\n")
	header := lines[0]
	footerLine := lines[len(lines)-1]
	footer := strings.TrimSpace(footerLine)
	if !strings.HasPrefix(header, " ") || strings.HasPrefix(header, "  ") || !strings.HasPrefix(footerLine, " ") || strings.HasPrefix(footerLine, "  ") {
		t.Fatalf("status bars are not inset by exactly one column: header=%q footer=%q", header, footerLine)
	}
	if !strings.Contains(header, "○○ Payments") || !strings.Contains(header, "● TG") || !strings.Contains(header, "▲ WA") || !strings.Contains(header, "3 jobs") || !strings.Contains(header, "922 logs") || !strings.Contains(header, "Ready") {
		t.Fatalf("status header is incomplete: %q", header)
	}
	if strings.Contains(footer, "Ready") || strings.Contains(footer, "Log") || strings.Contains(footer, "Jobs") || strings.Contains(footer, "transcript") {
		t.Fatalf("footer contains status data instead of only controls: %q", footer)
	}
	if footer != m.footerHint() {
		t.Fatalf("footer = %q, want %q", footer, m.footerHint())
	}
	if strings.Contains(footer, "Ctrl+L") {
		t.Fatalf("footer advertises the redraw fallback: %q", footer)
	}

	m.connection["telegram"] = channel.ConnectionStatus{Name: "telegram", State: channel.ConnectionUnconfigured}
	if view := m.View(); !strings.Contains(view, "○ TG") {
		t.Fatalf("unconfigured Telegram icon is missing: %q", view)
	}
}

func TestHeaderKeepsLogoAndTitleAccent(t *testing.T) {
	if got := titleStyle.GetForeground(); got != accent {
		t.Fatalf("logo/title foreground = %v, want pink accent %v", got, accent)
	}
	if got := statusStyle.GetForeground(); got != muted {
		t.Fatalf("remaining header foreground = %v, want muted blue %v", got, muted)
	}
}

func TestRuntimeEventUpdatesHeaderCounts(t *testing.T) {
	m := testModel()
	m.width = 80
	next, _ := m.Update(runtimeEvent{status: core.RuntimeStatus{Logs: 12, Jobs: 4}})
	got := next.(model)
	if view := got.View(); !strings.Contains(view, "12 logs") || !strings.Contains(view, "4 jobs") || strings.Contains(view, "Log (") || strings.Contains(view, "/jobs") {
		t.Fatalf("runtime status counts are missing or misplaced: %q", view)
	}
}

func TestCommandFooterReplacesOrdinaryComposerHints(t *testing.T) {
	m := testModel()
	ordinary := m.footerHint()
	m.input.SetValue("/")
	m.syncCommandMenu()
	command := m.footerHint()
	if ordinary == command || strings.Contains(command, "Shift+Enter") || strings.Contains(command, "Ready") {
		t.Fatalf("command footer did not replace ordinary hints: ordinary=%q command=%q", ordinary, command)
	}
}

func TestJobsStatusIsPlainTextWithoutSpinner(t *testing.T) {
	m := testModel()
	if got := runtimeCount(m.runtimeStatus.Jobs, "jobs"); got != "0 jobs" {
		t.Fatalf("jobs status = %q", got)
	}
}

func TestTitleEventRenamesWindow(t *testing.T) {
	m := testModel()
	m.title = "Spynel"

	next, _ := m.Update(titleEvent{title: "Production API"})
	got := next.(model)
	if got.title != "Production API" || got.status != "Title changed to Production API" {
		t.Fatalf("title event result: title=%q status=%q", got.title, got.status)
	}
}

func TestLoadTitleUsesPersistedValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tui-title")
	if err := os.WriteFile(path, []byte("Production API\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadTitle("Spynel", path)
	if err != nil || loaded != "Production API" {
		t.Fatalf("loadTitle() = %q, %v", loaded, err)
	}
}

func TestCommandMenuLayoutFitsTerminal(t *testing.T) {
	m := testModel()
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 28})
	next, _ = next.(model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	got := next.(model)
	view := got.View()
	lines := strings.Split(view, "\n")
	if len(lines) > 28 {
		t.Fatalf("view height = %d, want at most 28", len(lines))
	}
	for index, line := range lines {
		if width := lipgloss.Width(line); width > 80 {
			t.Fatalf("line %d width = %d, want at most 80: %q", index, width, line)
		}
	}
}

func TestFooterOccupiesFinalTerminalRow(t *testing.T) {
	for _, commandMenu := range []bool{false, true} {
		name := "ordinary"
		if commandMenu {
			name = "command menu"
		}
		t.Run(name, func(t *testing.T) {
			m := testModel()
			next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 28})
			m = next.(model)
			if commandMenu {
				next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
				m = next.(model)
			}

			lines := strings.Split(ansi.Strip(m.View()), "\n")
			if len(lines) != m.height {
				t.Fatalf("view height = %d, want terminal height %d", len(lines), m.height)
			}
			if footer := strings.TrimSpace(lines[len(lines)-1]); footer != m.footerHint() {
				t.Fatalf("final terminal row = %q, want footer %q", footer, m.footerHint())
			}
		})
	}
}

func TestLongPasteIsCompactAndAtomic(t *testing.T) {
	m := testModel()
	body := strings.Repeat("paste body ", 150)

	handled, err := m.handlePaste(body)
	if err != nil || !handled {
		t.Fatalf("handlePaste() = %t, %v", handled, err)
	}
	label := m.input.Value()
	if label != "[Pasted 1650 chars]" {
		t.Fatalf("input value = %q", label)
	}
	if got := m.expandTokens(label); got != body {
		t.Fatalf("expanded paste length = %d, want %d", len(got), len(body))
	}
	if !m.handleTokenKey(tea.KeyMsg{Type: tea.KeyLeft}) || inputCursorColumn(m) != 0 {
		t.Fatalf("Left did not jump to the start of token; column = %d", inputCursorColumn(m))
	}
	if !m.handleTokenKey(tea.KeyMsg{Type: tea.KeyRight}) || inputCursorColumn(m) != len([]rune(label)) {
		t.Fatalf("Right did not jump to the end of token; column = %d", inputCursorColumn(m))
	}
	if !m.handleTokenKey(tea.KeyMsg{Type: tea.KeyBackspace}) || m.input.Value() != "" {
		t.Fatalf("Backspace left input %q", m.input.Value())
	}
}

func TestPastedFileIsCopiedIntoAttachments(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(source, []byte("attachment body"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := testModel()
	m.attachments = filepath.Join(root, ".spynel", "attachments")

	handled, err := m.handlePaste(source)
	if err != nil || !handled {
		t.Fatalf("handlePaste() = %t, %v", handled, err)
	}
	if m.input.Value() != "[Attachment notes.txt]" {
		t.Fatalf("input value = %q", m.input.Value())
	}
	copied := filepath.Join(m.attachments, "notes.txt")
	data, err := os.ReadFile(copied)
	if err != nil || string(data) != "attachment body" {
		t.Fatalf("copied attachment = %q, %v", data, err)
	}
	if expanded := m.expandTokens(m.input.Value()); !strings.Contains(expanded, filepath.ToSlash(copied)) {
		t.Fatalf("expanded token %q does not link to copied attachment", expanded)
	}
}

func TestScrollbarTracksTopAndBottom(t *testing.T) {
	top := scrollbar(5, 0, 20)
	bottom := scrollbar(5, 15, 20)
	if !strings.Contains(top[0], "┃") || !strings.Contains(bottom[4], "┃") {
		t.Fatalf("scrollbar thumb did not track endpoints: top=%q bottom=%q", top, bottom)
	}
}

func TestScrollbarTrackUsesPanelBorderColor(t *testing.T) {
	if got, want := standardBorderStyle.GetForeground(), panel.GetBorderRightForeground(); got != want {
		t.Fatalf("scrollbar track color = %v, panel border color = %v", got, want)
	}
	if got := scrollThumbStyle.GetForeground(); got != accent {
		t.Fatalf("scrollbar thumb color = %v, want accent %v", got, accent)
	}
}

func TestScrollbarReplacesRightBorderOnlyWhenScrollable(t *testing.T) {
	box := panel.Width(24).Render(fitContent("one\ntwo\nthree", 3, 20))
	plain := replaceRightBorder(box, 3, 0, 3)
	plainLines := strings.Split(ansi.Strip(plain), "\n")
	for row := 1; row < len(plainLines)-1; row++ {
		if !strings.HasSuffix(plainLines[row], "│") || strings.Contains(plainLines[row], "┃") {
			t.Fatalf("non-scrollable row does not end in a standard border: %q", plainLines[row])
		}
	}

	scrolled := replaceRightBorder(box, 3, 0, 20)
	scrolledLines := strings.Split(ansi.Strip(scrolled), "\n")
	if !strings.HasSuffix(scrolledLines[1], "┃") {
		t.Fatalf("scroll thumb is not on the right border: %q", scrolledLines)
	}
	for row := range plainLines {
		if lipgloss.Width(plainLines[row]) != lipgloss.Width(scrolledLines[row]) {
			t.Fatalf("scrollbar changed box width on row %d: plain=%q scrolled=%q", row, plainLines[row], scrolledLines[row])
		}
	}
}

func TestCRLFAfterSendKeepsComposerEmpty(t *testing.T) {
	m := testModel()
	m.input.SetValue("hello")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	next, _ = next.(model).Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
	got := next.(model)
	if got.input.Value() != "" {
		t.Fatalf("input value = %q, want empty after CRLF", got.input.Value())
	}
	if got.composerRows != 1 {
		t.Fatalf("composer rows = %d, want 1", got.composerRows)
	}
	if !strings.Contains(got.input.View(), got.input.Placeholder) {
		t.Fatalf("input view %q does not contain placeholder %q", got.input.View(), got.input.Placeholder)
	}
}

func TestCRLFOnEmptyComposerKeepsPlaceholderVisible(t *testing.T) {
	m := testModel()

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	next, _ = next.(model).Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
	got := next.(model)
	if got.input.Value() != "" || !strings.Contains(got.input.View(), got.input.Placeholder) {
		t.Fatalf("input value = %q, view = %q", got.input.Value(), got.input.View())
	}
}

func TestEnterSendsAndResetsComposer(t *testing.T) {
	m := testModel()
	m.input.SetValue("hello")
	m.composerRows = 4

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := next.(model)
	if got.input.Value() != "" {
		t.Fatalf("input value = %q, want empty", got.input.Value())
	}
	if got.composerRows != 1 {
		t.Fatalf("composer rows = %d, want 1", got.composerRows)
	}
	if len(got.transcript) != 1 {
		t.Fatalf("transcript entries = %d, want 1", len(got.transcript))
	}
}

func TestWorkingSpinnerTrailsStreamingResponseUntilFinal(t *testing.T) {
	m := testModel()
	m.transcript = make([]transcriptEntry, 50)
	for index := range m.transcript {
		m.transcript[index] = transcriptEntry{role: "assistant", text: "previous response"}
	}
	m.refresh()
	m.input.SetValue("do some work")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := next.(model)
	firstWorkingFrame := got.workingSpinner.View()
	firstLogoFrame := got.logoSpinner.View()
	view := ansi.Strip(got.viewport.View())
	if !got.working || !strings.Contains(view, "Spy "+firstWorkingFrame) || strings.Contains(view, "Working") {
		t.Fatalf("initial activity placeholder is not spinner-only: working=%t view=%q", got.working, view)
	}
	if logo := got.spynelLogo(); logo != firstLogoFrame {
		t.Fatalf("working logo = %q, want logo frame %q", logo, firstLogoFrame)
	}
	got.viewport.SetYOffset(7)
	wantOffset := got.viewport.YOffset

	next, _ = got.Update(got.workingSpinner.Tick())
	got = next.(model)
	if got.workingSpinner.View() == firstWorkingFrame {
		t.Fatalf("working spinner did not advance from %q", firstWorkingFrame)
	}
	if got.viewport.YOffset != wantOffset {
		t.Fatalf("spinner moved history offset from %d to %d", wantOffset, got.viewport.YOffset)
	}

	next, _ = got.Update(got.logoSpinner.Tick())
	got = next.(model)
	if got.logoSpinner.View() == firstLogoFrame || got.spynelLogo() != got.logoSpinner.View() {
		t.Fatalf("header logo did not advance independently: logo=%q first=%q", got.spynelLogo(), firstLogoFrame)
	}

	next, _ = got.Update(uiEvent{event: core.Event{Kind: core.EventDelta, Text: "Confirmation received."}})
	got = next.(model)
	streamingFrame := got.workingSpinner.View()
	view = ansi.Strip(got.viewport.View())
	if !strings.Contains(view, "received."+streamingFrame) || strings.Contains(view, "Working") {
		t.Fatalf("spinner does not trail the streamed response: %q", view)
	}
	got.viewport.SetYOffset(7)
	wantOffset = got.viewport.YOffset
	next, _ = got.Update(got.workingSpinner.Tick())
	got = next.(model)
	if got.workingSpinner.View() == streamingFrame || got.viewport.YOffset != wantOffset {
		t.Fatalf("trailing spinner did not animate in place: frame=%q offset=%d want-offset=%d", got.workingSpinner.View(), got.viewport.YOffset, wantOffset)
	}

	next, _ = got.Update(uiEvent{event: core.Event{Kind: core.EventDelta, Text: " Next response"}})
	got = next.(model)
	view = ansi.Strip(got.viewport.View())
	if !strings.Contains(view, "response"+got.workingSpinner.View()) || strings.Contains(view, "Working") {
		t.Fatalf("spinner did not follow the newest streamed character: %q", view)
	}

	next, _ = got.Update(uiEvent{event: core.Event{Kind: core.EventFinal, Text: "finished", Done: true}})
	got = next.(model)
	view = ansi.Strip(got.viewport.View())
	if got.working || strings.Contains(view, "Working") || strings.Contains(view, got.workingSpinner.View()) || !strings.Contains(view, "finished") {
		t.Fatalf("activity spinner was not removed by final response: working=%t view=%q", got.working, view)
	}
	if logo := got.spynelLogo(); logo != "◉◉" {
		t.Fatalf("completed response logo = %q, want full logo", logo)
	}
}

func TestLiveTranscriptIsBoundedWithoutLosingNewestMessages(t *testing.T) {
	entries := make([]transcriptEntry, 0, maxTranscriptRows+100)
	for index := 0; index < maxTranscriptRows+100; index++ {
		entries = append(entries, transcriptEntry{role: "assistant", text: fmt.Sprintf("message-%03d", index)})
	}
	bounded := boundTranscript(entries)
	if len(bounded) != maxTranscriptRows || bounded[0].role != "status" || bounded[0].text != transcriptOmitted || bounded[len(bounded)-1].text != fmt.Sprintf("message-%03d", maxTranscriptRows+99) {
		t.Fatalf("bounded transcript = %d rows, first %#v, last %#v", len(bounded), bounded[0], bounded[len(bounded)-1])
	}
	bounded = boundTranscript(append(bounded, transcriptEntry{role: "user", text: "newest"}))
	if len(bounded) != maxTranscriptRows || bounded[0].text != transcriptOmitted || bounded[1].text == transcriptOmitted || bounded[len(bounded)-1].text != "newest" {
		t.Fatalf("rebounded transcript = %#v", bounded)
	}
	oversized := boundTranscript([]transcriptEntry{{role: "assistant", text: strings.Repeat("x", maxTranscriptRunes+100)}})
	if len(oversized) != 2 || oversized[0].text != transcriptOmitted || len([]rune(oversized[1].text)) > maxTranscriptRunes || !strings.HasSuffix(oversized[1].text, strings.Repeat("x", 100)) {
		t.Fatalf("oversized transcript entry = %d rows, %d runes", len(oversized), len([]rune(oversized[len(oversized)-1].text)))
	}
}

func TestFrameworkCommandDuringRecipientTurnKeepsResponsesAndSpinnerFlow(t *testing.T) {
	m := testModel()
	m.input.SetValue("start long work")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	next, _ = m.Update(uiEvent{event: core.Event{Kind: core.EventDelta, Text: "first response"}})
	m = next.(model)

	m.input.SetValue("/jobs")
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if !m.working || m.streaming != "" || len(m.transcript) != 3 {
		t.Fatalf("command did not commit active response: working=%t streaming=%q transcript=%#v", m.working, m.streaming, m.transcript)
	}
	if m.transcript[1] != (transcriptEntry{role: "assistant", text: "first response"}) || m.transcript[2] != (transcriptEntry{role: "user", text: "/jobs"}) {
		t.Fatalf("committed response and command are out of order: %#v", m.transcript)
	}

	next, _ = m.Update(uiEvent{event: core.Event{Kind: core.EventFinal, Text: "# Jobs\n\n1 job", Done: true, Local: true}})
	m = next.(model)
	if !m.working || len(m.transcript) != 4 || m.transcript[3].text != "# Jobs\n\n1 job" {
		t.Fatalf("framework final stopped or replaced active response: working=%t transcript=%#v", m.working, m.transcript)
	}
	view := ansi.Strip(m.viewport.View())
	if !strings.Contains(view, "/jobs") || !strings.Contains(view, "1 job") || !strings.Contains(view, "Spy "+m.workingSpinner.View()) {
		t.Fatalf("interleaved command flow is not visible: %q", view)
	}

	frame := m.workingSpinner.View()
	next, _ = m.Update(m.workingSpinner.Tick())
	m = next.(model)
	if m.workingSpinner.View() == frame {
		t.Fatalf("spinner stopped after framework response at frame %q", frame)
	}

	next, _ = m.Update(uiEvent{event: core.Event{Kind: core.EventDelta, Text: "\nsecond response"}})
	m = next.(model)
	view = ansi.Strip(m.viewport.View())
	if m.streaming != "\nsecond response" || !m.working || !strings.Contains(view, "second respo") || !strings.Contains(view, m.workingSpinner.View()) {
		t.Fatalf("subsequent harness output did not open after framework response: %q", view)
	}

	next, _ = m.Update(uiEvent{event: core.Event{Kind: core.EventFinal, Text: "first response\nsecond response", Done: true}})
	m = next.(model)
	if m.working || m.streaming != "" || len(m.transcript) != 5 {
		t.Fatalf("harness final did not finish the resumed entry: working=%t streaming=%q transcript=%#v", m.working, m.streaming, m.transcript)
	}
	if m.transcript[4].text != "\nsecond response" {
		t.Fatalf("harness final duplicated committed output: %#v", m.transcript)
	}
}

func TestConfigurationScreenReplacesChatAndSupportsFormNavigation(t *testing.T) {
	m := testModel()
	m.transcript = []transcriptEntry{{role: "user", text: "preserve chat"}}
	var saved map[string]string
	m.saveSettings = func(values map[string]string) error {
		saved = values
		return nil
	}
	screen := core.Screen{ID: "telegram", Title: "Telegram", Subtitle: "Configure the bot", Controls: []core.ScreenControl{
		{Key: "channels.telegram.enabled", Label: "enabled", Kind: "toggle", Value: "off", Options: []string{"on", "off"}, Description: "Run Telegram"},
		{Key: "channels.telegram.name", Label: "name", Kind: "text", Value: "spy", Description: "Friendly name"},
		{Key: "channels.telegram.token", Label: "token", Kind: "password", Configured: true, Description: "Secret token"},
	}}
	next, _ := m.Update(uiEvent{event: core.Event{Kind: core.EventScreen, Screen: &screen, Local: true}})
	m = next.(model)
	view := ansi.Strip(m.View())
	if m.screen == nil || !strings.Contains(view, "Telegram") || !strings.Contains(view, "Configure the bot") || !strings.Contains(m.screenFooterHint(), "Ctrl+S save") || strings.Contains(view, "preserve chat") || strings.Contains(view, m.input.Placeholder) {
		t.Fatalf("screen did not replace chat UI: %q", view)
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if m.screen.Controls[0].Value != "on" {
		t.Fatalf("toggle value = %q, want on", m.screen.Controls[0].Value)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("nel")})
	m = next.(model)
	if m.screenIndex != 1 || m.screen.Controls[1].Value != "spynel" {
		t.Fatalf("text control was not edited after Tab: index=%d controls=%#v", m.screenIndex, m.screen.Controls)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = next.(model)
	if m.screenIndex != 0 {
		t.Fatalf("Shift+Tab did not navigate backward: %d", m.screenIndex)
	}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = next.(model)
	if cmd == nil || !m.screenSaving {
		t.Fatal("Ctrl+S did not start form save")
	}
	next, _ = m.Update(cmd())
	m = next.(model)
	if m.screenSaving || saved["channels.telegram.enabled"] != "on" || saved["channels.telegram.name"] != "spynel" {
		t.Fatalf("unexpected saved form values: saving=%t values=%#v", m.screenSaving, saved)
	}
	if _, exists := saved["channels.telegram.token"]; exists {
		t.Fatalf("unchanged configured secret was overwritten: %#v", saved)
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(model)
	if m.screen != nil || len(m.transcript) != 1 || m.transcript[0].text != "preserve chat" {
		t.Fatalf("Esc did not restore preserved chat state: screen=%#v transcript=%#v", m.screen, m.transcript)
	}
}

func TestSelectionScreenFocusesCurrentChoiceAndAppliesWithNavigation(t *testing.T) {
	m := testModel()
	selected := ""
	m.screenAction = func(_ context.Context, screenID, action string, _ map[string]string) (*core.Screen, error) {
		if screenID != "model" {
			t.Fatalf("selection screen ID = %q", screenID)
		}
		selected = action
		return nil, nil
	}
	m.openScreen(core.Screen{
		ID: "model", Title: "Harness model", SaveDisabled: true, InitialControl: "select:second",
		Controls: []core.ScreenControl{
			{Key: "select:first", Kind: "action", Value: "first"},
			{Key: "select:second", Kind: "action", Value: "second"},
			{Key: "select:third", Kind: "action", Value: "third"},
		},
	})
	if m.screenIndex != 1 || !strings.Contains(m.screenFooterHint(), "Enter select") || strings.Contains(m.screenFooterHint(), "Ctrl+S") {
		t.Fatalf("initial selection screen state = index %d, footer %q", m.screenIndex, m.screenFooterHint())
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = next.(model)
	if m.screenIndex != 0 {
		t.Fatalf("Up selected index %d, want 0", m.screenIndex)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(model)
	if m.screenIndex != 2 {
		t.Fatalf("Down selected index %d, want 2", m.screenIndex)
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if cmd == nil || !m.screenSaving {
		t.Fatal("Enter did not apply the highlighted selection")
	}
	next, _ = m.Update(cmd())
	m = next.(model)
	if selected != "select:third" || m.screen != nil || m.status != "Selected third" {
		t.Fatalf("selection result = %q, screen %#v, status %q", selected, m.screen, m.status)
	}
}

func TestNestedHarnessSelectionReturnsToPreservedConfigOnEscapeAndEnter(t *testing.T) {
	m := testModel()
	m.openScreen(core.Screen{ID: "config", Title: "Spynel configuration", Controls: []core.ScreenControl{
		{Key: "harness", Kind: "action", Value: "Coding harness"},
		{Key: "notes", Kind: "text", Value: "unsaved edit"},
	}})
	m.screenIndex = 1
	m.screenAdvanced = true
	m.screenScroll = 3
	m.openScreen(core.Screen{ID: "harness", ParentID: "config", Title: "Coding harness", SaveDisabled: true, Controls: []core.ScreenControl{
		{Key: "select:codex", Kind: "action", Value: "Codex"},
	}})
	if !strings.Contains(m.screenFooterHint(), "Esc back") {
		t.Fatalf("nested selection footer = %q", m.screenFooterHint())
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(model)
	if m.screen == nil || m.screen.ID != "config" || m.screen.Controls[1].Value != "unsaved edit" || m.screenIndex != 1 || !m.screenAdvanced || m.screenScroll != 3 {
		t.Fatalf("Escape did not restore config state: screen=%#v index=%d advanced=%t scroll=%d", m.screen, m.screenIndex, m.screenAdvanced, m.screenScroll)
	}

	m.screenAction = func(_ context.Context, screenID, action string, _ map[string]string) (*core.Screen, error) {
		if screenID != "harness" || action != "select:codex" {
			t.Fatalf("selection action = %q %q", screenID, action)
		}
		return nil, nil
	}
	m.openScreen(core.Screen{ID: "harness", ParentID: "config", Title: "Coding harness", SaveDisabled: true, Controls: []core.ScreenControl{
		{Key: "select:codex", Kind: "action", Value: "Codex"},
	}})
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if cmd == nil {
		t.Fatal("Enter did not apply nested harness selection")
	}
	next, _ = m.Update(cmd())
	m = next.(model)
	if m.screen == nil || m.screen.ID != "config" || m.screen.Controls[1].Value != "unsaved edit" || m.status != "Selected codex" {
		t.Fatalf("Enter did not return to config: screen=%#v status=%q", m.screen, m.status)
	}
}

func TestChannelScreensStartWithLiveConnectionStatusAndDetail(t *testing.T) {
	tests := []struct {
		name      string
		screenID  string
		status    channel.ConnectionStatus
		indicator string
		identity  string
		detail    string
	}{
		{name: "Telegram connected", screenID: "telegram", status: channel.ConnectionStatus{Name: "telegram", State: channel.ConnectionConnected, Identity: "@spynel_test_bot", Link: "https://t.me/spynel_test_bot"}, indicator: "● Connected", identity: "Bot: @spynel_test_bot"},
		{name: "Telegram error", screenID: "telegram", status: channel.ConnectionStatus{Name: "telegram", State: channel.ConnectionError, Detail: "authentication failed\ncheck the bot token"}, indicator: "▲ Error", detail: "Detail: authentication failed check the bot token"},
		{name: "WhatsApp connected wizard", screenID: "wizard:whatsapp:pair", status: channel.ConnectionStatus{Name: "whatsapp", State: channel.ConnectionConnected, Detail: "paired device online"}, indicator: "● Connected", detail: "Detail: paired device online"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := testModel()
			m.connection = connectionMap([]channel.ConnectionStatus{test.status})
			m.openScreen(core.Screen{ID: test.screenID, Title: test.name, Banner: "PAIRING CONTENT", Subtitle: "Channel settings", Controls: []core.ScreenControl{{Key: "done", Kind: "action", Value: "Done"}}})
			content, _, _ := m.screenContent(30, 100)
			lines := strings.Split(ansi.Strip(content), "\n")
			statusLine := lineContaining(lines, "Status")
			indicatorLine := lineContaining(lines, test.indicator)
			identityLine := indicatorLine
			if test.identity != "" {
				identityLine = lineContaining(lines, test.identity)
				if identityLine <= indicatorLine || !strings.Contains(content, "\x1b]8;;https://t.me/spynel_test_bot\x1b\\") {
					t.Fatalf("Telegram bot identity is not a clickable status link: %q", content)
				}
			}
			detailLine := identityLine
			if test.detail != "" {
				detailLine = lineContaining(lines, test.detail)
			}
			bannerLine := lineContaining(lines, "PAIRING CONTENT")
			settingsLine := lineContaining(lines, "Channel settings")
			if statusLine != 0 || indicatorLine <= statusLine || detailLine <= indicatorLine || bannerLine <= detailLine || settingsLine <= bannerLine {
				t.Fatalf("channel status section order = %q", content)
			}
		})
	}
}

func TestConfigurationTextFieldsSupportSpacesAndCursorEditing(t *testing.T) {
	m := testModel()
	m.openScreen(core.Screen{ID: "telegram", Title: "Telegram", Controls: []core.ScreenControl{{
		Key: "name", Label: "name", Kind: "text", Value: "helloworld",
	}}})
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyHome},
		{Type: tea.KeyRight},
		{Type: tea.KeyRight},
		{Type: tea.KeyRight},
		{Type: tea.KeyRight},
		{Type: tea.KeyRight},
		{Type: tea.KeySpace},
		{Type: tea.KeyRunes, Runes: []rune("wide ")},
	} {
		next, _ := m.Update(key)
		m = next.(model)
	}
	if got := m.screen.Controls[0].Value; got != "hello wide world" {
		t.Fatalf("cursor insertion = %q, want %q", got, "hello wide world")
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDelete})
	m = next.(model)
	if got := m.screen.Controls[0].Value; got != "hello widworld" {
		t.Fatalf("cursor deletion = %q, want %q", got, "hello widworld")
	}

	m.openScreen(core.Screen{ID: "telegram", Title: "Telegram", Controls: []core.ScreenControl{{
		Key: "token", Label: "token", Kind: "password", Value: "secret", Secret: true,
	}}})
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyHome})
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = next.(model)
	content, _, _ := m.screenContent(20, 80)
	if !strings.Contains(ansi.Strip(content), "••█••••") {
		t.Fatalf("password cursor was not rendered in place: %q", ansi.Strip(content))
	}
}

func TestConfigurationScreenHidesAdvancedControlsUntilDisclosed(t *testing.T) {
	m := testModel()
	var saved map[string]string
	m.saveSettings = func(values map[string]string) error {
		saved = values
		return nil
	}
	m.openScreen(core.Screen{ID: "whatsapp", Title: "WhatsApp", Controls: []core.ScreenControl{
		{Key: "channels.whatsapp.mode", Label: "mode", Kind: "select", Value: "self-chat", Options: []string{"self-chat", "dedicated"}, Description: "Account behavior"},
		{Key: "advanced", Kind: "disclosure", Value: "Advanced settings", Description: "Show optional controls"},
		{Key: "channels.whatsapp.database", Label: "database", Kind: "text", Value: "session.db", Description: "Session storage", Advanced: true},
	}})
	view := ansi.Strip(m.View())
	if strings.Contains(view, "Session storage") || !strings.Contains(view, "Show advanced") {
		t.Fatalf("collapsed advanced form = %q", view)
	}
	content, _, _ := m.screenContent(30, 100)
	lines := strings.Split(ansi.Strip(content), "\n")
	disclosureLine := lineContaining(lines, "[ Show advanced settings ]")
	if disclosureLine < 2 || strings.TrimSpace(lines[disclosureLine-1]) != "" || strings.TrimSpace(lines[disclosureLine-2]) == "" {
		t.Fatalf("collapsed essential/disclosure spacing = %q", content)
	}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = next.(model)
	if m.screenIndex != 1 {
		t.Fatalf("disclosure index = %d, want 1", m.screenIndex)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	view = ansi.Strip(m.View())
	if !m.screenAdvanced || !strings.Contains(view, "Session storage") || !strings.Contains(view, "Hide advanced") {
		t.Fatalf("expanded advanced form = %q", view)
	}
	content, _, _ = m.screenContent(30, 100)
	lines = strings.Split(ansi.Strip(content), "\n")
	disclosureLine = lineContaining(lines, "[ Hide advanced settings ]")
	if disclosureLine < 2 || disclosureLine+3 >= len(lines) || strings.TrimSpace(lines[disclosureLine-1]) != "" || strings.TrimSpace(lines[disclosureLine+2]) != "" || !strings.Contains(lines[disclosureLine+3], "Database") {
		t.Fatalf("expanded disclosure/advanced spacing = %q", content)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = next.(model)
	if m.screenIndex != 2 {
		t.Fatalf("advanced control index = %d, want 2", m.screenIndex)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(".new")})
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if m.screenAdvanced {
		t.Fatal("advanced controls did not collapse")
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = next.(model)
	if cmd == nil {
		t.Fatal("collapsed form did not save its edited advanced value")
	}
	next, _ = m.Update(cmd())
	m = next.(model)
	if got := saved["channels.whatsapp.database"]; got != "session.db.new" {
		t.Fatalf("saved advanced database = %q", got)
	}
}

func lineContaining(lines []string, substring string) int {
	for index, line := range lines {
		if strings.Contains(line, substring) {
			return index
		}
	}
	return -1
}

func TestWizardScreenHidesStatePassesValuesAndRendersClickableLinks(t *testing.T) {
	m := testModel()
	var gotValues map[string]string
	m.screenAction = func(_ context.Context, screenID, action string, values map[string]string) (*core.Screen, error) {
		if screenID != "wizard:telegram:token" || action != "next" {
			t.Fatalf("wizard action = %q %q", screenID, action)
		}
		gotValues = values
		return &core.Screen{ID: "telegram", Title: "Telegram"}, nil
	}
	m.openScreen(core.Screen{
		ID: "wizard:telegram:token", Title: "Telegram setup", Markdown: true, SaveDisabled: true,
		Subtitle: "Open [BotFather](https://t.me/BotFather).",
		Controls: []core.ScreenControl{
			{Key: "channels.telegram.token", Label: "bot token", Kind: "password", Value: "secret"},
			{Key: "next", Kind: "action", Value: "Continue"},
			{Key: "state", Kind: "hidden", Value: "carried-value"},
		},
	})
	view := m.View()
	if !strings.Contains(view, "\x1b]8;;https://t.me/BotFather") || strings.Contains(ansi.Strip(view), "carried-value") {
		t.Fatalf("wizard rendering = %q", view)
	}
	if strings.Contains(m.screenFooterHint(), "Ctrl+S") {
		t.Fatalf("wizard footer offers disabled save: %q", m.screenFooterHint())
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = next.(model)
	if m.screenIndex != 1 || cmd != nil {
		t.Fatalf("wizard navigation = index %d command %#v", m.screenIndex, cmd)
	}
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if cmd == nil {
		t.Fatal("wizard action did not run")
	}
	next, _ = m.Update(cmd())
	m = next.(model)
	if gotValues["channels.telegram.token"] != "secret" || gotValues["state"] != "carried-value" {
		t.Fatalf("wizard values = %#v", gotValues)
	}
}

func TestChannelConfigurationSeparatesWizardButtonFromEssentialSettings(t *testing.T) {
	for _, screenID := range []string{"telegram", "whatsapp"} {
		t.Run(screenID, func(t *testing.T) {
			m := testModel()
			m.openScreen(core.Screen{ID: screenID, Title: strings.Title(screenID), Controls: []core.ScreenControl{ //nolint:staticcheck
				{Key: "wizard", Kind: "action", Value: "Setup wizard", Description: "Configure the connection step by step"},
				{Key: "essential", Label: "enabled", Kind: "toggle", Value: "on", Options: []string{"on", "off"}, Description: "Start the channel"},
			}})
			content, _, _ := m.screenContent(30, 100)
			lines := strings.Split(ansi.Strip(content), "\n")
			wizardLine := lineContaining(lines, "[ Setup wizard ]")
			essentialLine := lineContaining(lines, "Enabled")
			if wizardLine < 0 || essentialLine != wizardLine+3 || strings.TrimSpace(lines[wizardLine+1]) == "" || strings.TrimSpace(lines[wizardLine+2]) != "" {
				t.Fatalf("%s wizard/essential spacing = %q", screenID, content)
			}
		})
	}
}

func TestConfigurationDescriptionRendersClickableMarkdownLink(t *testing.T) {
	m := testModel()
	m.openScreen(core.Screen{ID: "telegram", Title: "Telegram", Controls: []core.ScreenControl{{
		Key: "allowed", Label: "allowed users", Kind: "text", Description: "Find your ID with [@userinfobot](https://t.me/userinfobot)", DescriptionMarkdown: true,
	}}})
	content, _, _ := m.screenContent(30, 100)
	if !strings.Contains(ansi.Strip(content), "Find your ID with @userinfobot") || !strings.Contains(content, "\x1b]8;;https://t.me/userinfobot\x1b\\") {
		t.Fatalf("Telegram whitelist description lacks a clickable link: %q", content)
	}
}

func TestWizardScreenSeparatesInputFromNavigationActions(t *testing.T) {
	m := testModel()
	m.openScreen(core.Screen{
		ID: "wizard:telegram:token", Title: "Telegram setup", SaveDisabled: true,
		Controls: []core.ScreenControl{
			{Key: "token", Label: "bot token", Kind: "password", Value: "secret", Description: "Edit this value"},
			{Key: "next", Kind: "action", Value: "Continue", Description: "Continue setup"},
			{Key: "back", Kind: "action", Value: "Back", Description: "Return to the previous step"},
		},
	})
	content, _, _ := m.screenContent(30, 100)
	lines := strings.Split(ansi.Strip(content), "\n")
	for index, line := range lines {
		if !strings.Contains(line, "[ Continue ]") {
			continue
		}
		if index < 2 || strings.TrimSpace(lines[index-1]) != "" || strings.TrimSpace(lines[index-2]) == "" {
			t.Fatalf("wizard input/action spacing = %q", content)
		}
		if index+2 >= len(lines) || strings.TrimSpace(lines[index+2]) == "" {
			t.Fatalf("wizard actions unexpectedly separated from each other = %q", content)
		}
		return
	}
	t.Fatalf("wizard Continue action missing from %q", content)
}

func TestWizardPairingScreenStartsAtQRCodeAndSupportsPaging(t *testing.T) {
	m := testModel()
	m.openScreen(core.Screen{
		ID: "wizard:whatsapp:pair", Title: "Pair", StartAtTop: true, SaveDisabled: true,
		Banner: strings.Repeat("QR-LINE\n", 30), Subtitle: "Scan the QR",
		Controls: []core.ScreenControl{{Key: "done", Kind: "action", Value: "Done"}},
	})
	if _, offset, _ := m.screenContent(12, 40); offset != 0 {
		t.Fatalf("pairing screen initial offset = %d, want top", offset)
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	m = next.(model)
	if _, offset, _ := m.screenContent(12, 40); offset == 0 {
		t.Fatal("PageDown did not scroll the pairing screen")
	}
}

func TestWhatsAppPairingEventUpdatesOpenConfigurationScreen(t *testing.T) {
	m := testModel()
	m.openScreen(core.Screen{ID: "whatsapp", Title: "WhatsApp", Subtitle: "Configure WhatsApp", Controls: []core.ScreenControl{{Key: "enabled", Kind: "toggle", Value: "on", Options: []string{"on", "off"}}}})
	next, _ := m.Update(pairingEvent{event: channel.PairingEvent{Name: "whatsapp", State: "code", Rendered: "QR-CODE", Detail: "Scan in Linked devices"}})
	got := next.(model)
	if got.screen == nil || got.screen.Banner != "QR-CODE" || got.screen.Status != "Scan in Linked devices" {
		t.Fatalf("pairing screen = %#v", got.screen)
	}
	view := ansi.Strip(got.View())
	if !strings.Contains(view, "QR-CODE") {
		t.Fatalf("pairing event was not rendered: %q", view)
	}
}

func TestWhatsAppPairingEventUpdatesWizardScreen(t *testing.T) {
	m := testModel()
	m.openScreen(core.Screen{ID: "wizard:whatsapp:pair", Title: "WhatsApp setup", Markdown: true, SaveDisabled: true, Subtitle: "Scan the code", Controls: []core.ScreenControl{{Key: "done", Kind: "action", Value: "Done"}}})
	next, _ := m.Update(pairingEvent{event: channel.PairingEvent{Name: "whatsapp", State: "code", Rendered: "WIZARD-QR", Detail: "Scan now"}})
	got := next.(model)
	if got.screen == nil || got.screen.Banner != "WIZARD-QR" || got.screen.Status != "Scan now" {
		t.Fatalf("wizard pairing screen = %#v", got.screen)
	}
}

func TestChatScreenResultSwitchesToBranchedConversation(t *testing.T) {
	m := testModel()
	m.transcript = []transcriptEntry{{role: "user", text: "old chat"}}
	m.openScreen(core.Screen{ID: "chat", Conversation: "resume-ab12cd34", Transcript: []core.ChatEntry{{Role: "user", Text: "remote"}, {Role: "assistant", Text: "answer"}}})
	if m.screen != nil || m.conversation != "resume-ab12cd34" || len(m.transcript) != 2 || m.transcript[1].text != "answer" {
		t.Fatalf("branched TUI state = conversation %q, screen %#v, transcript %#v", m.conversation, m.screen, m.transcript)
	}
	if !strings.Contains(m.status, "resume-ab12cd34") {
		t.Fatalf("resume status = %q", m.status)
	}
}

func TestRequiredInitializationScreenActionsAndCannotFallIntoChat(t *testing.T) {
	m := testModel()
	initialized := false
	m.screenAction = func(_ context.Context, screenID, action string, _ map[string]string) (*core.Screen, error) {
		if screenID == "initialize" && action == "initialize" {
			initialized = true
		}
		return nil, nil
	}
	screen := InitializationScreen("/workspace/new")
	m.openScreen(screen)
	view := ansi.Strip(m.View())
	if !strings.Contains(screen.Subtitle, "not configured") || !strings.Contains(view, "/workspace/new") || !strings.Contains(screen.Controls[0].Value, "Initialize Spynel") {
		t.Fatalf("initialization screen is incomplete: %q", view)
	}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if cmd == nil || !m.screenSaving {
		t.Fatal("initialize action did not start")
	}
	next, quit := m.Update(cmd())
	m = next.(model)
	if !initialized || m.screenResult != "initialize" || quit == nil {
		t.Fatalf("initialize action did not complete: initialized=%t result=%q quit=%#v", initialized, m.screenResult, quit)
	}
	if _, ok := quit().(tea.QuitMsg); !ok {
		t.Fatalf("successful initialization did not exit setup: %T", quit())
	}

	m = testModel()
	m.openScreen(InitializationScreen("/workspace/new"))
	next, quit = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(model)
	if m.screen == nil || m.screenResult != "exit" || quit == nil {
		t.Fatalf("required setup escaped into chat: screen=%#v result=%q", m.screen, m.screenResult)
	}
}

func TestSpynelLogoIsBlankBeforeFirstMessageAndUsesRequestedFrames(t *testing.T) {
	m := testModel()
	if logo := m.spynelLogo(); logo != "○○" {
		t.Fatalf("initial logo = %q, want blank logo", logo)
	}
	want := []string{"◉◉", "◑◉", "○◉", "○◑", "○○", "◐○", "◉○", "◉◐", "◉◉"}
	if got := m.logoSpinner.Spinner.Frames; strings.Join(got, "") != strings.Join(want, "") {
		t.Fatalf("Spynel logo frames = %q, want %q", got, want)
	}
	workingFrames := []string{"⠋", "⠙", "⠸", "⠴", "⠦", "⠇"}
	if got := m.workingSpinner.Spinner.Frames; strings.Join(got, "") != strings.Join(workingFrames, "") {
		t.Fatalf("Working spinner frames = %q, want %q", got, workingFrames)
	}
}

func TestConsecutiveStreamingEventsAreCopySafe(t *testing.T) {
	m := testModel()

	next, _ := m.Update(uiEvent{event: core.Event{Kind: core.EventDelta, Text: "hello "}})
	next, _ = next.(model).Update(uiEvent{event: core.Event{Kind: core.EventDelta, Text: "world"}})
	next, _ = next.(model).Update(uiEvent{event: core.Event{Kind: core.EventFinal}})
	got := next.(model)

	if got.streaming != "" {
		t.Fatalf("streaming text = %q, want empty after final event", got.streaming)
	}
	if len(got.transcript) != 1 || !strings.Contains(got.transcript[0].text, "hello world") {
		t.Fatalf("transcript = %#v, want one combined streamed response", got.transcript)
	}
}

func TestFinalEventPreservesPriorStreamedMessages(t *testing.T) {
	m := testModel()

	next, _ := m.Update(uiEvent{event: core.Event{Kind: core.EventDelta, Text: "first message\n"}})
	next, _ = next.(model).Update(uiEvent{event: core.Event{Kind: core.EventDelta, Text: "second message"}})
	next, _ = next.(model).Update(uiEvent{event: core.Event{Kind: core.EventFinal, Text: "second message", Done: true}})
	got := next.(model)

	if len(got.transcript) != 1 || got.transcript[0].text != "first message\nsecond message" {
		t.Fatalf("transcript = %#v, want both streamed messages separated by a newline", got.transcript)
	}
}

func TestFinalEventAppendsDistinctContent(t *testing.T) {
	if got, want := completedResponse("first message", "last message"), "first message\nlast message"; got != want {
		t.Fatalf("completed response = %q, want %q", got, want)
	}
}

func TestPersistedHistoryBuildsInitialTranscript(t *testing.T) {
	transcript := transcriptFromHistory([]history.Entry{
		{Role: "user", Content: "remember this"},
		{Role: "assistant", Content: "remembered"},
	})
	if len(transcript) != 2 || transcript[0].role != "user" || transcript[0].text != "remember this" || transcript[1].role != "assistant" || transcript[1].text != "remembered" {
		t.Fatalf("transcript = %#v", transcript)
	}
}

func TestClearEventRemovesVisibleTranscript(t *testing.T) {
	m := testModel()
	m.transcript = []transcriptEntry{{role: "user", text: "old message"}, {role: "assistant", text: "old response"}}
	m.streaming = "partial response"
	m.welcome = &core.Screen{ID: "welcome", Banner: core.SpynelASCII}

	next, _ := m.Update(uiEvent{event: core.Event{Kind: core.EventFinal, Text: "Conversation history and harness thread cleared.", Clear: true, Done: true}})
	got := next.(model)
	if len(got.transcript) != 0 || got.streaming != "" || got.welcome != nil {
		t.Fatalf("transcript = %#v, streaming = %q, welcome = %#v", got.transcript, got.streaming, got.welcome)
	}
	if got.status != "Conversation history and harness thread cleared." {
		t.Fatalf("status = %q", got.status)
	}
}

func TestWelcomeRendersInlineAboveChatWithoutTakingComposerFocus(t *testing.T) {
	m := testModel()
	m.transcript = []transcriptEntry{{role: "user", text: "hello"}}
	welcome := core.Screen{
		ID: "welcome", Banner: core.SpynelASCII,
		Subtitle: "Type /help to show commands.\nType /telegram to configure Telegram.",
		Controls: []core.ScreenControl{{Key: "old-button", Kind: "action", Value: "Old button"}},
	}
	m.openScreen(welcome)
	if m.screen != nil || m.welcome == nil || len(m.welcome.Controls) != 0 {
		t.Fatalf("welcome opened as a form or retained buttons: screen %#v, welcome %#v", m.screen, m.welcome)
	}
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = next.(model)
	view := ansi.Strip(m.viewport.View())
	logoLine := lineContaining(strings.Split(view, "\n"), "███████╗██████╗")
	helpLine := lineContaining(strings.Split(view, "\n"), "Type /help to show commands")
	messageLine := lineContaining(strings.Split(view, "\n"), "You hello")
	if logoLine < 0 || helpLine <= logoLine || messageLine <= helpLine || strings.Contains(view, "Old button") {
		t.Fatalf("inline welcome order = %q", view)
	}
	if !m.input.Focused() || m.viewport.YOffset != 0 {
		t.Fatalf("inline welcome stole composer focus or did not start at top: focused=%v offset=%d", m.input.Focused(), m.viewport.YOffset)
	}
}

func testModel() model {
	input := textarea.New()
	input.Placeholder = "Message Spynel"
	input.Prompt = ""
	styleComposer(&input)
	input.ShowLineNumbers = false
	input.SetWidth(20)
	input.SetHeight(maxComposerHeight)
	input.Focus()
	return model{
		ctx:            context.Background(),
		handler:        channel.Handler(func(context.Context, core.Message, core.Emit) error { return nil }),
		input:          input,
		inputWidth:     20,
		composerRows:   minComposerHeight,
		viewport:       viewport.New(20, 5),
		events:         make(chan core.Event, 1),
		logoSpinner:    newLogoSpinner(),
		workingSpinner: newWorkingSpinner(),
		commands: []core.SlashCommand{
			{Value: "/help", Usage: "/help", Description: "Show help"},
			{Value: "/status", Usage: "/status", Description: "Show status"},
			{Value: "/title ", Usage: "/title <name>", Description: "Rename window"},
			{Value: "/task ", Usage: "/task <request>", Description: "Create task"},
		},
		width:  24,
		height: 20,
		status: "Ready",
	}
}

func inputCursorColumn(m model) int {
	_, column := m.inputCursorPosition()
	return column
}
