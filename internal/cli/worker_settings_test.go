//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/instance"
)

type workerSettingsStartup struct{ enabled bool }

func (s *workerSettingsStartup) Sync(_ config.Config, enabled bool) error {
	s.enabled = enabled
	return nil
}
func (s *workerSettingsStartup) Enabled(config.Config) (bool, error) { return s.enabled, nil }

func TestWorkerSettingsCanonicalApplicationCommitAndControllerReplacement(t *testing.T) {
	executable, _ := os.Executable()
	bin := t.TempDir()
	if err := os.Symlink(executable, filepath.Join(bin, "codex")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	f := newWorkerAgentFixture(t)
	f.cfg.Orchestrator.SemanticHeartbeatMinutes = 0
	// Align the existing native fixture with the canonical application's paths.
	if _, err := config.SetSettings(&f.cfg, map[string]string{"harness.name": "codex", "harness.sandbox": "read-only", "harness.reasoning_effort": "inherit"}); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(f.cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := f.cfg.RuntimeHarness(f.harnessConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "configure", Config: &runtime, ExpectedConfig: f.client.Info().ConfigDigest}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = app.New(f.cfg, harness.NewSupervisor(harness.NewBuiltinRegistry(), runtime))
	f.service.Orchestrator.Worker = f.client
	identity := f.client.Info()
	t.Logf("canonical settings worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
	first, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "send", Sequence: 1, Key: "synthetic/settings", Prompt: "synthetic initial turn"})
	if err != nil || len(first.Executions) != 1 || first.Executions[0].Done {
		t.Fatal("native initial admission failed", err)
	}
	before, _ := f.cfg.Encode()
	f.client.Close()
	restore := dropWorkerReply(t, f.client.Info().Socket, "configure", false, false)
	if _, err := f.service.ApplySettings(map[string]string{"harness.model": "not-admitted"}); err == nil {
		t.Fatal("absent worker settings request claimed a commit")
	}
	restore()
	f.client.Close()
	if actual, _ := os.ReadFile(f.cfg.Path); !bytes.Equal(actual, before) || f.service.Settings.Snapshot().Harness.Model == "not-admitted" {
		t.Fatal("absent worker request changed canonical/application settings")
	}
	restore = dropWorkerReply(t, f.client.Info().Socket, "configure", true, false)
	if _, err := f.service.ApplySettings(map[string]string{"harness.model": "model-a", "harness.reasoning_effort": "high", "harness.service_mode": "priority", "orchestrator.interval_seconds": "19"}); err != nil {
		t.Fatal(err)
	}
	restore()
	f.client.Close()
	saved, err := config.Load(f.cfg.Path)
	if err != nil || saved.Harness.Model != "model-a" || saved.Harness.ServiceMode != "priority" || saved.Orchestrator.IntervalSec != 19 || f.service.Settings.Snapshot().Harness.Model != "model-a" {
		t.Fatal("canonical/application selection did not commit", err)
	}
	encoded, _ := saved.Encode()
	runtime, _ = saved.RuntimeHarness(runtime)
	inspection, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "inspect"})
	if err != nil || inspection.ConfigDigest != harness.ConfigDigest(runtime) || inspection.NextSequence != 2 || inspection.Executions[0].Done || inspection.Executions[0].ThreadID != first.Executions[0].ThreadID {
		t.Fatal("settings replaced or redispatched retained work", err)
	}
	// A structural change must fail before either canonical or runtime mutation.
	if _, err := f.service.ApplySettings(map[string]string{"harness.sandbox": "workspace-write"}); err == nil {
		t.Fatal("active retained work was replaced")
	}
	actual, _ := os.ReadFile(saved.Path)
	if !bytes.Equal(actual, encoded) || f.service.Settings.Snapshot().Harness.Sandbox != "read-only" {
		t.Fatal("rejected structural change altered saved/published settings")
	}
	if _, err := f.service.ApplySettings(map[string]string{"harness.service_mode": "unsupported"}); err == nil {
		t.Fatal("unverified service mode was saved")
	}
	// Repeated explicit confirmation after a lost reply has no provider admission.
	request := harness.WorkerRequest{Action: "configure", Config: &runtime, ExpectedConfig: identity.ConfigDigest,
		Settings: &harness.WorkerSettings{Expected: sha256.Sum256(before), YAML: encoded}}
	if _, err := f.client.Call(t.Context(), request); err != nil {
		t.Fatal("applied retry failed", err)
	}
	f.stop(t)
	if err := f.owner.Release(f.term); err != nil {
		t.Fatal(err)
	}
	f.owner, err = instance.New(saved.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	f.term, _ = f.owner.NewToken()
	if _, ok, err := f.owner.TryAcquire("127.0.0.1:10002", f.term); err != nil || !ok {
		t.Fatal("successor owner failed", err)
	}
	if _, err := f.client.Call(t.Context(), request); err == nil {
		t.Fatal("obsolete controller changed canonical settings")
	}
	f.client.Close()
	f.client, err = harness.OpenWorker(t.Context(), runtime, f.owner.ID(), f.term)
	if err != nil {
		t.Fatal(err)
	}
	if current := f.client.Info(); current.ID != identity.ID || current.Process != identity.Process {
		t.Fatal("canonical save/controller replacement changed native worker identity")
	}
	f.service = app.New(saved, harness.NewSupervisor(harness.NewBuiltinRegistry(), runtime))
	f.service.Orchestrator.Worker = f.client
	// Release the first turn and exercise a real next provider dispatch.
	if _, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "interrupt", Key: "synthetic/settings"}); err != nil {
		t.Fatal(err)
	}
	waitWorkerAgent(t, func() bool { return f.inspect(t)[0].Done })
	state := f.inspect(t)[0]
	if _, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "commit", Sequence: 1, After: state.LastCursor}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "ack", Sequence: 1, After: state.LastCursor}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "send", Sequence: 2, Key: "synthetic/settings", Prompt: "synthetic committed selection"}); err != nil {
		t.Fatal(err)
	}
	methods, err := os.ReadFile(f.log)
	if err != nil || strings.Count(string(methods), "turn/start\n") != 2 {
		t.Fatal("attachment/configuration introduced provider admissions", err)
	}
	var selections []map[string]any
	for _, line := range strings.Split(string(methods), "\n") {
		if strings.HasPrefix(line, "selection:") {
			var value map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "selection:")), &value) != nil {
				t.Fatal("invalid synthetic provider selection evidence")
			}
			selections = append(selections, value)
		}
	}
	if len(selections) != 2 || selections[0]["model"] == "model-a" || selections[1]["model"] != "model-a" || selections[1]["effort"] != "high" || selections[1]["serviceTierForTurn"] != "priority" {
		t.Fatalf("provider did not retain old/adopt new snapshots: %+v", selections)
	}
	// A canonical edit outside this application snapshot fails before runtime mutation.
	changed := saved
	changed.Orchestrator.IntervalSec = 23
	if err := config.Save(changed); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ApplySettings(map[string]string{"harness.model": "obsolete"}); err == nil {
		t.Fatal("stale application snapshot overwrote canonical settings")
	}
	if current, err := config.Load(saved.Path); err != nil || current.Orchestrator.IntervalSec != 23 || current.Harness.Model != "model-a" {
		t.Fatal("conflict refusal lost canonical data", err)
	}
	if err := config.Save(saved); err != nil {
		t.Fatal(err)
	}
	startup := &workerSettingsStartup{}
	f.service.Startup = startup
	f.client.Close()
	restore = dropWorkerReply(t, f.client.Info().Socket, "configure", true, true)
	if _, err := f.service.ApplySettings(map[string]string{"startup.enabled": "true"}); err == nil || !strings.Contains(err.Error(), "commit unconfirmed") {
		t.Fatal("unreachable confirmation invented a settings result", err)
	}
	if current, err := config.Load(saved.Path); err != nil || !current.Startup.Enabled || !startup.enabled || f.service.Settings.Snapshot().Startup.Enabled {
		t.Fatal("unconfirmed commit was published or its native registration was undone", err)
	}
	restore()
	f.client.Close()
	if _, err := f.service.ApplySettings(map[string]string{"startup.enabled": "true"}); err != nil || !f.service.Settings.Snapshot().Startup.Enabled || !startup.enabled {
		t.Fatal("explicit same-selection retry did not reconcile native and canonical settings", err)
	}
	if _, err := f.client.Call(context.Background(), harness.WorkerRequest{Action: "interrupt", Key: "synthetic/settings"}); err != nil {
		t.Fatal(err)
	}
}
