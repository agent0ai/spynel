//go:build linux || darwin

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/instance"
	"github.com/agent0ai/spynel/internal/localapi"
	"github.com/agent0ai/spynel/internal/updater"
	"github.com/agent0ai/spynel/internal/workspace"
)

func TestPrimaryWorkerSurvivesOrdinaryControllerReplacement(t *testing.T) {
	testPrimaryWorkerContinuity(t, []string{"synthetic-version", "synthetic-version", "synthetic-version"})
}

func TestPrimaryWorkerSurvivesVersionChangingControllerReplacement(t *testing.T) {
	testPrimaryWorkerContinuity(t, []string{"synthetic-version", "synthetic-next-version", "synthetic-final-version"})
}

func testPrimaryWorkerContinuity(t *testing.T, versions []string) {
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
	if _, err := config.SetSettings(&cfg, map[string]string{"harness.name": "codex", "harness.reasoning_effort": "inherit", "harness.sandbox": "read-only"}); err != nil {
		t.Fatal(err)
	}
	cfg.Orchestrator.IntervalSec, cfg.Orchestrator.SemanticHeartbeatMinutes = 1, 0
	cfg.Orchestrator.RetriggerUnrespondedMessages = false
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	executable, _ := os.Executable()
	// Only the provider image enters fixture protocol mode; controllers and
	// independent workers run their ordinary native entry points.
	script := "#!/bin/sh\nSPYNEL_TEST_WORKER_PROVIDER=1 exec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	methods := filepath.Join(root, "synthetic-methods")
	t.Setenv("SPYNEL_TEST_WORKER_EVIDENCE", methods)
	owner, err := instance.New(cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	api := localapi.NewClient(owner)
	defer api.HTTP.CloseIdleConnections()
	var controller *exec.Cmd
	var controllerDone chan error
	version := versions[0]
	start := func() {
		t.Helper()
		log, err := os.CreateTemp(root, "controller-*.log")
		if err != nil {
			t.Fatal(err)
		}
		defer log.Close()
		controller = exec.Command(executable, "__primary_worker_fixture", cfg.Path, version)
		controller.Stdout, controller.Stderr = log, log
		if err := controller.Start(); err != nil {
			t.Fatal(err)
		}
		controllerDone = make(chan error, 1)
		go func(cmd *exec.Cmd, done chan error) { done <- cmd.Wait() }(controller, controllerDone)
		ctx, cancel := context.WithTimeout(t.Context(), localapi.ReadinessTimeout)
		defer cancel()
		if _, err := api.WaitReady(ctx); err != nil {
			t.Fatal("ordinary primary readiness", err)
		}
		status, err := api.Status(ctx, "cli/surviving")
		if err != nil || status.Version != version {
			t.Fatal("ordinary controller did not serve its selected version", err)
		}
	}
	stop := func(signal os.Signal) {
		t.Helper()
		if err := controller.Process.Signal(signal); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-controllerDone:
			if signal == syscall.SIGTERM && err != nil {
				t.Fatal("graceful controller stop", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("controller did not exit")
		}
		controller = nil
	}
	var worker *harness.WorkerClient
	t.Cleanup(func() {
		if worker != nil {
			// This is authenticated fixture cleanup, never a PID-only signal.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := worker.Call(ctx, harness.WorkerRequest{Action: "shutdown"}); err != nil {
				t.Error("worker fixture shutdown", err)
			}
			worker.Close()
		}
		if controller != nil {
			_ = controller.Process.Signal(syscall.SIGTERM)
			select {
			case <-controllerDone:
			case <-time.After(10 * time.Second):
				_ = controller.Process.Kill()
				<-controllerDone
				t.Error("controller fixture required forced cleanup")
			}
		}
		if worker != nil {
			waitWorkerAgent(t, func() bool { _, err := os.Stat(filepath.Dir(worker.Info().Socket)); return os.IsNotExist(err) })
			waitWorkerAgent(t, func() bool { return errors.Is(updater.VerifyProcess(worker.Info().Process), syscall.ESRCH) })
		}
	})
	start()
	lock, err := harness.ProcessOwnershipPath(root)
	if err != nil {
		t.Fatal(err)
	}
	// Check normal startup created the rendezvous before calling OpenWorker;
	// otherwise a test-only launch could conceal missing production binding.
	data, err := os.ReadFile(lock + ".worker.json")
	var record struct{ Info harness.WorkerInfo }
	if err != nil || json.Unmarshal(data, &record) != nil || record.Info.PID <= 1 {
		t.Fatal("ordinary primary did not bind an independent worker", err)
	}
	open := func() {
		t.Helper()
		lease, err := owner.Current()
		if err != nil {
			t.Fatal(err)
		}
		runtime, err := cfg.RuntimeHarness(harness.HarnessConfig{Version: version})
		if err != nil {
			t.Fatal(err)
		}
		worker, err = harness.OpenWorker(t.Context(), runtime, lease.InstanceID, lease.Token)
		if err != nil {
			t.Fatal(err)
		}
		if worker.Info().Process != record.Info.Process || worker.Info().ID != record.Info.ID {
			t.Fatal("controller replacement changed native worker identity")
		}
	}
	open()
	t.Logf("ordinary worker pid=%d start=%s generation=%s", worker.Info().PID, worker.Info().Process.StartTime, worker.Info().Process.Generation)
	request, cancelRequest := context.WithCancel(t.Context())
	defer cancelRequest()
	requestDone := make(chan error, 1)
	go func() {
		requestDone <- api.Handle(request, core.Message{Channel: "cli", Conversation: "surviving", Text: "synthetic long-running request", SourceMessageID: "source"}, func(core.Event) {})
	}()
	inspect := func() harness.WorkerReply {
		t.Helper()
		reply, err := worker.Call(t.Context(), harness.WorkerRequest{Action: "inspect"})
		if err != nil {
			t.Fatal(err)
		}
		return reply
	}
	waitWorkerAgent(t, func() bool { return len(inspect().Executions) == 1 && inspect().Executions[0].ThreadID != "" })
	initial := inspect().Executions[0]
	for index, signal := range []os.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		obsolete := worker
		stop(signal)
		if signal == syscall.SIGTERM {
			select {
			case <-requestDone:
			case <-time.After(5 * time.Second):
				t.Fatal("controller shutdown did not release its live request")
			}
			cancelRequest()
		}
		if err := updater.VerifyProcess(record.Info.Process); err != nil {
			t.Fatal("controller exit killed the independent worker", err)
		}
		version = versions[index+1]
		start() // Abrupt exit uses ordinary stale takeover, without lease edits.
		open()
		if _, err := obsolete.Call(t.Context(), harness.WorkerRequest{Action: "interrupt", Key: initial.Key}); err == nil {
			t.Fatal("obsolete controller retained control")
		}
		obsolete.Close()
		current := inspect().Executions
		if len(current) != 1 || current[0].Sequence != initial.Sequence || current[0].ThreadID != initial.ThreadID || current[0].Done || current[0].ConversationTurns != initial.ConversationTurns {
			t.Fatal("ordinary primary replaced or completed the retained provider turn")
		}
	}
	// Settings must use the retained worker's runtime after a version change.
	if err := api.ApplySettings(t.Context(), map[string]string{"harness.model": "model-a"}); err != nil {
		t.Fatal("successor canonical settings", err)
	}
	configured := inspect()
	if configured.Config == nil || configured.Config.Version != versions[0] || configured.Config.Model != "model-a" {
		t.Fatal("successor settings changed worker image metadata or lost the selection")
	}
	if err := api.Handle(t.Context(), core.Message{Channel: "cli", Conversation: "surviving", Text: "/stop", SourceMessageID: "stop"}, func(core.Event) {}); err != nil {
		t.Fatal("successor application interruption", err)
	}
	waitWorkerAgent(t, func() bool { return len(inspect().Executions) == 0 })
	observed, err := os.ReadFile(methods)
	if err != nil || strings.Count(string(observed), "turn/start\n") != 1 || strings.Count(string(observed), "turn/interrupt\n") != 1 {
		t.Fatal("ordinary replacement duplicated provider admission or control", err)
	}
}
