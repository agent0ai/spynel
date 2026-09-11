package startup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestRemoveInstallation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix startup cleanup")
	}
	for _, platform := range []string{"linux", "darwin"} {
		for _, npm := range []bool{false, true} {
			t.Run(platform+"/npm="+strconv.FormatBool(npm), func(t *testing.T) {
				home := t.TempDir()
				tools := t.TempDir()
				log := filepath.Join(tools, "calls")
				t.Setenv("PATH", tools)
				t.Setenv("STARTUP_TEST_LOG", log)
				t.Setenv("XDG_RUNTIME_DIR", filepath.Join(home, "run"))
				if err := os.MkdirAll(filepath.Join(home, "run", "systemd", "private"), 0700); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"systemctl", "launchctl"} {
					if err := os.WriteFile(filepath.Join(tools, name), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$STARTUP_TEST_LOG\"\ncase \"$*\" in *stop*|*bootout*) [ -z \"${STARTUP_TEST_FAIL:-}\" ] || exit 1;; esac\ncase \"$*\" in *show*) echo active;; esac\n"), 0700); err != nil {
						t.Fatal(err)
					}
				}
				manager := &Manager{GOOS: platform, Home: home, Executable: filepath.Join(home, `install Ω $ ' " % \`, "spynel"), SystemWide: platform == "darwin", SystemLaunchDirectory: filepath.Join(home, "system")}
				if npm {
					manager.NPMLauncher = filepath.Join(home, `npm Ω $ ' " % \`, "spynel.js")
					manager.NodeExecutable = "/node"
				}
				manager.RunCommand = func(_ context.Context, name string, args ...string) (string, error) {
					if name == "launchctl" && args[0] == "print-disabled" {
						return "disabled services = {\n}\n", nil
					}
					if name == "systemctl" && strings.Contains(strings.Join(args, " "), "list-unit-files") {
						return args[len(args)-1] + " enabled enabled\n", nil
					}
					return "", nil
				}
				for _, path := range []string{manager.Executable, manager.NPMLauncher} {
					if path == "" {
						continue
					}
					if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0700); err != nil {
						t.Fatal(err)
					}
				}
				manager.NodeExecutable = manager.Executable
				cfg := startupTestConfig(t, filepath.Join(home, "workspace Ω"))
				if err := manager.Sync(cfg, true); err != nil {
					t.Fatal(err)
				}
				directory := filepath.Join(home, ".config", "systemd", "user")
				name := "spynel-" + workspaceID(cfg) + ".service"
				if platform == "darwin" {
					directory = manager.SystemLaunchDirectory
					name = "dev.spynel.workspace." + workspaceID(cfg) + ".plist"
				}
				path := filepath.Join(directory, name)
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				unrelated := filepath.Join(directory, strings.Replace(name, workspaceID(cfg), "12345678", 1))
				other := strings.ReplaceAll(string(data), "spynel", "different-program")
				if err := os.WriteFile(unrelated, []byte(other), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("STARTUP_TEST_FAIL", "1")
				if err := manager.RemoveInstallation(t.Context(), os.Getuid()); err == nil {
					t.Fatal("failed stop removed registration")
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatal("registration lost on stop failure")
				}
				t.Setenv("STARTUP_TEST_FAIL", "")
				if err := manager.StopInstallation(t.Context(), os.Getuid()); err != nil {
					t.Fatal(err)
				}
				if retained, err := os.ReadFile(path); err != nil || string(retained) != string(data) {
					t.Fatalf("stop changed future startup registration: %v", err)
				}
				if platform == "linux" {
					if _, err := os.Readlink(filepath.Join(directory, "default.target.wants", name)); err != nil {
						t.Fatal("stop removed future startup enablement:", err)
					}
				}
				if err := manager.RemoveInstallation(t.Context(), os.Getuid()); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("registration remains")
				}
				if _, err := os.Stat(unrelated); err != nil {
					t.Fatal("unrelated registration removed")
				}
				calls, err := os.ReadFile(log)
				if err != nil {
					t.Fatal(err)
				}
				want := "--user stop " + name
				if platform == "darwin" {
					want = "bootout system/" + strings.TrimSuffix(name, ".plist")
				}
				if !strings.Contains(string(calls), want) {
					t.Fatalf("missing stop: %s", calls)
				}
				if platform == "linux" {
					if _, err := os.Lstat(filepath.Join(directory, "default.target.wants", name)); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("startup enablement remains")
					}
					if err := os.RemoveAll(filepath.Join(home, "run")); err != nil {
						t.Fatal(err)
					}
					if err := manager.Sync(cfg, true); err != nil {
						t.Fatal(err)
					}
					t.Setenv("STARTUP_TEST_FAIL", "1")
					if err := manager.RemoveInstallation(t.Context(), os.Getuid()); err != nil {
						t.Fatal("offline registration cleanup:", err)
					}
				}
			})
		}
	}
}
