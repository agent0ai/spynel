package startup

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/instance"
)

func TestStartupCommandHelper(t *testing.T) {
	if os.Getenv("SPYNEL_STARTUP_COMMAND_HELPER") == "" {
		return
	}
	if os.Getenv("SPYNEL_STARTUP_COMMAND_HELPER") == "environment" {
		if id, err := instance.EnvironmentID(); err != nil || id != os.Getenv("SPYNEL_EXPECT_ENVIRONMENT_ID") {
			t.Fatalf("automatic startup identity differs from the interactive identity: %v", err)
		}
		if _, err := os.UserCacheDir(); err != nil {
			t.Fatal(err)
		}
		if _, err := New(""); err != nil {
			t.Fatal(err)
		}
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
	_, err := runCommand(context.Background(), &log, os.Args[0], "-test.run=TestStartupCommandHelper")
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

func startupTestConfig(t *testing.T, root string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Root = root
	cfg.Path = config.PathForRoot(root)
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func startupTestManager(t *testing.T, goos string) *Manager {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manager := &Manager{GOOS: goos, Home: t.TempDir(), Executable: executable, SystemUnitDirectory: t.TempDir()}
	manager.RunCommand = func(ctx context.Context, name string, args ...string) (string, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("autostart command has no deadline")
		}
		if name == "systemctl" && strings.Contains(strings.Join(args, " "), "list-unit-files") {
			directory := filepath.Join(manager.Home, ".config", "systemd", "user")
			if manager.SystemWide {
				directory = manager.SystemUnitDirectory
			}
			if _, err := os.Stat(filepath.Join(directory, args[len(args)-1])); os.IsNotExist(err) {
				return "", nil
			}
			return args[len(args)-1] + " enabled enabled\n", nil
		}
		if name == "launchctl" && args[0] == "print-disabled" {
			return "disabled services = {\n}\n", nil
		}
		return "", nil
	}
	return manager
}

func TestWorkspaceIDUsesCanonicalWorkspacePath(t *testing.T) {
	root := t.TempDir()
	first := startupTestConfig(t, root)
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
	cfg := startupTestConfig(t, root)
	manager := startupTestManager(t, "linux")
	home := manager.Home
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
			root := filepath.Join(t.TempDir(), `project café with "quotes" %h ${HOME} $USER #;& and \backslash`)
			if systemWide {
				root += " "
			} else {
				root += `\`
			}
			cfg := startupTestConfig(t, root)
			manager := startupTestManager(t, "linux")
			manager.SystemWide, manager.Executable = systemWide, executable
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
	cfg := startupTestConfig(t, root)
	manager := startupTestManager(t, "linux")
	home := manager.Home
	manager.Executable = filepath.Join(root, "spynel\nInjected=bad\tvalue")
	if err := os.WriteFile(manager.Executable, []byte("test executable"), 0o700); err != nil {
		t.Fatal(err)
	}
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
		cfg := config.Default()
		cfg.Root = filepath.Join(t.TempDir(), "project"+character+"Injected=bad")
		cfg.Path = config.PathForRoot(cfg.Root)
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
	cfg := startupTestConfig(t, root)
	manager := startupTestManager(t, "darwin")
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
	cfg := startupTestConfig(t, t.TempDir())
	var command string
	var arguments []string
	var queried bool
	manager := startupTestManager(t, "windows")
	manager.RunCommand = func(_ context.Context, name string, args ...string) (string, error) {
		if args[0] == "/Query" {
			queried = true
			return "", nil
		}
		command = name
		arguments = append([]string(nil), args...)
		return "", nil
	}
	if err := manager.Sync(cfg, true); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(arguments, " ")
	if !queried || command != "schtasks.exe" || !strings.Contains(joined, "/Create /SC ONLOGON") || !strings.Contains(joined, manager.Executable) || !strings.Contains(joined, cfg.Path) || !strings.Contains(joined, "--automatic-startup") {
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
	cfg := startupTestConfig(t, filepath.Join(t.TempDir(), "workspace"))
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

func TestSystemServiceStartsWithOnlyItsGeneratedHomeEnvironment(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux system service environment")
	}
	cfg := startupTestConfig(t, t.TempDir())
	manager := startupTestManager(t, "linux")
	manager.SystemWide = true
	manager.Home = filepath.Join(manager.Home, `home café %h $HOME "quotes" \slash`)
	if err := os.MkdirAll(manager.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", manager.Home)
	for _, customXDG := range []bool{false, true} {
		t.Run(strconv.FormatBool(customXDG), func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", "")
			t.Setenv("XDG_CACHE_HOME", "")
			if customXDG {
				t.Setenv("XDG_CONFIG_HOME", filepath.Join(manager.Home, "custom config"))
				t.Setenv("XDG_CACHE_HOME", filepath.Join(manager.Home, "custom cache"))
			}
			id, err := instance.EnvironmentID()
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.Sync(cfg, true); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(manager.SystemUnitDirectory, "spynel-"+workspaceID(cfg)+".service"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "\nUser=root\n") {
				t.Fatal("root system service omitted its explicit login user")
			}
			command := exec.CommandContext(t.Context(), manager.Executable, "-test.run=^TestStartupCommandHelper$")
			command.Env = []string{"SPYNEL_STARTUP_COMMAND_HELPER=environment", "SPYNEL_EXPECT_ENVIRONMENT_ID=" + id}
			command.Dir = cfg.Root
			for _, line := range strings.Split(string(data), "\n") {
				if value, ok := strings.CutPrefix(line, "Environment="); ok {
					decoded, err := strconv.Unquote(value)
					if err != nil {
						t.Fatal(err)
					}
					command.Env = append(command.Env, strings.ReplaceAll(decoded, "%%", "%"))
				}
			}
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("startup with the generated service environment failed: %v\n%s", err, output)
			}
		})
	}
}

func TestLinuxActionsValidateEveryAttemptAndReportNativeFailures(t *testing.T) {
	cfg := startupTestConfig(t, t.TempDir())
	manager := startupTestManager(t, "linux")
	base := manager.RunCommand
	var calls []string
	var fail string
	manager.RunCommand = func(ctx context.Context, name string, args ...string) (string, error) {
		step := name
		if name == "systemctl" {
			step = args[2]
		}
		calls = append(calls, step)
		if step == fail {
			return "", fmt.Errorf("%s: permission denied", step)
		}
		return base(ctx, name, args...)
	}
	for _, step := range []string{"systemd-analyze", "daemon-reload", "list-unit-files"} {
		fail = step
		if err := manager.Sync(cfg, true); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("%s failure was hidden: %v", step, err)
		}
	}
	fail = ""
	for range 2 {
		calls = nil
		if err := manager.Sync(cfg, true); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(calls, ","); got != "systemd-analyze,daemon-reload,list-unit-files" {
			t.Fatalf("enable did not revalidate: %s", got)
		}
	}
	fail = "daemon-reload"
	if err := manager.Sync(cfg, false); err == nil {
		t.Fatal("removal reported success without reloading systemd")
	}
	fail = ""
	for range 2 {
		if err := manager.Sync(cfg, false); err != nil {
			t.Fatalf("repeated removal failed: %v", err)
		}
	}
	for _, state := range []string{"enabled-runtime", "static", "masked", "", "enabled\ndisabled"} {
		manager.RunCommand = func(context.Context, string, ...string) (string, error) {
			if state == "" {
				return "", nil
			}
			return "spynel-" + workspaceID(cfg) + ".service " + state + " enabled\n", nil
		}
		if err := manager.Sync(cfg, true); err == nil {
			t.Fatalf("accepted unverified enable state %q", state)
		}
		if err := manager.Sync(cfg, false); err == nil && state != "" {
			t.Fatalf("accepted unverified removal state %q", state)
		}
	}
	manager.RunCommand = func(context.Context, string, ...string) (string, error) {
		return "disabled", errors.New("cannot reach systemd")
	}
	if err := manager.Sync(cfg, false); err == nil {
		t.Fatal("unreachable systemd was treated as verified removal")
	}
}

func TestLinuxNativeRegistrationQueries(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native systemd query requires Linux")
	}
	for _, name := range []string{"systemd-analyze", "systemctl"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skip(name + " is unavailable")
		}
	}
	root := t.TempDir()
	cfg := startupTestConfig(t, t.TempDir())
	manager := startupTestManager(t, "linux")
	manager.SystemWide = true
	manager.SystemUnitDirectory = filepath.Join(root, "etc", "systemd", "system")
	manager.RunCommand = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "systemctl" {
			if args[1] == "daemon-reload" {
				// No system manager runs in the test container. Native file-state
				// queries and validation operate on this private filesystem root.
				return "", nil
			}
			args = append([]string{"--root", root}, args...)
		}
		return runCommand(ctx, nil, name, args...)
	}
	for _, enabled := range []bool{true, true, false, false} {
		if err := manager.Sync(cfg, enabled); err != nil {
			t.Fatalf("native registration enabled=%t: %v", enabled, err)
		}
		cfg.Startup.Enabled = !enabled
		actual, err := manager.Enabled(cfg)
		if err != nil || actual != enabled {
			t.Fatalf("native state = %t, %v; expected %t", actual, err, enabled)
		}
	}
}

func TestInvalidStartupPathsDoNotRegister(t *testing.T) {
	cfg := startupTestConfig(t, t.TempDir())
	manager := startupTestManager(t, "linux")
	manager.Executable = filepath.Join(t.TempDir(), "missing-spynel")
	if err := manager.Sync(cfg, true); err == nil || !strings.Contains(err.Error(), "missing-spynel") {
		t.Fatalf("missing executable was not reported: %v", err)
	}
	if _, err := os.Stat(filepath.Join(manager.Home, ".config")); !os.IsNotExist(err) {
		t.Fatalf("invalid startup command created registration artifacts: %v", err)
	}
}

func TestNativeStartupStateReadDoesNotMutateRegistration(t *testing.T) {
	cfg := startupTestConfig(t, t.TempDir())
	m := startupTestManager(t, "linux")
	unit := "spynel-" + workspaceID(cfg) + ".service"
	for _, test := range []struct {
		output        string
		enabled, fail bool
	}{
		{unit + " enabled enabled\n", true, false}, {unit + " disabled enabled\n", false, false}, {"", false, false},
		{unit + " enabled-runtime enabled\n", false, true}, {"unrelated.service enabled enabled\n", false, true},
	} {
		m.RunCommand = func(_ context.Context, name string, args ...string) (string, error) {
			if name != "systemctl" || !strings.Contains(strings.Join(args, " "), "list-unit-files") || strings.Contains(strings.Join(args, " "), "daemon-reload") {
				t.Fatalf("state inspection mutated OS: %s %v", name, args)
			}
			return test.output, nil
		}
		got, err := m.Enabled(cfg)
		if got != test.enabled || (err != nil) != test.fail {
			t.Fatalf("state %q = %t, %v", test.output, got, err)
		}
	}
}

func TestLaunchdStateUsesExactNativeOverride(t *testing.T) {
	label := "dev.spynel.workspace.example"
	for _, test := range []struct {
		output         string
		disabled, fail bool
	}{
		{"disabled services = {\n}", false, false},
		{"disabled services = {\n\"" + label + "\" => true\n}", true, false},
		{"disabled services = {\n\"" + label + "\" => false\n}", false, false},
		{"disabled services = {\n\"" + label + "\" => disabled\n}", true, false},
		{"disabled services = {\n\"" + label + "\" => enabled\n}", false, false},
		{"disabled services = {\n\"" + label + "\" => unknown\n}", false, true},
		{"disabled services = {\n\"" + label + "-other\" => true\n}", false, false},
		{"permission denied", false, true},
	} {
		got, err := launchdDisabled(test.output, label)
		if got != test.disabled || (err != nil) != test.fail {
			t.Fatalf("state %q = %t, %v", test.output, got, err)
		}
	}
	if runtime.GOOS == "darwin" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		output, err := runCommand(ctx, nil, "launchctl", "print-disabled", "system")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := launchdDisabled(output, label); err != nil {
			t.Fatalf("native launchctl response: %v", err)
		}
	}
}
