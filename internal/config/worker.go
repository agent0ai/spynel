package config

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"

	"github.com/agent0ai/spynel/internal/fsx"
	"github.com/agent0ai/spynel/internal/harness"
)

// RuntimeHarness shares canonical-to-runtime selection between local and
// worker settings. Process-owned fields are retained from the current runtime.
func (cfg Config) RuntimeHarness(current harness.HarnessConfig) (harness.HarnessConfig, error) {
	current.Name, current.Cwd = cfg.Harness.Name, cfg.Root
	current.Model, current.Effort = cfg.Harness.Model, cfg.Harness.ReasoningEffort
	current.LegacyEffort, current.ServiceMode = cfg.Harness.UsesLegacyReasoningEffort(), cfg.Harness.ServiceMode
	current.ApprovalPolicy, current.Sandbox, current.Network = "never", cfg.Harness.Sandbox, false
	current.SessionsFile, current.Args = cfg.HarnessSessionsPath(cfg.Harness.Name), cfg.HarnessArgs()
	command, err := harness.ResolveConfiguredCommand(cfg.Harness.Name, cfg.Harness.ACPCommand, nil)
	if err != nil {
		if definition, ok := harness.Lookup(cfg.Harness.Name); ok {
			current.Command = definition.Command
		}
		return current, err
	}
	current.Command = command
	return current, nil
}

// PrepareWorkerSettings runs under exact election ownership. The worker owns
// persistence through dispatch publication, even if the requesting client exits.
func PrepareWorkerSettings(root string, current, next harness.HarnessConfig, change harness.WorkerSettings) (commit, restore func() error, err error) {
	path := PathForRoot(root)
	if len(change.YAML) == 0 || len(change.YAML) > 128*1024 {
		return nil, nil, errors.New("invalid canonical settings size")
	}
	original, err := readWorkerSettings(path)
	if err != nil {
		return nil, nil, errors.New("canonical settings are unavailable")
	}
	saved, err := decode(original, path)
	if err != nil {
		return nil, nil, errors.New("canonical settings are invalid")
	}
	previous, err := saved.Encode()
	if err != nil {
		return nil, nil, err
	}
	desired, err := decode(change.YAML, path)
	if err != nil {
		return nil, nil, errors.New("requested canonical settings are invalid")
	}
	encoded, err := desired.Encode()
	if err != nil {
		return nil, nil, err
	}
	// An applied retry may confirm its exact target, never overwrite a third choice.
	if sha256.Sum256(previous) != change.Expected && !bytes.Equal(previous, encoded) {
		return nil, nil, errors.New("canonical settings changed before commit")
	}
	expected, err := desired.RuntimeHarness(current)
	if err != nil || !reflect.DeepEqual(expected, next) {
		return nil, nil, errors.New("runtime selection does not match canonical settings")
	}
	written := false
	commit = func() error {
		latest, err := readWorkerSettings(path)
		if err != nil || !bytes.Equal(latest, original) {
			return errors.New("canonical settings changed during commit")
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
	restore = func() error {
		if !written {
			return nil
		}
		latest, err := readWorkerSettings(path)
		if err != nil || !bytes.Equal(latest, encoded) {
			return errors.New("canonical settings changed before restoration")
		}
		return fsx.AtomicWriteFile(path, original, 0600)
	}
	return commit, restore, nil
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
