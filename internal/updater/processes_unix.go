//go:build linux || darwin

package updater

import (
	"errors"
	"os"
	"syscall"
)

func RestartSignal() os.Signal     { return syscall.SIGUSR1 }
func terminationSignal() os.Signal { return syscall.SIGTERM }

func processAlive(pid int) bool {
	alive, _, err := inspectProcess(pid)
	return processMayBeAlive(alive, err, syscall.Kill(pid, 0))
}

func processMayBeAlive(alive bool, inspectionErr, probeErr error) bool {
	if inspectionErr == nil {
		return alive
	}
	// Only affirmative absence is safe; EPERM and unreadable state remain live.
	return !errors.Is(probeErr, syscall.ESRCH)
}
