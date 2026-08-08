//go:build !windows

package media

import (
	"context"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func lockSpeechCache(ctx context.Context, path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
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
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
	_ = file.Close()
}
