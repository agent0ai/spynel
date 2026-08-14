package workspace

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/fsx"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/theme"
)

//go:embed templates/*.md templates/*.yaml
var templates embed.FS

var detectCodingHarness = harness.Detect

type fileSpec struct {
	Path       string
	Template   string
	Executable bool
}

var files = []fileSpec{
	{Path: config.FileName, Template: "templates/config.yaml"},
	{Path: ".spynel/AGENTS.md", Template: "templates/workspace-AGENTS.md"},
	{Path: ".spynel/tasks/AGENTS.md", Template: "templates/tasks-AGENTS.md"},
	{Path: ".spynel/goals/AGENTS.md", Template: "templates/goals-AGENTS.md"},
	{Path: ".spynel/prompts/chat.md", Template: "templates/chat.md"},
	{Path: ".spynel/prompts/create-task.md", Template: "templates/create-task.md"},
	{Path: ".spynel/prompts/create-goal.md", Template: "templates/create-goal.md"},
	{Path: ".spynel/prompts/task.md", Template: "templates/task.md"},
	{Path: ".spynel/prompts/goal.md", Template: "templates/goal.md"},
	{Path: ".spynel/prompts/goal-review.md", Template: "templates/goal-review.md"},
	{Path: ".spynel/prompts/recovery.md", Template: "templates/recovery.md"},
	{Path: ".spynel/prompts/review.md", Template: "templates/review.md"},
	{Path: ".spynel/prompts/heartbeat.md", Template: "templates/heartbeat.md"},
	{Path: ".spynel/prompts/notification.md", Template: "templates/notification.md"},
	{Path: ".spynel/instructions/agent-chat.md", Template: "templates/agent-chat.md"},
	{Path: ".spynel/instructions/agent-developer.md", Template: "templates/agent-developer.md"},
	{Path: ".spynel/instructions/agent-reviewer.md", Template: "templates/agent-reviewer.md"},
	{Path: ".spynel/instructions/agent-notification.md", Template: "templates/agent-notification.md"},
	{Path: ".spynel/instructions/agent-heartbeat.md", Template: "templates/agent-heartbeat.md"},
	{Path: ".spynel/extensions/README.md", Template: "templates/extensions.md"},
}

var directories = []string{
	".spynel/history", ".spynel/jobs", ".spynel/attachments", ".spynel/runtime/leases", ".spynel/extensions", ".spynel/themes", ".spynel/instructions",
	".spynel/tasks/todo", ".spynel/tasks/working", ".spynel/tasks/review", ".spynel/tasks/reviewing", ".spynel/tasks/waiting", ".spynel/tasks/done", ".spynel/tasks/failed", ".spynel/tasks/cancelled", ".spynel/tasks/archive",
	".spynel/goals/proposed", ".spynel/goals/planning", ".spynel/goals/active", ".spynel/goals/review", ".spynel/goals/reviewing", ".spynel/goals/waiting", ".spynel/goals/done", ".spynel/goals/abandoned",
}

func ensureRealDirectory(path, description string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		if err := os.Mkdir(path, 0o700); err == nil {
			return nil
		} else if !os.IsExist(err) {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must not be a symbolic link", description)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s must be a directory", description)
	}
	return nil
}

func ensureInstructionBoundary(root string) error {
	stateRoot := filepath.Join(root, ".spynel")
	if err := ensureRealDirectory(stateRoot, ".spynel state path"); err != nil {
		return err
	}
	return ensureRealDirectory(filepath.Join(stateRoot, "instructions"), ".spynel/instructions path")
}

func Init(root string, force bool) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return err
	}
	if err := ensureRealDirectory(filepath.Join(abs, ".spynel"), ".spynel state path"); err != nil {
		return err
	}
	configPath := config.PathForRoot(abs)
	if !force {
		if _, err := os.Stat(configPath); err == nil {
			return errors.New(".spynel/config.yaml already exists (use --force to restore missing templates)")
		}
	}
	if err := ensureInstructionBoundary(abs); err != nil {
		return err
	}
	for _, dir := range directories {
		if dir == ".spynel/instructions" {
			continue
		}
		if err := os.MkdirAll(filepath.Join(abs, filepath.FromSlash(dir)), 0o700); err != nil {
			return err
		}
	}
	if err := theme.InstallBuiltins(filepath.Join(abs, ".spynel", "themes")); err != nil {
		return err
	}
	createdConfig := false
	for _, spec := range files {
		target := filepath.Join(abs, filepath.FromSlash(spec.Path))
		if force && spec.Path == config.FileName {
			if _, err := os.Stat(target); err == nil {
				continue
			}
		}
		if _, err := os.Stat(target); err == nil {
			continue
		}
		data, err := templates.ReadFile(spec.Template)
		if err != nil {
			return err
		}
		data = []byte(strings.ReplaceAll(string(data), "{{PROJECT_ROOT}}", filepath.ToSlash(abs)))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if spec.Executable {
			mode = 0o700
		}
		if err := fsx.AtomicCreateFile(target, data, mode); err != nil {
			if os.IsExist(err) {
				continue
			}
			return err
		}
		if spec.Path == config.FileName {
			createdConfig = true
		}
	}
	if createdConfig {
		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		if detected, _, ok := detectCodingHarness(nil); ok {
			cfg.Harness.Name = detected.Name
		}
		if err := config.Save(cfg); err != nil {
			return err
		}
	}
	return nil
}

// Upgrade restores missing runtime directories and embedded support files. It
// preserves current configuration and user-owned prompts, instructions,
// themes, and extensions. The retired TUI launch preference is the sole
// one-time configuration normalization.
func Upgrade(root string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return err
	}
	if err := ensureInstructionBoundary(abs); err != nil {
		return err
	}
	configPath := config.PathForRoot(abs)
	if _, err := config.NormalizeLegacyFile(configPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, dir := range directories {
		if dir == ".spynel/instructions" {
			continue
		}
		if err := os.MkdirAll(filepath.Join(abs, filepath.FromSlash(dir)), 0o700); err != nil {
			return err
		}
	}
	for _, spec := range files {
		if spec.Path == config.FileName {
			continue
		}
		target := filepath.Join(abs, filepath.FromSlash(spec.Path))
		if _, err := os.Stat(target); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		data, err := templates.ReadFile(spec.Template)
		if err != nil {
			return err
		}
		data = []byte(strings.ReplaceAll(string(data), "{{PROJECT_ROOT}}", filepath.ToSlash(abs)))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if spec.Executable {
			mode = 0o700
		}
		if err := fsx.AtomicCreateFile(target, data, mode); err != nil {
			if os.IsExist(err) {
				continue
			}
			return err
		}
	}
	return nil
}

func Template(name string) ([]byte, error) {
	data, err := templates.ReadFile("templates/" + name)
	if err != nil {
		return nil, fmt.Errorf("embedded template %s: %w", name, err)
	}
	return data, nil
}
