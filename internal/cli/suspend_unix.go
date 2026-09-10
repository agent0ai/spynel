//go:build !windows

package cli

import (
	"os"
	"os/signal"
	"syscall"
)

func preventJobSuspension() func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTSTP)
	return func() { signal.Stop(ch) }
}
