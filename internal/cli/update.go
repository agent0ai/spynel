package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/updater"
)

// The shell update command operates on its installation, independently of
// whichever workspace or older primary happens to be in the current directory.
func runUpdateCommand(args []string, version string) error {
	flags := flag.NewFlagSet("update", flag.ContinueOnError)
	jsonOutput := flags.Bool("json", false, "emit update state as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	action := strings.Join(flags.Args(), " ")
	if action != "" && action != "check" && action != "install" {
		return errors.New("usage: spynel update [--json] [check]")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	manager := updater.Detect(version)
	result, err := manager.Check(ctx)
	if err != nil {
		return err
	}
	if result.Source == "" {
		return errors.New("this Spynel binary is unmanaged; update it through its original installation method")
	}
	if action == "check" {
		if *jsonOutput {
			return json.NewEncoder(os.Stdout).Encode(result)
		}
		fmt.Fprintf(os.Stdout, "Spynel %s; latest %s release: %s.\n", result.Current, result.Source, result.Latest)
		return nil
	}
	if err := manager.PrepareUpdate(ctx, result); err != nil {
		return err
	}
	message := "Updating Spynel and restarting all instances of this installation."
	if *jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(core.Event{Kind: core.EventFinal, Text: message, Done: true, Local: true}); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(os.Stdout, message)
	}
	restartArgs := []string{"version"}
	if *jsonOutput {
		restartArgs = append(restartArgs, "--quiet")
	}
	return &updateRequest{args: restartArgs, standalone: manager.InstallRoot != "", result: result, manager: manager}
}
