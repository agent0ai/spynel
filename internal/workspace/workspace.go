package workspace

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/frdel/spynel/internal/config"
	"github.com/frdel/spynel/internal/harness"
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
	{Path: ".spynel/AGENTS.md", Template: "templates/workspace-AGENTS.md"},
	{Path: ".spynel/tasks/AGENTS.md", Template: "templates/tasks-AGENTS.md"},
	{Path: ".spynel/goals/AGENTS.md", Template: "templates/goals-AGENTS.md"},
	{Path: ".spynel/prompts/chat.md", Template: "templates/chat.md"},
	{Path: ".spynel/prompts/task.md", Template: "templates/task.md"},
	{Path: ".spynel/prompts/goal.md", Template: "templates/goal.md"},
	{Path: ".spynel/prompts/recovery.md", Template: "templates/recovery.md"},
	{Path: ".spynel/extensions/README.md", Template: "templates/extensions.md"},
}

var directories = []string{
	".spynel/history", ".spynel/attachments", ".spynel/runtime/leases", ".spynel/extensions",
	".spynel/tasks/todo", ".spynel/tasks/working", ".spynel/tasks/waiting", ".spynel/tasks/done", ".spynel/tasks/failed",
	".spynel/goals/active", ".spynel/goals/working", ".spynel/goals/waiting", ".spynel/goals/done",
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

func Template(name string) ([]byte, error) {
	data, err := templates.ReadFile("templates/" + name)
	if err != nil {
		return nil, fmt.Errorf("embedded template %s: %w", name, err)
	}
	return data, nil
}
