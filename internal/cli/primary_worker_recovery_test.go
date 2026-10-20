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
	"github.com/agent0ai/spynel/internal/fsx"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/instance"
	"github.com/agent0ai/spynel/internal/localapi"
	"github.com/agent0ai/spynel/internal/orchestrator"
	"github.com/agent0ai/spynel/internal/updater"
	"github.com/agent0ai/spynel/internal/workspace"
)

func TestPrimaryRecoversConfirmedDeadWorker(t *testing.T) {
	for _, role := range []string{"workflow", "conversation"} {
		for _, cancelled := range []bool{false, true} {
			name := role + "/recover"
			if cancelled {
				name = role + "/cancelled"
			}
			t.Run(name, func(t *testing.T) { primaryWorkerRecovery(t, role, cancelled, false) })
		}
	}
}

func TestPrimaryRecoversConfirmedDeadIncompatibleWorker(t *testing.T) {
	primaryWorkerRecovery(t, "workflow", false, true)
}

func primaryWorkerRecovery(t *testing.T, role string, cancelled, incompatible bool) {
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
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	executable, _ := os.Executable()
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
	log, err := os.Create(filepath.Join(root, "controller.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	controller := exec.Command(executable, "__primary_worker_fixture", cfg.Path)
	controller.Stdout, controller.Stderr = log, log
	if err := controller.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- controller.Wait() }()
	var worker *harness.WorkerClient
	t.Cleanup(func() {
		shutdown := false
		if worker != nil && updater.VerifyProcess(worker.Info().Process) == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := worker.Call(ctx, harness.WorkerRequest{Action: "shutdown"}); err != nil {
				t.Error(err)
			} else {
				shutdown = true
			}
		}
		_ = controller.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-done:
			if err != nil {
				t.Error("controller exit", err)
			}
		case <-time.After(10 * time.Second):
			_ = controller.Process.Kill()
			<-done
			t.Error("controller required forced cleanup")
		}
		if worker != nil {
			worker.Close()
			if shutdown {
				waitWorkerAgent(t, func() bool {
					_, err := os.Stat(filepath.Dir(worker.Info().Socket))
					return os.IsNotExist(err) && errors.Is(updater.VerifyProcess(worker.Info().Process), syscall.ESRCH)
				})
			}
		}
	})
	ready, cancel := context.WithTimeout(t.Context(), localapi.ReadinessTimeout)
	defer cancel()
	if _, err := api.WaitReady(ready); err != nil {
		t.Fatal("ordinary primary readiness", err)
	}
	lock, err := harness.ProcessOwnershipPath(root)
	if err != nil {
		t.Fatal(err)
	}
	readRecord := func() harness.WorkerInfo {
		t.Helper()
		data, err := os.ReadFile(lock + ".worker.json")
		var record struct{ Info harness.WorkerInfo }
		if err != nil || json.Unmarshal(data, &record) != nil {
			t.Fatal("ordinary worker rendezvous", err)
		}
		return record.Info
	}
	open := func() {
		t.Helper()
		lease, err := owner.Current()
		if err != nil {
			t.Fatal(err)
		}
		runtime, err := cfg.RuntimeHarness(harness.HarnessConfig{Version: "synthetic-version"})
		if err != nil {
			t.Fatal(err)
		}
		worker, err = harness.OpenWorker(t.Context(), runtime, lease.InstanceID, lease.Token)
		if err != nil {
			t.Fatal(err)
		}
	}
	initialInfo := readRecord() // Observe before OpenWorker, which could launch.
	open()
	inspect := func() harness.WorkerReply {
		t.Helper()
		reply, err := worker.Call(t.Context(), harness.WorkerRequest{Action: "inspect"})
		if err != nil {
			t.Fatal(err)
		}
		return reply
	}
	var task string
	if role == "workflow" {
		task, err = orchestrator.Create(cfg, "tasks", "synthetic automatic recovery", "")
		if err != nil {
			t.Fatal(err)
		}
	} else {
		request, stop := context.WithCancel(t.Context())
		defer stop()
		go func() {
			_ = api.Handle(request, core.Message{Channel: "cli", Conversation: "recovering", Text: "synthetic request", SourceMessageID: "source"}, func(core.Event) {})
		}()
	}
	waitWorkerAgent(t, func() bool { return len(inspect().Executions) == 1 && inspect().Executions[0].ThreadID != "" })
	initial := inspect().Executions[0]
	lease, _ := owner.Current()
	if role == "workflow" && !cancelled {
		// A temporarily missing endpoint must not replace a verified live worker.
		if err := os.Rename(initialInfo.Socket, initialInfo.Socket+".held"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(instance.HeartbeatInterval + time.Second)
		if err := os.Rename(initialInfo.Socket+".held", initialInfo.Socket); err != nil {
			t.Fatal(err)
		}
		current, err := owner.Current()
		if err != nil || current.Token != lease.Token || readRecord().Process != initialInfo.Process {
			t.Fatal("endpoint outage replaced a live worker or controller term", err)
		}
	}
	if cancelled {
		command := "/stop"
		if role == "workflow" {
			command = "/job kill 1"
		}
		if err := api.Handle(t.Context(), core.Message{Channel: "cli", Conversation: "recovering", Text: command, SourceMessageID: "stop"}, func(core.Event) {}); err != nil {
			t.Fatal(err)
		}
	}
	guardian := workerCrashGuardian(t)
	t.Logf("automatic recovery worker pid=%d start=%s generation=%s; guardian pid=%d start=%s generation=%s", initialInfo.PID, initialInfo.Process.StartTime, initialInfo.Process.Generation, guardian.PID, guardian.StartTime, guardian.Generation)
	if err := updater.VerifyProcess(initialInfo.Process); err != nil {
		t.Fatal(err)
	}
	if role == "conversation" && !cancelled {
		// Model a checkpoint write interrupted by the imminent native death.
		if err := os.WriteFile(filepath.Join(filepath.Dir(initialInfo.Socket), ".spynel-write-1234567890"), []byte{0xff, '{'}, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if incompatible {
		// Model a previous protocol's strict current-schema crash-left record;
		// a separate retained native test also runs the actual older image.
		data, err := os.ReadFile(lock + ".worker.json")
		var record map[string]json.RawMessage
		if err != nil || json.Unmarshal(data, &record) != nil {
			t.Fatal("read fixture rendezvous", err)
		}
		info := initialInfo
		info.Protocol--
		record["Info"], err = json.Marshal(info)
		if err != nil {
			t.Fatal(err)
		}
		data, err = json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := fsx.AtomicWriteFile(lock+".worker.json", data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	process, err := os.FindProcess(initialInfo.PID)
	if err != nil {
		t.Fatal(err)
	}
	defer process.Release()
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	obsolete := worker
	defer obsolete.Close()
	waitWorkerAgent(t, func() bool {
		return errors.Is(updater.VerifyProcess(initialInfo.Process), syscall.ESRCH) && errors.Is(updater.VerifyProcess(guardian), syscall.ESRCH)
	})
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(initialInfo.Socket)) })
	// Read-only observation proves the ordinary controller launched recovery.
	waitWorkerAgent(t, func() bool {
		data, err := os.ReadFile(lock + ".worker.json")
		var record struct{ Info harness.WorkerInfo }
		return err == nil && json.Unmarshal(data, &record) == nil && record.Info.ID != initialInfo.ID
	})
	currentInfo := readRecord()
	open()
	currentLease, err := owner.Current()
	if err != nil || currentLease.InstanceID != lease.InstanceID || currentLease.Token == lease.Token || currentInfo.Process != worker.Info().Process {
		t.Fatal("worker death did not rebuild the same controller's fixed binding", err)
	}
	t.Logf("automatic replacement worker pid=%d start=%s generation=%s", currentInfo.PID, currentInfo.Process.StartTime, currentInfo.Process.Generation)
	if cancelled {
		time.Sleep(2 * time.Second) // Repeated ordinary scans retain the pause.
		if len(inspect().Executions) != 0 {
			t.Fatal("cancelled work was redispatched after worker death")
		}
	} else {
		waitWorkerAgent(t, func() bool { return len(inspect().Executions) == 1 })
		current := inspect().Executions[0]
		if role == "workflow" && (current.Workflow.LeaseID != initial.Workflow.LeaseID || current.Reservation.ProviderIterations != 2) {
			t.Fatal("automatic recovery lost its workflow claim or repeated reservation")
		}
		command := "/stop"
		if role == "workflow" {
			command = "/job kill 1"
		}
		if err := api.Handle(t.Context(), core.Message{Channel: "cli", Conversation: "recovering", Text: command, SourceMessageID: "final-stop"}, func(core.Event) {}); err != nil {
			t.Fatal(err)
		}
		waitWorkerAgent(t, func() bool { return len(inspect().Executions) == 0 })
	}
	want := 2
	if cancelled {
		want = 1
	}
	data, err := os.ReadFile(methods)
	if err != nil || strings.Count(string(data), "turn/start\n") != want {
		t.Fatal("unexpected actual provider admission count", err)
	}
	if task != "" {
		document, err := orchestrator.ReadDocument(cfg.StatePath("tasks", "working", filepath.Base(task)))
		if err != nil || document.FrontMatter["provider_iterations"] != want {
			t.Fatal("automatic recovery inflated durable provider reservations", err)
		}
	}
}
