//go:build windows

package media

import (
	"context"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func lockSpeechCache(ctx context.Context, path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		overlapped := new(windows.Overlapped)
		err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
		if err == nil {
			return file, nil
		}
		if err != windows.ERROR_LOCK_VIOLATION {
			_ = file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func unlockSpeechCache(file *os.File) {
	overlapped := new(windows.Overlapped)
	_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
	_ = file.Close()
}
