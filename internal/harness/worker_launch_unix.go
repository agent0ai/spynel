//go:build linux || darwin

package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

func newWorkerDirectory() (string, error) {
	root := os.TempDir()
	// Darwin allows 103 socket-path bytes; CreateTemp adds up to ten digits.
	if len(filepath.Join(root, "spynel-worker-0000000000", "control.sock")) > 103 {
		root = "/tmp"
	}
	return os.MkdirTemp(root, "spynel-worker-")
}

func detachWorker(command *exec.Cmd) error {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return nil
}
