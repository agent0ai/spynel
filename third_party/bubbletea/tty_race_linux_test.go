//go:build linux

package tea

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// Terminal restoration and the SIGWINCH listener both use the same output.
// Re-entering initTerminal must not write the descriptor read by checkResize.
func TestTerminalRestoreKeepsResizeOutputStable(t *testing.T) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
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
	if err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 80}); err != nil {
		t.Fatal(err)
	}
	p := NewProgram(&testModel{}, WithInput(nil), WithOutput(slave))
	defer p.cancel()
	p.renderer = newRenderer(slave, false, 60)
	p.msgs = make(chan Msg, 100)
	p.errs = make(chan error, 100)
	if err := p.initTerminal(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 100 {
			p.checkResize()
		}
	}()
	for range 100 {
		if err := p.initTerminal(); err != nil {
			t.Error(err)
		}
	}
	wg.Wait()
	if len(p.errs) != 0 || len(p.msgs) != 100 {
		t.Fatalf("resize errors=%d, messages=%d", len(p.errs), len(p.msgs))
	}
	for range 100 {
		if msg := <-p.msgs; msg != (WindowSizeMsg{Width: 80, Height: 24}) {
			t.Fatalf("resize=%v", msg)
		}
	}
}
