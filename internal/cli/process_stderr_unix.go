//go:build linux || darwin

package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

// Capture descriptor 2 as well as os.Stderr: Go runtime and native libraries
// bypass Go writers. The reader inherited the original terminal before this.
func redirectDiagnosticStderr(writer *os.File) (func(), *os.File, error) {
	original, err := unix.Dup(2)
	if err != nil {
		return nil, nil, err
	}
	unix.CloseOnExec(original)
	if err := unix.Dup2(int(writer.Fd()), 2); err != nil {
		_ = unix.Close(original)
		return nil, nil, err
	}
	return func() {
		_ = unix.Dup2(original, 2)
	}, os.NewFile(uintptr(original), "original-stderr"), nil
}
