package startup

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// RemoveInstallation stops and removes only registrations for this executable
// or npm launcher. Ordinary preference changes retain Sync's non-stopping behavior.
func (m *Manager) RemoveInstallation(ctx context.Context, userID int) error {
	scopes := []bool{false}
	if m.SystemWide {
		scopes = append(scopes, true)
	}
	for _, system := range scopes {
		directory := filepath.Join(m.Home, ".config", "systemd", "user")
		pattern := "spynel-????????.service"
		if m.GOOS == "darwin" {
			directory = filepath.Join(m.Home, "Library", "LaunchAgents")
			pattern = "dev.spynel.workspace.????????.plist"
		}
		if system {
			directory = m.SystemUnitDirectory
			if m.GOOS == "darwin" {
				directory = m.SystemLaunchDirectory
			}
		}
		paths, err := filepath.Glob(filepath.Join(directory, pattern))
		if err != nil {
			return err
		}
		if len(paths) > 4096 {
			return errors.New("too many startup registrations to inspect safely")
		}
		for _, path := range paths {
			data, err := readRegistration(path)
			if err != nil {
				return err
			}
			matches, err := m.registrationMatches(data)
			if err != nil {
				return fmt.Errorf("inspect startup registration %s: %w", path, err)
			}
			if !matches {
				continue
			}
			name := filepath.Base(path)
			if m.GOOS == "darwin" {
				domain := "gui/" + strconv.Itoa(userID)
				if system {
					domain = "system"
				}
				service := domain + "/" + strings.TrimSuffix(name, ".plist")
				if _, err := runCommand(ctx, m.Log, "launchctl", "bootout", service); err != nil {
					// An installed plist need not have been loaded this login.
					if output, queryErr := runCommand(ctx, m.Log, "launchctl", "print", service); queryErr == nil || ctx.Err() != nil || !(strings.Contains(output+fmt.Sprint(queryErr), "Could not find service") || strings.Contains(output+fmt.Sprint(queryErr), "Could not find domain")) {
						return fmt.Errorf("stop startup registration %s: %w", name, err)
					}
				}
				if err := os.Remove(path); err != nil {
					return err
				}
				continue
			}
			args := []string{"--no-ask-password"}
			if !system {
				args = append(args, "--user")
			}
			runtimePath := filepath.Join("/run/user", strconv.Itoa(userID))
			if userID == os.Getuid() && os.Getenv("XDG_RUNTIME_DIR") != "" {
				runtimePath = os.Getenv("XDG_RUNTIME_DIR")
			}
			managerPath := filepath.Join(runtimePath, "systemd", "private")
			if system {
				managerPath = "/run/systemd/system"
			}
			_, managerErr := os.Stat(managerPath)
			if managerErr != nil && !errors.Is(managerErr, os.ErrNotExist) {
				return managerErr
			}
			run := func(arguments ...string) (string, error) {
				if managerErr != nil {
					return "", nil // No manager is running; remove the future registration.
				}
				if !system && os.Geteuid() == 0 && userID != 0 {
					prefix := []string{"-u", "#" + strconv.Itoa(userID), "--", "env", "XDG_RUNTIME_DIR=" + runtimePath, "systemctl"}
					return runCommand(ctx, m.Log, "sudo", append(prefix, arguments...)...)
				}
				return runCommand(ctx, m.Log, "systemctl", arguments...)
			}
			if _, err := run(append(args, "stop", name)...); err != nil {
				state, queryErr := run(append(args, "show", "--property=ActiveState", "--value", name)...)
				if queryErr != nil || strings.TrimSpace(state) != "inactive" && strings.TrimSpace(state) != "failed" {
					return fmt.Errorf("stop startup registration %s: %w", name, err)
				}
			}
			for _, target := range []string{"default.target.wants", "multi-user.target.wants"} {
				link := filepath.Join(directory, target, name)
				if destination, err := os.Readlink(link); err == nil && (destination == filepath.Join("..", name) || destination == path) {
					if err := os.Remove(link); err != nil {
						return err
					}
				}
			}
			if err := os.Remove(path); err != nil {
				return err
			}
			if _, err := run(append(args, "daemon-reload")...); err != nil {
				return err
			}
		}
	}
	return nil
}

func readRegistration(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxCommandOutput {
		return nil, fmt.Errorf("invalid startup registration %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxCommandOutput+1))
	if len(data) > maxCommandOutput {
		return nil, errors.New("startup registration exceeds size limit")
	}
	return data, err
}

func (m *Manager) registrationMatches(data []byte) (bool, error) {
	if m.GOOS == "linux" {
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.HasPrefix(line, "ExecStart=") {
				continue
			}
			line = strings.TrimPrefix(line, "ExecStart=")
			line = strings.TrimPrefix(line, ":")
			if strings.HasPrefix(line, systemdQuote(m.Executable)+" ") {
				return true, nil
			}
			if m.NPMLauncher != "" && strings.Contains(line, " "+systemdQuote(m.NPMLauncher)+" "+systemdQuote("serve")+" ") {
				return true, nil
			}
		}
		return false, nil
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "key" {
			continue
		}
		var key string
		if err := decoder.DecodeElement(&key, &start); err != nil {
			return false, err
		}
		if key != "ProgramArguments" {
			continue
		}
		var arguments struct {
			Values []string `xml:"string"`
		}
		if err := decoder.Decode(&arguments); err != nil {
			return false, err
		}
		args := arguments.Values
		return len(args) > 0 && args[0] == m.Executable || m.NPMLauncher != "" && len(args) > 1 && args[1] == m.NPMLauncher, nil
	}
}
