//go:build windows

package orchestrator

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func lockProviderTurn(documentPath string) (*os.File, error) {
	path := providerTurnLockPath(documentPath)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open provider-turn lock: %w", err)
	}
	overlapped := new(windows.Overlapped)
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, overlapped); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock provider turn: %w", err)
	}
	return file, nil
}

func unlockProviderTurn(file *os.File) {
	overlapped := new(windows.Overlapped)
	_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
	_ = file.Close()
}
