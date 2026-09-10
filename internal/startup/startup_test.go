package startup

import (
	"bytes"
	"context"
	"encoding/xml"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/agent0ai/spynel/internal/config"
)

func TestStartupCommandHelper(t *testing.T) {
	if os.Getenv("SPYNEL_STARTUP_COMMAND_HELPER") == "" {
		return
	}
	_, _ = os.Stderr.WriteString("authorization: Bearer startup-secret\nstartup helper failed")
	code, _ := strconv.Atoi(os.Getenv("SPYNEL_STARTUP_COMMAND_EXIT"))
	os.Exit(code)
}

func TestRunCommandCapturesBoundedAttributedFailureEvidence(t *testing.T) {
	t.Setenv("SPYNEL_STARTUP_COMMAND_HELPER", "1")
	t.Setenv("SPYNEL_STARTUP_COMMAND_EXIT", "17")
	var log bytes.Buffer
	err := runCommand(context.Background(), &log, os.Args[0], "-test.run=TestStartupCommandHelper")
	if err == nil {
		t.Fatal("runCommand succeeded")
	}
	entry := log.String()
	for _, want := range []string{"process=" + filepath.Base(os.Args[0]), "stream=stderr", "truncated=false", "startup helper failed", "event=exit", "status=failed", "exit_code=17"} {
		if !strings.Contains(entry, want) {
			t.Fatalf("command evidence missing %q (length %d)", want, len(entry))
		}
	}
	bounded := &boundedOutput{}
	_, _ = bounded.Write([]byte(strings.Repeat("x", maxCommandOutput+1024)))
	if bounded.Len() != maxCommandOutput || !bounded.truncated {
		t.Fatalf("bounded output = %d bytes, truncated=%t", bounded.Len(), bounded.truncated)
	}
}

func startupTestConfig(root string) config.Config {
	cfg := config.Default()
	cfg.Root = root
	cfg.Path = config.PathForRoot(root)
	return cfg
}

func TestWorkspaceIDUsesCanonicalWorkspacePath(t *testing.T) {
	root := t.TempDir()
	first := startupTestConfig(root)
	second := first
	second.Path = filepath.Join(root, "caller-supplied-alias.yaml")
	if workspaceID(first) != workspaceID(second) {
		t.Fatalf("workspace ID depends on caller-supplied config path: %q != %q", workspaceID(first), workspaceID(second))
	}
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
	if !strings.Contains(unit, `ExecStart=:"`+manager.Executable+`" "serve" "--automatic-startup" "--config" "`+cfg.Path+`"`) || !strings.Contains(unit, "WorkingDirectory="+cfg.Root+"\n") {
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

func TestLinuxStartupUnitPassesSystemdValidation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd validation requires Linux")
	}
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze is unavailable")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, systemWide := range []bool{false, true} {
		t.Run(strconv.FormatBool(systemWide), func(t *testing.T) {
			cfg := startupTestConfig(filepath.Join(t.TempDir(), `project café with "quotes" %h ${HOME} $USER #;& and \backslash`))
			if systemWide {
				cfg.Root += " "
			} else {
				cfg.Root += `\`
			}
			cfg.Path = config.PathForRoot(cfg.Root)
			manager := &Manager{GOOS: "linux", Home: t.TempDir(), Executable: executable,
				SystemWide: systemWide, SystemUnitDirectory: t.TempDir()}
			if err := manager.Sync(cfg, true); err != nil {
				t.Fatal(err)
			}
			directory := filepath.Join(manager.Home, ".config", "systemd", "user")
			if systemWide {
				directory = manager.SystemUnitDirectory
			}
			path := filepath.Join(directory, "spynel-"+workspaceID(cfg)+".service")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "WorkingDirectory="+strings.ReplaceAll(cfg.Root, "%", "%%")+"/\n") {
				t.Fatalf("working directory was changed by command quoting: %s", data)
			}
			if !strings.Contains(string(data), `ExecStart=:"`) || !strings.Contains(string(data), `${HOME} $USER`) {
				t.Fatalf("startup command does not preserve literal environment-like path text: %s", data)
			}
			command := exec.CommandContext(t.Context(), analyze, "verify", "--man=no", path)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("systemd rejected generated unit: %v\n%s", err, output)
			}
		})
	}
}

func TestLinuxStartupEscapesControlCharactersInUnitValues(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := startupTestConfig(root)
	home := t.TempDir()
	manager := &Manager{GOOS: "linux", Home: home, Executable: filepath.Join(root, "spynel\nInjected=bad\tvalue")}
	if err := manager.Sync(cfg, true); err != nil {
		t.Fatal(err)
	}
	name := "spynel-" + workspaceID(cfg) + ".service"
	data, err := os.ReadFile(filepath.Join(home, ".config", "systemd", "user", name))
	if err != nil {
		t.Fatal(err)
	}
	unit := string(data)
	if strings.Contains(unit, "\nInjected=bad") || strings.Contains(unit, "\tvalue") {
		t.Fatalf("unit contains unescaped control characters: %q", unit)
	}
	if !strings.Contains(unit, `spynel\nInjected=bad\tvalue`) {
		t.Fatalf("unit does not contain escaped path: %q", unit)
	}
}

func TestLinuxStartupRejectsControlCharactersInWorkingDirectory(t *testing.T) {
	for _, character := range []string{"\n", "\r", "\t", "\x00", "\x7f"} {
		cfg := startupTestConfig(filepath.Join(t.TempDir(), "project"+character+"Injected=bad"))
		manager := &Manager{GOOS: "linux", Home: t.TempDir(), Executable: "/bin/true"}
		if err := manager.Sync(cfg, true); err == nil || !strings.Contains(err.Error(), "control characters") {
			t.Fatalf("workspace containing %q was not rejected: %v", character, err)
		}
		if _, err := os.Stat(filepath.Join(manager.Home, ".config")); !os.IsNotExist(err) {
			t.Fatalf("rejected workspace created startup artifacts: %v", err)
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
	if !strings.Contains(string(data), manager.Executable) || !strings.Contains(string(data), cfg.Path) || !strings.Contains(string(data), "automatic-startup") || !strings.Contains(string(data), "RunAtLoad") {
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
	if command != "schtasks.exe" || !strings.Contains(joined, "/Create /SC ONLOGON") || !strings.Contains(joined, manager.Executable) || !strings.Contains(joined, cfg.Path) || !strings.Contains(joined, "--automatic-startup") {
		t.Fatalf("task scheduler call = %s %q", command, arguments)
	}
	if err := manager.Sync(cfg, false); err != nil {
		t.Fatal(err)
	}
	if joined = strings.Join(arguments, " "); !strings.Contains(joined, "/Delete") {
		t.Fatalf("delete task call = %s %q", command, arguments)
	}
}

func TestNPMStartupUsesNodeLauncherWithoutProactiveCheck(t *testing.T) {
	cfg := startupTestConfig(filepath.Join(t.TempDir(), "workspace"))
	manager := &Manager{
		Executable:     filepath.Join(t.TempDir(), "npm", "vendor", "spynel"),
		NodeExecutable: filepath.Join(t.TempDir(), "node"),
		NPMLauncher:    filepath.Join(t.TempDir(), "spynel.js"),
	}
	executable, arguments := manager.startupCommand(cfg)
	if executable != manager.NodeExecutable {
		t.Fatalf("startup executable = %q", executable)
	}
	want := []string{manager.NPMLauncher, "serve", "--automatic-startup", "--config", cfg.Path}
	if strings.Join(arguments, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("startup arguments = %#v, want %#v", arguments, want)
	}
}
