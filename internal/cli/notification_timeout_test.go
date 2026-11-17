//go:build linux || darwin

package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/orchestrator"
)

func TestNotificationDeadlineCanonicalSaveToNativeWorkerDispatch(t *testing.T) {
	executable, _ := os.Executable()
	bin := t.TempDir()
	if err := os.Symlink(executable, filepath.Join(bin, "codex")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	f := newWorkerAgentFixture(t)
	f.cfg.Orchestrator.SemanticHeartbeatMinutes = 0
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
	m := f.service.Orchestrator
	m.Worker = f.client
	m.NotificationContext = func(orchestrator.Origin) (string, error) { return "synthetic original context", nil }
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	admit := func(title string) harness.WorkerExecution {
		t.Helper()
		file, err := orchestrator.CreateWithOptions(f.cfg, "tasks", title, "", orchestrator.CreateOptions{Notify: true, Origin: "cli/synthetic", Outcomes: []string{"failed"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := m.ScanOnce(ctx); err != nil {
			t.Fatal(err)
		}
		waitWorkerAgent(t, func() bool { xs := f.inspect(t); return len(xs) == 1 && xs[0].Workflow != nil && xs[0].ThreadID != "" })
		work := f.inspect(t)[0]
		path := f.cfg.StatePath("tasks", "working", filepath.Base(file))
		document, err := orchestrator.ReadDocument(path)
		if err != nil {
			t.Fatal(err)
		}
		document.FrontMatter["status"] = "failed"
		if err := orchestrator.WriteDocument(path, document); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path, f.cfg.StatePath("tasks", "failed", filepath.Base(file))); err != nil {
			t.Fatal(err)
		}
		if _, err := f.client.Call(ctx, harness.WorkerRequest{Action: "interrupt", Key: work.Key}); err != nil {
			t.Fatal(err)
		}
		m.Wait()
		if err := m.ScanOnce(ctx); err != nil {
			t.Fatal(err)
		}
		waitWorkerAgent(t, func() bool {
			xs := f.inspect(t)
			return len(xs) == 1 && xs[0].Agent != nil && xs[0].Agent.Kind == "notification" && xs[0].ThreadID != ""
		})
		return f.inspect(t)[0]
	}
	first := admit("first deadline")
	if first.Agent.Deadline.Sub(first.Agent.StartedAt) != 2*time.Minute {
		t.Fatal("default worker deadline", first.Agent)
	}
	if _, err := f.service.ApplySettings(map[string]string{"orchestrator.notification_timeout_seconds": "600"}); err != nil {
		t.Fatal(err)
	}
	saved, err := config.Load(f.cfg.Path)
	if err != nil || saved.Orchestrator.NotificationTimeoutSeconds != 600 || f.service.Settings.Snapshot().Orchestrator.NotificationTimeoutSeconds != 600 {
		t.Fatal("canonical application save/reload", err)
	}
	current := f.inspect(t)[0]
	if !current.Agent.Deadline.Equal(first.Agent.Deadline) || current.Sequence != first.Sequence {
		t.Fatal("save changed an admitted worker deadline")
	}
	if _, err := f.client.Call(ctx, harness.WorkerRequest{Action: "interrupt", Key: first.Key}); err != nil {
		t.Fatal(err)
	}
	m.Wait()
	if xs := f.inspect(t); len(xs) != 0 {
		t.Fatal("first notification not retired")
	}
	second := admit("next deadline")
	if second.Agent.Deadline.Sub(second.Agent.StartedAt) != 10*time.Minute {
		t.Fatal("next dispatch missed saved deadline", second.Agent)
	}
	if _, err := f.client.Call(ctx, harness.WorkerRequest{Action: "interrupt", Key: second.Key}); err != nil {
		t.Fatal(err)
	}
	m.Wait()
}
