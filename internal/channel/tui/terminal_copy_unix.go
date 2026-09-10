//go:build !windows

package tui

import (
	"errors"
	"io"
	"time"

	"github.com/charmbracelet/x/term"
)

func (c *terminalCopy) Run() (err error) {
	input, ok := c.input.(*terminalInput)
	if !ok {
		return errors.New("terminal input unavailable")
	}
	state, err := term.MakeRaw(input.Fd())
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, term.Restore(input.Fd(), state)) }()
	// Keep paste framed while native mouse selection/copy/scrolling stays free.
	// Otherwise a pasted space/newline could exit and type the tail into chat.
	defer func() {
		_, restoreErr := io.WriteString(c.output, "\x1b[?2004l")
		err = errors.Join(err, restoreErr)
	}()
	if _, err := io.WriteString(c.output, "\x1b[?2004h"); err != nil {
		return err
	}
	if _, err := io.WriteString(c.output, terminalCopyText(c.text)); err != nil {
		return err
	}
	var buf [256]byte
	exiting := false
	var lastInput time.Time
	for {
		if c.ctx != nil && c.ctx.Err() != nil {
			return c.ctx.Err()
		}
		pasting := input.frames.paste
		n, err := input.Read(buf[:]) // bounded, framed reads; mouse reports stay inert
		if err != nil {
			return err
		}
		if n > 0 {
			lastInput = time.Now()
		}
		// Drain coalesced copy-view input through a quiet read before Tea
		// resumes; never leave a partial key or paste for the composer.
		// Empty reads can also be interrupted polls, not quiet intervals.
		if exiting && time.Since(lastInput) >= 30*time.Millisecond && n == 0 && len(input.frames.pending) == 0 && !input.frames.paste && input.frames.discard == 0 {
			return nil
		}
		if !pasting && !input.frames.paste && terminalCopyExit(buf[:n]) {
			exiting = true
		}
	}
}
