//go:build windows

package cli

func preventJobSuspension() func() { return func() {} }
