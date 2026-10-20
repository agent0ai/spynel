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
	fields, err := processStat(pid)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(fields[1])
}

func inspectProcess(pid int) (bool, string, error) {
	fields, err := processStat(pid)
	if err != nil {
		return false, "", err
	}
	if fields[0] == "Z" || fields[0] == "X" || fields[0] == "x" {
		return false, "", nil
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return false, "", err
	}
	if len(strings.TrimSpace(string(boot))) == 0 {
		return false, "", errors.New("process boot identity unavailable")
	}
	return true, strings.TrimSpace(string(boot)) + ":" + fields[19], nil
}

func processStat(pid int) ([]string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return nil, err
	}
	return parseProcessStat(string(data))
}

func parseProcessStat(data string) ([]string, error) {
	end := strings.LastIndexByte(data, ')')
	if end < 0 {
		return nil, errors.New("invalid process identity")
	}
	fields := strings.Fields(data[end+1:])
	if len(fields) < 20 || len(fields[0]) != 1 {
		return nil, errors.New("invalid process identity")
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return nil, errors.New("invalid process start time")
	}
	return fields, nil
}
