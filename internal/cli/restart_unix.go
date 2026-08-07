//go:build !windows

package cli

import (
	"os"
	"syscall"
)

func replaceCurrentProcess(args []string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	argv := append([]string{executable}, args...)
	return syscall.Exec(executable, argv, os.Environ())
}
