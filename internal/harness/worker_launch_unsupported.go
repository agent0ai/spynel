//go:build !linux && !darwin

package harness

import (
	"errors"
	"os/exec"
)

func detachWorker(*exec.Cmd) error { return errors.New("independent workers require Linux or macOS") }
