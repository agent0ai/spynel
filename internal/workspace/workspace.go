package workspace

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent0ai/spynel/internal/config"
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
	{Path: "spynel.yaml", Template: "templates/spynel.yaml"},
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
	{Path: ".spynel/extensions/README.md", Template: "templates/extensions.md"},
}

var directories = []string{
	".spynel/history", ".spynel/attachments", ".spynel/runtime/leases", ".spynel/extensions", ".spynel/themes",
	".spynel/tasks/todo", ".spynel/tasks/working", ".spynel/tasks/review", ".spynel/tasks/reviewing", ".spynel/tasks/waiting", ".spynel/tasks/done", ".spynel/tasks/failed", ".spynel/tasks/cancelled",
	".spynel/goals/proposed", ".spynel/goals/planning", ".spynel/goals/active", ".spynel/goals/review", ".spynel/goals/reviewing", ".spynel/goals/waiting", ".spynel/goals/done", ".spynel/goals/abandoned",
}

func Init(root string, force bool) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	configPath := filepath.Join(abs, "spynel.yaml")
	if !force {
		if _, err := os.Stat(configPath); err == nil {
			return errors.New("spynel.yaml already exists (use --force to restore missing templates)")
		}
	}
	for _, dir := range directories {
		if err := os.MkdirAll(filepath.Join(abs, filepath.FromSlash(dir)), 0o700); err != nil {
			return err
		}
	}
	createdConfig := false
	for _, spec := range files {
		target := filepath.Join(abs, filepath.FromSlash(spec.Path))
		if force && spec.Path == "spynel.yaml" {
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
		if err := os.WriteFile(target, data, mode); err != nil {
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

// Upgrade restores only missing runtime directories and embedded support
// files. It never rewrites spynel.yaml or any existing user-owned prompt,
// instruction, theme, or extension file.
func Upgrade(root string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	for _, dir := range directories {
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
		if err := os.WriteFile(target, data, mode); err != nil {
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
