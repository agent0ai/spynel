//go:build !linux && !darwin

package harness

import (
	"errors"
	"os"
)

func lockProcessOwnership(string) (*os.File, error) {
	return nil, errors.New("harness ownership locking requires Linux or macOS")
}

func RunProvider([]string) error { return errors.New("provider guardians require Linux or macOS") }
