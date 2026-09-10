//go:build !windows

package tui

import (
	"io"
	"os"
	"time"

	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"
)

// Deliberately implements term.File, but not cancelreader.File (no Name).
// Read returns at least every 30 ms so Tea's fallback cancellation can fence
// an in-flight read before suspend/restore. No input goroutine survives exit.
type terminalInput struct {
	file     *os.File
	frames   terminalFrames
	escapeAt time.Time
}

func (r *terminalInput) Fd() uintptr                 { return r.file.Fd() }
func (r *terminalInput) Write(p []byte) (int, error) { return r.file.Write(p) }
func (r *terminalInput) Close() error                { return nil } // stdin is caller-owned
func (r *terminalInput) Read(p []byte) (int, error) {
	// Tea supplies 256 bytes; the largest permitted protocol frame is 128.
	if len(p) == 0 {
		return 0, nil
	}
	if len(p) < 128 {
		return 0, io.ErrShortBuffer
	}
	if frame := r.frames.next(len(p), !r.escapeAt.IsZero() && time.Since(r.escapeAt) >= 60*time.Millisecond); frame != nil {
		return copy(p, frame), nil
	}
	fds := []unix.PollFd{{Fd: int32(r.file.Fd()), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, 30)
	if err == unix.EINTR {
		return 0, nil
	}
	if err != nil || n == 0 {
		return 0, err
	}
	var buf [4096]byte
	n, err = r.file.Read(buf[:])
	if n == 0 && err == nil {
		return 0, io.EOF
	}
	if n > 0 {
		r.frames.pending = append(r.frames.pending, buf[:n]...)
		r.escapeAt = time.Now()
	}
	if frame := r.frames.next(len(p), false); frame != nil {
		return copy(p, frame), nil
	}
	return 0, err
}

func terminalProgramInput() (io.Reader, func(), error) {
	f := os.Stdin
	// Preserve Tea's /dev/tty fallback for redirected stdin.
	if !term.IsTerminal(f.Fd()) {
		var err error
		f, err = os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			return nil, nil, err
		}
		return &terminalInput{file: f}, func() { _ = f.Close() }, nil
	}
	return &terminalInput{file: f}, func() {}, nil
}
