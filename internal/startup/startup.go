package startup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/frdel/spynel/internal/config"
	"github.com/frdel/spynel/internal/fsx"
)

type CommandRunner func(context.Context, string, ...string) error

type Manager struct {
	GOOS                  string
	Home                  string
	Executable            string
	SystemWide            bool
	SystemUnitDirectory   string
	SystemLaunchDirectory string
	RunCommand            CommandRunner
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
	systemWide := false
	if runtime.GOOS != "windows" {
		systemWide = os.Geteuid() == 0
	}
	return &Manager{
		GOOS: runtime.GOOS, Home: home, Executable: executable, SystemWide: systemWide,
		SystemUnitDirectory: "/etc/systemd/system", SystemLaunchDirectory: "/Library/LaunchDaemons",
		RunCommand: runCommand,
	}, nil
}

func (m *Manager) Sync(cfg config.Config, enabled bool) error {
	if cfg.Path == "" {
		return errors.New("cannot configure startup without a loaded spynel.yaml")
	}
	switch m.GOOS {
	case "linux":
		if enabled {
			return m.enableLinux(cfg)
		}
		return m.disableLinux(cfg)
	case "darwin":
		if enabled {
			return m.enableDarwin(cfg)
		}
		return m.disableDarwin(cfg)
	case "windows":
		return m.syncWindows(cfg, enabled)
	default:
		return fmt.Errorf("run at startup is not supported on %s", m.GOOS)
	}
}

func workspaceID(cfg config.Config) string {
	hash := sha256.Sum256([]byte(filepath.Clean(cfg.Path)))
	return hex.EncodeToString(hash[:4])
}

func (m *Manager) enableLinux(cfg config.Config) error {
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
	unit := strings.Join([]string{
		"[Unit]",
		"Description=Spynel workspace " + systemdEscape(cfg.Root),
		"Wants=network-online.target",
		"After=network-online.target",
		"",
		"[Service]",
		"Type=simple",
		"WorkingDirectory=" + systemdQuote(cfg.Root),
		"ExecStart=" + systemdQuote(m.Executable) + " serve --config " + systemdQuote(cfg.Path),
		"Restart=on-failure",
		"RestartSec=5",
		"",
		"[Install]",
		"WantedBy=" + target,
		"",
	}, "\n")
	unitPath := filepath.Join(unitDirectory, unitName)
	if err := fsx.AtomicWriteFile(unitPath, []byte(unit), 0o600); err != nil {
		return err
	}
	linkPath := filepath.Join(wantsDirectory, unitName)
	if target, err := os.Readlink(linkPath); err == nil {
		if target == filepath.Join("..", unitName) {
			return nil
		}
		return fmt.Errorf("startup link %s already points to %s", linkPath, target)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("startup target %s already exists and is not a Spynel symlink", linkPath)
	}
	return os.Symlink(filepath.Join("..", unitName), linkPath)
}

func (m *Manager) disableLinux(cfg config.Config) error {
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
	return nil
}

func (m *Manager) enableDarwin(cfg config.Config) error {
	label := "dev.spynel.workspace." + workspaceID(cfg)
	plist := struct {
		XMLName xml.Name  `xml:"plist"`
		Version string    `xml:"version,attr"`
		Dict    plistDict `xml:"dict"`
	}{Version: "1.0", Dict: plistDict{
		Label: label, ProgramArguments: []string{m.Executable, "serve", "--config", cfg.Path},
		WorkingDirectory: cfg.Root, RunAtLoad: true, KeepAlive: true,
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
	return fsx.AtomicWriteFile(filepath.Join(directory, label+".plist"), data, 0o600)
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

func (m *Manager) syncWindows(cfg config.Config, enabled bool) error {
	name := "Spynel-" + workspaceID(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !enabled {
		return m.RunCommand(ctx, "schtasks.exe", "/Delete", "/TN", name, "/F")
	}
	action := fmt.Sprintf(`"%s" serve --config "%s"`, strings.ReplaceAll(m.Executable, `"`, `\"`), strings.ReplaceAll(cfg.Path, `"`, `\"`))
	return m.RunCommand(ctx, "schtasks.exe", "/Create", "/SC", "ONLOGON", "/TN", name, "/TR", action, "/F")
}

func runCommand(ctx context.Context, name string, arguments ...string) error {
	command := exec.CommandContext(ctx, name, arguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func systemdQuote(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, `%`, `%%`)
	return `"` + value + `"`
}

func systemdEscape(value string) string {
	return strings.NewReplacer("\n", " ", "\r", " ", "%", "%%").Replace(value)
}

type plistDict struct {
	Label            string
	ProgramArguments []string
	WorkingDirectory string
	RunAtLoad        bool
	KeepAlive        bool
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
