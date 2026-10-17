//go:build linux || darwin

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/instance"
	"github.com/agent0ai/spynel/internal/orchestrator"
	"github.com/agent0ai/spynel/internal/updater"
	"github.com/agent0ai/spynel/internal/workspace"
)

func TestWorkerWorkflowManagerRestoresJobWithoutDispatch(t *testing.T) {
	for _, moved := range []bool{false, true} {
		name := "active-claim"
		if moved {
			name = "moved-before-attachment"
		}
		t.Run(name, func(t *testing.T) { workerWorkflowManagerFixture(t, moved) })
	}
}

func workerWorkflowManagerFixture(t *testing.T, movedEarly bool) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
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
	cfg.Orchestrator.IntervalSec = 60
	cfg.Orchestrator.SemanticHeartbeatMinutes = 0
	owner, err := instance.New(cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	term, _ := owner.NewToken()
	if _, ok, err := owner.TryAcquire("127.0.0.1:10001", term); err != nil || !ok {
		t.Fatal("owner acquisition failed", err)
	}
	executable, _ := os.Executable()
	log := filepath.Join(root, "synthetic-provider-methods")
	t.Setenv("SPYNEL_TEST_WORKER_PROVIDER", "1")
	t.Setenv("SPYNEL_TEST_WORKER_EVIDENCE", log)
	harnessConfig := harness.HarnessConfig{Name: "codex", Command: executable, Cwd: root, Sandbox: "read-only", ApprovalPolicy: "never", SessionsFile: filepath.Join(root, "sessions.json")}
	client, err := harness.OpenWorker(t.Context(), harnessConfig, owner.ID(), term)
	if err != nil {
		t.Fatal(err)
	}
	identity := client.Info()
	t.Logf("native workflow worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
	ctx, cancel := context.WithCancel(t.Context())
	service := app.New(cfg, harness.NewSupervisor(harness.NewBuiltinRegistry(), harnessConfig))
	service.Orchestrator.Worker = client
	t.Cleanup(func() {
		cancel()
		service.Orchestrator.Wait()
		_ = service.Close()
		_, shutdownErr := client.Call(context.Background(), harness.WorkerRequest{Action: "shutdown"})
		if shutdownErr != nil {
			t.Error(shutdownErr)
		}
		client.Close()
		_ = owner.Release(term)
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(filepath.Dir(client.Info().Socket)); os.IsNotExist(err) {
				break
			}
			if time.Now().After(deadline) {
				t.Error("worker endpoint/journal cleanup timed out")
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	path, err := orchestrator.Create(cfg, "tasks", "synthetic worker restoration", "")
	if err != nil {
		t.Fatal(err)
	}
	working := cfg.StatePath("tasks", "working", filepath.Base(path))
	completed := cfg.StatePath("tasks", "failed", filepath.Base(path))
	moveTask := func() {
		t.Helper()
		document, err := orchestrator.ReadDocument(working)
		if err != nil {
			t.Fatal(err)
		}
		document.FrontMatter["status"] = "failed"
		if err := orchestrator.WriteDocument(working, document); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(working, completed); err != nil {
			t.Fatal(err)
		}
	}
	delivered := make(chan harness.WorkerDelivery, 1)
	var failOnce sync.Once
	var failedDeliveries atomic.Int32
	service.Orchestrator.JobDelivery = func(id int, delivery harness.WorkerDelivery) error {
		if err := service.Runtime.RecordJobDelivery(id, delivery); err != nil {
			return err
		}
		failOnce.Do(func() { delivered <- delivery })
		failedDeliveries.Add(1)
		return errors.New("synthetic controller loss after archive sync before cursor commit")
	}
	firstRunDone := make(chan error, 1)
	firstManager := service.Orchestrator
	go func() { firstRunDone <- firstManager.Run(ctx) }()
	var effect harness.WorkerDelivery
	select {
	case effect = <-delivered:
	case <-time.After(10 * time.Second):
		reply, _ := client.Call(t.Context(), harness.WorkerRequest{Action: "inspect"})
		t.Fatalf("workflow did not reach durable job consumer: worker=%+v jobs=%+v logs=%+v", reply, service.Runtime.Jobs(), service.Runtime.Logs())
	}
	service.Orchestrator.Wait()
	time.Sleep(100 * time.Millisecond)
	if failedDeliveries.Load() != 1 {
		t.Fatal("failed worker delivery caused immediate no-progress scan churn")
	}
	jobs := service.Runtime.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("jobs=%d", len(jobs))
	}
	firstJob := jobs[0]
	before, err := client.Call(t.Context(), harness.WorkerRequest{Action: "inspect"})
	if err != nil || len(before.Executions) != 1 || before.Executions[0].CommittedCursor >= effect.Cursor {
		t.Fatal("failed effect was committed", err)
	}
	leaseID := before.Executions[0].Workflow.LeaseID
	key := before.Executions[0].Key
	if movedEarly {
		moveTask()
	}
	cancel()
	select {
	case <-firstRunDone:
	case <-time.After(3 * time.Second):
		t.Fatal("first manager did not detach")
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Release(term); err != nil {
		t.Fatal(err)
	}
	obsolete := client
	defer obsolete.Close()
	owner, err = instance.New(cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	term, _ = owner.NewToken()
	if _, ok, err := owner.TryAcquire("127.0.0.1:10002", term); err != nil || !ok {
		t.Fatal("replacement acquisition failed", err)
	}
	client, err = harness.OpenWorker(t.Context(), harnessConfig, owner.ID(), term)
	if err != nil {
		t.Fatal(err)
	}
	if client.Info().PID != identity.PID || client.Info().Process.StartTime != identity.Process.StartTime || client.Info().Process.Generation != identity.Process.Generation || updater.VerifyProcess(identity.Process) != nil {
		t.Fatal("worker execution changed")
	}
	if _, err := obsolete.Call(t.Context(), harness.WorkerRequest{Action: "interrupt", Key: key}); err == nil {
		t.Fatal("obsolete controller interrupted retained workflow")
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	service = app.New(cfg, harness.NewSupervisor(harness.NewBuiltinRegistry(), harnessConfig))
	service.Orchestrator.Worker = client
	leasePath := cfg.StatePath("runtime", "leases", leaseID+".json")
	leaseBytes, err := os.ReadFile(leasePath)
	if err != nil {
		t.Fatal(err)
	}
	var changed orchestrator.Lease
	if err := json.Unmarshal(leaseBytes, &changed); err != nil {
		t.Fatal(err)
	}
	changed.ClaimAttempt++
	changedBytes, _ := json.Marshal(changed)
	if err := os.WriteFile(leasePath, changedBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if err := service.Orchestrator.ScanOnce(ctx); err == nil {
		t.Fatal("changed retained claim was adopted")
	}
	if err := os.WriteFile(leasePath, leaseBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(leasePath, leasePath+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := service.Orchestrator.ScanOnce(ctx); err == nil {
		t.Fatal("missing lease dispatched replacement work")
	}
	if err := os.Symlink(leasePath+".saved", leasePath); err != nil {
		t.Fatal(err)
	}
	if err := service.Orchestrator.ScanOnce(ctx); err == nil {
		t.Fatal("symlinked lease was adopted")
	}
	if err := os.Remove(leasePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(leasePath+".saved", leasePath); err != nil {
		t.Fatal(err)
	}
	admit := service.Orchestrator.JobStarted
	service.Orchestrator.JobStarted = func(orchestrator.Lease, string, time.Time, int, int) (int, error) {
		return 0, errors.New("synthetic replacement job admission failure")
	}
	if err := service.Orchestrator.ScanOnce(ctx); err != nil {
		t.Fatal(err)
	}
	service.Orchestrator.Wait()
	rejected, err := client.Call(t.Context(), harness.WorkerRequest{Action: "inspect"})
	if err != nil || rejected.Executions[0].CommittedCursor != 0 {
		t.Fatal("job admission failure consumed worker events", err)
	}
	service.Orchestrator.JobStarted = admit
	if err := service.Orchestrator.ScanOnce(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		reply, err := client.Call(t.Context(), harness.WorkerRequest{Action: "inspect"})
		if err != nil {
			t.Fatal(err)
		}
		if len(reply.Executions) == 1 && reply.Executions[0].CommittedCursor >= effect.Cursor {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("replacement consumer did not settle replay")
		}
		time.Sleep(10 * time.Millisecond)
	}
	jobs = service.Runtime.Jobs()
	if len(jobs) != 1 || jobs[0].Number != firstJob.Number || jobs[0].StableID != firstJob.StableID || jobs[0].Generation != firstJob.Generation {
		t.Fatal("restoration changed public job identity")
	}
	for i := 0; i < 3; i++ {
		if err := service.Orchestrator.ScanOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	currentPath := working
	if movedEarly {
		currentPath = completed
	}
	document, err := orchestrator.ReadDocument(currentPath)
	if err != nil || document.FrontMatter["provider_iterations"] != 1 || document.FrontMatter["attempt"] != 1 {
		t.Fatalf("attachment changed durable counters: %v, %v", document.FrontMatter, err)
	}
	data, err := os.ReadFile(log)
	if err != nil || strings.Count(string(data), "turn/start\n") != 1 {
		t.Fatal("attachment dispatched another provider turn", err)
	}
	// A durable task move remains pending until its retained terminal is consumed.
	if !movedEarly {
		moveTask()
	}

	if err := service.Orchestrator.ScanOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.StatePath("runtime", "leases", leaseID+".json")); err != nil {
		t.Fatal("transition retired an unconsumed execution")
	}
	if _, err := client.Call(t.Context(), harness.WorkerRequest{Action: "interrupt", Key: key}); err != nil {
		t.Fatal(err)
	}
	service.Orchestrator.Wait()
	if err := service.Orchestrator.ScanOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.StatePath("runtime", "leases", leaseID+".json")); !os.IsNotExist(err) {
		t.Fatal("settled transition retained its lease")
	}
	_, output, err := service.Runtime.ArchivedJob(firstJob.Number)
	if err != nil {
		t.Fatal(err)
	}
	if effect.Event.Text != "" && strings.Count(output, effect.Event.Text) != 1 {
		t.Fatal("uncommitted archive effect replayed twice")
	}
	reply, err := client.Call(t.Context(), harness.WorkerRequest{Action: "inspect"})
	if err != nil || len(reply.Executions) != 0 {
		t.Fatal("settled worker stream was not retired", err)
	}
}
