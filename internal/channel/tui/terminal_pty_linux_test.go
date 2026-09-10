//go:build linux

package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"golang.org/x/sys/unix"
)

type ptySnapshot struct {
	Input, Selected, View     string
	OutputFocus               bool
	InputBounds, OutputBounds [4]int
	InputScroll, OutputScroll int
	Dragging                  bool
	TerminalSize              [2]int
}
type ptyFixtureModel struct {
	model
	snapshotFile string
}

func (m ptyFixtureModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		if k.Type == tea.KeyCtrlQ {
			return m, tea.Quit
		}
		if k.Type == tea.KeyCtrlG {
			selected := m.input.SelectedText()
			if m.outputFocus {
				selected = m.selectedOutput()
			}
			in, out := m.bounds(inputPane), m.bounds(outputPane)
			state := ptySnapshot{m.input.Value(), selected, m.View(), m.outputFocus, [4]int{in.x, in.y, in.width, in.height}, [4]int{out.x, out.y, out.width, out.height}, m.input.ScrollOffset(), m.viewport.YOffset, m.drag.pane != noPane, [2]int{m.width, m.height}}
			data, _ := json.Marshal(state)
			f, err := os.OpenFile(m.snapshotFile, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
			if err != nil {
				panic(err)
			}
			_, _ = f.Write(append(data, '\n'))
			_ = f.Close()
			return m, nil
		}
	}
	next, cmd := m.model.Update(msg)
	m.model = next.(model)
	return m, cmd
}

// This subprocess runs the real Tea decoder, raw PTY, model, and renderer,
// with synthetic content only. No application owner, harness, history, or
// desktop clipboard is opened. Parent snapshots are private test artifacts.
func TestSemanticPTYFixture(t *testing.T) {
	path := os.Getenv("SPYNEL_SEMANTIC_PTY_FIXTURE")
	if path == "" {
		t.Skip("PTY subprocess only")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()
	lipgloss.SetColorProfile(termenv.TrueColor)
	m := testModel()
	m.width, m.height, m.inputWidth = 0, 0, 0 // Startup must learn real PTY geometry.
	m.ctx = ctx
	m.transcript = []transcriptEntry{{role: "assistant", text: strings.Repeat("Synthetic output row with 界 and e\u0301.\n", 30)}}
	m.invalidateHistoryRender()
	m.renderHistory()
	m.viewport.GotoTop()
	out := &terminalOutput{File: os.Stdout}
	m.writeClipboard = out
	p := tea.NewProgram(ptyFixtureModel{m, path}, tea.WithInput(&terminalInput{file: os.Stdin}), tea.WithOutput(out), tea.WithAltScreen(), tea.WithContext(ctx), tea.WithFilter(filterTerminalEvents))
	if _, err := p.Run(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// Exercise restored canonical input/echo after Tea has returned, and leave
	// genuine post-shutdown stderr visible. Late messages must not render.
	p.Send(tea.WindowSizeMsg{Width: 60, Height: 24})
	time.Sleep(50 * time.Millisecond)
	fmt.Fprintln(os.Stderr, "shutdown diagnostic")
	fmt.Fprint(os.Stdout, "shell-ready> ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil || line != "shell input\n" {
		t.Fatalf("restored shell input: %q, %v", line, err)
	}
	fmt.Fprint(os.Stdout, "shell-read: "+line)
}

func TestRealPTYSemanticInputAndModeRestoration(t *testing.T) {
	for _, exit := range []string{"ctrl-c", "after-f6", "slash-quit", "sigterm"} {
		t.Run(exit, func(t *testing.T) { runSemanticPTY(t, exit) })
	}
}

func runSemanticPTY(t *testing.T, exit string) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	if err = unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	// Copy's temporary raw mode must unwind on both output failure and
	// cancellation before Tea resumes its own terminal ownership.
	original, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	closedOutput, err := os.CreateTemp(t.TempDir(), "closed-output")
	if err != nil {
		t.Fatal(err)
	}
	_ = closedOutput.Close()
	cancelled, cancelCopy := context.WithCancel(context.Background())
	cancelCopy()
	for _, copy := range []terminalCopy{
		{ctx: context.Background(), input: &terminalInput{file: slave}, output: closedOutput},
		{ctx: cancelled, input: &terminalInput{file: slave}, output: io.Discard},
	} {
		copyErr := copy.Run()
		restored, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
		if copyErr == nil || err != nil || *restored != *original {
			t.Fatalf("terminal copy error cleanup: %v, termios: %v", copyErr, err)
		}
		if copy.ctx == cancelled && !errors.Is(copyErr, context.Canceled) {
			t.Fatalf("copy cancellation: %v", copyErr)
		}
	}
	if err = unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 60}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snapshots.jsonl")
	cmd := exec.Command(os.Args[0], "-test.run=^TestSemanticPTYFixture$", "-test.timeout=20s")
	cmd.Env = append(os.Environ(), "SPYNEL_SEMANTIC_PTY_FIXTURE="+path, "SSH_CONNECTION=synthetic", "TERM=xterm-256color", "COLORTERM=truecolor", "COLORFGBG=15;0")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	capture := &terminalCapture{}
	// Optional replay for scripts/terminal-copy-screen.mjs. Only synthetic PTY
	// output is recorded; resize boundaries retain the actual terminal geometry.
	var screenEvents []map[string]any
	screenOffset := 0
	recordScreen := func(phase string, width, height int, selection string) {
		text := capture.String()
		screenEvents = append(screenEvents, map[string]any{"phase": phase, "output": text[screenOffset:], "cols": width, "rows": height, "selection": selection})
		screenOffset = len(text)
	}
	recordScreen("resize", 60, 24, "")
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("synthetic terminal output tail: %q", capture.String()[max(0, len(capture.String())-512):])
		}
	})
	readDone := make(chan struct{})
	go func() { _, _ = io.Copy(capture, master); close(readDone) }()
	write := func(text string) {
		t.Helper()
		if _, err := io.WriteString(master, text); err != nil {
			t.Fatal(err)
		}
	}
	initial := waitForTerminalOutput(t, capture, func(s string) bool {
		return strings.Contains(s, "\x1b]11;?") || strings.Contains(s, "Synthetic output")
	})
	if strings.Contains(initial, "\x1b]11;?") {
		write("\x1b]11;rgb:0000/0000/0000\x1b\\\x1b[1;1R")
	}
	waitForTerminalOutput(t, capture, func(s string) bool {
		return strings.Contains(s, "\x1b[?1006h") && strings.Contains(s, "Synthetic output")
	})
	count := 0
	snapshot := func() ptySnapshot {
		t.Helper()
		write("\x07")
		count++
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			data, _ := os.ReadFile(path)
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) >= count {
				var state ptySnapshot
				if json.Unmarshal([]byte(lines[count-1]), &state) == nil {
					return state
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("no PTY snapshot %d", count)
		return ptySnapshot{}
	}
	s := snapshot()
	if s.TerminalSize != [2]int{60, 24} || s.InputBounds[2] != 56 {
		t.Fatalf("startup did not learn PTY geometry: terminal=%v input=%v", s.TerminalSize, s.InputBounds)
	}
	resize := func(width, height int) ptySnapshot {
		t.Helper()
		recordScreen("resize", width, height, "")
		if err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: uint16(height), Col: uint16(width)}); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Process.Signal(syscall.SIGWINCH); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			s := snapshot()
			if s.TerminalSize == [2]int{width, height} && s.InputBounds[2] == width-4 {
				lines := strings.Split(ansi.Strip(s.View), "\n")
				if len(lines) != height {
					t.Fatalf("rendered height=%d, terminal height=%d", len(lines), height)
				}
				for _, line := range lines {
					if ansi.StringWidth(line) > width {
						t.Fatalf("render overflow at width %d: %q", width, line)
					}
				}
				return s
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("PTY resize to %dx%d not delivered", width, height)
		return ptySnapshot{}
	}
	finish := func() {
		waitForTerminalQuiescence(t, capture, 40*time.Millisecond)
		recordScreen("before-exit", 0, 0, "")
		if exit == "sigterm" {
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
		} else if exit == "slash-quit" {
			write("/quit\r")
		} else {
			write("\x03") // Real idle Ctrl+C dispatch, not the fixture's Ctrl+Q.
		}
		waitForTerminalOutput(t, capture, func(text string) bool { return strings.Contains(text, "shell-ready> ") })
		write("shell input\n")
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("PTY didn't exit")
		}
		restored, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
		if err != nil || *restored != *original {
			t.Fatalf("PTY exit did not restore original termios: %v", err)
		}
		output := capture.String()
		for _, mode := range []string{"\x1b[?1006h", "\x1b[?1006l", "\x1b[?2004h", "\x1b[?2004l", "\x1b[?1004h", "\x1b[?1004l", "\x1b[?1049l"} {
			if !strings.Contains(output, mode) {
				t.Errorf("missing mode transition %q", mode)
			}
		}
		_ = slave.Close()
		<-readDone // Closing the parent slave permits real PTY EOF.
		recordScreen("exit", 0, 0, "")
		if path := os.Getenv("SPYNEL_COPY_SCREEN_CAPTURE"); path != "" {
			if exit != "after-f6" {
				path += "." + exit
			}
			data, err := json.Marshal(screenEvents)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	s = resize(42, 20)
	if exit != "after-f6" {
		write("\x1b[6~") // Quit with a populated, scrolled transcript after resize.
		if snapshot().OutputScroll == 0 {
			t.Fatal("exit fixture did not scroll")
		}
		finish()
		return
	}
	// Raw Ctrl+Z/Ctrl+Y travel through terminalFrames, Tea, and the actual
	// composer route. One Unicode multiline replacement is one undo action.
	undoDraft := "undo 界é👩🏽‍💻\n  second line"
	write("\x1b[200~" + undoDraft + "\x1b[201~\x01\x1b[200~replacement\n  tail\x1b[201~")
	write("\x1a")
	s = snapshot()
	if s.Input != undoDraft || s.Selected != undoDraft || strings.Contains(s.View, "suspension is disabled") {
		t.Fatal("raw Ctrl+Z did not silently restore selection paste")
	}
	write("\x19")
	if snapshot().Input != "replacement\n  tail" {
		t.Fatal("raw Ctrl+Y did not redo multiline replacement")
	}
	write("\x1a\x1a")
	if snapshot().Input != "" {
		t.Fatal("raw multiple undo did not restore empty draft")
	}
	write("\x19\x1anew\x19")
	if snapshot().Input != "new" {
		t.Fatal("raw typing after undo did not invalidate redo")
	}
	write("\x03\x1a\x19") // clear is a draft boundary
	s = snapshot()
	if s.Input != "" {
		t.Fatal("raw undo/redo crossed clear boundary")
	}
	write("hello\x7f\x7f\x7f\x1a")
	if snapshot().Input != "hello" {
		t.Fatal("raw Backspace run did not undo together")
	}
	write("\x19")
	if snapshot().Input != "he" {
		t.Fatal("raw Backspace run did not redo together")
	}
	write("\x03hello\x1b[H\x1b[3~\x1b[3~\x1b[3~\x1a")
	if snapshot().Input != "hello" {
		t.Fatal("raw forward Delete run did not undo together")
	}
	write("\x19")
	if snapshot().Input != "lo" {
		t.Fatal("raw forward Delete run did not redo together")
	}
	write("\x03")
	s = snapshot()
	x, y := s.InputBounds[0], s.InputBounds[1]
	report := func(button, x, y int, release bool) string {
		suffix := "M"
		if release {
			suffix = "m"
		}
		return fmt.Sprintf("\x1b[<%d;%d;%d%s", button, x+1, y+1, suffix)
	}
	// Repeated clicks reach the model through real split/coalesced SGR frames,
	// including releases. The fixture's snapshot key does not alter the model.
	click := report(0, 6, 2, false) + report(0, 6, 2, true)
	for _, b := range []byte(click) {
		write(string([]byte{b}))
		time.Sleep(time.Millisecond)
	}
	write(click)
	if got := snapshot().Selected; got != "Synthetic" {
		t.Fatalf("PTY double click: %q", got)
	}
	write(click)
	s = snapshot()
	logicalLine := "Synthetic output row with 界 and é."
	if s.Selected != logicalLine || !s.OutputFocus || s.Dragging {
		t.Fatalf("PTY triple click/release: %q", s.Selected)
	}
	write("\x03")
	waitForTerminalOutput(t, capture, func(text string) bool { return strings.Contains(text, clipboardOSC(logicalLine)) })
	// Held multi-click drags extend and reverse through the actual ingress.
	write("\x1b")
	time.Sleep(90 * time.Millisecond)
	click = report(0, 16, 2, false) + report(0, 16, 2, true)
	write(click + report(0, 16, 2, false) + report(32, 27, 2, false))
	if got := snapshot().Selected; got != "output row with" {
		t.Fatalf("PTY held word forward: %q", got)
	}
	write(report(32, 6, 2, false))
	if got := snapshot().Selected; got != "Synthetic output" {
		t.Fatalf("PTY held word reverse: %q", got)
	}
	write(report(0, 6, 2, true) + "\x03")
	waitForTerminalOutput(t, capture, func(text string) bool { return strings.Contains(text, clipboardOSC("Synthetic output")) })
	// Clear output focus via typing, then replace a word selected on the right
	// half of a wide glyph in a wrapped composer line.
	word := strings.Repeat("界é👩🏽‍💻", 9)
	draft := "before " + word + " after\nsentinel"
	write("x\x01\x1b[200~" + draft + "\x1b[201~")
	s = snapshot()
	x, y = s.InputBounds[0], s.InputBounds[1]
	click = report(0, x+1, y+1, false) + report(0, x+1, y+1, true)
	write(click + click)
	if got := snapshot().Selected; got != word {
		t.Fatalf("PTY wrapped Unicode double click: %q", got)
	}
	write(click)
	if got := snapshot().Selected; got != "before "+word+" after" {
		t.Fatalf("PTY composer logical line: %q", got)
	}
	write("\x1b[200~REPLACED\x1b[201~!")
	if got := snapshot().Input; got != "REPLACED!\nsentinel" {
		t.Fatalf("PTY click selection replacement: %q", got)
	}
	write("\x01\x1b[200~before\nleft " + word + " right\nlast\nsentinel\x1b[201~")
	s = snapshot()
	x, y = s.InputBounds[0], s.InputBounds[1]
	click = report(0, x+1, y+2, false) + report(0, x+1, y+2, true)
	write(click + click + report(0, x+1, y+2, false))
	write(report(32, x+2, y, false))
	if got := snapshot().Selected; got != "before\nleft "+word+" right" {
		t.Fatalf("PTY held wrapped line reverse: %q", got)
	}
	write(report(32, x+1, y+4, false))
	if got := snapshot().Selected; got != "left "+word+" right\nlast" {
		t.Fatalf("PTY held wrapped line forward: %q", got)
	}
	write(report(0, x+1, y+4, true) + "\x1b[200~DRAGGED\x1b[201~")
	if got := snapshot().Input; got != "before\nDRAGGED\nsentinel" {
		t.Fatalf("PTY held line replacement: %q", got)
	}
	// X10 releases have no button identity; they must retain the word too.
	write("\x01\x1b[200~word next\x1b[201~")
	s = snapshot()
	x, y = s.InputBounds[0], s.InputBounds[1]
	x10 := string([]byte{'\x1b', '[', 'M', 32, byte(x + 33), byte(y + 33), '\x1b', '[', 'M', 35, byte(x + 33), byte(y + 33)})
	write(x10 + x10)
	if got := snapshot().Selected; got != "word" {
		t.Fatalf("PTY X10 double click: %q", got)
	}
	write("\x18")
	if got := snapshot().Input; got != " next" {
		t.Fatalf("PTY word cut: %q", got)
	}
	write("\x01\x08")
	s = snapshot()
	x, y = s.InputBounds[0], s.InputBounds[1]
	// Reproduce the previous short-read failure, including a lone ESC that
	// expires before the remaining mouse report reaches the slave.
	write("\x1b")
	time.Sleep(90 * time.Millisecond)
	for _, b := range []byte(report(0, x, y, false)[1:]) {
		write(string([]byte{b}))
		time.Sleep(time.Millisecond)
	}
	write(report(32, x+2, y, false) + report(0, x+2, y, true) + report(64, 8, 3, false) + "\x1b[>65;2;3M")
	write("\x1b[" + strings.Repeat("1", 200))
	time.Sleep(40 * time.Millisecond) // enter bounded discard across actual reads
	write(report(0, x+2, y, true) + "\x1b[\x00A\x1bO1;5D")
	if got := snapshot().Input; got != "" {
		t.Fatalf("mouse report leaked into composer: %q", got)
	}
	// Plain clicks create a caret, not a selection of subsequent input.
	write(report(0, x, y, false) + report(0, x, y, true) + "abc")
	s = snapshot()
	if s.Input != "abc" || s.Selected != "" || !strings.Contains(ansi.Strip(s.View), "abc") {
		t.Fatalf("PTY click/type: input=%q selected=%q", s.Input, s.Selected)
	}
	write(report(0, x+3, y, false) + report(0, x+3, y, true) + "\x1b[200~PASTED\x1b[201~XY")
	s = snapshot()
	if s.Input != "abcPASTEDXY" || s.Selected != "" || !strings.Contains(ansi.Strip(s.View), "abcPASTEDXY") {
		t.Fatalf("PTY click/paste/type: input=%q selected=%q", s.Input, s.Selected)
	}
	// Supported nonstandard keyboard encodings must not consume later input.
	write("\x01\x1b[200~hello world\x1b[201~")
	for _, b := range []byte("\x1b[7$") {
		write(string([]byte{b}))
		time.Sleep(time.Millisecond)
	}
	if snapshot().Selected != "hello world" {
		t.Fatal("rxvt Shift+Home did not select the line")
	}
	write("\x03")
	waitForTerminalOutput(t, capture, func(text string) bool { return strings.Contains(text, clipboardOSC("hello world")) })
	write("X")
	if snapshot().Input != "X" {
		t.Fatal("rxvt Shift+Home swallowed copy/replacement input")
	}
	write("\x01\x1b[200~hello world\x1b[201~")
	for _, b := range []byte("\x1b\x1b[D") {
		write(string([]byte{b}))
		time.Sleep(time.Millisecond)
	}
	write("X\x1b[[A\x1b\x1b[99~\x1b\x1b[\x00A")
	s = snapshot()
	if s.Input != "hello Xworld" || !strings.Contains(ansi.Strip(s.View), "hello Xworld") {
		t.Fatalf("Alt+Left, console F1 or unsupported Alt report corrupted input: %q", s.Input)
	}
	write("\x01\x08")
	// Multiline bracketed paste is one edit and preserves literal mouse-like text.
	write("\x1b[200~first\n  界e\u0301👩🏽‍💻 second\n[<64;1;1M literal\x1b[201~")
	s = snapshot()
	if s.Input != "first\n  界e\u0301👩🏽‍💻 second\n[<64;1;1M literal" {
		t.Fatalf("PTY paste=%q", s.Input)
	}
	x, y = s.InputBounds[0], s.InputBounds[1]
	write(report(0, x, y, false) + report(32, x+3, y+1, false) + report(0, x+3, y+1, true))
	s = snapshot()
	if s.Selected != "first\n  界" {
		t.Fatalf("PTY mouse range=%q", s.Selected)
	}
	if !strings.Contains(s.View, "\x1b[") {
		t.Fatal("fixture did not exercise styled renderer")
	}
	write("X")
	if got := snapshot().Input; got != "Xe\u0301👩🏽‍💻 second\n[<64;1;1M literal" {
		t.Fatalf("PTY replacement=%q", got)
	}
	write("\x1b[1;5F\x1b[1;5D\x1b[3;5~")
	if got := snapshot().Input; strings.Contains(got, "literal") {
		t.Fatalf("PTY word deletion=%q", got)
	}
	// Replace a selection away from the buffer tail with a short paste, then
	// type through the actual terminal decoder while preparation completes.
	write("\x01\x1b[200~abcDEFghi\nsentinel\x1b[201~")
	s = snapshot()
	x, y = s.InputBounds[0], s.InputBounds[1]
	write(report(0, x+3, y, false) + report(32, x+6, y, false) + report(0, x+6, y, true))
	if got := snapshot().Selected; got != "DEF" {
		t.Fatalf("short-paste setup selection=%q", got)
	}
	write("\x1b[200~XY\x1b[201~Z")
	if got := snapshot().Input; got != "abcXYZghi\nsentinel" {
		t.Fatalf("PTY short-paste replacement/caret=%q", got)
	}
	// A second paste at the ten-row cap must reveal the insertion immediately,
	// through both the model snapshot and the actual terminal output stream.
	write("\x01\x1b[200~" + strings.Repeat("row\n", 20) + "tail\x1b[201~")
	s = snapshot()
	beforeScroll := s.InputScroll
	write("\x1b[200~\nPASTED-END\x1b[201~")
	s = snapshot()
	if !strings.HasSuffix(s.Input, "\nPASTED-END") || s.InputScroll <= beforeScroll || !strings.Contains(ansi.Strip(s.View), "PASTED-END") {
		t.Fatal("real PTY paste did not reveal its insertion/caret")
	}
	waitForTerminalOutput(t, capture, func(text string) bool { return strings.Contains(ansi.Strip(text), "PASTED-END") })
	// Reflow a visible, selected tail through real SIGWINCH while capped.
	write("\x01\x1b[200~" + strings.Repeat("0123456789 ", 60) + "VISIBLE-TAIL\x1b[201~\x1b[1;6D")
	s = snapshot()
	if s.Selected != "VISIBLE-TAIL" {
		t.Fatalf("resize setup selection=%q", s.Selected)
	}
	captureStart := len(capture.String())
	s = resize(24, 20)
	if s.Selected != "VISIBLE-TAIL" || !strings.Contains(ansi.Strip(s.View), "VISIBLE-TAIL") {
		t.Fatal("PTY width reflow hid or changed the selected caret/tail")
	}
	waitForTerminalOutput(t, capture, func(text string) bool {
		return strings.Contains(ansi.Strip(text[captureStart:]), "VISIBLE-TAIL")
	})
	s = resize(30, 12)
	if !strings.Contains(ansi.Strip(s.View), "VISIBLE-TAIL") {
		t.Fatal("PTY height shrink hid caret/tail")
	}
	// Resizing a deliberately scrolled composer must leave its caret detached.
	x, y = s.InputBounds[0], s.InputBounds[1]
	write(report(64, x, y, false) + report(64, x, y, false))
	s = snapshot()
	beforeScroll = s.InputScroll
	s = resize(24, 16)
	if s.InputScroll != beforeScroll || s.Selected != "VISIBLE-TAIL" {
		t.Fatal("PTY resize undid wheel scrolling or changed selection")
	}
	_ = resize(42, 20)
	write("\x01\x1b[200~abcXYZghi\nsentinel\x1b[201~")
	_ = snapshot()
	// Output selection/copy is independent from input; cut cannot edit history.
	write(report(0, 5, 2, false) + report(32, 14, 2, false) + report(0, 14, 2, true) + "\x03")
	s = snapshot()
	if !s.OutputFocus || s.Selected != "Synthetic" {
		t.Fatalf("PTY output selection=%q focus=%t", s.Selected, s.OutputFocus)
	}
	waitForTerminalOutput(t, capture, func(text string) bool { return strings.Contains(text, clipboardOSC("Synthetic")) })
	write("\x18")
	if snapshot().Selected != "Synthetic" {
		t.Fatal("PTY cut mutated read-only history")
	}
	// A stopped drag cannot continue after the terminal reports focus loss.
	write(report(0, 5, 2, false) + report(32, 10, 19, false))
	time.Sleep(180 * time.Millisecond)
	s = snapshot()
	if s.OutputScroll == 0 || !s.Dragging {
		t.Fatal("PTY outside drag did not autoscroll")
	}
	write("\x1b[O")
	s = snapshot()
	offset := s.OutputScroll
	time.Sleep(100 * time.Millisecond)
	if next := snapshot(); next.Dragging || next.OutputScroll != offset {
		t.Fatal("PTY focus-loss didn't fence timer")
	}
	write("\x1b[I")
	// Ctrl+Z must never enter Tea's process-group suspension path.
	write("\x1a")
	_ = snapshot() // This would hang in Tea's SIGCONT waiter even in an orphan group.
	for i, exitKey := range []string{"\x1b", "\r", " ", "\x7f", "\x08", "\t", "\x1b[3~", "\x1bOP", "\x1b[[E", "\x1b[24~", "\x1b[34~"} {
		output := i == 9
		write("\x1b")
		time.Sleep(90 * time.Millisecond)
		if output {
			write(report(0, 5, 2, false) + report(0, 14, 2, true) + "\x01")
		} else {
			text := []string{"short", "short", "short", "first\nsecond\nthird", "tiny", strings.Repeat("wrapped ", 12), "tiny", strings.Repeat("copy row\n", 35) + "  界é👩🏽‍💻 tail  ", "tiny"}[min(i, 8)]
			write("\x01\x1b[200~" + text + "\x1b[201~\x01")
		}
		before := snapshot()
		recordScreen("enter", 0, 0, "")
		start := len(capture.String())
		write("\x1b[17~") // F6
		waitForTerminalOutput(t, capture, func(text string) bool {
			return strings.Contains(text[start:], terminalCopyText(before.Selected))
		})
		released := capture.String()[start:]
		for _, mode := range []string{"\x1b[?1006l", "\x1b[?2004l", "\x1b[?1004l", "\x1b[?1049l"} {
			if !strings.Contains(released, mode) {
				t.Fatalf("copy view did not release %q", mode)
			}
		}
		clearAt := strings.LastIndex(released, "\x1b[H\x1b[J")
		if clearAt < strings.Index(released, "\x1b[?1049l") || released[clearAt:] != terminalCopyText(before.Selected) || strings.Contains(released, "\x1b[3J") {
			t.Fatal("copy output was not cleared after normal-screen entry, contained chrome, or erased scrollback")
		}
		if strings.LastIndex(released, "\x1b[?2004h") < strings.Index(released, "\x1b[?2004l") {
			t.Fatal("copy view cannot isolate bracketed paste")
		}
		recordScreen("copy", 0, 0, before.Selected)
		staticStart := len(capture.String())
		// Stale reports, ordinary typing and Ctrl+Z stay outside the composer.
		write(report(64, 3, 3, false) + "ignored\x1a\x03\x1b[200~ space\r\n\t\x1bOP\x1b[3~ignored\x1b[201~")
		recordScreen("resize", 50, 22, "")
		if err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 22, Col: 50}); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Process.Signal(syscall.SIGWINCH)
		time.Sleep(140 * time.Millisecond)
		if len(capture.String()) != staticStart {
			t.Fatal("copy view redrew or echoed input")
		}
		for _, b := range []byte(exitKey) {
			write(string([]byte{b}))
			time.Sleep(5 * time.Millisecond)
		}
		if exitKey != "\x1b" {
			write("ignored\x1b[3~\x1b[200~coalesced paste\r\x1b[201~")
		}
		waitForTerminalOutput(t, capture, func(text string) bool { return strings.Contains(text[staticStart:], "\x1b[?1006h") })
		after := snapshot()
		if after.Input != before.Input || after.Selected != before.Selected || after.OutputFocus != before.OutputFocus || after.TerminalSize != [2]int{50, 22} {
			t.Fatalf("copy return lost draft, selection, focus or dimensions: before=%+v after=%+v", before, after)
		}
		waitForTerminalQuiescence(t, capture, 40*time.Millisecond)
		restoredOutput := capture.String()[staticStart:]
		if strings.Contains(restoredOutput, "\x1b[?1049l") || strings.Count(restoredOutput, "\x1b[?1049h") != 1 {
			t.Fatal("F6 return toggled the screen again after Tea restored it")
		}
		recordScreen("return", 0, 0, "")
		_ = resize(42, 20)
	}
	if strings.Contains(strings.ToLower(capture.String()), "copy requested") {
		t.Fatal("routine copy notice remains")
	}
	write("\x1b")
	time.Sleep(90 * time.Millisecond)
	write("\x03") // Clear the restored nonempty draft before actual quit.
	if state := snapshot(); state.Input != "" || state.Selected != "" {
		t.Fatal("exit setup retained draft or selection")
	}
	finish()
}
