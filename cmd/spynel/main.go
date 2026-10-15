package main

import (
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/agent0ai/spynel/internal/cli"
)

var version = "dev"

func main() {
	if err := cli.Run(os.Args[1:], version); err != nil {
		if observed, ok := err.(interface{ ExitSignal() os.Signal }); ok && observed.ExitSignal() != nil {
			signal.Reset(observed.ExitSignal())
			process, _ := os.FindProcess(os.Getpid())
			_ = process.Signal(observed.ExitSignal())
			time.Sleep(time.Second)
		}
		if exit, ok := err.(interface{ ExitCode() int }); ok {
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "spynel:", err)
		os.Exit(1)
	}
}
