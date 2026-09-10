//go:build windows

package tui

import "errors"

func (c *terminalCopy) Run() error { return errors.New("terminal copy requires Linux or macOS") }
