//go:build linux

package tea

import (
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSignalDispatchUnblocksWhenContextEnds(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			guard := make(chan os.Signal, 1)
			signal.Notify(guard, sig)
			defer signal.Stop(guard)
			p := NewProgram(&testModel{}, WithInput(nil))
			done := p.handleSignals()
			defer func() {
				p.cancel()
				select {
				case <-done:
				case <-p.msgs: // Release the broken handler if the assertion fails.
					<-done
				}
			}()

			// Withhold the event-loop receiver. Observe dispatch before cancelling
			// so scheduling cannot make the old blocking send pass accidentally.
			deadline := time.Now().Add(3 * time.Second)
			blocked := false
			stack := make([]byte, 1<<20)
			for time.Now().Before(deadline) && !blocked {
				if err := syscall.Kill(os.Getpid(), sig); err != nil {
					t.Fatal(err)
				}
				time.Sleep(time.Millisecond)
				for _, g := range strings.Split(string(stack[:runtime.Stack(stack, true)]), "\n\n") {
					if strings.Contains(g, "(*Program).handleSignals.func1") &&
						(strings.Contains(g, "[chan send]") || strings.Contains(g, "(*Program).Send")) {
						blocked = true
						break
					}
				}
			}
			if !blocked {
				t.Fatal("signal-dispatch barrier was not reached")
			}
			p.cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("signal handler remains blocked after cancellation; shutdown cannot restore the terminal")
			}
		})
	}
}
