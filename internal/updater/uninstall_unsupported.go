//go:build !linux && !darwin

package updater

import "errors"

func installationWritable(string) bool { return false }

func installationProcessIDs() ([]int, error) {
	return nil, errors.New("uninstall supports macOS and Linux")
}

func installationProcessPath(int) (string, error) {
	return "", errors.New("uninstall supports macOS and Linux")
}
