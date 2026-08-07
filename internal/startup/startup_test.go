package startup

import (
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frdel/spynel/internal/config"
)

func startupTestConfig(root string) config.Config {
	cfg := config.Default()
	cfg.Root = root
	cfg.Path = filepath.Join(root, config.FileName)
	return cfg
}

func TestLinuxStartupRegistrationIsWorkspaceSpecificAndReversible(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project with spaces")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := startupTestConfig(root)
	home := t.TempDir()
	manager := &Manager{GOOS: "linux", Home: home, Executable: filepath.Join(root, "spynel")}
	if err := manager.Sync(cfg, true); err != nil {
		t.Fatal(err)
	}
	name := "spynel-" + workspaceID(cfg) + ".service"
	unitPath := filepath.Join(home, ".config", "systemd", "user", name)
	data, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	unit := string(data)
	if !strings.Contains(unit, `ExecStart="`+manager.Executable+`" serve --config "`+cfg.Path+`"`) || !strings.Contains(unit, `WorkingDirectory="`+cfg.Root+`"`) {
		t.Fatalf("unit = %q", unit)
	}
	link := filepath.Join(home, ".config", "systemd", "user", "default.target.wants", name)
	if target, err := os.Readlink(link); err != nil || target != filepath.Join("..", name) {
		t.Fatalf("startup link = %q, %v", target, err)
	}
	if err := manager.Sync(cfg, false); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{unitPath, link} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("startup artifact still exists: %s (%v)", path, err)
		}
	}
}

func TestDarwinStartupWritesValidLaunchAgent(t *testing.T) {
	root := t.TempDir()
	cfg := startupTestConfig(root)
	manager := &Manager{GOOS: "darwin", Home: t.TempDir(), Executable: filepath.Join(root, "spynel")}
	if err := manager.Sync(cfg, true); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(manager.Home, "Library", "LaunchAgents", "dev.spynel.workspace."+workspaceID(cfg)+".plist")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document any
	if err := xml.Unmarshal(data, &document); err != nil {
		t.Fatalf("invalid plist XML: %v\n%s", err, data)
	}
	if !strings.Contains(string(data), manager.Executable) || !strings.Contains(string(data), cfg.Path) || !strings.Contains(string(data), "RunAtLoad") {
		t.Fatalf("plist = %s", data)
	}
	if err := manager.Sync(cfg, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("LaunchAgent still exists: %v", err)
	}
}

func TestWindowsStartupUsesTaskSchedulerArguments(t *testing.T) {
	cfg := startupTestConfig(`C:\work\project`)
	var command string
	var arguments []string
	manager := &Manager{GOOS: "windows", Home: `C:\Users\test`, Executable: `C:\bin\spynel.exe`, RunCommand: func(_ context.Context, name string, args ...string) error {
		command = name
		arguments = append([]string(nil), args...)
		return nil
	}}
	if err := manager.Sync(cfg, true); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(arguments, " ")
	if command != "schtasks.exe" || !strings.Contains(joined, "/Create /SC ONLOGON") || !strings.Contains(joined, manager.Executable) || !strings.Contains(joined, cfg.Path) {
		t.Fatalf("task scheduler call = %s %q", command, arguments)
	}
	if err := manager.Sync(cfg, false); err != nil {
		t.Fatal(err)
	}
	if joined = strings.Join(arguments, " "); !strings.Contains(joined, "/Delete") {
		t.Fatalf("delete task call = %s %q", command, arguments)
	}
}
