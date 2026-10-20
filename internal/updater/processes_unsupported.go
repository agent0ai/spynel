//go:build !linux && !darwin

package updater

import (
	"errors"
	"os"
)

func processAlive(int) bool { return false }

func inspectProcess(int) (bool, string, error) {
	return false, "", errors.New("unsupported process inspection")
}

func RestartSignal() os.Signal                   { return os.Interrupt }
func terminationSignal() os.Signal               { return os.Interrupt }
func processImagePath(_ int, path string) string { return path }

func processParentID(int) (int, error) { return 0, errors.New("unsupported process inspection") }
