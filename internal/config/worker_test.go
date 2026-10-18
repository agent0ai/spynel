package config

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/agent0ai/spynel/internal/harness"
)

func workerSettingsConfig(t *testing.T) (Config, harness.HarnessConfig) {
	t.Helper()
	cfg := Default()
	cfg.Root = t.TempDir()
	cfg.Path = PathForRoot(cfg.Root)
	cfg.Harness.Name, cfg.Harness.ACPCommand = "acp", os.Args[0]
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := cfg.RuntimeHarness(harness.HarnessConfig{Version: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	return cfg, runtime
}

func TestWorkerSettingsPersistenceConflictRetryAndRestore(t *testing.T) {
	previous, runtime := workerSettingsConfig(t)
	before, _ := previous.Encode()
	next := previous
	if _, err := SetSettings(&next, map[string]string{"harness.model": "new", "orchestrator.interval_seconds": "19"}); err != nil {
		t.Fatal(err)
	}
	encoded, _ := next.Encode()
	target, _ := next.RuntimeHarness(runtime)
	change := harness.WorkerSettings{Expected: sha256.Sum256(before), YAML: encoded}
	plan, err := PrepareWorkerSettings(previous.Root, runtime, target, change)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Commit(); err != nil {
		t.Fatal(err)
	}
	store := NewStore(previous)
	if _, err := store.Reload(before); err == nil || store.Snapshot().Harness.Model != previous.Harness.Model {
		t.Fatal("stale expected configuration was published")
	}
	if saved, err := store.Reload(encoded); err != nil || saved.Harness.Model != "new" {
		t.Fatal("committed settings could not be published", err)
	}
	retry, err := PrepareWorkerSettings(previous.Root, target, target, change)
	if err != nil || retry.Commit() != nil {
		t.Fatal("applied retry could not confirm the canonical save", err)
	}
	other := next
	other.Harness.Model = "third"
	otherBytes, _ := other.Encode()
	otherRuntime, _ := other.RuntimeHarness(runtime)
	if _, err := PrepareWorkerSettings(previous.Root, target, otherRuntime, harness.WorkerSettings{Expected: change.Expected, YAML: otherBytes}); err == nil {
		t.Fatal("stale writer overwrote a third selection")
	}
	if err := plan.Restore(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(previous.Path)
	if !bytes.Equal(got, before) {
		t.Fatal("rollback did not restore original bytes")
	}
}

func TestWorkerSettingsRejectsMismatchedUnsafeAndChangedCanonicalInput(t *testing.T) {
	previous, runtime := workerSettingsConfig(t)
	before, _ := previous.Encode()
	next := previous
	next.Harness.Model = "new"
	encoded, _ := next.Encode()
	target, _ := next.RuntimeHarness(runtime)
	change := harness.WorkerSettings{Expected: sha256.Sum256(before), YAML: encoded}
	if _, err := PrepareWorkerSettings(previous.Root, runtime, runtime, change); err == nil {
		t.Fatal("canonical/runtime mismatch accepted")
	}
	for _, invalid := range [][]byte{nil, []byte("harness: ["), bytes.Repeat([]byte("x"), 128*1024+1), []byte("harness:\n  name: acp\n  reasoning_effort: high\n")} {
		bad := change
		bad.YAML = invalid
		if _, err := PrepareWorkerSettings(previous.Root, runtime, target, bad); err == nil {
			t.Fatal("invalid settings accepted")
		}
	}
	plan, err := PrepareWorkerSettings(previous.Root, runtime, target, change)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(previous.Path, append(before, []byte("# user edit\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if plan.Commit() == nil || plan.Restore() != nil {
		t.Fatal("concurrent file edit was overwritten or unnecessary restoration attempted")
	}
	if err := os.Rename(previous.Path, previous.Path+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(previous.Path)+".real", previous.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareWorkerSettings(previous.Root, runtime, target, change); err == nil {
		t.Fatal("symlinked canonical file accepted")
	}
}

func TestWorkerSettingsUnavailableSelectionRemainsCanonical(t *testing.T) {
	for _, name := range []string{"", "codex", "claude-code", "pi", "acp"} {
		t.Run("selected-"+name, func(t *testing.T) {
			previous, current := workerSettingsConfig(t)
			t.Setenv("PATH", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			if _, err := SetSettings(&previous, map[string]string{"harness.name": name, "harness.acp_command": filepath.Join(previous.Root, "missing custom provider"), "harness.model": "", "harness.reasoning_effort": "inherit"}); err != nil {
				t.Fatal(err)
			}
			if err := Save(previous); err != nil {
				t.Fatal(err)
			}
			before, _ := previous.Encode()
			next := previous
			next.Orchestrator.IntervalSec = 19
			encoded, _ := next.Encode()
			target, unavailable := next.RuntimeHarness(current)
			if unavailable == nil || target.Version != current.Version {
				t.Fatal("missing provider lost availability or process-owned selection")
			}
			if name == "acp" && target.Command != previous.Harness.ACPCommand {
				t.Fatal("unavailable custom selection lost its configured command")
			}
			plan, err := PrepareWorkerSettings(previous.Root, current, target, harness.WorkerSettings{Expected: sha256.Sum256(before), YAML: encoded})
			if err != nil || plan.Unavailable == nil || plan.Commit == nil || plan.Restore == nil {
				t.Fatal("valid unavailable intent was rejected", err)
			}
			if err := plan.Commit(); err != nil {
				t.Fatal(err)
			}
			if saved, err := Load(previous.Path); err != nil || saved.Orchestrator.IntervalSec != 19 {
				t.Fatal("unavailable canonical save did not reload", err)
			}
			if err := plan.Restore(); err != nil {
				t.Fatal(err)
			}
			if restored, _ := os.ReadFile(previous.Path); !bytes.Equal(restored, before) {
				t.Fatal("unavailable rollback changed original bytes")
			}
			target.Command = current.Command
			if _, err := PrepareWorkerSettings(previous.Root, current, target, harness.WorkerSettings{Expected: sha256.Sum256(before), YAML: encoded}); err == nil {
				t.Fatal("availability error permitted a runtime mismatch")
			}
		})
	}
}
