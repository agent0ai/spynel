//go:build windows

package cli

import (
	"os"
	"os/exec"
)

func replaceCurrentProcess(args []string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		return err
	}
	command := exec.Command(executable, args...)
	command.Dir = workingDirectory
	command.Env = os.Environ()
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Start()
}
