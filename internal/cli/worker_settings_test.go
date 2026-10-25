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
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/instance"
	"github.com/agent0ai/spynel/internal/workspace"
)

type workerSettingsStartup struct{ enabled bool }

func (s *workerSettingsStartup) Sync(_ config.Config, enabled bool) error {
	s.enabled = enabled
	return nil
}

func TestWorkerSettingsUnavailableCanonicalSaveAndRecovery(t *testing.T) {
	for _, name := range []string{"codex", "acp"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			bin := t.TempDir()
			t.Setenv("PATH", bin)
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := workspace.Init(root, false); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(config.PathForRoot(root))
			if err != nil {
				t.Fatal(err)
			}
			command := filepath.Join(bin, "codex")
			model := "pending-model"
			if name == "acp" {
				command = filepath.Join(bin, "custom provider")
				model = ""
			}
			if _, err := config.SetSettings(&cfg, map[string]string{"harness.name": name, "harness.acp_command": command, "harness.reasoning_effort": "inherit", "harness.sandbox": "read-only"}); err != nil {
				t.Fatal(err)
			}
			cfg.Orchestrator.SemanticHeartbeatMinutes = 0
			if err := config.Save(cfg); err != nil {
				t.Fatal(err)
			}
			runtime, unavailable := cfg.RuntimeHarness(harness.HarnessConfig{Version: "synthetic-version"})
			if unavailable == nil || name == "acp" && runtime.Command != command {
				t.Fatal("missing canonical selection lost its command or availability")
			}
			f := &workerAgentFixture{cfg: cfg, harnessConfig: runtime, log: filepath.Join(root, "synthetic-methods")}
			t.Setenv("SPYNEL_TEST_WORKER_PROVIDER", "1")
			t.Setenv("SPYNEL_TEST_WORKER_EVIDENCE", f.log)
			completion := filepath.Join(root, "complete")
			t.Setenv("SPYNEL_TEST_WORKER_COMPLETIONS", completion)
			f.open(t)
			t.Cleanup(func() {
				f.stop(t)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if _, err := f.client.Call(ctx, harness.WorkerRequest{Action: "shutdown"}); err != nil {
					t.Error(err)
				}
				f.client.Close()
				_ = f.owner.Release(f.term)
				waitWorkerAgent(t, func() bool { _, err := os.Stat(filepath.Dir(f.client.Info().Socket)); return os.IsNotExist(err) })
			})
			identity := f.client.Info()
			t.Logf("unavailable canonical worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
			f.client.Close()
			restore := dropWorkerReply(t, identity.Socket, "configure", true, false)
			_, saveErr := f.service.ApplySettings(map[string]string{"harness.model": model, "orchestrator.interval_seconds": "19"})
			restore()
			f.client.Close()
			if saveErr != nil {
				t.Fatal("valid unavailable canonical intent was rejected", saveErr)
			}
			saved, err := config.Load(cfg.Path)
			if err != nil || saved.Harness.Model != model || saved.Orchestrator.IntervalSec != 19 || f.service.Settings.Snapshot().Harness.Model != model {
				t.Fatal("unavailable canonical/application selection did not commit", err)
			}
			runtime, unavailable = saved.RuntimeHarness(runtime)
			inspection, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "inspect"})
			if err != nil || unavailable == nil || inspection.ConfigDigest != harness.ConfigDigest(runtime) || inspection.Availability == nil || inspection.Availability.Available || inspection.Availability.Detail == "" || inspection.NextSequence != 1 || len(inspection.Executions) != 0 {
				t.Fatal("unavailable save invented readiness or provider work", err)
			}
			if _, err := os.Stat(f.log); !os.IsNotExist(err) {
				t.Fatal("unavailable canonical save ran a provider", err)
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
				t.Fatal("successor ownership failed", err)
			}
			if _, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "inspect"}); err == nil {
				t.Fatal("unavailable save retained obsolete control")
			}
			f.client.Close()
			f.client, err = harness.OpenWorker(t.Context(), runtime, f.owner.ID(), f.term)
			if err != nil {
				t.Fatal("saved unavailable selection could not reattach", err)
			}
			if f.client.Info().Process != identity.Process || f.client.Info().ID != identity.ID {
				t.Fatal("unavailable settings/controller replacement changed worker identity")
			}
			f.service = app.New(saved, harness.NewSupervisor(harness.NewBuiltinRegistry(), runtime))
			f.service.Orchestrator.Worker = f.client
			executable, _ := os.Executable()
			if err := os.Symlink(executable, command); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.ApplySettings(map[string]string{"harness.model": model}); err != nil {
				t.Fatal("explicit unchanged save did not recover the installed provider", err)
			}
			inspection, err = f.client.Call(t.Context(), harness.WorkerRequest{Action: "inspect"})
			if err != nil || inspection.Availability == nil || !inspection.Availability.Available || inspection.NextSequence != 1 || len(inspection.Executions) != 0 {
				t.Fatal("installation recovery replaced worker or admitted provider work", err)
			}
			before, _ := os.ReadFile(saved.Path)
			if _, err := f.service.ApplySettings(map[string]string{"harness.name": "acp", "harness.acp_command": filepath.Join(bin, "still missing")}); err == nil {
				t.Fatal("unavailable selection replaced a working provider")
			}
			if after, _ := os.ReadFile(saved.Path); !bytes.Equal(after, before) {
				t.Fatal("refused unavailable replacement changed canonical settings")
			}
			inspection, err = f.client.Call(t.Context(), harness.WorkerRequest{Action: "inspect"})
			if err != nil || inspection.Availability == nil || !inspection.Availability.Available {
				t.Fatal("refused unavailable replacement stopped the provider", err)
			}
			send, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "send", Sequence: 1, Key: "synthetic/settings", Prompt: "synthetic recovered selection"})
			if err != nil || len(send.Executions) != 1 || send.Executions[0].Done {
				t.Fatal("recovered selection did not admit the explicit turn", err)
			}
			if err := os.WriteFile(completion+".1", nil, 0600); err != nil {
				t.Fatal(err)
			}
			waitWorkerAgent(t, func() bool { return f.inspect(t)[0].Done })
			methods, err := os.ReadFile(f.log)
			method := "turn/start\n"
			if name == "acp" {
				method = "session/prompt\n"
			}
			if err != nil || strings.Count(string(methods), method) != 1 {
				t.Fatal("settings/replacement introduced extra provider turns", err)
			}
		})
	}
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
	refreshes, refreshedTimeout := 0, 0
	refreshSpeech := func() {
		saved, err := config.Load(f.cfg.Path)
		if err != nil || f.service.Settings.Snapshot().Speech != saved.Speech {
			t.Fatal("speech refresh preceded confirmed canonical/application publication", err)
		}
		refreshes++
		refreshedTimeout = saved.Speech.IdleTimeoutSec
	}
	f.service.RefreshSpeechIdleTimeout = refreshSpeech
	identity := f.client.Info()
	t.Logf("canonical settings worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
	first, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "send", Sequence: 1, Key: "synthetic/settings", Prompt: "synthetic initial turn"})
	if err != nil || len(first.Executions) != 1 || first.Executions[0].Done {
		t.Fatal("native initial admission failed", err)
	}
	before, _ := f.cfg.Encode()
	f.client.Close()
	restore := dropWorkerReply(t, f.client.Info().Socket, "configure", false, false)
	if _, err := f.service.ApplySettings(map[string]string{"harness.model": "not-admitted", "speech.idle_timeout_seconds": "300"}); err == nil {
		t.Fatal("absent worker settings request claimed a commit")
	}
	restore()
	f.client.Close()
	if actual, _ := os.ReadFile(f.cfg.Path); !bytes.Equal(actual, before) || f.service.Settings.Snapshot().Harness.Model == "not-admitted" || refreshes != 0 || f.service.Settings.Snapshot().Speech.IdleTimeoutSec != 120 {
		t.Fatal("absent worker request changed canonical/application settings")
	}
	restore = dropWorkerReply(t, f.client.Info().Socket, "configure", true, false)
	if _, err := f.service.ApplySettings(map[string]string{"harness.model": "model-a", "harness.reasoning_effort": "high", "harness.service_mode": "priority", "orchestrator.interval_seconds": "19", "speech.idle_timeout_seconds": "300", "channels.verbose": "off"}); err != nil {
		t.Fatal(err)
	}
	restore()
	f.client.Close()
	if refreshes != 1 || refreshedTimeout != 300 || f.service.SharedState().Verbose != "off" {
		t.Fatal("reconciled worker save did not refresh speech exactly once")
	}
	// A timeout-only save must not replace the worker or admit another provider turn.
	if _, err := f.service.ApplySettings(map[string]string{"speech.idle_timeout_seconds": "600", "channels.verbose": "on"}); err != nil {
		t.Fatal(err)
	}
	if refreshes != 2 || refreshedTimeout != 600 || f.client.Info().Process != identity.Process {
		t.Fatal("timeout-only save lost speech refresh or retained worker identity")
	}
	saved, err := config.Load(f.cfg.Path)
	if err != nil || saved.Channels.Verbose != "on" || f.service.SharedState().Verbose != "on" || saved.Harness.Model != "model-a" || saved.Harness.ServiceMode != "priority" || saved.Orchestrator.IntervalSec != 19 || f.service.Settings.Snapshot().Harness.Model != "model-a" {
		t.Fatal("canonical/application selection did not commit", err)
	}
	encoded, _ := saved.Encode()
	runtime, _ = saved.RuntimeHarness(runtime)
	inspection, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "inspect"})
	if err != nil || inspection.ConfigDigest != harness.ConfigDigest(runtime) || inspection.NextSequence != 2 || inspection.Executions[0].Done || inspection.Executions[0].ThreadID != first.Executions[0].ThreadID {
		t.Fatal("settings replaced or redispatched retained work", err)
	}
	// A structural change must fail before either canonical or runtime mutation.
	if _, err := f.service.ApplySettings(map[string]string{"harness.sandbox": "workspace-write", "speech.idle_timeout_seconds": "700"}); err == nil {
		t.Fatal("active retained work was replaced")
	}
	actual, _ := os.ReadFile(saved.Path)
	if !bytes.Equal(actual, encoded) || f.service.Settings.Snapshot().Harness.Sandbox != "read-only" || refreshes != 2 || f.service.Settings.Snapshot().Speech.IdleTimeoutSec != 600 {
		t.Fatal("rejected structural change altered saved/published settings")
	}
	if _, err := f.service.ApplySettings(map[string]string{"harness.service_mode": "unsupported"}); err == nil {
		t.Fatal("unverified service mode was saved")
	}
	if _, err := f.service.ApplySettings(map[string]string{"speech.idle_timeout_seconds": "1.5"}); err == nil {
		t.Fatal("invalid speech timeout was saved")
	}
	if actual, _ := os.ReadFile(saved.Path); !bytes.Equal(actual, encoded) || refreshes != 2 {
		t.Fatal("invalid timeout changed canonical speech or refreshed it")
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
	f.service.RefreshSpeechIdleTimeout = refreshSpeech
	if f.service.SharedState().Verbose != "on" {
		t.Fatal("controller replacement lost global verbose")
	}
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
	if _, err := f.service.ApplySettings(map[string]string{"startup.enabled": "true", "speech.idle_timeout_seconds": "900"}); err == nil || !strings.Contains(err.Error(), "commit unconfirmed") {
		t.Fatal("unreachable confirmation invented a settings result", err)
	}
	if current, err := config.Load(saved.Path); err != nil || !current.Startup.Enabled || !startup.enabled || f.service.Settings.Snapshot().Startup.Enabled || current.Speech.IdleTimeoutSec != 900 || f.service.Settings.Snapshot().Speech.IdleTimeoutSec != 600 || refreshes != 2 {
		t.Fatal("unconfirmed commit was published or its native registration was undone", err)
	}
	restore()
	f.client.Close()
	if _, err := f.service.ApplySettings(map[string]string{"startup.enabled": "true", "speech.idle_timeout_seconds": "900"}); err != nil || !f.service.Settings.Snapshot().Startup.Enabled || !startup.enabled || refreshes != 3 || refreshedTimeout != 900 {
		t.Fatal("explicit same-selection retry did not reconcile native and canonical settings", err)
	}
	if _, err := f.client.Call(context.Background(), harness.WorkerRequest{Action: "interrupt", Key: "synthetic/settings"}); err != nil {
		t.Fatal(err)
	}
}
