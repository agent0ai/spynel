//go:build windows

package tui

import (
	"io"
	"os"
)

// Windows distribution is currently unsupported; retain native console input
// for cross-platform package compilation instead of enabling Unix framing.
type terminalInput struct{ *os.File }

func terminalProgramInput() (io.Reader, func(), error) { return os.Stdin, func() {}, nil }
