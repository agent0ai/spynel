package updater

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

func processParentID(pid int) (int, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return 0, errors.New("invalid process identity")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 2 {
		return 0, errors.New("invalid process identity")
	}
	return strconv.Atoi(fields[1])
}
