//go:build !windows

package orchestrator

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func lockProviderTurn(documentPath string) (*os.File, error) {
	path := providerTurnLockPath(documentPath)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open provider-turn lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock provider turn: %w", err)
	}
	return file, nil
}

func unlockProviderTurn(file *os.File) {
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
	_ = file.Close()
}
