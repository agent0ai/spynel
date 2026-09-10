//go:build !windows

package updater

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func lockInstall(root string) (func(), error) {
	fd, err := unix.Open(filepath.Join(root, ".install.lock"), unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), ".install.lock")
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("another installer is active; retry after it finishes: %w", err)
	}
	return func() { _ = unix.Flock(fd, unix.LOCK_UN); _ = file.Close() }, nil
}
