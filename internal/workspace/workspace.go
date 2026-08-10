package workspace

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/fsx"
	"github.com/agent0ai/spynel/internal/harness"
)

//go:embed templates/*
var templates embed.FS

var detectCodingHarness = harness.Detect

type fileSpec struct {
	Path       string
	Template   string
	Executable bool
}

var files = []fileSpec{
	{Path: config.FileName, Template: "templates/config.yaml"},
	{Path: ".spynel/themes/spynel.yaml", Template: "templates/themes/spynel.yaml"},
	{Path: ".spynel/themes/hack-the-box.yaml", Template: "templates/themes/hack-the-box.yaml"},
	{Path: ".spynel/themes/github-colorblind-dark.yaml", Template: "templates/themes/github-colorblind-dark.yaml"},
	{Path: ".spynel/themes/gruvbox-dark.yaml", Template: "templates/themes/gruvbox-dark.yaml"},
	{Path: ".spynel/themes/nord.yaml", Template: "templates/themes/nord.yaml"},
	{Path: ".spynel/themes/okabe-ito-dark.yaml", Template: "templates/themes/okabe-ito-dark.yaml"},
	{Path: ".spynel/themes/gruvbox-light.yaml", Template: "templates/themes/gruvbox-light.yaml"},
	{Path: ".spynel/themes/rose-pine-dawn.yaml", Template: "templates/themes/rose-pine-dawn.yaml"},
	{Path: ".spynel/themes/tol-muted-light.yaml", Template: "templates/themes/tol-muted-light.yaml"},
	{Path: ".spynel/themes/catppuccin-latte.yaml", Template: "templates/themes/catppuccin-latte.yaml"},
	{Path: ".spynel/themes/okabe-ito-light.yaml", Template: "templates/themes/okabe-ito-light.yaml"},
	{Path: ".spynel/themes/solarized-light.yaml", Template: "templates/themes/solarized-light.yaml"},
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
	".spynel/history", ".spynel/attachments", ".spynel/runtime/leases", ".spynel/extensions", ".spynel/themes", ".spynel/instructions",
	".spynel/tasks/todo", ".spynel/tasks/working", ".spynel/tasks/review", ".spynel/tasks/reviewing", ".spynel/tasks/waiting", ".spynel/tasks/done", ".spynel/tasks/failed", ".spynel/tasks/cancelled",
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

var persistentInstructionRoles = [...]string{"chat", "developer", "reviewer", "notification", "heartbeat"}

func safeInstructionFile(path, relative string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("persistent instruction %s must be a regular, non-symlink file", relative)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("persistent instruction %s must not be group- or world-writable", relative)
	}
	return info, nil
}

// migrateLegacyInstructions publishes each legacy file under its canonical
// agent-prefixed name without a clobber window. A hard link preserves the exact
// bytes and mode; removing the legacy name makes retries idempotent. Dual files
// are collapsed only when their bytes are identical.
func migrateLegacyInstructions(root string) error {
	dir := filepath.Join(root, ".spynel", "instructions")
	for _, role := range persistentInstructionRoles {
		legacyName := role + ".md"
		canonicalName := "agent-" + role + ".md"
		legacyPath := filepath.Join(dir, legacyName)
		canonicalPath := filepath.Join(dir, canonicalName)
		if _, err := safeInstructionFile(legacyPath, ".spynel/instructions/"+legacyName); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}

		if _, err := safeInstructionFile(canonicalPath, ".spynel/instructions/"+canonicalName); os.IsNotExist(err) {
			if err := os.Link(legacyPath, canonicalPath); err != nil {
				if os.IsExist(err) {
					// A concurrent writer won publication; compare it below.
				} else {
					return fmt.Errorf("migrate persistent instruction %s to %s: %w", legacyName, canonicalName, err)
				}
			} else if err := os.Remove(legacyPath); err != nil {
				return fmt.Errorf("remove migrated legacy persistent instruction %s: %w", legacyName, err)
			} else {
				continue
			}
		} else if err != nil {
			return err
		}

		legacyData, err := os.ReadFile(legacyPath)
		if err != nil {
			return fmt.Errorf("read legacy persistent instruction %s: %w", legacyName, err)
		}
		canonicalData, err := os.ReadFile(canonicalPath)
		if err != nil {
			return fmt.Errorf("read canonical persistent instruction %s: %w", canonicalName, err)
		}
		if !bytes.Equal(legacyData, canonicalData) {
			return fmt.Errorf("persistent instruction conflict: both .spynel/instructions/%s and .spynel/instructions/%s exist with different content; preserve both and resolve the conflict manually", legacyName, canonicalName)
		}
		if err := os.Remove(legacyPath); err != nil {
			return fmt.Errorf("remove identical legacy persistent instruction %s: %w", legacyName, err)
		}
	}
	return nil
}

// migrateRetiredNotificationPrompt replaces only the byte-identical stock
// JSON-triage prompt retired by the CLI-action notification contract. Any
// user-authored variation remains untouched.
func migrateRetiredNotificationPrompt(root string) error {
	path := filepath.Join(root, ".spynel", "prompts", "notification.md")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	existing, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	retired, err := templates.ReadFile("templates/migrations/notification-triage-v1.md")
	if err != nil {
		return err
	}
	if !bytes.Equal(existing, retired) {
		return nil
	}
	current, err := templates.ReadFile("templates/notification.md")
	if err != nil {
		return err
	}
	return fsx.AtomicWriteFile(path, current, info.Mode().Perm())
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
	legacyConfigPath := filepath.Join(abs, config.LegacyFileName)
	if _, legacyErr := os.Stat(legacyConfigPath); legacyErr == nil {
		if _, err := config.Load(legacyConfigPath); err != nil {
			return err
		}
	} else if !os.IsNotExist(legacyErr) {
		return legacyErr
	}
	if !force {
		if _, err := os.Stat(configPath); err == nil {
			return errors.New(".spynel/config.yaml already exists (use --force to restore missing templates)")
		}
	}
	if err := ensureInstructionBoundary(abs); err != nil {
		return err
	}
	if err := migrateLegacyInstructions(abs); err != nil {
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
// never rewrites .spynel/config.yaml or user-owned prompts, instructions,
// themes, or extensions; the sole prompt rewrite is an exact retired stock
// notification template migrated to its current replacement.
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
	if err := migrateLegacyInstructions(abs); err != nil {
		return err
	}
	if err := migrateRetiredNotificationPrompt(abs); err != nil {
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
		if spec.Path == config.FileName || strings.HasPrefix(spec.Path, ".spynel/themes/") {
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
