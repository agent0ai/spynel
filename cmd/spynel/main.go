package main

import (
	"fmt"
	"os"

	"github.com/frdel/spynel/internal/cli"
)

var version = "dev"

func main() {
	if err := cli.Run(os.Args[1:], version); err != nil {
		fmt.Fprintln(os.Stderr, "spynel:", err)
		os.Exit(1)
	}
}
