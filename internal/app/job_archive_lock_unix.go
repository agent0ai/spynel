//go:build !windows

package app

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockJobCounter(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX)
}

func tryLockJobOwner(file *os.File) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if err == unix.EWOULDBLOCK || err == unix.EAGAIN {
		return false, nil
	}
	return false, err
}

func unlockJobCounter(file *os.File) {
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
