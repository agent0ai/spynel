package tui

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func TestTerminalCopySafetyAndQuietClipboard(t *testing.T) {
	text := "  a\t界\n\nlast  "
	unsafe := text + "\x1b[2J\x1b]52;c;aGlqYWNr\a\x00\r\x7f\u009b"
	if got := terminalCopyText(unsafe); got != terminalCopyText(text) {
		t.Fatalf("unsafe terminal content: %q", got)
	}
	if got := terminalCopyText(unsafe); got != "\x1b[H\x1b[J  a\t界\r\n\r\nlast  " {
		t.Fatalf("copy must clear only the visible screen and print only text: %q", got)
	}
	m := semanticFixture()
	m.input.SetValue(text)
	m.input.SelectAll()
	before := m.input.SelectedText()
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyF6})
	if cmd == nil || m.input.SelectedText() != before || !strings.Contains(m.footerHint(), "F6") {
		t.Fatal("F6 lost selection or discoverability")
	}
	var out bytes.Buffer
	t.Setenv("SSH_CONNECTION", "synthetic")
	m.writeClipboard = &out
	m.status = "Ready"
	result := m.copySelection(text)()
	next, _ := m.Update(result)
	m = next.(model)
	if out.String() != clipboardOSC(text) || m.editorNotice != "" || m.status != "Ready" || m.clipboardText != text || !m.clipboardFallback {
		t.Fatal("copy lost transport/internal text or showed routine feedback")
	}
	m.writeClipboard = nil
	next, _ = m.Update(m.copySelection(text)())
	if !strings.Contains(next.(model).editorNotice, "F6") {
		t.Fatal("transport failure lost actionable fallback")
	}
	next, _ = m.Update(terminalCopyResult{errors.New("synthetic failure")})
	if !strings.Contains(next.(model).editorNotice, "synthetic failure") || next.(model).input.SelectedText() != before {
		t.Fatal("copy error lost feedback or selection")
	}
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlZ})
	if cmd != nil || next.(model).input.SelectedText() != before {
		t.Fatal("Ctrl+Z suspended or changed selection")
	}
}

func semanticFixture() model {
	m := testModel()
	next, _ := m.Update(tea.WindowSizeMsg{Width: 42, Height: 20})
	m = next.(model)
	m.transcript = []transcriptEntry{{role: "assistant", text: "Alpha **bold** message wraps across rows.\nNext line.\n\n```go\n    x := 1  \n\n    界e\u0301👩🏽‍💻\n```"}, {role: "user", text: "  genuine indentation\n\nlast  "}, {role: "assistant", text: "Another reply"}}
	m.invalidateHistoryRender()
	m.renderHistory()
	m.viewport.GotoTop()
	return m
}

func TestTerminalOutputPreservesFileWithoutUnlockedWriteMethods(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "terminal")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	out := &terminalOutput{File: file}
	if out.Fd() != file.Fd() {
		t.Fatal("terminal descriptor was lost")
	}
	if _, ok := any(out).(io.StringWriter); ok {
		t.Fatal("io.WriteString can bypass the terminal lock")
	}
	if _, ok := any(out).(io.ReaderFrom); ok {
		t.Fatal("io.Copy can bypass the terminal lock")
	}
	if _, err := io.WriteString(out, clipboardOSC("synthetic")); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(file.Name()); err != nil || string(data) != clipboardOSC("synthetic") {
		t.Fatalf("terminal write failed: %q, %v", data, err)
	}
}

func TestResizeKeepsComposerCaretVisibleAndScrollDetached(t *testing.T) {
	for _, detached := range []bool{false, true} {
		m := semanticFixture()
		m.input.SetValue(strings.Repeat("0123456789 ", 60) + "VISIBLE-TAIL")
		m.resizeComposer()
		if !strings.Contains(ansi.Strip(m.renderInput()), "VISIBLE-TAIL") {
			t.Fatal("setup tail not visible")
		}
		if detached {
			m.input.ScrollBy(-1000)
			m.input.ScrollBy(2)
		}
		for _, size := range []tea.WindowSizeMsg{{Width: 24, Height: 20}, {Width: 30, Height: 12}, {Width: 42, Height: 20}} {
			next, _ := m.Update(size)
			m = next.(model)
			if detached {
				if m.input.ScrollOffset() != 2 {
					t.Fatal("resize undid intentional composer scrolling")
				}
			} else if !strings.Contains(ansi.Strip(m.renderInput()), "VISIBLE-TAIL") {
				t.Fatalf("resize %v hid tail: %q", size, ansi.Strip(m.renderInput()))
			}
		}
	}
}

func updateMouse(m *model, button tea.MouseButton, action tea.MouseAction, x, y int) tea.Cmd {
	next, cmd := m.Update(tea.MouseMsg{Button: button, Action: action, X: x, Y: y})
	*m = next.(model)
	return cmd
}

func TestComposerClickThenRepeatedInsertion(t *testing.T) {
	for _, original := range []string{"", "hello "} {
		for _, pasted := range []bool{false, true} {
			m := semanticFixture()
			m.input.SetValue(original)
			m.resizeComposer()
			b := m.bounds(inputPane)
			updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionPress, b.x+len(original), b.y)
			updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionRelease, b.x+len(original), b.y)
			for i, value := range []string{"界é👩🏽‍💻", "X", "Y"} {
				next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(value), Paste: pasted && i == 0})
				m = next.(model)
			}
			want := original + "界é👩🏽‍💻XY"
			if m.input.Value() != want || m.input.HasSelection() || !strings.Contains(ansi.Strip(m.renderInput()), want) {
				t.Fatalf("click/paste=%t: value=%q selected=%q want=%q", pasted, m.input.Value(), m.input.SelectedText(), want)
			}
		}
	}
}

func TestTranscriptSelectionModesAndRenderedCells(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })
	for _, labels := range []bool{false, true} {
		m := semanticFixture()
		x := 5
		if labels {
			x = 1
		}
		updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionPress, x, 2)
		if m.selection.labels != labels {
			t.Fatal("initial hit region was not latched")
		}
		// Extend semantically across all messages, then reverse the direction.
		last := len(m.output) - 1
		m.selection.caret = textPoint{last, len([]rune(m.output[last].layout.Text))}
		want := strings.Join([]string{m.output[0].layout.Text, m.output[1].layout.Text, m.output[2].layout.Text}, "\n\n")
		got := m.selectedOutput()
		if labels {
			if !strings.HasPrefix(got, "Spy Alpha bold") || !strings.Contains(got, "\n\nYou   genuine indentation") || !strings.HasSuffix(got, "Spy Another reply") {
				t.Fatalf("label copy=%q", got)
			}
		} else if got != want {
			t.Fatalf("content copy=%q want=%q", got, want)
		}
		if strings.ContainsAny(got, "\x1b│┃") || strings.Contains(got, "across\nrows") {
			t.Fatalf("display artifacts in copy: %q", got)
		}
		m.selection.anchor, m.selection.caret = m.selection.caret, m.selection.anchor
		if m.selectedOutput() != got {
			t.Fatal("reverse range changed copy")
		}
		next, _ := m.Update(tea.WindowSizeMsg{Width: 29, Height: 18})
		m = next.(model)
		if m.selectedOutput() != got {
			t.Fatal("resize changed semantic selection")
		}
	}
	m := semanticFixture()
	m.transcript = []transcriptEntry{{role: "assistant", text: "ab `cd` ef"}}
	m.invalidateHistoryRender()
	m.renderHistory()
	updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionPress, 5, 2)
	updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionMotion, 8, 2)
	if got := m.selectedOutput(); got != "ab " {
		t.Fatalf("padding boundary copy=%q", got)
	}
	view := m.selectedHistoryView()
	if !strings.Contains(view, m.styles.selectedCommand.Render("ab ")) {
		t.Fatalf("highlight doesn't match range: %q", view)
	}
	if strings.Contains(view, m.styles.selectedCommand.Render("Spy")) {
		t.Fatal("content mode highlighted label")
	}
	// Scrollbar cannot begin a selection.
	m.clearSelections()
	updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionPress, m.width-1, 2)
	if m.selection.active {
		t.Fatal("scrollbar became source text")
	}
}

func TestSelectionSurvivesStreamingReformatAndFinal(t *testing.T) {
	m := semanticFixture()
	m.transcript = nil
	m.invalidateHistoryRender()
	m.streaming = "A **partial"
	m.working = true
	m.renderHistory()
	updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionPress, 5, 2)
	m.selection.caret = textPoint{0, 11}
	before := m.selectedOutput()
	m.streaming += " bold** reply grows"
	m.streamRendered = renderAgentMarkdownText(m.streaming, m.chatMarkdownWidth(), m.activeTheme)
	m.streamRenderText = m.streaming
	m.streamWidth = m.viewport.Width
	m.streamTheme = m.activeTheme
	m.renderHistory()
	if m.selectedOutput() != before {
		t.Fatalf("stream reinterpretation changed selection: %q -> %q", before, m.selectedOutput())
	}
	m.appendTranscript(transcriptEntry{role: "assistant", text: m.streaming})
	m.streaming = ""
	m.working = false
	m.renderHistory()
	if m.selectedOutput() != before {
		t.Fatal("finalization changed selection")
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(model)
	if m.selection.active || strings.Contains(ansi.Strip(m.viewport.View()), "**") {
		t.Fatal("clearing selection didn't publish deferred formatting")
	}
}

func TestComposerMouseAutoScrollAndFocusRouting(t *testing.T) {
	for _, which := range []pane{inputPane, outputPane} {
		m := semanticFixture()
		m.input.SetValue(strings.Repeat("  row 界e\u0301\n", 32))
		m.resizeComposer()
		m.input.ScrollBy(-100)
		if which == outputPane {
			m.transcript = []transcriptEntry{{role: "user", text: strings.Repeat("  output row\n", 70)}}
			m.invalidateHistoryRender()
			m.renderHistory()
			m.viewport.GotoTop()
		}
		b := m.bounds(which)
		x := b.x + 5
		updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionPress, x, b.y+1)
		cmd := updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionMotion, x, b.y+b.height+2)
		if cmd == nil || !m.drag.ticking {
			t.Fatal("no below-pane timer")
		}
		generation := m.dragGeneration
		for range 20 {
			next, _ := m.Update(dragTick{generation})
			m = next.(model)
		}
		var selected string
		offset := m.viewport.YOffset
		if which == inputPane {
			selected = m.input.SelectedText()
			offset = m.input.ScrollOffset()
		} else {
			selected = m.selectedOutput()
		}
		if offset == 0 || strings.Count(selected, "\n") < b.height {
			t.Fatalf("autoscroll did not extend %v: offset=%d selected=%q", which, offset, selected)
		}
		// Return inside, stale tick, release, and focus loss all fence the timer.
		updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionMotion, x, b.y+1)
		if m.drag.ticking {
			t.Fatal("inside motion retained timer")
		}
		next, _ := m.Update(dragTick{generation})
		m = next.(model)
		updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionMotion, x, b.y-2)
		for range 50 {
			next, _ := m.Update(dragTick{m.dragGeneration})
			m = next.(model)
		}
		if m.drag.ticking {
			t.Fatal("timer runs at upper boundary")
		}
		updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionRelease, x, b.y)
		if m.drag.pane != noPane {
			t.Fatal("release retained drag")
		}
		updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionPress, x, b.y)
		updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionMotion, x, b.y+b.height+2)
		next, _ = m.Update(tea.BlurMsg{})
		m = next.(model)
		if m.drag.pane != noPane || m.drag.ticking {
			t.Fatal("focus loss retained drag")
		}
	}
	m := semanticFixture()
	m.input.SetValue("first\n  second")
	m.resizeComposer()
	b := m.bounds(inputPane)
	updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionPress, b.x, b.y)
	updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionRelease, b.x+3, b.y+1)
	if got := m.input.SelectedText(); got != "first\n  s" {
		t.Fatalf("input hit range=%q", got)
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("X")})
	m = next.(model)
	if m.input.Value() != "Xecond" {
		t.Fatalf("mouse selection replacement=%q", m.input.Value())
	}
}

func TestClipboardSelectionPriorityAndSafeFallback(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "synthetic")
	m := semanticFixture()
	var terminal bytes.Buffer
	m.writeClipboard = &terminal
	m.input.SetValue("one\n  two")
	m.input.SelectAll()
	m.working = true
	before := len(m.transcript)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = next.(model)
	if m.input.Value() != "one\n  two" || len(m.transcript) != before {
		t.Fatal("copy cleared input or dispatched stop")
	}
	next, _ = m.Update(cmd())
	m = next.(model)
	if terminal.String() != clipboardOSC("one\n  two") || !m.clipboardFallback {
		t.Fatal("OSC copy was not encoded or fallback unavailable")
	}
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlX})
	m = next.(model)
	if m.input.Value() != "" {
		t.Fatal("cut did not delete input selection")
	}
	next, _ = m.Update(cmd())
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	m = next.(model)
	if m.input.Value() != "one\n  two" {
		t.Fatal("internal paste didn't restore cut text")
	}
	m.input.SelectAll()
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Paste: true, Runes: []rune("literal [<0;3;4M\r\n  pasted")})
	m = next.(model)
	if m.input.Value() != "literal [<0;3;4M\n  pasted" || len(m.transcript) != before {
		t.Fatal("bracket paste interpreted mouse/newline as actions")
	}
	updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionPress, 1, 2)
	copyText := m.selectedOutput()
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlX})
	m = next.(model)
	if m.selectedOutput() != copyText || len(m.transcript) != before {
		t.Fatal("cut mutated history")
	}
	if strings.Contains(clipboardOSC("\x1b]malicious\a"), "malicious") {
		t.Fatal("raw clipboard control injection")
	}
	m.clearSelections()
	m.input.Reset()
	m.working = true
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = next.(model)
	if cmd == nil || len(m.transcript) == before {
		t.Fatal("no-selection stop behavior changed")
	}
}

// Exercise the model's real async command boundary as well as every clipboard
// ingress. Rejection must happen before preparing or mutating the selection.
func TestPasteIngressPreservesDraftAndTypingPosition(t *testing.T) {
	for _, ingress := range []string{"bracketed", "native", "internal", "queue-full"} {
		for _, reject := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reject=%t", ingress, reject), func(t *testing.T) {
				m := semanticFixture()
				original, pasted := "abcDEFghi\nsentinel", "XY"
				if reject {
					original = strings.Repeat(strings.Repeat("a", 99)+"\n", 655)
					pasted = strings.Repeat("b", 40)
				}
				m.input.CharLimit = 65536
				m.input.SetValue(original)
				m.input.Select(6, 3)
				m.clipboardFallback, m.clipboardText = true, pasted
				var msg tea.Msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(pasted), Paste: true}
				switch ingress {
				case "native":
					msg = clipboardPaste{text: pasted, generation: m.input.HistoryGeneration()}
				case "internal":
					msg = tea.KeyMsg{Type: tea.KeyCtrlV}
				case "queue-full":
					m.pasteQueue = make([]pendingPaste, maxPendingPastes)
				}
				next, cmd := m.Update(msg)
				m = next.(model)
				if reject {
					if m.input.Value() != original || m.input.SelectedText() != "aaa" || m.input.CursorOffset() != 3 || m.editorNotice == "" || cmd != nil {
						t.Fatal("oversized paste changed source/selection or started file work")
					}
					return
				}
				if ingress != "queue-full" && cmd == nil {
					t.Fatal("short paste did not schedule preparation")
				}
				// Typing while file I/O is pending must remain after the paste.
				next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Z")})
				m = next.(model)
				if cmd != nil {
					next, _ = m.Update(cmd())
					m = next.(model)
				}
				if m.input.Value() != "abcXYZghi\nsentinel" || m.input.CursorOffset() != 6 || m.input.HasSelection() {
					t.Fatalf("prepared paste/typing = %q, caret=%d", m.input.Value(), m.input.CursorOffset())
				}
			})
		}
	}
}

func TestPreparedAttachmentsPreserveConcurrentEdits(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(source, []byte("synthetic attachment"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"typing", "selection", "limit", "repeated", "deleted"} {
		t.Run(action, func(t *testing.T) {
			m := semanticFixture()
			m.attachments = filepath.Join(t.TempDir(), "attachments")
			m.input.SetValue("prefix OLD\nsentinel")
			m.input.Select(7, 10)
			cmd := m.enqueuePaste(source)
			if cmd == nil {
				t.Fatal("file preparation was not queued")
			}
			switch action {
			case "typing":
				m.input.InsertString("!")
			case "selection":
				n := len([]rune(m.input.Value()))
				m.input.Select(n, n-8)
			case "limit":
				m.input.CharLimit = len([]rune(m.input.Value()))
			case "repeated":
				m.input.InsertString(" " + source)
			case "deleted":
				m.input.SelectAll()
				m.input.InsertString("new draft")
			}
			before, caret := m.input.Value(), m.input.CursorOffset()
			result := cmd().(pastePreparedMsg)
			if result.err != nil || !result.handled {
				t.Fatalf("actual file preparation failed: %v", result.err)
			}
			if action == "limit" {
				// Preparation can discover a display label longer than its input.
				result.tokens[0].label = strings.Repeat("L", len(source)+1)
			}
			next, _ := m.Update(result)
			m = next.(model)
			if action == "typing" || action == "selection" {
				want := "prefix [Attachment notes.txt]\nsentinel"
				if action == "typing" {
					want = "prefix [Attachment notes.txt]!\nsentinel"
					if m.input.CursorOffset() != len("prefix [Attachment notes.txt]!") {
						t.Fatal("attachment completion moved caret")
					}
				} else if m.input.SelectedText() != "sentinel" {
					t.Fatal("attachment completion moved unrelated selection")
				}
				if m.input.Value() != want || len(m.tokens) != 1 || !strings.Contains(m.expandTokens(m.input.Value()), ".txt>)") {
					t.Fatalf("prepared attachment = %q", m.input.Value())
				}
			} else if m.input.Value() != before || m.input.CursorOffset() != caret || len(m.tokens) != 0 || m.editorNotice == "" {
				t.Fatal("rejected or ambiguous attachment changed current draft")
			}
		})
	}
}

func TestPasteRevealsCaretAtComposerCap(t *testing.T) {
	for _, ingress := range []string{"bracketed", "native", "internal", "queue-full"} {
		for _, pasted := range []string{"\nPASTED-END", strings.Repeat("x", 60) + "PASTED-END"} {
			t.Run(fmt.Sprintf("%s/multiline=%t", ingress, strings.Contains(pasted, "\n")), func(t *testing.T) {
				m := semanticFixture()
				m.input.SetValue(strings.Repeat("row\n", 20) + "tail")
				m.resizeComposer()
				before := m.input.ScrollOffset()
				var msg tea.Msg = tea.KeyMsg{Type: tea.KeyRunes, Paste: true, Runes: []rune(pasted)}
				switch ingress {
				case "native":
					msg = clipboardPaste{text: pasted, generation: m.input.HistoryGeneration()}
				case "internal":
					m.clipboardFallback, m.clipboardText = true, pasted
					msg = tea.KeyMsg{Type: tea.KeyCtrlV}
				case "queue-full":
					m.pasteQueue = make([]pendingPaste, maxPendingPastes)
				}
				next, cmd := m.Update(msg)
				m = next.(model)
				if m.input.ScrollOffset() <= before || !strings.Contains(ansi.Strip(m.renderInput()), "PASTED-END") {
					t.Fatalf("paste hidden below composer: scroll %d -> %d", before, m.input.ScrollOffset())
				}
				// Wheel navigation during ordinary paste preparation stays put.
				b := m.bounds(inputPane)
				updateMouse(&m, tea.MouseButtonWheelUp, tea.MouseActionPress, b.x, b.y)
				offset := m.input.ScrollOffset()
				if cmd != nil {
					next, _ = m.Update(cmd())
					m = next.(model)
				}
				next, _ = m.Update(tea.FocusMsg{})
				m = next.(model)
				_ = m.View()
				if m.input.ScrollOffset() != offset {
					t.Fatal("non-edit event undid wheel scrolling")
				}
			})
		}
	}
}

func TestPreparedAttachmentKeepsVisibleCaret(t *testing.T) {
	m := semanticFixture()
	m.input.SetValue(strings.Repeat("row\n", 20) + "tail")
	m.resizeComposer()
	next, _ := m.Update(pastePreparedMsg{paste: pendingPaste{value: "tail", generation: m.input.HistoryGeneration()}, handled: true, tokens: []composerToken{{label: "[Attachment tail]", expansion: "synthetic"}}})
	m = next.(model)
	if !strings.Contains(ansi.Strip(m.renderInput()), "[Attachment tail]") {
		t.Fatal("prepared attachment hid the current caret")
	}
}

func TestAttachmentDeletionPreservesVisibleEditingPosition(t *testing.T) {
	m := semanticFixture()
	label := "[Attachment tail]"
	m.tokens = []composerToken{{label: label, expansion: "synthetic"}}
	m.input.SetValue("START " + label + "\n" + strings.Repeat("row\n", 20) + "sentinel")
	m.resizeComposer()
	m.input.Select(len("START "+label), len("START "+label))
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = next.(model)
	if m.input.Value() != "START \n"+strings.Repeat("row\n", 20)+"sentinel" || m.input.CursorOffset() != len("START ") || m.input.ScrollOffset() != 0 || !strings.Contains(ansi.Strip(m.renderInput()), "START") {
		t.Fatal("atomic token deletion lost the visible editing position")
	}
}

func TestTranscriptCodeBoundaryWhitespaceCopiesInBothModes(t *testing.T) {
	for _, labels := range []bool{false, true} {
		m := semanticFixture()
		m.transcript = []transcriptEntry{{role: "assistant", text: "```text\n\n  hello\n\n```"}, {role: "user", text: "next"}}
		m.invalidateHistoryRender()
		m.renderHistory()
		m.selection = outputSelection{active: true, labels: labels, anchor: textPoint{1, 4}, caret: textPoint{0, 0}}
		want := "\n  hello\n\n\nnext"
		if labels {
			want = "Spy \n      hello\n    \n\nYou next"
		}
		if got := m.selectedOutput(); got != want {
			t.Fatalf("labels=%t copied=%q want=%q", labels, got, want)
		}
		if len(m.output[0].layout.Rows) != 3 {
			t.Fatal("source boundary blank rows are absent from rendered layout")
		}
	}
}

func clickAt(m *model, x, y int) {
	updateMouse(m, tea.MouseButtonLeft, tea.MouseActionPress, x, y)
	updateMouse(m, tea.MouseButtonLeft, tea.MouseActionRelease, x, y)
}

func TestMultiClickLogicalSelection(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "synthetic")
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })
	line := "  alpha,beta " + strings.Repeat("界é👩🏽‍💻", 9) + " tail  "
	for _, which := range []pane{outputPane, inputPane} {
		t.Run(fmt.Sprint(which), func(t *testing.T) {
			m := semanticFixture()
			m.transcript = []transcriptEntry{{role: "user", text: line + "\nsentinel"}, {role: "user", text: "other message"}}
			m.input.SetValue(line + "\nsentinel")
			m.resizeComposer()
			m.invalidateHistoryRender()
			m.renderHistory()
			m.viewport.GotoTop()
			m.input.ScrollBy(-1000)
			clock := time.Unix(100, 0)
			m.now = func() time.Time { return clock }
			b := m.bounds(which)
			x := b.x + 2
			if which == outputPane {
				x = 7 // two authored spaces after the label/gutter
			}
			selected := func() string {
				if which == outputPane {
					return m.selectedOutput()
				}
				return m.input.SelectedText()
			}
			clickAt(&m, x, b.y)
			if selected() != "" {
				t.Fatal("single click selected text")
			}
			clock = clock.Add(100 * time.Millisecond)
			clickAt(&m, x+1, b.y)
			if selected() != "alpha,beta" || m.drag.pane != noPane {
				t.Fatalf("word/release: %q", selected())
			}
			view := m.selectedHistoryView()
			style := m.styles.selectedCommand
			if which == inputPane {
				view, style = m.input.View(), m.input.SelectionStyle
			}
			if !strings.Contains(view, style.Render("alpha,beta")) {
				t.Fatal("word highlight lost its range")
			}
			clock = clock.Add(100 * time.Millisecond)
			clickAt(&m, x, b.y)
			if selected() != line || m.drag.pane != noPane {
				t.Fatalf("logical line/release: %q", selected())
			}
			var out bytes.Buffer
			m.writeClipboard = &out
			handled, cmd := m.handleSelectionKey(tea.KeyMsg{Type: tea.KeyCtrlC})
			if !handled || cmd == nil {
				t.Fatal("no copy command")
			}
			cmd()
			if out.String() != clipboardOSC(line) || m.clipboardText != line {
				t.Fatal("copy introduced labels, soft wraps or another line/message")
			}
			m.clearSelections()
			m.input.ScrollBy(-1000)
			x = b.x
			if which == outputPane {
				x = 5
			}
			clickAt(&m, x+1, b.y+1)
			clickAt(&m, x+1, b.y+1)
			if selected() != strings.Repeat("界é👩🏽‍💻", 9) {
				t.Fatalf("wrapped word from continuation row: %q", selected())
			}
			clickAt(&m, x+1, b.y+1)
			if selected() != line {
				t.Fatalf("logical line from continuation row: %q", selected())
			}
			if which == inputPane {
				next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("replacement"), Paste: true})
				m = next.(model)
				if m.input.Value() != "replacement\nsentinel" || m.input.HasSelection() {
					t.Fatalf("line replacement: %q", m.input.Value())
				}
			}
		})
	}
}

func TestMultiClickResets(t *testing.T) {
	for _, which := range []pane{outputPane, inputPane} {
		for _, reset := range []string{"delayed", "distant", "row", "cross-pane", "wheel", "drag", "release-moved", "key", "focus", "resize", "shift", "unreleased", "fourth"} {
			t.Run(fmt.Sprintf("%d/%s", which, reset), func(t *testing.T) {
				m := semanticFixture()
				m.input.SetValue("alpha beta\nnext line")
				m.resizeComposer()
				clock := time.Unix(100, 0)
				m.now = func() time.Time { return clock }
				b := m.bounds(which)
				x, y := b.x, b.y
				if which == outputPane {
					x = 5
				}
				clickAt(&m, x, y)
				clock = clock.Add(100 * time.Millisecond)
				switch reset {
				case "delayed":
					clock = clock.Add(501 * time.Millisecond)
				case "distant":
					x += 3
				case "row":
					y++
				case "cross-pane":
					other := inputPane
					if which == inputPane {
						other = outputPane
					}
					b = m.bounds(other)
					otherX := b.x
					if other == outputPane {
						otherX = 5
					}
					clickAt(&m, otherX, b.y)
				case "wheel":
					updateMouse(&m, tea.MouseButtonWheelUp, tea.MouseActionPress, x, y)
				case "drag", "release-moved":
					updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionPress, x, y)
					if reset == "drag" {
						updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionMotion, x+1, y)
					}
					updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionRelease, x+1, y)
				case "key":
					next, _ := m.Update(tea.KeyMsg{Type: tea.KeyLeft})
					m = next.(model)
				case "focus":
					next, _ := m.Update(tea.BlurMsg{})
					m = next.(model)
				case "resize":
					next, _ := m.Update(tea.WindowSizeMsg{Width: 43, Height: 20})
					m = next.(model)
				case "shift":
					m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: x, Y: y, Shift: true})
					updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionRelease, x, y)
				case "unreleased":
					updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionPress, x, y)
				case "fourth":
					clickAt(&m, x, y)
					clickAt(&m, x, y)
				}
				clickAt(&m, x, y)
				if m.clicks.count > 1 || m.input.HasSelection() || m.selectedOutput() != "" {
					t.Fatalf("reset failed: count=%d input=%q output=%q", m.clicks.count, m.input.SelectedText(), m.selectedOutput())
				}
			})
		}
	}
}

func TestMultiClickContentCells(t *testing.T) {
	for _, source := range []string{"界", "é", "👩🏽‍💻", "alpha,beta", "  \t "} {
		for _, which := range []pane{outputPane, inputPane} {
			for half := 0; half < ansi.StringWidth(strings.ReplaceAll(source, "\t", "    ")); half++ {
				m := semanticFixture()
				m.transcript = []transcriptEntry{{role: "user", text: source + "\nsentinel"}}
				m.input.SetValue(source + "\nsentinel")
				m.resizeComposer()
				m.invalidateHistoryRender()
				m.renderHistory()
				m.viewport.GotoTop()
				b := m.bounds(which)
				x := b.x + half
				if which == outputPane {
					x = 5 + half
				}
				clickAt(&m, x, b.y)
				clickAt(&m, x, b.y)
				got := m.input.SelectedText()
				if which == outputPane {
					got = m.selectedOutput()
				}
				if got != source {
					t.Fatalf("pane=%d cell=%d source=%q got=%q", which, half, source, got)
				}
			}
		}
	}
	m := semanticFixture()
	m.transcript = []transcriptEntry{{role: "assistant", text: "a `cd`"}, {role: "user", text: "next"}}
	m.input.SetValue("x")
	m.resizeComposer()
	m.invalidateHistoryRender()
	m.renderHistory()
	m.viewport.GotoTop()
	for _, cell := range m.output[0].layout.Rows[0].Cells {
		if cell.Start == cell.End {
			for range 3 {
				clickAt(&m, 5+cell.X, 2)
			}
			if m.selectedOutput() != "" || m.clicks.count != 0 {
				t.Fatal("synthetic inline padding selected text")
			}
		}
	}
	for _, pos := range [][2]int{{m.width - 1, 2}, {m.width - 3, 2}, {5, 3}, {5, 10}, {m.bounds(inputPane).x + 1, m.bounds(inputPane).y}} {
		for range 3 {
			clickAt(&m, pos[0], pos[1])
		}
		if m.selectedOutput() != "" || m.input.HasSelection() || m.clicks.count != 0 {
			t.Fatalf("non-content hit selected text at %v", pos)
		}
	}
	for range 3 {
		clickAt(&m, 1, 2)
	}
	if !m.selection.labels || m.selectedOutput() != "Spy a cd" {
		t.Fatalf("label selection changed: %q", m.selectedOutput())
	}
}

// Resolve test coordinates from the real cell maps, including wrapped graphemes.
func unitDragPosition(m model, which pane, offset int) (int, int) {
	b := m.bounds(which)
	if which == outputPane {
		for y, row := range m.output[0].layout.Rows {
			for _, c := range row.Cells {
				if c.Start <= offset && offset < c.End {
					return 5 + c.X, b.y + y - m.viewport.YOffset
				}
			}
		}
	} else {
		for y, row := range m.input.VisualRows() {
			if row.Start <= offset && offset < row.End {
				return b.x + ansi.StringWidth(string([]rune(row.Text)[:offset-row.Start])), b.y + y - m.input.ScrollOffset()
			}
		}
	}
	panic("test offset has no character cell")
}

func TestUnitDragReversalCopyAndReplacement(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "synthetic")
	word := strings.Repeat("界é👩🏽‍💻", 9)
	line := "left " + word + " right"
	text := "before\n" + line + "\nlast\nsentinel"
	wordStart, wordEnd := len([]rune("before\nleft ")), len([]rune("before\nleft "+word))
	lineStart, lineEnd := len([]rune("before\n")), len([]rune("before\n"+line))
	for _, which := range []pane{outputPane, inputPane} {
		for _, clicks := range []int{2, 3} {
			t.Run(fmt.Sprintf("%d/%d", which, clicks), func(t *testing.T) {
				m := semanticFixture()
				if which == outputPane {
					m.transcript = []transcriptEntry{{role: "user", text: text}}
					m.invalidateHistoryRender()
					m.renderHistory()
					m.viewport.GotoTop()
				} else {
					m.input.SetValue(text)
					m.resizeComposer()
					m.input.ScrollBy(-1000)
				}
				m.now = func() time.Time { return time.Unix(100, 0) }
				x, y := unitDragPosition(m, which, wordStart)
				for range clicks - 1 {
					clickAt(&m, x+1, y)
				}
				updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionPress, x+1, y)
				start, end := wordStart, wordEnd
				if clicks == 3 {
					start, end = lineStart, lineEnd
				}
				for _, target := range []struct{ at, start, end int }{
					{lineEnd + 2, start, lineEnd + 5}, // extend to last, excluding its newline
					{1, 0, end},                       // reverse across the complete original unit
					{wordStart + 7, start, end},       // shrink back inside the original unit
					{wordEnd + 2, start, lineEnd},
					{lineStart + 1, lineStart, end},
				} {
					x, y = unitDragPosition(m, which, target.at)
					updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionMotion, x, y)
					got := m.input.SelectedText()
					if which == outputPane {
						got = m.selectedOutput()
					}
					want := string([]rune(text)[target.start:target.end])
					if got != want {
						t.Fatalf("target %d: got %q want %q", target.at, got, want)
					}
				}
				// A moved release must use the same unit extension boundary.
				x, y = unitDragPosition(m, which, lineEnd+2)
				updateMouse(&m, tea.MouseButtonNone, tea.MouseActionRelease, x, y)
				want := string([]rune(text)[start : lineEnd+5])
				var out bytes.Buffer
				m.writeClipboard = &out
				_, cmd := m.handleSelectionKey(tea.KeyMsg{Type: tea.KeyCtrlC})
				cmd()
				if out.String() != clipboardOSC(want) || m.drag.pane != noPane {
					t.Fatal("released range/copy changed")
				}
				if which == inputPane {
					next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("X"), Paste: true})
					want = string([]rune(text)[:start]) + "X\nsentinel"
					if next.(model).input.Value() != want {
						t.Fatalf("replacement: %q", next.(model).input.Value())
					}
				}
			})
		}
	}
}

func TestUnitDragAutoscroll(t *testing.T) {
	text := strings.Repeat("word tail\n", 30) + "last end"
	for _, which := range []pane{outputPane, inputPane} {
		for _, clicks := range []int{2, 3} {
			m := semanticFixture()
			if which == outputPane {
				m.transcript = []transcriptEntry{{role: "user", text: text}}
				m.invalidateHistoryRender()
				m.renderHistory()
				m.viewport.GotoTop()
			} else {
				m.input.SetValue(text)
				m.resizeComposer()
				m.input.ScrollBy(-1000)
			}
			m.now = func() time.Time { return time.Unix(100, 0) }
			b := m.bounds(which)
			x, y := unitDragPosition(m, which, 11)
			for range clicks - 1 {
				clickAt(&m, x, y)
			}
			updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionPress, x, y)
			updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionMotion, x, b.y+b.height+2)
			for range 20 {
				m.autoScroll(dragTick{m.dragGeneration})
			}
			got := m.input.SelectedText()
			if which == outputPane {
				got = m.selectedOutput()
			}
			want := strings.TrimSuffix(text[len("word tail\n"):], " end")
			if clicks == 3 {
				want += " end"
			}
			if got != want {
				t.Fatalf("pane=%d clicks=%d bottom=%q", which, clicks, got)
			}
			updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionMotion, x, b.y-3)
			for range 20 {
				m.autoScroll(dragTick{m.dragGeneration})
			}
			updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionRelease, x, b.y-3)
			got = m.input.SelectedText()
			if which == outputPane {
				got = m.selectedOutput()
			}
			want = "word tail\nword"
			if clicks == 3 {
				want += " tail"
			}
			if got != want || m.drag.pane != noPane {
				t.Fatalf("pane=%d clicks=%d reverse=%q", which, clicks, got)
			}
		}
	}
}

func TestUnitDragAcrossMessages(t *testing.T) {
	for _, count := range []int{2, 3} {
		m := semanticFixture()
		m.transcript = []transcriptEntry{{role: "user", text: "alpha beta"}, {role: "user", text: "middle word"}, {role: "user", text: "last ending"}}
		m.invalidateHistoryRender()
		m.renderHistory()
		m.viewport.GotoTop()
		for range count - 1 {
			clickAt(&m, 6, 4)
		}
		updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionPress, 6, 4)
		updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionMotion, 7, 6)
		want := "middle word\n\nlast"
		if count == 3 {
			want += " ending"
		}
		if got := m.selectedOutput(); got != want {
			t.Fatalf("forward unit messages: %q", got)
		}
		updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionMotion, 6, 2)
		updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionRelease, 6, 2)
		want = "alpha beta\n\nmiddle"
		if count == 3 {
			want += " word"
		}
		if got := m.selectedOutput(); got != want || m.selection.labels {
			t.Fatalf("reverse unit messages: %q", got)
		}
	}
}
