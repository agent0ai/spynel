//go:build !linux && !darwin

package cli

import (
	"errors"
	"os"
)

func redirectDiagnosticStderr(writer *os.File) (func(), *os.File, error) {
	return nil, nil, errors.New("native stderr capture requires Linux or macOS")
}
