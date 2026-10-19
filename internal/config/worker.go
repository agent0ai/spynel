package config

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/agent0ai/spynel/internal/fsx"
	"github.com/agent0ai/spynel/internal/harness"
)

// RuntimeHarness shares canonical-to-runtime selection between local and
// worker settings. Process-owned fields are retained from the current runtime.
// An availability error still returns the complete selected command.
func (cfg Config) RuntimeHarness(current harness.HarnessConfig) (harness.HarnessConfig, error) {
	current.Name, current.Cwd = cfg.Harness.Name, cfg.Root
	current.Model, current.Effort = cfg.Harness.Model, cfg.Harness.ReasoningEffort
	current.LegacyEffort, current.ServiceMode = cfg.Harness.UsesLegacyReasoningEffort(), cfg.Harness.ServiceMode
	current.ApprovalPolicy, current.Sandbox, current.Network = "never", cfg.Harness.Sandbox, false
	current.SessionsFile, current.Args = cfg.HarnessSessionsPath(cfg.Harness.Name), cfg.HarnessArgs()
	current.Command = ""
	if definition, ok := harness.Lookup(cfg.Harness.Name); ok {
		current.Command = definition.Command
		if definition.Custom {
			current.Command = strings.TrimSpace(cfg.Harness.ACPCommand)
		}
	}
	command, err := harness.ResolveConfiguredCommand(cfg.Harness.Name, cfg.Harness.ACPCommand, nil)
	if err != nil {
		return current, err
	}
	current.Command = command
	return current, nil
}

// WorkerSettings captures current canonical intent without rewriting comments or
// unused keys when only executable discovery needs to be refreshed.
func (cfg Config) WorkerSettings() (harness.WorkerSettings, error) {
	original, err := readWorkerSettings(cfg.Path)
	if err != nil || len(original) > 128*1024 {
		return harness.WorkerSettings{}, errors.New("canonical worker settings are unavailable or oversized")
	}
	saved, err := decode(original, cfg.Path)
	if err != nil {
		return harness.WorkerSettings{}, errors.New("canonical worker settings are invalid")
	}
	expected, err := cfg.Encode()
	actual, encodeErr := saved.Encode()
	if err != nil || encodeErr != nil || !bytes.Equal(expected, actual) {
		return harness.WorkerSettings{}, errors.New("canonical worker settings changed before binding")
	}
	return harness.WorkerSettings{Expected: sha256.Sum256(expected), YAML: original}, nil
}

// PrepareWorkerSettings runs under exact election ownership. The worker owns
// persistence through dispatch publication, even if the requesting client exits.
func PrepareWorkerSettings(root string, current, next harness.HarnessConfig, change harness.WorkerSettings) (plan harness.WorkerSettingsPlan, err error) {
	path := PathForRoot(root)
	if len(change.YAML) == 0 || len(change.YAML) > 128*1024 {
		return plan, errors.New("invalid canonical settings size")
	}
	original, err := readWorkerSettings(path)
	if err != nil {
		return plan, errors.New("canonical settings are unavailable")
	}
	saved, err := decode(original, path)
	if err != nil {
		return plan, errors.New("canonical settings are invalid")
	}
	previous, err := saved.Encode()
	if err != nil {
		return plan, err
	}
	desired, err := decode(change.YAML, path)
	if err != nil {
		return plan, errors.New("requested canonical settings are invalid")
	}
	encoded, err := desired.Encode()
	if err != nil {
		return plan, err
	}
	// An applied retry may confirm its exact target, never overwrite a third choice.
	if sha256.Sum256(previous) != change.Expected && !bytes.Equal(previous, encoded) {
		return plan, errors.New("canonical settings changed before commit")
	}
	expected, unavailable := desired.RuntimeHarness(current)
	if !reflect.DeepEqual(expected, next) {
		return plan, errors.New("runtime selection does not match canonical settings")
	}
	plan.Unavailable = unavailable
	written := false
	plan.Commit = func() error {
		latest, err := readWorkerSettings(path)
		if err != nil || !bytes.Equal(latest, original) {
			return errors.New("canonical settings changed during commit")
		}
		if bytes.Equal(original, change.YAML) {
			return nil
		}
		if err := fsx.AtomicWriteFile(path, encoded, 0600); err != nil {
			return errors.New("canonical settings could not be saved")
		}
		written = true
		reloaded, err := Load(path)
		if err != nil || !reflect.DeepEqual(reloaded, desired) {
			return errors.New("canonical settings could not be reloaded")
		}
		return nil
	}
	plan.Restore = func() error {
		if !written {
			return nil
		}
		latest, err := readWorkerSettings(path)
		if err != nil || !bytes.Equal(latest, encoded) {
			return errors.New("canonical settings changed before restoration")
		}
		return fsx.AtomicWriteFile(path, original, 0600)
	}
	return plan, nil
}

func readWorkerSettings(path string) ([]byte, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return nil, errors.New("canonical settings path is unavailable or unsafe")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("canonical settings are not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("canonical settings changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return nil, errors.New("canonical settings are unreadable or oversized")
	}
	return data, nil
}
