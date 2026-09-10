package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agent0ai/spynel/internal/startup"
	"github.com/agent0ai/spynel/internal/updater"
)

func runUninstallBundles(args []string) error {
	flags := flag.NewFlagSet("uninstall-bundles", flag.ContinueOnError)
	root := flags.String("root", "", "standalone installation directory")
	npmRoot := flags.String("npm-root", "", "npm installation directory")
	userID := flags.Int("user-id", os.Getuid(), "user whose startup registrations are removed")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !filepath.IsAbs(*root) || *userID < 0 || os.Geteuid() != 0 && *userID != os.Getuid() {
		return errors.New("uninstall-bundles requires an absolute root")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	installations := []*updater.Manager{{InstallRoot: *root}}
	if npm, err := exec.LookPath("npm"); err == nil && *npmRoot == "" {
		command := exec.CommandContext(ctx, npm, "root", "--global")
		pipe, err := command.StdoutPipe()
		if err != nil {
			return err
		}
		if err := command.Start(); err != nil {
			return err
		}
		output, readErr := io.ReadAll(io.LimitReader(pipe, 4097))
		if readErr != nil || len(output) > 4096 {
			_ = command.Process.Kill()
			_ = command.Wait()
			return errors.New("cannot inspect npm installation")
		}
		if err := command.Wait(); err != nil {
			return fmt.Errorf("inspect npm installation: %w", err)
		}
		modules := strings.TrimSpace(string(output))
		if !filepath.IsAbs(modules) || strings.ContainsAny(modules, "\r\n") {
			return errors.New("npm returned an invalid global package directory")
		}
		*npmRoot = filepath.Join(modules, "spynel")
	}
	if *npmRoot != "" {
		if !filepath.IsAbs(*npmRoot) {
			return errors.New("uninstall requires an absolute npm root")
		}
		installations = append(installations, &updater.Manager{PackageRoot: *npmRoot})
	}
	if os.Geteuid() != 0 {
		for _, installation := range installations {
			if installation.NeedsAdministrator() {
				executable, err := os.Executable()
				if err != nil {
					return err
				}
				home, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				fmt.Fprintln(os.Stderr, "Administrator access is required to uninstall Spynel.")
				command := exec.CommandContext(ctx, "sudo", "--", "env", "HOME="+home, "PATH="+os.Getenv("PATH"), executable, "uninstall-bundles", "--root", *root, "--npm-root", *npmRoot, "--user-id", strconv.Itoa(*userID))
				command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
				return command.Run()
			}
		}
	}
	for _, installation := range installations {
		if err := installation.Uninstall(ctx, func() error {
			manager, err := startup.New("")
			if err != nil {
				return err
			}
			manager.Executable = filepath.Join(installation.InstallRoot, "spynel")
			manager.NPMLauncher = ""
			if installation.PackageRoot != "" {
				manager.Executable = filepath.Join(installation.PackageRoot, "npm", "vendor", "spynel")
				manager.NPMLauncher = filepath.Join(installation.PackageRoot, "npm", "bin", "spynel.js")
			}
			return manager.RemoveInstallation(ctx, *userID)
		}); err != nil {
			return err
		}
	}
	fmt.Fprintln(os.Stdout, "Spynel uninstalled.")
	return nil
}
