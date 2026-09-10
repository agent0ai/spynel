package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent0ai/spynel/internal/core"
	tea "github.com/charmbracelet/bubbletea"
)

func undoKey(t *testing.T, m *model, key tea.KeyType) {
	t.Helper()
	status := m.status
	next, cmd := m.Update(tea.KeyMsg{Type: key})
	*m = next.(model)
	if cmd != nil || m.editorNotice != "" || m.status != status {
		t.Fatalf("undo/redo emitted a command or routine notice: %q / %q", m.status, m.editorNotice)
	}
}

func TestComposerUndoKeyRoute(t *testing.T) {
	m := semanticFixture()
	m.input.SetValue("prefix OLD\n\t界é👩🏽‍💻")
	m.input.Select(10, 7)
	before, caret := m.input.Value(), m.input.CursorOffset()
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("new\n  text"), Paste: true})
	m = next.(model)
	replacement := m.input.Value()
	undoKey(t, &m, tea.KeyCtrlZ)
	if m.input.Value() != before || m.input.SelectedText() != "OLD" || m.input.CursorOffset() != caret {
		t.Fatal("key route did not undo atomic multiline selection paste")
	}
	undoKey(t, &m, tea.KeyCtrlY)
	if m.input.Value() != replacement {
		t.Fatal("key route did not redo paste")
	}
	m.input.Select(0, 6)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlX}) // do not execute external clipboard work
	m = next.(model)
	undoKey(t, &m, tea.KeyCtrlZ)
	if m.input.Value() != replacement || m.input.SelectedText() != "prefix" {
		t.Fatal("cut was not reversible")
	}
	m.outputFocus = true
	m.selection = outputSelection{active: true, anchor: textPoint{0, 0}, caret: textPoint{0, 5}}
	selected, transcript := m.selectedOutput(), len(m.transcript)
	undoKey(t, &m, tea.KeyCtrlZ)
	undoKey(t, &m, tea.KeyCtrlY)
	if m.input.Value() != replacement || m.selectedOutput() != selected || !m.outputFocus || len(m.transcript) != transcript {
		t.Fatal("output-focused history keys edited a pane")
	}
	m.outputFocus = false
	m.screen = &core.Screen{ID: "config"}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlZ})
	m = next.(model)
	if m.input.Value() != replacement {
		t.Fatal("modal key escaped into composer")
	}
}

func TestComposerUndoDraftBoundaries(t *testing.T) {
	for _, boundary := range []string{"submit", "clear", "new", "resume", "command"} {
		t.Run(boundary, func(t *testing.T) {
			m := semanticFixture()
			m.input.InsertString("old draft")
			late := clipboardPaste{text: "late clipboard", generation: m.input.HistoryGeneration()}
			prepared := pastePreparedMsg{paste: pendingPaste{value: "old draft", generation: m.input.HistoryGeneration()}, handled: true, tokens: []composerToken{{label: "[Attachment old]", expansion: "synthetic"}}}
			switch boundary {
			case "submit":
				m.dispatchMessage(m.input.Value(), m.input.Value()) // inspect admission without running a handler
			case "clear":
				next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
				m = next.(model)
			case "new":
				m.openScreen(core.Screen{ID: "welcome", Conversation: "new"})
			case "resume":
				m.openScreen(core.Screen{ID: "chat", Conversation: "resumed"})
			case "command":
				m.input.SetValue("/")
				m.syncCommandMenu()
				m.commandIndex = 1
				want := m.commandMatches()[1].Value
				m.handleCommandMenuKey(tea.KeyMsg{Type: tea.KeyTab})
				if m.input.Value() != want {
					t.Fatal("draft replacement changed command choice")
				}
			}
			before, transcript := m.input.Value(), len(m.transcript)
			undoKey(t, &m, tea.KeyCtrlZ)
			undoKey(t, &m, tea.KeyCtrlY)
			next, _ := m.Update(late)
			m = next.(model)
			if m.input.Value() != before || len(m.tokens) != 0 || len(m.transcript) != transcript || m.editorNotice != "" {
				t.Fatal("undo or delayed clipboard crossed draft boundary")
			}
			m.input.SetValue("old draft") // identical text in a fresh draft is still unrelated
			next, _ = m.Update(prepared)
			m = next.(model)
			if m.input.Value() != "old draft" || len(m.tokens) != 0 || m.editorNotice != "" {
				t.Fatal("late attachment completion matched an unrelated replacement draft")
			}
		})
	}
}

func TestComposerUndoFencesPasteCompletion(t *testing.T) {
	m := semanticFixture()
	m.input.SetValue("base ")
	prepared := 0
	m.preparePaste = func(context.Context, string, string) ([]composerToken, bool, error) {
		prepared++
		return []composerToken{{label: "[Attachment file]", expansion: "prepared file"}}, true, nil
	}
	cmd := m.enqueuePaste("file")
	result := cmd() // delay delivery even though file preparation already completed
	late := clipboardPaste{text: "late", generation: m.input.HistoryGeneration()}
	m.enqueuePaste("queued")
	undoKey(t, &m, tea.KeyCtrlZ)
	undoKey(t, &m, tea.KeyCtrlZ)
	undoKey(t, &m, tea.KeyCtrlY)
	if m.input.Value() != "base file" || len(m.pasteQueue) != 0 {
		t.Fatal("undo did not cancel queued preparation or redo literal text")
	}
	for _, msg := range []tea.Msg{result, late} {
		next, _ := m.Update(msg)
		m = next.(model)
	}
	if m.input.Value() != "base file" || len(m.tokens) != 0 || m.pasteBusy || m.editorNotice != "" || prepared != 1 {
		t.Fatal("late completion changed redone text or repeated preparation")
	}
	// A new accepted paste can proceed after the cancelled worker settles.
	cmd = m.enqueuePaste(" fresh")
	next, _ := m.Update(cmd())
	m = next.(model)
	if !strings.Contains(m.input.Value(), "[Attachment file]") || prepared != 2 {
		t.Fatal("cancellation blocked subsequent preparation")
	}
}

func TestComposerAttachmentUndoRetainsMetadataWithoutIO(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(source, []byte("synthetic attachment"), 0600); err != nil {
		t.Fatal(err)
	}
	m := semanticFixture()
	m.attachments = filepath.Join(root, "attachments")
	cmd := m.enqueuePaste(source)
	next, _ := m.Update(cmd())
	m = next.(model)
	label, expanded := m.input.Value(), m.expandTokens(m.input.Value())
	if len(m.tokens) != 1 || label == source || expanded == label {
		t.Fatal("attachment not prepared")
	}
	// Conversion is its own range replacement; undoing it restores the literal
	// path, and redo uses the existing metadata rather than copying again.
	undoKey(t, &m, tea.KeyCtrlZ)
	if m.input.Value() != source {
		t.Fatal("conversion did not undo to literal path")
	}
	undoKey(t, &m, tea.KeyCtrlY)
	if m.input.Value() != label || m.expandTokens(label) != expanded {
		t.Fatal("redone attachment lost expansion")
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = next.(model)
	if m.input.Value() != "" {
		t.Fatal("atomic attachment deletion failed")
	}
	undoKey(t, &m, tea.KeyCtrlZ)
	if m.input.Value() != label || m.expandTokens(label) != expanded {
		t.Fatal("undo deletion lost metadata")
	}
	undoKey(t, &m, tea.KeyCtrlZ) // conversion
	undoKey(t, &m, tea.KeyCtrlZ) // paste
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("new")})
	m = next.(model)
	if len(m.tokens) != 0 || m.input.Redo() {
		t.Fatal("discarded redo branch retained attachment metadata")
	}
	files, err := os.ReadDir(m.attachments)
	if err != nil || len(files) != 1 {
		t.Fatal("undo/redo repeated or removed prepared file I/O")
	}
	if data, err := os.ReadFile(source); err != nil || string(data) != "synthetic attachment" {
		t.Fatal("undo/redo changed the user's original file")
	}
}

func TestComposerDeletionGroupingRoute(t *testing.T) {
	for _, k := range []tea.KeyMsg{
		{Type: tea.KeyBackspace}, {Type: tea.KeyDelete}, {Type: tea.KeyCtrlW},
		{Type: tea.KeyRunes, Alt: true, Runes: []rune("d")},
	} {
		t.Run(k.String(), func(t *testing.T) {
			m := semanticFixture()
			m.input.SetValue("one two three four")
			if k.Type == tea.KeyDelete || k.Alt {
				m.input.CursorStart()
			}
			for range 3 {
				next, _ := m.Update(k)
				m = next.(model)
			}
			after := m.input.Value()
			undoKey(t, &m, tea.KeyCtrlZ)
			if m.input.Value() != "one two three four" {
				t.Fatalf("composer split a deletion run: %q", m.input.Value())
			}
			undoKey(t, &m, tea.KeyCtrlY)
			if m.input.Value() != after {
				t.Fatal("redo split a deletion run")
			}
		})
	}
}

func TestComposerReplacedAttachmentMetadata(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "notes.txt")
	m := semanticFixture()
	m.attachments = filepath.Join(root, "attachments")
	pasteVersion := func(body string) (string, string) {
		t.Helper()
		if err := os.WriteFile(source, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := m.enqueuePaste(source)
		result := cmd().(pastePreparedMsg)
		want := result.tokens[0].expansion
		next, _ := m.Update(result)
		m = next.(model)
		if got := m.expandTokens(m.input.Value()); got != want {
			t.Fatalf("attachment expansion = %q, want %q", got, want)
		}
		return m.input.Value(), want
	}
	first, firstExpansion := pasteVersion("first version")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = next.(model)
	if m.input.Value() != "" {
		t.Fatal("fixture did not delete the first attachment")
	}
	second, secondExpansion := pasteVersion("second version")
	if first == second || firstExpansion == secondExpansion {
		t.Fatal("separate attachment copies share metadata identity")
	}
	for range 3 { // conversion -> literal paste -> deletion -> first token
		undoKey(t, &m, tea.KeyCtrlZ)
	}
	if m.input.Value() != first || m.expandTokens(first) != firstExpansion {
		t.Fatal("undo restored the wrong attachment version")
	}
	for range 3 {
		undoKey(t, &m, tea.KeyCtrlY)
	}
	if m.input.Value() != second || m.expandTokens(second) != secondExpansion {
		t.Fatal("redo restored the wrong attachment version")
	}
	var sent string
	m.handler = func(_ context.Context, msg core.Message, _ core.Emit) error {
		sent = msg.Text
		return nil
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	runCommandAt(cmd, 1) // Enter's repaint precedes the dispatch command.
	if sent != secondExpansion || m.input.Value() != "" || len(m.tokens) != 0 {
		t.Fatalf("dispatch did not expand and reset the current attachment: %q", sent)
	}
	for i, name := range []string{"notes.txt", "notes-2.txt"} {
		data, err := os.ReadFile(filepath.Join(m.attachments, name))
		if err != nil || string(data) != []string{"first version", "second version"}[i] {
			t.Fatalf("undo/redo changed prepared file %s", name)
		}
	}
}

func TestComposerUndoMouseAndResize(t *testing.T) {
	m := semanticFixture()
	for _, r := range "hello" {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(model)
	}
	b := m.bounds(inputPane)
	updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionPress, b.x+2, b.y)
	updateMouse(&m, tea.MouseButtonLeft, tea.MouseActionRelease, b.x+2, b.y)
	next, _ := m.Update(tea.WindowSizeMsg{Width: m.width + 3, Height: m.height + 1})
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("X")})
	m = next.(model)
	undoKey(t, &m, tea.KeyCtrlZ)
	if m.input.Value() != "hello" || m.input.CursorOffset() != 2 {
		t.Fatal("mouse relocation failed to isolate the next edit")
	}
	undoKey(t, &m, tea.KeyCtrlZ)
	if m.input.Value() != "" {
		t.Fatal("mouse or resize created a fake edit")
	}
}

func TestComposerAttachmentLabelsWithinBatch(t *testing.T) {
	m := semanticFixture()
	m.input.SetValue("files")
	m.applyPreparedPaste(pastePreparedMsg{
		paste: pendingPaste{value: "files", generation: m.input.HistoryGeneration()}, handled: true,
		tokens: []composerToken{
			{label: "[Attachment notes]", expansion: "[Attachment notes](<first>)"},
			{label: "[Attachment notes]", expansion: "[Attachment notes](<second>)"},
			{label: "[Attachment notes (2)]", expansion: "[Attachment notes (2)](<third>)"},
		},
	})
	want := "[Attachment notes](<first>) [Attachment notes](<second>) [Attachment notes (2)](<third>)"
	if got := m.expandTokens(m.input.Value()); got != want {
		t.Fatalf("same-batch labels or generated Markdown were expanded incorrectly: %q", got)
	}
	undoKey(t, &m, tea.KeyCtrlZ)
	undoKey(t, &m, tea.KeyCtrlY)
	if m.expandTokens(m.input.Value()) != want {
		t.Fatal("redo changed same-batch attachment identities")
	}
}
