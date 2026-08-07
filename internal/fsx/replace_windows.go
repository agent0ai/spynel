//go:build windows

package fsx

import "golang.org/x/sys/windows"

func replace(source, target string) error {
	return windows.Rename(source, target)
}
