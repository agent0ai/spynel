package updater

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

func installationProcessIDs() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var ids []int
	for _, entry := range entries {
		if id, err := strconv.Atoi(entry.Name()); err == nil {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func installationWritable(path string) bool { return syscall.Access(path, 2) == nil }

func installationProcessPath(pid int) (string, error) {
	return os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
}

func processImagePath(pid int, _ string) string {
	return filepath.Join("/proc", strconv.Itoa(pid), "exe")
}
