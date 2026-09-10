package tui

import (
	"context"
	"io"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

type terminalCopyResult struct{ err error }

// Tea's Exec owns stopping the renderer, releasing all capture modes and
// returning to the normal screen. The application/primary keeps running.
type terminalCopy struct {
	ctx    context.Context
	text   string
	input  io.Reader
	output io.Writer
}

func (c *terminalCopy) SetStdin(r io.Reader)  { c.input = r }
func (c *terminalCopy) SetStdout(w io.Writer) { c.output = w }
func (c *terminalCopy) SetStderr(io.Writer)   {}

func terminalCopyText(text string) string {
	// Source controls never become terminal instructions. CRLF is emitted
	// explicitly because the temporary raw input mode also disables OPOST.
	// Home + ED 0 erases visible rows in place. ED 2 can archive them in
	// normal-screen terminals such as Warp; ED 3 destroys unrelated history.
	return "\x1b[H\x1b[J" + strings.ReplaceAll(stripUnsafeTerminalControls(text), "\n", "\r\n")
}

func terminalCopyExit(frame []byte) bool {
	k, ok := tea.DecodeKey(frame)
	if !ok || k.Paste {
		return false
	}
	switch k.Type {
	case tea.KeyEsc, tea.KeyCtrlC, tea.KeyEnter, tea.KeyCtrlJ, tea.KeySpace, tea.KeyBackspace, tea.KeyCtrlH, tea.KeyDelete, tea.KeyTab:
		return true
	}
	return k.Type <= tea.KeyF1 && k.Type >= tea.KeyF20
}
