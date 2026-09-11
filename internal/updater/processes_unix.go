//go:build linux || darwin

package updater

import (
	"os"
	"syscall"
)

func RestartSignal() os.Signal     { return syscall.SIGUSR1 }
func terminationSignal() os.Signal { return syscall.SIGTERM }

func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }
