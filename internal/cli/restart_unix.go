//go:build !windows

package cli

import (
	"os"
	"syscall"

	"github.com/agent0ai/spynel/internal/updater"
)

func replaceCurrentProcess(args []string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable = updater.RestartExecutable(executable)
	argv := append([]string{executable}, args...)
	return syscall.Exec(executable, argv, os.Environ())
}
