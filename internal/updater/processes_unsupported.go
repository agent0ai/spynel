//go:build !linux && !darwin

package updater

import "os"

func processAlive(int) bool { return false }

func RestartSignal() os.Signal                   { return os.Interrupt }
func terminationSignal() os.Signal               { return os.Interrupt }
func processImagePath(_ int, path string) string { return path }
