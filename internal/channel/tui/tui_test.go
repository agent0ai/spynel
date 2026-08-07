package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/history"
	"github.com/agent0ai/spynel/internal/theme"
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

func TestComposerPlaceholderIsMutedItalicOnInputSurface(t *testing.T) {
	input := textarea.New()
	styles := stylesFor(theme.Default())
	styleComposer(&input, styles)

	for name, style := range map[string]textarea.Style{
		"focused": input.FocusedStyle,
		"blurred": input.BlurredStyle,
	} {
		if !style.Placeholder.GetItalic() {
			t.Errorf("%s placeholder is not italic", name)
		}
		if style.Placeholder.GetForeground() != styles.status.GetForeground() {
			t.Errorf("%s placeholder foreground = %v, want %v", name, style.Placeholder.GetForeground(), styles.status.GetForeground())
		}
		if style.Placeholder.GetBackground() != (lipgloss.NoColor{}) || style.CursorLine.GetBackground() != (lipgloss.NoColor{}) {
			t.Errorf("%s placeholder or cursor line paints its own background", name)
		}
		if style.CursorLine.Render("selected line") != style.Text.Render("selected line") {
			t.Errorf("%s cursor line differs from ordinary input text", name)
		}
	}
	if input.Cursor.Style.GetForeground() != styles.selectedCommand.GetBackground() || input.Cursor.Style.GetBackground() != (lipgloss.NoColor{}) || input.Cursor.TextStyle.GetBackground() != (lipgloss.NoColor{}) {
		t.Fatal("composer cursor does not use semantic input/selection colors")
	}
}

func TestApplyThemeRebindsRenderedComposerPlaceholder(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })
	m := testModel()
	selected, ok := theme.Find(theme.Builtins(), "rose-pine-dawn")
	if !ok {
		t.Fatal("rose-pine-dawn built-in is missing")
	}
	m.applyTheme(selected)
	view := m.input.View()
	if !strings.Contains(view, "38;2;111;105;134") {
		t.Fatalf("placeholder did not rebind to active text_muted color: %q", view)
	}
	if strings.Contains(view, "38;2;130;146;174") {
		t.Fatalf("placeholder retained default Spynel text_muted color: %q", view)
	}
}

func TestFreshConversationUsesConciseCopy(t *testing.T) {
	m := testModel()
	if m.input.Placeholder != "Message Spynel, / for commands" {
		t.Fatalf("composer placeholder = %q", m.input.Placeholder)
	}
	m.renderHistory()
	view := ansi.Strip(m.viewport.View())
	if !strings.Contains(view, "A fresh start.") || strings.Contains(view, "fresh conversation is ready") {
		t.Fatalf("empty conversation copy = %q", view)
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
		t.Fatalf("chat rows = %q and %q, want aligned sender prefixes", user, agent)
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

func TestMarkdownChatDoesNotOrphanBoundaryLettersBesideScrollbar(t *testing.T) {
	m := testModel()
	next, _ := m.Update(tea.WindowSizeMsg{Width: 109, Height: 12})
	m = next.(model)
	message := "Got it — I’ll update the active theme-rebalance job so the existing `spynel` and `hack-the-box` themes are explicitly preserved.\nUpdated the active job: the existing `Spynel` and `Hack The Box` themes and their palettes must remain unchanged."
	markdown := m.renderAgentMarkdown(message)
	rendered := ansi.Strip(m.renderMarkdownChatMessage(agentChatLabel, m.styles.agent, markdown))
	for _, line := range strings.Split(rendered, "\n") {
		if width := lipgloss.Width(line); width > m.viewport.Width {
			t.Fatalf("Markdown chat row width = %d, viewport width = %d: %q", width, m.viewport.Width, rendered)
		}
		if orphan := strings.TrimSpace(line); orphan == "s" || orphan == "n" {
			t.Fatalf("Markdown boundary word left orphan %q beside the scrollbar: rendered=%q markdown=%q", orphan, rendered, ansi.Strip(markdown))
		}
	}
	if !strings.Contains(rendered, "are explicitly preserved.") || strings.Contains(rendered, "\n    are\n") {
		t.Fatalf("Markdown renderer stranded a short word despite room on its continuation row: %q", rendered)
	}
	if got, want := strings.Join(strings.Fields(rendered), " "), "Spy "+strings.Join(strings.Fields(strings.NewReplacer("`", "").Replace(message)), " "); got != want {
		t.Fatalf("wrapped Markdown text changed: got %q, want %q", got, want)
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
	styles := stylesFor(theme.Default())
	if got, want := styles.user.GetForeground(), lipgloss.Color(theme.Default().Colors.User); got != want {
		t.Fatalf("user label foreground = %v, want %v", got, want)
	}
	if !styles.user.GetBold() {
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

func TestComposerScrollsOnFirstCharacterWrappedPastCap(t *testing.T) {
	m := testModel()
	m.inputWidth = 4
	m.input.SetWidth(4)
	m.input.SetValue(strings.Repeat("x\n", maxComposerHeight-1) + "a b")
	m.composerRows = maxComposerHeight

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Z")})
	got := next.(model)
	visible := ansi.Strip(got.input.View())
	if !strings.Contains(visible, "bZ") {
		t.Fatalf("first character on wrapped row is hidden until a later delimiter: %q", visible)
	}
	offset, total := got.inputScrollMetrics()
	if offset != 1 || total != maxComposerHeight+1 {
		t.Fatalf("first wrapped character scroll metrics = offset %d, total %d", offset, total)
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

func TestGrowingComposerKeepsNewestHistoryVisibleAtBottom(t *testing.T) {
	m := testModel()
	m.height = 24
	m.inputWidth = 4
	m.input.SetWidth(4)
	m.resizeComposer()
	lines := make([]string, 60)
	for index := range lines {
		lines[index] = fmt.Sprintf("history-%02d", index)
	}
	m.viewport.SetContent(strings.Join(lines, "\n"))
	m.viewport.GotoBottom()
	oldHeight := m.viewport.Height
	oldOffset := m.viewport.YOffset

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1234")})
	got := next.(model)
	if got.composerRows != 2 || got.viewport.Height != oldHeight-1 {
		t.Fatalf("resized rows=%d viewport height=%d, want rows=2 height=%d", got.composerRows, got.viewport.Height, oldHeight-1)
	}
	if !got.viewport.AtBottom() || got.viewport.YOffset != oldOffset+1 {
		t.Fatalf("history lost bottom anchor: bottom=%t offset=%d, want %d", got.viewport.AtBottom(), got.viewport.YOffset, oldOffset+1)
	}
	visible := strings.Split(got.viewport.View(), "\n")
	if !strings.Contains(visible[len(visible)-1], "history-59") {
		t.Fatalf("newest history row is not visible after composer growth: %q", visible)
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
	if !placeholderIsVisible(got.input.View(), got.input.Placeholder) {
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
	next, _ = got.Update(tea.KeyMsg{Type: tea.KeyShiftUp})
	got = next.(model)
	if got.viewport.YOffset != pageOffset-1 || got.input.Line() != inputLine {
		t.Fatalf("after Shift+Up: offset = %d, want %d; input line = %d", got.viewport.YOffset, pageOffset-1, got.input.Line())
	}
	next, _ = got.Update(tea.KeyMsg{Type: tea.KeyShiftDown})
	got = next.(model)
	if got.viewport.YOffset != pageOffset || got.input.Line() != inputLine {
		t.Fatalf("after Shift+Down: offset = %d, want %d; input line = %d", got.viewport.YOffset, pageOffset, got.input.Line())
	}

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
	if got.footerHint() != "↑↓ choose · ⇥ insert · ↵ send · ␛ close" {
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

func TestCtrlCClearsComposerWithoutStoppingActiveHarness(t *testing.T) {
	m := testModel()
	m.working = true
	m.input.SetValue("keep neither this draft nor its paste")
	m.tokens = []composerToken{{label: "[Pasted 4 chars]", expansion: "body"}}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	got := next.(model)
	if cmd != nil {
		t.Fatal("Ctrl+C with input dispatched a command")
	}
	if got.input.Value() != "" || len(got.tokens) != 0 || !got.working || len(got.transcript) != 0 {
		t.Fatalf("Ctrl+C did not only clear active input: input=%q tokens=%#v working=%t transcript=%#v", got.input.Value(), got.tokens, got.working, got.transcript)
	}
}

func TestCtrlCStopsActiveHarnessWhenComposerIsEmpty(t *testing.T) {
	m := testModel()
	m.working = true
	m.streaming = "partial response"
	m.responseText = m.streaming
	received := make(chan core.Message, 1)
	m.handler = channel.Handler(func(_ context.Context, message core.Message, _ core.Emit) error {
		received <- message
		return nil
	})

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	got := next.(model)
	if cmd == nil {
		t.Fatal("Ctrl+C while working returned no stop command")
	}
	if !got.working || got.input.Value() != "" || got.ignoreNextLF {
		t.Fatalf("stop dispatch corrupted active state: working=%t input=%q ignore-next-lf=%t", got.working, got.input.Value(), got.ignoreNextLF)
	}
	if len(got.transcript) != 2 || got.transcript[0] != (transcriptEntry{role: "assistant", text: "partial response"}) || got.transcript[1] != (transcriptEntry{role: "user", text: "/stop"}) {
		t.Fatalf("stop dispatch transcript = %#v", got.transcript)
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("Ctrl+C stop command returned %T, want tea.BatchMsg", cmd())
	}
	for _, command := range batch {
		_ = command()
	}
	select {
	case message := <-received:
		if message.Channel != "tui" || message.Conversation != m.conversation || message.Text != "/stop" {
			t.Fatalf("Ctrl+C dispatched %#v, want local /stop", message)
		}
	case <-time.After(time.Second):
		t.Fatal("Ctrl+C did not dispatch /stop")
	}
}

func TestCommandMenuSurfaceHasConciseTitle(t *testing.T) {
	m := testModel()
	m.width = 80
	m.input.SetValue("/")
	m.syncCommandMenu()

	view := m.commandMenuView()
	if !strings.Contains(view, "Commands") || strings.Contains(view, "tab=insert") || strings.Contains(view, "enter=send") || !strings.Contains(view, "╭") {
		t.Fatalf("command menu title = %q", view)
	}
	rows := strings.Split(ansi.Strip(view), "\n")
	if len(rows) < 2 || !strings.HasPrefix(strings.TrimLeft(rows[1], "│ "), "/help") || strings.Contains(rows[1], ">") {
		t.Fatalf("selected command row is not flush-left and marker-free: %q", rows)
	}
}

func TestThemeMenuPreviewsCancelsAndPersistsSelection(t *testing.T) {
	m := testModel()
	alternate := theme.Default()
	alternate.Name = "alternate"
	alternate.Description = "Alternate preview"
	alternate.Colors.Background = "#020304"
	alternate.Colors.Primary = "#AABBCC"
	m.themes = []theme.Theme{m.activeTheme, alternate}
	m.openThemeMenu()
	if !m.themeMenu || m.themeIndex != 0 {
		t.Fatalf("theme menu state = open %t index %d", m.themeMenu, m.themeIndex)
	}
	title := ansi.Strip(m.themeMenuView())
	if !strings.Contains(title, "╭─ Themes ") || strings.Contains(title, "◈") || strings.Contains(title, "Themes ·") {
		t.Fatalf("theme menu title is not concise: %q", title)
	}
	if handled, _ := m.handleThemeMenuKey(tea.KeyMsg{Type: tea.KeyDown}); !handled || m.activeTheme.Name != alternate.Name || m.styles.title.GetForeground() != lipgloss.Color(alternate.Colors.Primary) {
		t.Fatalf("theme was not previewed immediately: active=%#v", m.activeTheme)
	}
	if handled, _ := m.handleThemeMenuKey(tea.KeyMsg{Type: tea.KeyEsc}); !handled || m.activeTheme.Name != theme.DefaultName || m.themeMenu {
		t.Fatalf("theme preview did not cancel: active=%q open=%t", m.activeTheme.Name, m.themeMenu)
	}

	var saved string
	m.saveTheme = func(name string) error { saved = name; return nil }
	m.openThemeMenu()
	_, _ = m.handleThemeMenuKey(tea.KeyMsg{Type: tea.KeyDown})
	handled, command := m.handleThemeMenuKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !handled || command == nil {
		t.Fatal("theme Enter did not request persistence")
	}
	next, _ := m.Update(command())
	m = next.(model)
	if saved != alternate.Name || m.activeTheme.Name != alternate.Name || m.themeMenu {
		t.Fatalf("saved theme = %q active=%q open=%t", saved, m.activeTheme.Name, m.themeMenu)
	}
}

func TestThemeMenuReloadsFilesBeforePreview(t *testing.T) {
	m := testModel()
	reloaded := theme.Default()
	reloaded.Colors.Primary = "#123456"
	m.loadThemes = func() ([]theme.Theme, error) { return []theme.Theme{reloaded}, nil }
	m.openThemeMenu()
	if !m.themeMenu || len(m.themes) != 1 || m.styles.title.GetForeground() != lipgloss.Color(reloaded.Colors.Primary) {
		t.Fatalf("reloaded theme was not previewed: active=%#v themes=%#v", m.activeTheme, m.themes)
	}
	m.cancelThemeMenu()
	if m.activeTheme.Colors.Primary != theme.Default().Colors.Primary {
		t.Fatalf("cancelling a reloaded preview did not restore the prior palette: %#v", m.activeTheme)
	}
}

func TestChatLayoutUsesUnframedHistoryAndBorderedComposer(t *testing.T) {
	m := testModel()
	m.width = 90
	m.height = 22
	m.viewport.Width = 87
	m.viewport.Height = 15
	m.inputWidth = 86
	m.appendTranscript(transcriptEntry{role: "assistant", text: "A response"})
	m.renderHistory()
	view := ansi.Strip(m.View())
	lines := strings.Split(view, "\n")
	if strings.HasPrefix(lines[1], "╭") || strings.HasPrefix(lines[1], "│") || !strings.HasPrefix(lines[1], " ") {
		t.Fatalf("chat history is framed or lacks its left inset: %q", lines[1])
	}
	if !strings.Contains(view, "╭────────────────") || strings.Contains(view, "╭─ Message ") || !strings.Contains(view, "╯") {
		t.Fatalf("composer panel border is missing: %q", view)
	}
	for index, line := range lines {
		if width := lipgloss.Width(line); width != 90 {
			t.Fatalf("row %d width = %d, want 90: %q", index, width, line)
		}
	}
	if m.styles.base.GetBackground() != lipgloss.Color(m.activeTheme.Colors.Background) || m.styles.headerFill.GetForeground() != ribbonThemeColor(m.activeTheme.Colors.Background, m.activeTheme.Colors.User) || m.styles.footerFill.GetForeground() != ribbonThemeColor(m.activeTheme.Colors.Background, m.activeTheme.Colors.User) || m.styles.header.GetBackground() != lipgloss.Color(m.activeTheme.Colors.Background) || m.styles.surface.GetBackground() != lipgloss.Color(m.activeTheme.Colors.Surface) {
		t.Fatalf("header ribbon/input surface do not use semantic colors")
	}
}

func TestRibbonColorUsesMutedUserAccent(t *testing.T) {
	styles := stylesFor(theme.Default())
	want := lipgloss.Color("#395F91")
	if got := styles.headerFill.GetForeground(); got != want {
		t.Fatalf("header ribbon = %v, want muted user accent %v", got, want)
	}
	if got := styles.footerFill.GetForeground(); got != want {
		t.Fatalf("footer ribbon = %v, want muted user accent %v", got, want)
	}
}

func TestHistoryRenderCacheInvalidatesWhenTranscriptChanges(t *testing.T) {
	m := testModel()
	m.appendTranscript(transcriptEntry{role: "assistant", text: "first"})
	m.renderHistory()
	if !m.historyValid || len(m.historyCache) != 1 {
		t.Fatalf("history cache was not populated: valid=%t entries=%d", m.historyValid, len(m.historyCache))
	}
	m.appendTranscript(transcriptEntry{role: "assistant", text: "second"})
	if m.historyValid {
		t.Fatal("appending a transcript entry did not invalidate the render cache")
	}
	m.renderHistory()
	if len(m.historyCache) != 2 {
		t.Fatalf("history cache entries = %d, want 2", len(m.historyCache))
	}
}

func TestHelpMarkdownHeadingsStayOnOneChatRow(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })
	narrow := ansi.Strip(visualNarrowHelpModel().View())
	if lineContaining(strings.Split(narrow, "\n"), "About Spynel") < 0 {
		t.Fatalf("help heading wrapped in the narrow full-frame render: %q", narrow)
	}
	for _, heading := range []string{"Spynel help", "About Spynel"} {
		for _, terminalWidth := range []int{24, 40, 80, 110} {
			m := testModel()
			next, _ := m.Update(tea.WindowSizeMsg{Width: terminalWidth, Height: 24})
			m = next.(model)
			m.appendTranscript(transcriptEntry{role: "assistant", text: "# " + heading + "\n\nHelp text."})
			m.renderHistory()
			rendered := ansi.Strip(m.View())
			lines := strings.Split(rendered, "\n")
			headingLine := lineContaining(lines, heading)
			if headingLine < 0 || !strings.Contains(lines[headingLine], heading) {
				t.Fatalf("%q heading wrapped at terminal width %d: %q", heading, terminalWidth, rendered)
			}
			for row, line := range strings.Split(m.View(), "\n") {
				if width := lipgloss.Width(line); width != terminalWidth {
					t.Fatalf("%q frame row %d width = %d, want %d: %q", heading, row, width, terminalWidth, ansi.Strip(line))
				}
			}
		}
	}
}

func TestWizardTabsUseBoldLabelsAndActiveUnderline(t *testing.T) {
	m := testModel()
	m.openScreen(core.Screen{ID: "wizard:telegram:token", Title: "Telegram setup", Tabs: []string{"Start", "Create", "Token", "Access"}, ActiveTab: 2})
	rendered := m.tabsView(80)
	plain := strings.Split(ansi.Strip(rendered), "\n")
	if len(plain) != 2 {
		t.Fatalf("wizard tab rows = %d, want 2: %q", len(plain), plain)
	}
	for _, expected := range []string{"Start", "Create", "Token", "Access"} {
		if !strings.Contains(plain[0], expected) {
			t.Fatalf("wizard tabs missing %q: %q", expected, plain[0])
		}
	}
	if strings.ContainsAny(plain[0], "✓•") {
		t.Fatalf("wizard tabs retained progress markers: %q", plain[0])
	}
	if lipgloss.Width(plain[1]) != 80 || strings.Trim(plain[1], "━") != "" {
		t.Fatalf("wizard underline row = %q", plain[1])
	}
	if !strings.Contains(rendered, m.styles.base.Bold(true).Render("Token")) || !strings.Contains(rendered, m.styles.thumb.Render("━━━━━")) {
		t.Fatalf("active tab label or underline is not highlighted: %q", rendered)
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
	if !strings.HasPrefix(header, "▀▀○○ Payments") || !strings.HasSuffix(header, "▀▀") || !strings.HasPrefix(footerLine, "▄▄") || !strings.HasSuffix(footerLine, "▄") {
		t.Fatalf("status ribbons do not surround their items: header=%q footer=%q", header, footerLine)
	}
	if !strings.Contains(header, "● TG▀▀▲ WA▀▀3 jobs▀▀922 logs▀▀") {
		t.Fatalf("status header is incomplete: %q", header)
	}
	if strings.Contains(header, "Ready") || strings.Contains(header, "local") {
		t.Fatalf("header contains data outside the compact contract: %q", header)
	}
	if strings.Contains(footer, "Ready") || strings.Contains(footer, "Log") || strings.Contains(footer, "Jobs") || strings.Contains(footer, "transcript") {
		t.Fatalf("footer contains status data instead of only controls: %q", footer)
	}
	if !strings.HasPrefix(footerLine, "▄▄"+strings.Split(m.footerHint(), " · ")[0]) {
		t.Fatalf("footer controls are not left-aligned: %q", footerLine)
	}
	for _, item := range strings.Split(m.footerHint(), " · ") {
		if !strings.Contains(footer, item) {
			t.Fatalf("footer = %q, missing hint %q", footer, item)
		}
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
	styles := stylesFor(theme.Default())
	if got, want := styles.title.GetForeground(), lipgloss.Color(theme.Default().Colors.Primary); got != want {
		t.Fatalf("logo/title foreground = %v, want primary %v", got, want)
	}
	if got, want := styles.status.GetForeground(), lipgloss.Color(theme.Default().Colors.TextMuted); got != want {
		t.Fatalf("remaining header foreground = %v, want muted %v", got, want)
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
	if !strings.Contains(ordinary, "PgUp/⇧+↑↓ scroll") {
		t.Fatalf("ordinary footer lacks the history scroll binding: %q", ordinary)
	}
	m.input.SetValue("/")
	m.syncCommandMenu()
	command := m.footerHint()
	if ordinary == command || !strings.Contains(ordinary, "⇧↵ line") || strings.Contains(command, "⇧↵") || strings.Contains(command, "Ready") {
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
			footer := strings.TrimSpace(lines[len(lines)-1])
			for _, item := range strings.Split(m.footerHint(), " · ") {
				if !strings.Contains(footer, item) {
					t.Fatalf("final terminal row = %q, missing hint %q", footer, item)
				}
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
	styles := stylesFor(theme.Default())
	top := scrollbar(5, 0, 20, styles, styles.base.GetBackground())
	bottom := scrollbar(5, 15, 20, styles, styles.base.GetBackground())
	if !strings.Contains(top[0], "┃") || !strings.Contains(bottom[4], "┃") {
		t.Fatalf("scrollbar thumb did not track endpoints: top=%q bottom=%q", top, bottom)
	}
}

func TestHistorySurfaceKeepsInsetsAndScrollbarOnThemeBackground(t *testing.T) {
	m := testModel()
	view := m.historySurface("one\ntwo\nthree", 3, 24, 0, 20)
	lines := strings.Split(ansi.Strip(view), "\n")
	if strings.TrimSpace(lines[0]) != "" || strings.TrimSpace(lines[len(lines)-1]) != "" || !strings.HasPrefix(lines[1], " one") || !strings.HasSuffix(lines[1], " ┃") {
		t.Fatalf("history top/side padding or right-edge scrollbar is incorrect: %q", lines)
	}
	for index, line := range lines {
		if lipgloss.Width(line) != 24 {
			t.Fatalf("history row %d width = %d, want 24: %q", index, lipgloss.Width(line), line)
		}
	}
	if background := fmt.Sprintf("48;2;%d;%d;%d", 17, 25, 39); !strings.Contains(view, background) {
		t.Fatalf("history surface does not paint the theme background: %q", view)
	}
}

func TestBorderedSurfaceUsesPageBackgroundAroundInput(t *testing.T) {
	m := testModel()
	if got, want := m.panelBorderStyle().GetBackground(), m.styles.base.GetBackground(); got != want {
		t.Fatalf("border background = %v, want page background %v", got, want)
	}
}

func TestScrollbarUsesSemanticBorderAndPrimaryColors(t *testing.T) {
	styles := stylesFor(theme.Default())
	if got, want := styles.track.GetForeground(), lipgloss.Color(theme.Default().Colors.Border); got != want {
		t.Fatalf("scrollbar track color = %v, want border %v", got, want)
	}
	if got, want := styles.thumb.GetForeground(), lipgloss.Color(theme.Default().Colors.Primary); got != want {
		t.Fatalf("scrollbar thumb color = %v, want primary %v", got, want)
	}
}

func TestChatSurfaceReplacesRightBorderWithUsableScrollbar(t *testing.T) {
	m := testModel()
	plain := m.borderedSurface("", "one\ntwo\nthree", 3, 24, 0, 3, m.styles.base)
	plainLines := strings.Split(ansi.Strip(plain), "\n")
	for _, line := range plainLines[1 : len(plainLines)-1] {
		if !strings.HasSuffix(line, "│") || strings.Contains(line, "┃") {
			t.Fatalf("non-scrollable chat has an invalid right border: %q", line)
		}
	}

	scrolled := m.borderedSurface("", "one\ntwo\nthree", 3, 24, 0, 20, m.styles.base)
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
	if !placeholderIsVisible(got.input.View(), got.input.Placeholder) {
		t.Fatalf("input view %q does not contain placeholder %q", got.input.View(), got.input.Placeholder)
	}
}

func TestCRLFOnEmptyComposerKeepsPlaceholderVisible(t *testing.T) {
	m := testModel()

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	next, _ = next.(model).Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
	got := next.(model)
	if got.input.Value() != "" || !placeholderIsVisible(got.input.View(), got.input.Placeholder) {
		t.Fatalf("input value = %q, view = %q", got.input.Value(), got.input.View())
	}
}

func placeholderIsVisible(view, placeholder string) bool {
	return strings.Contains(strings.Join(strings.Fields(ansi.Strip(view)), " "), placeholder)
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
	if m.screen == nil || !strings.Contains(view, "Telegram") || !strings.Contains(view, "Configure the bot") || !strings.Contains(m.screenFooterHint(), "⌃S save") || strings.Contains(view, "preserve chat") || strings.Contains(view, m.input.Placeholder) {
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
	if m.screenIndex != 1 || !strings.Contains(m.screenFooterHint(), "↵ select") || strings.Contains(m.screenFooterHint(), "⌃S") {
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

func TestResumeScreenUsesItsOwnHintsAndDeleteHandler(t *testing.T) {
	m := testModel()
	encoded := []string{"first", "second", "third"}
	hints := []core.ScreenHint{
		{Key: "↑↓/⇥", Action: "nav"},
		{Key: "␠/↵", Action: "choose"},
		{Key: "⌦", Action: "delete"},
		{Key: "␛", Action: "exit"},
	}
	m.openScreen(core.Screen{
		ID: "resume", Title: "Resume a conversation", SaveDisabled: true,
		Hints: hints,
		Controls: []core.ScreenControl{
			{Key: "resume:" + encoded[0], Kind: "action", Value: "telegram · TG-1"},
			{Key: "resume:" + encoded[1], Kind: "action", Value: "telegram · TG-2"},
			{Key: "resume:" + encoded[2], Kind: "action", Value: "telegram · TG-3"},
		},
	})
	if got, want := m.screenFooterHint(), "↑↓/⇥ nav · ␠/↵ choose · ⌦  delete · ␛ exit"; got != want {
		t.Fatalf("resume footer = %q, want %q", got, want)
	}
	content, _, _ := m.screenContent(20, 80)
	if strings.Contains(ansi.Strip(content), "Choose an option") {
		t.Fatalf("resume screen contains redundant section heading: %q", content)
	}

	m.screenIndex = 1
	called := []string{}
	m.screenAction = func(_ context.Context, screenID, action string, _ map[string]string) (*core.Screen, error) {
		if screenID != "resume" {
			t.Fatalf("screen ID = %q", screenID)
		}
		called = append(called, action)
		controls := []core.ScreenControl{{Key: "resume:" + encoded[0], Kind: "action", Value: "telegram · TG-1"}}
		if len(called) == 1 {
			controls = append(controls, core.ScreenControl{Key: "resume:" + encoded[2], Kind: "action", Value: "telegram · TG-3"})
		}
		return &core.Screen{ID: "resume", Title: "Resume a conversation", SaveDisabled: true, Hints: hints, Controls: controls}, nil
	}
	// Backspace is the convenient macOS delete key and removes the selected
	// middle row. Its former next row should retain the cursor.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = next.(model)
	if cmd == nil || !m.screenSaving {
		t.Fatal("Backspace did not start the selected conversation deletion")
	}
	next, _ = m.Update(cmd())
	m = next.(model)
	if !reflect.DeepEqual(called, []string{"delete:" + encoded[1]}) || m.screen == nil || m.screenIndex != 1 || m.screen.Controls[m.screenIndex].Key != "resume:"+encoded[2] {
		t.Fatalf("middle delete result: actions=%q index=%d screen=%#v", called, m.screenIndex, m.screen)
	}

	// Forward Delete removes the now-last row and falls back to the previous
	// row because there is no item below it.
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyDelete})
	m = next.(model)
	if cmd == nil {
		t.Fatal("Delete did not start the selected conversation deletion")
	}
	next, _ = m.Update(cmd())
	m = next.(model)
	if !reflect.DeepEqual(called, []string{"delete:" + encoded[1], "delete:" + encoded[2]}) || m.screenIndex != 0 || len(m.screen.Controls) != 1 {
		t.Fatalf("last delete result: actions=%q index=%d screen=%#v", called, m.screenIndex, m.screen)
	}
}

func TestResumePreviewIsOneTrimmedRowIndentedFiveCells(t *testing.T) {
	m := testModel()
	m.openScreen(core.Screen{
		ID: "resume", Title: "Resume a conversation", SaveDisabled: true,
		Controls: []core.ScreenControl{{
			Key: "resume:one", Kind: "action", Value: "TG   2026-08-07 15:21  TG-7.jsonl",
			Description: "assistant: A long final message with\nembedded whitespace that must truncate instead of wrapping onto another line in the list.",
		}},
	})
	content, _, _ := m.screenContent(20, 42)
	lines := strings.Split(ansi.Strip(content), "\n")
	if len(lines) != 2 {
		t.Fatalf("resume choice rendered as %d rows, want exactly 2: %q", len(lines), content)
	}
	if !strings.HasPrefix(lines[1], "     assistant:") || !strings.HasSuffix(lines[1], "…") || strings.Contains(lines[1], "\n") {
		t.Fatalf("resume preview is not indented and truncated: %q", lines[1])
	}
	if lipgloss.Width(lines[1]) > 41 {
		t.Fatalf("resume preview exceeds available row width: width=%d row=%q", lipgloss.Width(lines[1]), lines[1])
	}
}

func TestScreenFooterNeverAdvertisesTypingAsAControl(t *testing.T) {
	m := testModel()
	for _, screen := range []core.Screen{
		{ID: "config", Controls: []core.ScreenControl{{Key: "name", Kind: "text"}}},
		{ID: "wizard:telegram:token", SaveDisabled: true, Controls: []core.ScreenControl{{Key: "token", Kind: "password"}}},
	} {
		m.openScreen(screen)
		if hint := m.screenFooterHint(); strings.Contains(hint, "type edit") {
			t.Fatalf("%s footer contains obsolete typing hint: %q", screen.ID, hint)
		}
		if hint := m.screenFooterHint(); strings.Contains(hint, "␛ chat") {
			t.Fatalf("%s footer uses the obsolete Escape label: %q", screen.ID, hint)
		}
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
	if !strings.Contains(m.screenFooterHint(), "␛ back") {
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

func TestNestedChannelWizardCancelReturnsToPreservedSettings(t *testing.T) {
	m := testModel()
	m.openScreen(core.Screen{ID: "telegram", Controls: []core.ScreenControl{{Key: "wizard", Kind: "action", Value: "Setup wizard"}}})
	m.openScreen(core.Screen{
		ID: "wizard:telegram:intro", ParentID: "telegram", Title: "Telegram setup", SaveDisabled: true,
		Controls: []core.ScreenControl{{Key: "cancel", Kind: "action", Value: "Cancel setup"}},
	})
	m.screenAction = func(_ context.Context, screenID, action string, _ map[string]string) (*core.Screen, error) {
		if screenID != "wizard:telegram:intro" || action != "cancel" {
			t.Fatalf("wizard cancel action = %q %q", screenID, action)
		}
		return nil, nil
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if cmd == nil {
		t.Fatal("wizard cancel did not run its action")
	}
	next, _ = m.Update(cmd())
	m = next.(model)
	if m.screen == nil || m.screen.ID != "telegram" || len(m.screenStack) != 0 || m.status != "Editing Telegram" {
		t.Fatalf("wizard cancel did not restore Telegram settings: screen=%#v stack=%d status=%q", m.screen, len(m.screenStack), m.status)
	}

	m.clearScreen()
	m.openScreen(core.Screen{
		ID: "wizard:telegram:intro", Title: "Telegram setup", SaveDisabled: true,
		Controls: []core.ScreenControl{{Key: "cancel", Kind: "action", Value: "Cancel setup"}},
	})
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if cmd == nil {
		t.Fatal("direct wizard cancel did not run its action")
	}
	next, _ = m.Update(cmd())
	m = next.(model)
	if m.screen != nil || m.status != "Ready" {
		t.Fatalf("direct wizard cancel did not return to chat: screen=%#v status=%q", m.screen, m.status)
	}
}

func TestChannelScreensStartWithLiveConnectionStatusAndDetail(t *testing.T) {
	tests := []struct {
		name      string
		screenID  string
		status    channel.ConnectionStatus
		heading   string
		indicator string
		identity  string
		detail    string
	}{
		{name: "Telegram connected", screenID: "telegram", status: channel.ConnectionStatus{Name: "telegram", State: channel.ConnectionConnected, Identity: "@spynel_test_bot", Link: "https://t.me/spynel_test_bot"}, heading: "Telegram Status", indicator: "● Connected", identity: "Bot: @spynel_test_bot"},
		{name: "Telegram error", screenID: "telegram", status: channel.ConnectionStatus{Name: "telegram", State: channel.ConnectionError, Detail: "authentication failed\ncheck the bot token"}, heading: "Telegram Status", indicator: "▲ Error", detail: "authentication failed check the bot token"},
		{name: "WhatsApp connected", screenID: "whatsapp", status: channel.ConnectionStatus{Name: "whatsapp", State: channel.ConnectionConnected, Detail: "WhatsApp connected", Identity: "+15551234567", Link: "https://wa.me/15551234567"}, heading: "WhatsApp Status", indicator: "● Connected", identity: "Account: +15551234567"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := testModel()
			m.connection = connectionMap([]channel.ConnectionStatus{test.status})
			m.openScreen(core.Screen{ID: test.screenID, Title: test.name, Banner: "PAIRING CONTENT", Subtitle: "Channel settings", Controls: []core.ScreenControl{{Key: "done", Kind: "action", Value: "Done"}}})
			content, _, _ := m.screenContent(30, 100)
			lines := strings.Split(ansi.Strip(content), "\n")
			statusLine := lineContaining(lines, test.heading+" ─")
			indicatorLine := lineContaining(lines, test.indicator)
			if indicatorLine != statusLine+2 || strings.TrimSpace(lines[statusLine+1]) != "" {
				t.Fatalf("channel status does not have one blank row before its indicator: %q", content)
			}
			identityLine := indicatorLine
			if test.identity != "" {
				identityLine = lineContaining(lines, test.identity)
				if identityLine <= indicatorLine || !strings.Contains(content, "\x1b]8;;"+test.status.Link+"\x1b\\") {
					t.Fatalf("channel identity is not a clickable status link: %q", content)
				}
			}
			detailLine := identityLine
			if test.detail != "" {
				detailLine = lineContaining(lines, test.detail)
				if detailLine != indicatorLine+1 {
					t.Fatalf("channel error is not directly below its indicator: %q", content)
				}
			}
			bannerLine := lineContaining(lines, "PAIRING CONTENT")
			settingsLine := lineContaining(lines, "Channel settings")
			validOrder := statusLine == 0 && indicatorLine > statusLine
			if test.screenID == "whatsapp" {
				validOrder = validOrder && bannerLine == -1 && settingsLine > detailLine
			} else {
				validOrder = validOrder && bannerLine > detailLine && settingsLine > bannerLine
			}
			if !validOrder {
				t.Fatalf("channel status section order = %q", content)
			}
			if test.status.State == channel.ConnectionConnected && test.status.Detail != "" && strings.Contains(ansi.Strip(content), test.status.Detail) {
				t.Fatalf("connected channel repeats redundant detail: %q", content)
			}
		})
	}
}

func TestChannelWizardOmitsLiveConnectionStatus(t *testing.T) {
	for _, screenID := range []string{"wizard:telegram:token", "wizard:whatsapp:pair"} {
		t.Run(screenID, func(t *testing.T) {
			m := testModel()
			channelName := "telegram"
			if strings.Contains(screenID, "whatsapp") {
				channelName = "whatsapp"
			}
			m.connection = connectionMap([]channel.ConnectionStatus{{Name: channelName, State: channel.ConnectionConnected, Detail: "live transport detail"}})
			m.openScreen(core.Screen{
				ID: screenID, Title: "Setup", Tabs: []string{"Start", "Configure"}, ActiveTab: 1,
				Subtitle: "Complete this setup step.", Controls: []core.ScreenControl{{Key: "next", Kind: "action", Value: "Continue"}},
			})
			content, _, _ := m.screenContent(30, 100)
			plain := ansi.Strip(content)
			if strings.Contains(plain, "Connected") || strings.Contains(plain, "live transport detail") || strings.Contains(plain, "Status ─") {
				t.Fatalf("wizard contains live connection status: %q", plain)
			}
			if !strings.Contains(plain, "Complete this setup step.") || !strings.Contains(plain, "Continue") {
				t.Fatalf("wizard setup content is missing: %q", plain)
			}
		})
	}
}

func TestChannelEnabledToggleStaysInsideStatusBeforeConfiguration(t *testing.T) {
	for _, screenID := range []string{"telegram", "whatsapp"} {
		t.Run(screenID, func(t *testing.T) {
			m := testModel()
			m.connection = connectionMap([]channel.ConnectionStatus{{Name: screenID, State: channel.ConnectionConnected}})
			m.openScreen(core.Screen{ID: screenID, Controls: []core.ScreenControl{
				{Key: "enabled", Label: "enabled", Kind: "toggle", Value: "on", Options: []string{"on", "off"}, Description: "Start the channel"},
				{Key: "wizard", Section: "Setup", Kind: "action", Value: "Setup wizard", Description: "Configure the connection"},
				{Key: "basic", Section: "Basic settings", Label: "account", Kind: "text", Value: "configured", Description: "Connection settings"},
			}})
			content, _, _ := m.screenContent(30, 100)
			lines := strings.Split(ansi.Strip(content), "\n")
			statusLine := lineContaining(lines, "Status ─")
			indicatorLine := lineContaining(lines, "● Connected")
			enabledLine := lineContaining(lines, "Enabled")
			setupLine := lineContaining(lines, "Setup ─")
			basicLine := lineContaining(lines, "Basic settings ─")
			if statusLine != 0 || indicatorLine <= statusLine || enabledLine <= indicatorLine || setupLine <= enabledLine || basicLine <= setupLine {
				t.Fatalf("%s status/configuration order = %q", screenID, content)
			}
			for _, sectionLine := range []int{setupLine, basicLine} {
				if sectionLine < 3 || strings.TrimSpace(lines[sectionLine-1]) != "" || strings.TrimSpace(lines[sectionLine-2]) != "" || strings.TrimSpace(lines[sectionLine-3]) == "" {
					t.Fatalf("%s section does not have exactly two blank rows above it: %q", screenID, content)
				}
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
	if !strings.Contains(ansi.Strip(content), "**█****") {
		t.Fatalf("password cursor was not rendered in place: %q", ansi.Strip(content))
	}
}

func TestConfigurationFieldsUseLabelValueAndDescriptionRows(t *testing.T) {
	m := testModel()
	m.openScreen(core.Screen{ID: "telegram", Title: "Telegram", Controls: []core.ScreenControl{
		{Key: "token", Label: "bot token", Kind: "password", Value: "secret", Secret: true, Description: "Telegram bot token generated by BotFather in the previous step"},
		{Key: "wizard", Kind: "action", Value: "Wizard", Description: "Configure the connection step by step"},
	}})
	content, _, _ := m.screenContent(20, 64)
	plain := strings.Split(ansi.Strip(content), "\n")
	tokenLine := lineContaining(plain, "Bot Token")
	if tokenLine < 0 || !strings.HasSuffix(plain[tokenLine], "******█") {
		t.Fatalf("label and value are not aligned on one row: %q", ansi.Strip(content))
	}
	if tokenLine+1 >= len(plain) || strings.TrimSpace(plain[tokenLine+1]) != "Telegram bot token generated by BotFather in the previous step" {
		t.Fatalf("description is not directly below its field: %q", ansi.Strip(content))
	}
	buttonLine := lineContaining(plain, "Wizard ↵")
	if buttonLine != tokenLine+3 || strings.TrimSpace(plain[tokenLine+2]) != "" {
		t.Fatalf("settings do not have exactly one blank row between items: %q", ansi.Strip(content))
	}
	if buttonLine < 0 || !strings.Contains(plain[buttonLine], " Wizard ↵ ") {
		t.Fatalf("action button lacks filled-side button content: %q", ansi.Strip(content))
	}
	button := m.styles.elevated.Render(" Wizard ↵ ")
	if !strings.Contains(content, button) {
		t.Fatalf("action button side spaces are not inside its background style: %q", content)
	}
}

func TestConfigurationUsesNamedSectionsWithoutRedundantIntroduction(t *testing.T) {
	m := testModel()
	m.openScreen(core.Screen{ID: "config", Controls: []core.ScreenControl{
		{Key: "harness", Section: "Core settings", Kind: "action", Value: "Coding harness · Codex", Description: "Choose the coding harness"},
		{Key: "model", Kind: "action", Value: "Model · GPT-5", Description: "Choose the model"},
		{Key: "advanced", Section: "Advanced settings", Kind: "disclosure", Value: "Advanced settings", Description: "Show optional controls"},
	}})
	content, _, _ := m.screenContent(30, 80)
	plain := strings.Split(ansi.Strip(content), "\n")
	coreLine := lineContaining(plain, "Core settings ─")
	harnessLine := lineContaining(plain, "Coding harness")
	modelLine := lineContaining(plain, "Model · GPT-5")
	disclosureLine := lineContaining(plain, "Show Advanced Settings ↵")
	if coreLine != 0 || harnessLine != coreLine+2 {
		t.Fatalf("core section layout = %q", ansi.Strip(content))
	}
	if modelLine != harnessLine+3 || strings.TrimSpace(plain[harnessLine+2]) != "" {
		t.Fatalf("core settings spacing = %q", ansi.Strip(content))
	}
	if disclosureLine != modelLine+4 || strings.TrimSpace(plain[disclosureLine-1]) != "" || strings.TrimSpace(plain[disclosureLine-2]) != "" || strings.TrimSpace(plain[disclosureLine-3]) == "" || !strings.Contains(plain[disclosureLine], "↵ ─") || lineContaining(plain, "Advanced settings ─") >= 0 {
		t.Fatalf("advanced section layout = %q", ansi.Strip(content))
	}
	if strings.Contains(ansi.Strip(m.View()), "Spynel configuration") || strings.Contains(ansi.Strip(m.View()), "Harness, context, task management") {
		t.Fatalf("configuration view retained redundant introduction: %q", ansi.Strip(m.View()))
	}
}

func TestConfigurationScreenCanvasIsBorderlessWithOneCellPadding(t *testing.T) {
	m := testModel()
	canvas := ansi.Strip(m.screenCanvas("Title\nBody", 6, 24, 0, 2))
	lines := strings.Split(canvas, "\n")
	if len(lines) != 6 {
		t.Fatalf("screen canvas height = %d, want 6: %q", len(lines), canvas)
	}
	if strings.TrimSpace(lines[0]) != "" || strings.TrimSpace(lines[len(lines)-1]) != "" {
		t.Fatalf("screen canvas lacks top/bottom padding: %q", canvas)
	}
	if !strings.HasPrefix(lines[1], " Title") || !strings.HasPrefix(lines[2], " Body") {
		t.Fatalf("screen canvas lacks left padding: %q", canvas)
	}
	if strings.ContainsAny(canvas, "╭╮╯╰│") {
		t.Fatalf("screen canvas unexpectedly has a border: %q", canvas)
	}
	for _, line := range lines {
		if lipgloss.Width(line) != 24 {
			t.Fatalf("screen canvas row width = %d, want 24: %q", lipgloss.Width(line), line)
		}
	}
}

func TestConfigurationScreenHidesAdvancedControlsUntilDisclosed(t *testing.T) {
	m := testModel()
	var saved map[string]string
	m.saveSettings = func(values map[string]string) error {
		saved = values
		return nil
	}
	m.openScreen(core.Screen{ID: "whatsapp", Controls: []core.ScreenControl{
		{Key: "channels.whatsapp.mode", Label: "mode", Kind: "select", Value: "self-chat", Options: []string{"self-chat", "dedicated"}, Description: "Account behavior"},
		{Key: "advanced", Section: "Advanced settings", Kind: "disclosure", Value: "Advanced settings", Description: "Show optional controls"},
		{Key: "channels.whatsapp.database", Label: "database", Kind: "text", Value: "session.db", Description: "Session storage", Advanced: true},
	}})
	view := ansi.Strip(m.View())
	if strings.Contains(view, "Session storage") || !strings.Contains(view, "Show Advanced") {
		t.Fatalf("collapsed advanced form = %q", view)
	}
	content, _, _ := m.screenContent(30, 100)
	lines := strings.Split(ansi.Strip(content), "\n")
	disclosureLine := lineContaining(lines, "Show Advanced Settings")
	if disclosureLine < 3 || strings.TrimSpace(lines[disclosureLine-1]) != "" || strings.TrimSpace(lines[disclosureLine-2]) != "" || strings.TrimSpace(lines[disclosureLine-3]) == "" || !strings.Contains(lines[disclosureLine], "↵ ─") {
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
	if !m.screenAdvanced || !strings.Contains(view, "Session storage") || !strings.Contains(view, "Hide Advanced") {
		t.Fatalf("expanded advanced form = %q", view)
	}
	content, _, _ = m.screenContent(30, 100)
	lines = strings.Split(ansi.Strip(content), "\n")
	disclosureLine = lineContaining(lines, "Hide Advanced Settings")
	if disclosureLine < 3 || disclosureLine+3 >= len(lines) || strings.TrimSpace(lines[disclosureLine-1]) != "" || strings.TrimSpace(lines[disclosureLine-2]) != "" || strings.TrimSpace(lines[disclosureLine-3]) == "" || strings.TrimSpace(lines[disclosureLine+2]) != "" || !strings.Contains(lines[disclosureLine+3], "Database") || !strings.Contains(lines[disclosureLine], "↵ ─") {
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
	if strings.Contains(m.screenFooterHint(), "⌃S") {
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
			wizardLine := lineContaining(lines, "Setup wizard")
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
		if !strings.Contains(line, "Continue") {
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

func TestWhatsAppFullscreenQRCodeUsesEntireFrameAndAnyKeyReturns(t *testing.T) {
	m := testModel()
	m.openScreen(core.Screen{
		ID: "wizard:whatsapp:pair", Title: "PAIRING CHROME", SaveDisabled: true,
		Controls: []core.ScreenControl{{Key: "show_qr", Kind: "action", Value: "Show QR"}},
	})
	m.openScreen(core.Screen{ID: core.ScreenWhatsAppQR, ParentID: "wizard:whatsapp:pair", Banner: "QR-LINE-ONE\nQR-LINE-TWO", SaveDisabled: true})
	view := ansi.Strip(m.View())
	if !strings.Contains(view, "QR-LINE-ONE") || strings.Contains(view, "PAIRING CHROME") || strings.Contains(view, "Show QR") {
		t.Fatalf("fullscreen QR view contains surrounding chrome: %q", view)
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = next.(model)
	if m.screen == nil || m.screen.ID != "wizard:whatsapp:pair" || len(m.screenStack) != 0 {
		t.Fatalf("any key did not restore pairing wizard: screen %#v stack %d", m.screen, len(m.screenStack))
	}
}

func TestScreenProseWrapsBeforePaddingWithoutLosingBoundaryWords(t *testing.T) {
	m := testModel()
	next, _ := m.Update(tea.WindowSizeMsg{Width: 112, Height: 30})
	m = next.(model)
	markdownSubtitle := "On your primary phone, open **Linked devices → Link a device** (under ⋮ on Android or Settings on iPhone). Open the QR by itself so the terminal can use every available row, or link with a phone-number code instead."
	m.openScreen(core.Screen{ID: "wizard:whatsapp:pair", Title: "WhatsApp setup", Markdown: true, Subtitle: markdownSubtitle, Controls: []core.ScreenControl{{Key: "back", Kind: "action", Value: "Back"}}})
	plain := strings.Join(strings.Fields(ansi.Strip(m.View())), " ")
	if !strings.Contains(plain, "Settings on iPhone). Open the QR by itself") {
		t.Fatalf("Markdown boundary word was truncated instead of wrapped: %q", plain)
	}

	plainSubtitle := "Boundary wrapping preserves every ordinary word when content reaches the right padding instead of truncating it."
	m.openScreen(core.Screen{ID: "plain-screen", Title: "Plain prose", Subtitle: plainSubtitle, Controls: []core.ScreenControl{{Key: "done", Kind: "action", Value: "Done"}}})
	plain = strings.Join(strings.Fields(ansi.Strip(m.View())), " ")
	if !strings.Contains(plain, plainSubtitle) {
		t.Fatalf("plain boundary words were truncated instead of wrapped: %q", plain)
	}
}

func TestWhatsAppPairingEventUpdatesOpenConfigurationScreen(t *testing.T) {
	m := testModel()
	m.openScreen(core.Screen{ID: "whatsapp", Banner: "STALE-QR", Controls: []core.ScreenControl{{Key: "enabled", Kind: "toggle", Value: "on", Options: []string{"on", "off"}}}})
	if m.screen == nil || m.screen.Banner != "" {
		t.Fatalf("general settings retained an initial QR banner: %#v", m.screen)
	}
	next, _ := m.Update(pairingEvent{event: channel.PairingEvent{Name: "whatsapp", State: "code", Rendered: "QR-CODE", Detail: "Scan in Linked devices"}})
	got := next.(model)
	if got.screen == nil || got.screen.Banner != "" || got.screen.Status != "Scan in Linked devices" {
		t.Fatalf("pairing screen = %#v", got.screen)
	}
	view := ansi.Strip(got.View())
	if strings.Contains(view, "QR-CODE") {
		t.Fatalf("QR leaked into configuration screen: %q", view)
	}
}

func TestWhatsAppPairingEventUpdatesWizardScreen(t *testing.T) {
	m := testModel()
	m.openScreen(core.Screen{ID: "wizard:whatsapp:pair", Title: "WhatsApp setup", Markdown: true, SaveDisabled: true, Subtitle: "Scan the code", Controls: []core.ScreenControl{{Key: "done", Kind: "action", Value: "Done"}}})
	next, _ := m.Update(pairingEvent{event: channel.PairingEvent{Name: "whatsapp", State: "code", Rendered: "WIZARD-QR", Detail: "Scan now"}})
	got := next.(model)
	if got.screen == nil || got.screen.Banner != "" || got.screen.Status != "Scan now" {
		t.Fatalf("wizard pairing screen = %#v", got.screen)
	}
}

func TestWhatsAppFullscreenQRCodeRefreshesAndTimeoutReturnsToWizard(t *testing.T) {
	m := testModel()
	m.openScreen(core.Screen{ID: "wizard:whatsapp:pair", Title: "WhatsApp setup", Status: "Ready", Controls: []core.ScreenControl{{Key: "show_qr", Kind: "action", Value: "Show QR"}}})
	m.openScreen(core.Screen{ID: core.ScreenWhatsAppQR, ParentID: "wizard:whatsapp:pair", Banner: "OLD-QR", SaveDisabled: true})

	next, _ := m.Update(pairingEvent{event: channel.PairingEvent{Name: "whatsapp", State: "code", Rendered: "NEW-QR", Detail: "Fresh QR"}})
	m = next.(model)
	if m.screen == nil || m.screen.ID != core.ScreenWhatsAppQR || m.screen.Banner != "NEW-QR" || !strings.Contains(ansi.Strip(m.View()), "NEW-QR") {
		t.Fatalf("refreshed fullscreen QR = %#v", m.screen)
	}

	next, _ = m.Update(pairingEvent{event: channel.PairingEvent{Name: "whatsapp", State: "timeout", Detail: "WhatsApp pairing: timeout"}})
	m = next.(model)
	if m.screen == nil || m.screen.ID != "wizard:whatsapp:pair" || m.screen.Status != "WhatsApp pairing: timeout" || strings.Contains(ansi.Strip(m.View()), "NEW-QR") {
		t.Fatalf("timeout did not restore wizard = %#v", m.screen)
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

func TestContinuingFinalKeepsHarnessActivityForQueuedFollowUp(t *testing.T) {
	m := testModel()
	next, _ := m.Update(uiEvent{event: core.Event{Kind: core.EventDelta, Text: "first answer"}})
	next, _ = next.(model).Update(uiEvent{event: core.Event{Kind: core.EventFinal, Text: "first answer", Done: true, Continues: true}})
	got := next.(model)
	if !got.working || got.status != "Harness working" || len(got.transcript) != 1 || got.transcript[0].text != "first answer" {
		t.Fatalf("continuing final state = working %t, status %q, transcript %#v", got.working, got.status, got.transcript)
	}
	next, _ = got.Update(uiEvent{event: core.Event{Kind: core.EventDelta, Text: "follow-up answer"}})
	next, _ = next.(model).Update(uiEvent{event: core.Event{Kind: core.EventFinal, Text: "follow-up answer", Done: true}})
	got = next.(model)
	if got.working || got.status != "Ready" || len(got.transcript) != 2 || got.transcript[1].text != "follow-up answer" {
		t.Fatalf("queued completion state = working %t, status %q, transcript %#v", got.working, got.status, got.transcript)
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
	logoLine := lineContaining(strings.Split(view, "\n"), "███████ ██████")
	helpLine := lineContaining(strings.Split(view, "\n"), "Type /help to show commands")
	messageLine := lineContaining(strings.Split(view, "\n"), "You hello")
	if logoLine < 0 || helpLine <= logoLine || messageLine <= helpLine || strings.Contains(view, "Old button") {
		t.Fatalf("inline welcome order = %q", view)
	}
	if !strings.Contains(view, "     ████     ████") || !strings.Contains(view, "  ██  ██  ███  ██  ██") {
		t.Fatalf("inline welcome does not use the canonical five-row Spynel logo: %q", view)
	}
	if !m.input.Focused() || m.viewport.YOffset != 0 {
		t.Fatalf("inline welcome stole composer focus or did not start at top: focused=%v offset=%d", m.input.Focused(), m.viewport.YOffset)
	}
}

func TestPersistedWelcomeLogoUsesThemePrimaryStyle(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })
	m := testModel()
	wantFirstRow := m.styles.title.Render(strings.Split(core.SpynelASCII, "\n")[0])
	rendered := m.renderAgentMarkdown(core.SpynelLogoMarkdown + "\n\nWelcome back.")
	if !strings.HasPrefix(rendered, wantFirstRow) || !strings.Contains(ansi.Strip(rendered), "Welcome back.") {
		t.Fatalf("persisted welcome logo did not use the primary title style:\nwant prefix %q\ngot %q", wantFirstRow, rendered)
	}
}

func TestPersistedWelcomeReplacesHistoricalLogoBodyWithCanonicalLogo(t *testing.T) {
	m := testModel()
	legacy := "```spynel-logo\nOLD WELCOME LOGO\n```\n\nWelcome back."
	rendered := ansi.Strip(m.renderAgentMarkdown(legacy))
	if !strings.HasPrefix(rendered, strings.Split(core.SpynelASCII, "\n")[0]) || strings.Contains(rendered, "OLD WELCOME LOGO") || !strings.Contains(rendered, "Welcome back.") {
		t.Fatalf("historical welcome logo was not canonicalized: %q", rendered)
	}
}

func testModel() model {
	input := textarea.New()
	input.Placeholder = composerPlaceholder
	input.Prompt = ""
	activeTheme := theme.Default()
	styles := stylesFor(activeTheme)
	styleComposer(&input, styles)
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
		themes:         []theme.Theme{activeTheme},
		activeTheme:    activeTheme,
		styles:         styles,
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
