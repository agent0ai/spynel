package extensions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

const ManifestName = ".spynel-extension.yaml"

const (
	maxHookStdout = 1 << 20
	maxHookStderr = 64 << 10
)

var supportedHooks = map[string]struct{}{
	"message.received": {},
	"harness.before":   {},
	"harness.after":    {},
	"task.claimed":     {},
	"task.completed":   {},
}

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
	Log       io.Writer
}

type boundedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	length := len(data)
	remaining := b.limit - b.Len()
	if remaining > 0 {
		_, _ = b.Buffer.Write(data[:min(len(data), remaining)])
	}
	if length > remaining {
		b.truncated = true
	}
	return length, nil
}

type discovered struct {
	id       string
	root     string
	manifest Manifest
}

func (r Runner) Run(ctx context.Context, hook string, payload map[string]any) (HookOutput, error) {
	return r.run(ctx, hook, payload, nil, nil)
}

// RunTracked executes matching hooks using durable successful-completion
// receipts. A missing receipt deliberately causes retry, so externally visible
// effects must be persistently deduplicated by the stable event_id in payload.
func (r Runner) RunTracked(ctx context.Context, hook string, payload map[string]any, completed map[string]bool, onCompleted func(string) error) (HookOutput, error) {
	return r.run(ctx, hook, payload, completed, onCompleted)
}

func (r Runner) run(ctx context.Context, hook string, payload map[string]any, completed map[string]bool, onCompleted func(string) error) (HookOutput, error) {
	result := HookOutput{Payload: clone(payload)}
	extensions, err := r.discover()
	if err != nil {
		return result, err
	}
	for _, extension := range extensions {
		if completed[extension.id] {
			continue
		}
		command := extension.manifest.Hooks[hook]
		if len(command) == 0 {
			continue
		}
		timeout := r.Timeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		hookCtx, cancel := context.WithTimeout(ctx, timeout)
		input, _ := json.Marshal(HookInput{Hook: hook, Payload: result.Payload})
		cmd := exec.CommandContext(hookCtx, command[0], command[1:]...)
		cmd.Dir = extension.root
		cmd.Stdin = bytes.NewReader(input)
		cmd.Env = append(os.Environ(), "SPYNEL_HOOK="+hook, "SPYNEL_EXTENSION="+extension.manifest.Name)
		stdout := boundedBuffer{limit: maxHookStdout}
		stderr := boundedBuffer{limit: maxHookStderr}
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Start()
		if err == nil {
			err = cmd.Wait()
		}
		cancel()
		if r.Log != nil && stderr.Len() > 0 {
			_, _ = fmt.Fprintf(r.Log, "extension=%s hook=%s stderr_truncated=%t stderr=%s\n", extension.manifest.Name, hook, stderr.truncated, stderr.String())
		}
		if err != nil {
			if r.Log != nil {
				_, _ = fmt.Fprintf(r.Log, "extension=%s hook=%s process_failed=%v stderr_present=%t\n", extension.manifest.Name, hook, err, stderr.Len() > 0)
			}
			return result, fmt.Errorf("extension %s hook %s failed: %w", extension.manifest.Name, hook, err)
		}
		if stdout.truncated {
			return result, fmt.Errorf("extension %s hook %s exceeded %d-byte stdout limit", extension.manifest.Name, hook, maxHookStdout)
		}
		var output HookOutput
		if stdout.Len() > 0 {
			if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
				return result, fmt.Errorf("extension %s returned invalid JSON: %w", extension.manifest.Name, err)
			}
			if output.Payload != nil {
				result.Payload = output.Payload
			}
			if output.Message != "" {
				result.Message = output.Message
			}
		}
		if onCompleted != nil {
			if err := onCompleted(extension.id); err != nil {
				return result, fmt.Errorf("record extension %s hook %s completion: %w", extension.manifest.Name, hook, err)
			}
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
		for hook := range manifest.Hooks {
			if _, ok := supportedHooks[hook]; !ok {
				return nil, fmt.Errorf("extension %s declares unsupported hook %q", entry.Name(), hook)
			}
		}
		result = append(result, discovered{id: entry.Name(), root: root, manifest: manifest})
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
