//go:build linux || darwin

package harness

import (
	"os/exec"
	"syscall"
)

func detachWorker(command *exec.Cmd) error {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return nil
}
