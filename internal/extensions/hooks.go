package extensions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

const ManifestName = ".spynel-extension.yaml"

type Manifest struct {
	Name  string              `yaml:"name"`
	Hooks map[string][]string `yaml:"hooks"`
}

type HookInput struct {
	Hook    string         `json:"hook"`
	Payload map[string]any `json:"payload"`
}

type HookOutput struct {
	Payload map[string]any `json:"payload,omitempty"`
	Cancel  bool           `json:"cancel,omitempty"`
	Message string         `json:"message,omitempty"`
}

type Runner struct {
	Directory string
	Timeout   time.Duration
}

type discovered struct {
	root     string
	manifest Manifest
}

func (r Runner) Run(ctx context.Context, hook string, payload map[string]any) (HookOutput, error) {
	result := HookOutput{Payload: clone(payload)}
	extensions, err := r.discover()
	if err != nil {
		return result, err
	}
	for _, extension := range extensions {
		resolvedHook := hook
		command := extension.manifest.Hooks[resolvedHook]
		// Keep version-one extensions working while making harness.* the
		// canonical terminology for new manifests and hook payloads.
		if len(command) == 0 {
			legacy := map[string]string{
				"harness.before": "recipient.before",
				"harness.after":  "recipient.after",
			}[hook]
			if legacy != "" {
				resolvedHook = legacy
				command = extension.manifest.Hooks[legacy]
			}
		}
		if len(command) == 0 {
			continue
		}
		timeout := r.Timeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		hookCtx, cancel := context.WithTimeout(ctx, timeout)
		input, _ := json.Marshal(HookInput{Hook: resolvedHook, Payload: result.Payload})
		cmd := exec.CommandContext(hookCtx, command[0], command[1:]...)
		cmd.Dir = extension.root
		cmd.Stdin = bytes.NewReader(input)
		cmd.Env = append(os.Environ(), "SPYNEL_HOOK="+resolvedHook, "SPYNEL_EXTENSION="+extension.manifest.Name)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		cancel()
		if err != nil {
			return result, fmt.Errorf("extension %s hook %s: %w: %s", extension.manifest.Name, resolvedHook, err, stderr.String())
		}
		if stdout.Len() == 0 {
			continue
		}
		var output HookOutput
		if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
			return result, fmt.Errorf("extension %s returned invalid JSON: %w", extension.manifest.Name, err)
		}
		if output.Payload != nil {
			result.Payload = output.Payload
		}
		if output.Message != "" {
			result.Message = output.Message
		}
		if output.Cancel {
			result.Cancel = true
			return result, nil
		}
	}
	return result, nil
}

func (r Runner) discover() ([]discovered, error) {
	entries, err := os.ReadDir(r.Directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result []discovered
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		root := filepath.Join(r.Directory, entry.Name())
		data, err := os.ReadFile(filepath.Join(root, ManifestName))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var manifest Manifest
		if err := yaml.Unmarshal(data, &manifest); err != nil {
			return nil, fmt.Errorf("parse extension %s: %w", entry.Name(), err)
		}
		if manifest.Name == "" {
			manifest.Name = entry.Name()
		}
		result = append(result, discovered{root: root, manifest: manifest})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].manifest.Name < result[j].manifest.Name })
	return result, nil
}

func clone(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
