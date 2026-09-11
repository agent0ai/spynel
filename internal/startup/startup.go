package startup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/fsx"
	"github.com/agent0ai/spynel/internal/updater"
)

type CommandRunner func(context.Context, string, ...string) (string, error)

type Manager struct {
	GOOS                  string
	Home                  string
	Executable            string
	NodeExecutable        string
	NPMLauncher           string
	SystemWide            bool
	SystemUnitDirectory   string
	SystemLaunchDirectory string
	RunCommand            CommandRunner
	Log                   io.Writer
}

const maxCommandOutput = 64 << 10

type boundedOutput struct {
	bytes.Buffer
	truncated bool
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	length := len(data)
	remaining := maxCommandOutput - b.Len()
	if remaining > 0 {
		_, _ = b.Buffer.Write(data[:min(length, remaining)])
	}
	if length > remaining {
		b.truncated = true
	}
	return length, nil
}

func New(executable string) (*Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	if executable == "" {
		executable, err = os.Executable()
		if err != nil {
			return nil, err
		}
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, err
	}
	executable = updater.RestartExecutable(executable)
	systemWide := false
	if runtime.GOOS != "windows" {
		systemWide = os.Geteuid() == 0
	}
	manager := &Manager{
		GOOS: runtime.GOOS, Home: home, Executable: executable, SystemWide: systemWide,
		SystemUnitDirectory: "/etc/systemd/system", SystemLaunchDirectory: "/Library/LaunchDaemons",
	}
	manager.RunCommand = func(ctx context.Context, name string, arguments ...string) (string, error) {
		return runCommand(ctx, manager.Log, name, arguments...)
	}
	nodeExecutable := strings.TrimSpace(os.Getenv("SPYNEL_NPM_NODE"))
	npmLauncher := strings.TrimSpace(os.Getenv("SPYNEL_NPM_LAUNCHER"))
	if nodeExecutable != "" && npmLauncher != "" && updater.Detect("").LauncherManaged {
		if nodeExecutable, err = filepath.Abs(nodeExecutable); err != nil {
			return nil, err
		}
		if npmLauncher, err = filepath.Abs(npmLauncher); err != nil {
			return nil, err
		}
		manager.NodeExecutable = nodeExecutable
		manager.NPMLauncher = npmLauncher
	}
	return manager, nil
}

func (m *Manager) Sync(cfg config.Config, enabled bool) error {
	if cfg.Path == "" {
		return errors.New("cannot configure startup without a loaded .spynel/config.yaml")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if enabled {
		if err := m.validatePaths(cfg); err != nil {
			return err
		}
	}
	switch m.GOOS {
	case "linux":
		if enabled {
			return m.enableLinux(ctx, cfg)
		}
		return m.disableLinux(ctx, cfg)
	case "darwin":
		var err error
		action := "enable"
		if enabled {
			err = m.enableDarwin(ctx, cfg)
		} else {
			action = "disable"
			err = m.disableDarwin(cfg)
		}
		if err != nil {
			return err
		}
		domain := "gui/" + strconv.Itoa(os.Getuid())
		if m.SystemWide {
			domain = "system"
		}
		_, err = m.run(ctx, "launchctl", action, domain+"/dev.spynel.workspace."+workspaceID(cfg))
		if err != nil {
			return fmt.Errorf("%s autostart registration: %w", action, err)
		}
		return nil
	case "windows":
		return m.syncWindows(ctx, cfg, enabled)
	default:
		return fmt.Errorf("run at startup is not supported on %s", m.GOOS)
	}
}

func (m *Manager) run(ctx context.Context, name string, arguments ...string) (string, error) {
	if m.RunCommand != nil {
		return m.RunCommand(ctx, name, arguments...)
	}
	return runCommand(ctx, m.Log, name, arguments...)
}

// Native syntax validation precedes replacement of any existing registration.
func (m *Manager) writeValidated(ctx context.Context, path string, data []byte, command string, args ...string) error {
	stage, err := os.MkdirTemp(filepath.Dir(path), ".spynel-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	candidate := filepath.Join(stage, filepath.Base(path))
	if err := fsx.AtomicWriteFile(candidate, data, 0o600); err != nil {
		return err
	}
	if _, err := m.run(ctx, command, append(args, candidate)...); err != nil {
		return fmt.Errorf("validate autostart registration: %w", err)
	}
	return fsx.AtomicWriteFile(path, data, 0o600)
}

func (m *Manager) validatePaths(cfg config.Config) error {
	if m.GOOS == "linux" {
		if _, err := systemdWorkingDirectory(cfg.Root); err != nil {
			return err
		}
	}
	if !filepath.IsAbs(m.Home) {
		return errors.New("autostart requires an absolute home directory")
	}
	for _, directory := range []string{m.Home, cfg.Root} {
		info, err := os.Stat(directory)
		if err != nil {
			return fmt.Errorf("autostart directory %q: %w", directory, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("autostart directory %q is not a directory", directory)
		}
	}
	executable, _ := m.startupCommand(cfg)
	paths := []string{cfg.Path, executable}
	if m.NPMLauncher != "" {
		paths = append(paths, m.NPMLauncher)
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("autostart file %q: %w", path, err)
		}
		if !filepath.IsAbs(path) || !info.Mode().IsRegular() {
			return fmt.Errorf("autostart file %q must be an absolute regular file", path)
		}
		if path == executable && m.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("autostart executable %q is not executable", path)
		}
	}
	return nil
}

// These per-user locations must agree with interactive launches, including
// the private environment identity and the shared speech/harness caches.
func (m *Manager) environment() map[string]string {
	values := map[string]string{"HOME": m.Home}
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
		if value := os.Getenv(key); value != "" {
			values[key] = value
		}
	}
	return values
}

func workspaceID(cfg config.Config) string {
	identityPath := config.PathForRoot(cfg.Root)
	hash := sha256.Sum256([]byte(filepath.Clean(identityPath)))
	return hex.EncodeToString(hash[:4])
}

func (m *Manager) startupCommand(cfg config.Config) (string, []string) {
	executable := m.Executable
	arguments := []string{"serve", "--automatic-startup", "--config", cfg.Path}
	if m.NodeExecutable != "" && m.NPMLauncher != "" {
		executable = m.NodeExecutable
		arguments = append([]string{m.NPMLauncher}, arguments...)
	}
	return executable, arguments
}

func (m *Manager) enableLinux(ctx context.Context, cfg config.Config) error {
	workingDirectory, err := systemdWorkingDirectory(cfg.Root)
	if err != nil {
		return err
	}
	unitName := "spynel-" + workspaceID(cfg) + ".service"
	unitDirectory := filepath.Join(m.Home, ".config", "systemd", "user")
	target := "default.target"
	if m.SystemWide {
		unitDirectory = m.SystemUnitDirectory
		target = "multi-user.target"
	}
	wantsDirectory := filepath.Join(unitDirectory, target+".wants")
	if err := os.MkdirAll(wantsDirectory, 0o700); err != nil {
		return err
	}
	executable, arguments := m.startupCommand(cfg)
	// Paths are literal; ':' disables systemd's $VAR/${VAR} substitution.
	execStart := ":" + systemdQuote(executable)
	for _, argument := range arguments {
		execStart += " " + systemdQuote(argument)
	}
	serviceEnvironment := ""
	if m.SystemWide {
		// A system service's implicit root user does not receive login variables.
		serviceEnvironment = "User=root\n"
	}
	environment := m.environment()
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
		if value, ok := environment[key]; ok {
			serviceEnvironment += "Environment=" + systemdQuote(key+"="+value) + "\n"
		}
	}
	unit := strings.Join([]string{
		"[Unit]",
		"Description=Spynel workspace " + workingDirectory,
		"Wants=network-online.target",
		"After=network-online.target",
		"",
		"[Service]",
		serviceEnvironment + "Type=simple",
		"WorkingDirectory=" + workingDirectory,
		"ExecStart=" + execStart,
		"Restart=on-failure",
		"RestartSec=5",
		"",
		"[Install]",
		"WantedBy=" + target,
		"",
	}, "\n")
	unitPath := filepath.Join(unitDirectory, unitName)
	verifyArgs := []string{"verify", "--man=no"}
	if !m.SystemWide {
		verifyArgs = append(verifyArgs, "--user")
	}
	if err := m.writeValidated(ctx, unitPath, []byte(unit), "systemd-analyze", verifyArgs...); err != nil {
		return err
	}
	linkPath := filepath.Join(wantsDirectory, unitName)
	if target, err := os.Readlink(linkPath); err == nil {
		if target != filepath.Join("..", unitName) {
			return fmt.Errorf("startup link %s already points to %s", linkPath, target)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("startup target %s already exists and is not a Spynel symlink", linkPath)
	} else if err := os.Symlink(filepath.Join("..", unitName), linkPath); err != nil {
		return err
	}
	return m.verifyLinux(ctx, unitName, true)
}

func (m *Manager) disableLinux(ctx context.Context, cfg config.Config) error {
	unitName := "spynel-" + workspaceID(cfg) + ".service"
	unitDirectory := filepath.Join(m.Home, ".config", "systemd", "user")
	target := "default.target"
	if m.SystemWide {
		unitDirectory = m.SystemUnitDirectory
		target = "multi-user.target"
	}
	for _, path := range []string{filepath.Join(unitDirectory, target+".wants", unitName), filepath.Join(unitDirectory, unitName)} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return m.verifyLinux(ctx, unitName, false)
}

func (m *Manager) verifyLinux(ctx context.Context, unitName string, enabled bool) error {
	arguments := []string{"--no-ask-password"}
	if !m.SystemWide {
		arguments = append(arguments, "--user")
	}
	if _, err := m.run(ctx, "systemctl", append(arguments, "daemon-reload")...); err != nil {
		return fmt.Errorf("reload autostart registration: %w", err)
	}
	output, err := m.run(ctx, "systemctl", append(arguments, "list-unit-files", "--no-legend", "--no-pager", unitName)...)
	// list-unit-files returns exit 1 without output when the exact pattern has
	// no matches. Any diagnostic, timeout, or other failure remains an error.
	var exit *exec.ExitError
	if !enabled && ctx.Err() == nil && output == "" && errors.As(err, &exit) && exit.ExitCode() == 1 && len(exit.Stderr) == 0 {
		return nil
	}
	if err != nil {
		return fmt.Errorf("verify autostart registration: %w", err)
	}
	fields := strings.Fields(output)
	if !enabled && len(fields) == 0 {
		return nil
	}
	if (len(fields) == 2 || len(fields) == 3) && fields[0] == unitName {
		if enabled && fields[1] == "enabled" || !enabled && fields[1] == "disabled" {
			return nil
		}
	}
	return fmt.Errorf("verify autostart registration: unexpected systemd registration %q", strings.TrimSpace(output))
}

func (m *Manager) enableDarwin(ctx context.Context, cfg config.Config) error {
	label := "dev.spynel.workspace." + workspaceID(cfg)
	executable, arguments := m.startupCommand(cfg)
	plist := struct {
		XMLName xml.Name  `xml:"plist"`
		Version string    `xml:"version,attr"`
		Dict    plistDict `xml:"dict"`
	}{Version: "1.0", Dict: plistDict{
		Label: label, ProgramArguments: append([]string{executable}, arguments...),
		WorkingDirectory: cfg.Root, RunAtLoad: true, KeepAlive: true,
		EnvironmentVariables: m.environment(),
	}}
	data, err := xml.MarshalIndent(plist, "", "  ")
	if err != nil {
		return err
	}
	data = append([]byte(xml.Header+`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">`+"\n"), append(data, '\n')...)
	directory := filepath.Join(m.Home, "Library", "LaunchAgents")
	if m.SystemWide {
		directory = m.SystemLaunchDirectory
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	path := filepath.Join(directory, label+".plist")
	return m.writeValidated(ctx, path, data, "plutil", "-lint")
}

func (m *Manager) disableDarwin(cfg config.Config) error {
	label := "dev.spynel.workspace." + workspaceID(cfg)
	directory := filepath.Join(m.Home, "Library", "LaunchAgents")
	if m.SystemWide {
		directory = m.SystemLaunchDirectory
	}
	err := os.Remove(filepath.Join(directory, label+".plist"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (m *Manager) syncWindows(ctx context.Context, cfg config.Config, enabled bool) error {
	name := "Spynel-" + workspaceID(cfg)
	if !enabled {
		_, err := m.run(ctx, "schtasks.exe", "/Delete", "/TN", name, "/F")
		return err
	}
	executable, arguments := m.startupCommand(cfg)
	action := windowsQuote(executable)
	for _, argument := range arguments {
		action += " " + windowsQuote(argument)
	}
	if _, err := m.run(ctx, "schtasks.exe", "/Create", "/SC", "ONLOGON", "/TN", name, "/TR", action, "/F"); err != nil {
		return err
	}
	_, err := m.run(ctx, "schtasks.exe", "/Query", "/TN", name)
	return err
}

func windowsQuote(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}

func runCommand(ctx context.Context, logWriter io.Writer, name string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, name, arguments...)
	stdout := &boundedOutput{}
	stderr := &boundedOutput{}
	command.Stdout = stdout
	command.Stderr = stderr
	command.WaitDelay = time.Second
	err := command.Run()
	commandName := filepath.Base(name)
	writeCommandOutput(logWriter, commandName, "stdout", stdout)
	writeCommandOutput(logWriter, commandName, "stderr", stderr)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			exit.Stderr = bytes.Clone(stderr.Bytes())
		}
		if logWriter != nil {
			_, _ = fmt.Fprintf(logWriter, "process=%s event=exit status=failed exit_code=%d error=%v\n", commandName, processExitCode(command), err)
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if runes := []rune(detail); len(runes) > 4096 {
			detail = string(runes[:4096]) + " (truncated)"
		}
		return stdout.String(), fmt.Errorf("%s: %w: %s", name, err, detail)
	}
	if stdout.truncated || stderr.truncated {
		return "", fmt.Errorf("%s: command output exceeded validation limit", name)
	}
	if logWriter != nil {
		_, _ = fmt.Fprintf(logWriter, "process=%s event=exit status=success exit_code=0\n", commandName)
	}
	return stdout.String(), nil
}

func processExitCode(command *exec.Cmd) int {
	if command.ProcessState == nil {
		return -1
	}
	return command.ProcessState.ExitCode()
}

func writeCommandOutput(logWriter io.Writer, commandName, stream string, output *boundedOutput) {
	if logWriter == nil || output.Len() == 0 {
		return
	}
	_, _ = fmt.Fprintf(logWriter, "process=%s stream=%s truncated=%t output=%s\n", commandName, stream, output.truncated, strings.TrimSpace(output.String()))
}

// WorkingDirectory is a literal path with specifier expansion, not a quoted
// command argument. Backslash escapes would change the selected directory.
func systemdWorkingDirectory(value string) (string, error) {
	if !filepath.IsAbs(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", errors.New("systemd startup requires an absolute workspace path without control characters")
	}
	value = strings.ReplaceAll(value, "%", "%%")
	// A final slash preserves trailing spaces and prevents line continuation.
	if strings.HasSuffix(value, " ") || strings.HasSuffix(value, `\`) {
		value += "/"
	}
	return value, nil
}

// systemdQuote encodes an ExecStart argument, whose parser unquotes it.
func systemdQuote(value string) string {
	var escaped strings.Builder
	escaped.WriteByte('"')
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch character {
		case '\\':
			escaped.WriteString(`\\`)
		case '"':
			escaped.WriteString(`\"`)
		case '%':
			escaped.WriteString(`%%`)
		case '\n':
			escaped.WriteString(`\n`)
		case '\r':
			escaped.WriteString(`\r`)
		case '\t':
			escaped.WriteString(`\t`)
		default:
			if character < 0x20 || character == 0x7f {
				_, _ = fmt.Fprintf(&escaped, `\x%02x`, character)
			} else {
				escaped.WriteByte(character)
			}
		}
	}
	escaped.WriteByte('"')
	return escaped.String()
}

type plistDict struct {
	Label                string
	ProgramArguments     []string
	WorkingDirectory     string
	RunAtLoad            bool
	KeepAlive            bool
	EnvironmentVariables map[string]string
}

func (d plistDict) MarshalXML(encoder *xml.Encoder, start xml.StartElement) error {
	if err := encoder.EncodeToken(start); err != nil {
		return err
	}
	write := func(key string, value any) error {
		if err := encoder.EncodeElement(key, xml.StartElement{Name: xml.Name{Local: "key"}}); err != nil {
			return err
		}
		switch typed := value.(type) {
		case string:
			return encoder.EncodeElement(typed, xml.StartElement{Name: xml.Name{Local: "string"}})
		case bool:
			name := "false"
			if typed {
				name = "true"
			}
			return encoder.EncodeToken(xml.StartElement{Name: xml.Name{Local: name}})
		case []string:
			if err := encoder.EncodeToken(xml.StartElement{Name: xml.Name{Local: "array"}}); err != nil {
				return err
			}
			for _, item := range typed {
				if err := encoder.EncodeElement(item, xml.StartElement{Name: xml.Name{Local: "string"}}); err != nil {
					return err
				}
			}
			return encoder.EncodeToken(xml.EndElement{Name: xml.Name{Local: "array"}})
		case map[string]string:
			if err := encoder.EncodeToken(xml.StartElement{Name: xml.Name{Local: "dict"}}); err != nil {
				return err
			}
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if err := encoder.EncodeElement(key, xml.StartElement{Name: xml.Name{Local: "key"}}); err != nil {
					return err
				}
				if err := encoder.EncodeElement(typed[key], xml.StartElement{Name: xml.Name{Local: "string"}}); err != nil {
					return err
				}
			}
			return encoder.EncodeToken(xml.EndElement{Name: xml.Name{Local: "dict"}})
		default:
			return fmt.Errorf("unsupported plist value %T", value)
		}
	}
	for _, field := range []struct {
		key   string
		value any
	}{
		{"Label", d.Label}, {"ProgramArguments", d.ProgramArguments}, {"WorkingDirectory", d.WorkingDirectory},
		{"RunAtLoad", d.RunAtLoad}, {"KeepAlive", d.KeepAlive},
		{"EnvironmentVariables", d.EnvironmentVariables},
	} {
		if err := write(field.key, field.value); err != nil {
			return err
		}
		if boolean, ok := field.value.(bool); ok {
			name := "false"
			if boolean {
				name = "true"
			}
			if err := encoder.EncodeToken(xml.EndElement{Name: xml.Name{Local: name}}); err != nil {
				return err
			}
		}
	}
	return encoder.EncodeToken(start.End())
}
