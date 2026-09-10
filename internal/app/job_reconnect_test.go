package app

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/orchestrator"
	"github.com/agent0ai/spynel/internal/workspace"
)

// Adapter wire mapping is covered by TestCodexRetryRecoveryLifecycle. This
// exercises its provider-neutral sequence through real orchestration, leases,
// runtime/archive persistence, and the commands shared by every interface.
func TestJobReconnectAcrossSharedInspection(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	provider := newHeldServiceHarness()
	service := New(cfg, provider)
	defer service.Close()
	if _, err := orchestrator.Create(cfg, "tasks", "reconnect fixture", ""); err != nil {
		t.Fatal(err)
	}
	if err := service.Orchestrator.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	service.Orchestrator.Wait()
	jobs := service.Runtime.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("jobs = %#v", jobs)
	}
	job := jobs[0]
	provider.mu.Lock()
	emit := provider.emits[job.SessionKey]
	provider.mu.Unlock()
	if emit == nil {
		t.Fatal("orchestrator did not admit provider work")
	}
	check := func(state JobExecutionState) {
		t.Helper()
		current, ok := service.Runtime.Job(job.ID)
		if !ok || current.Execution != state || current.Health != healthFromExecution(state) {
			t.Fatalf("runtime job = %#v", current)
		}
		archived, _, err := service.Runtime.ArchivedJob(job.StableID)
		if err != nil || archived.State != string(state) {
			t.Fatalf("archive state = %q, err = %v", archived.State, err)
		}
		for _, channel := range []string{"tui", "cli", "telegram", "whatsapp"} {
			for _, command := range []string{"/jobs", "/jobs recent", fmt.Sprintf("/job info %d", job.Number)} {
				var output string
				if err := service.Handle(context.Background(), core.Message{Channel: channel, Conversation: "fixture", Text: command}, func(event core.Event) {
					if event.Kind == core.EventFinal {
						output = event.Text
					}
				}); err != nil {
					t.Fatal(err)
				}
				want := string(state)
				if strings.HasPrefix(command, "/job info") {
					want = "Execution status: " + strings.ReplaceAll(want, "_", " ")
				}
				if !strings.Contains(output, want) {
					t.Fatalf("%s %s missing %q: %s", channel, command, want, output)
				}
			}
		}
	}
	for range 2 {
		emit(core.Event{Kind: core.EventStatus, Text: "synthetic connection failure", Execution: &core.ExecutionStatus{State: "reconnecting", Detail: "synthetic connection failure"}})
		check(JobReconnecting)
		// Generic activity updates the lease heartbeat without proving recovery.
		emit(core.Event{Kind: core.EventStatus, Text: "tool remains active"})
		check(JobReconnecting)
	}
	emit(core.Event{Kind: core.EventStatus, Execution: &core.ExecutionStatus{State: "running"}})
	emit(core.Event{Kind: core.EventDelta, Text: "fresh model output"})
	check(JobRunning)
	if lease, ok := service.Orchestrator.LeaseForSession(job.SessionKey); !ok || lease.State != "processing" || lease.LastError != "" {
		t.Fatalf("retry status became a durable error: %#v", lease)
	}
	current, _ := service.Runtime.Job(job.ID)
	if current.StatusDetail != "" {
		t.Fatalf("recovered job retained current retry detail: %q", current.StatusDetail)
	}
	_, output, err := service.Runtime.ArchivedJob(job.StableID)
	if err != nil || !strings.Contains(output, "synthetic connection failure") {
		t.Fatalf("historical retry diagnostic lost: %v", err)
	}

	// A fresh service has no ownership, even though the document stays working.
	restarted := New(cfg, newServiceHarness())
	if len(restarted.Runtime.Jobs()) != 0 || !strings.Contains(runJobCommand(t, restarted, "/job info 1"), "State: interrupted") {
		t.Fatal("retained working task/archive was treated as a live execution")
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}

	emit(core.Event{Kind: core.EventError, Text: "retry exhausted", Execution: &core.ExecutionStatus{State: "error", Detail: "retry exhausted"}})
	check(JobError)
	emit(core.Event{Kind: core.EventStatus, Text: "generic activity"})
	check(JobError)
	emit(core.Event{Kind: core.EventError, Text: "retry exhausted", Done: true, Execution: &core.ExecutionStatus{State: "error", Detail: "retry exhausted"}})
	check(JobError)
	// A completed provider turn can await its durable transition, but a late
	// provider event cannot resurrect it as running.
	emit(core.Event{Kind: core.EventStatus, Execution: &core.ExecutionStatus{State: "running"}})
	if current, _ := service.Runtime.Job(job.ID); !executionStateIsTerminal(current.Execution) {
		t.Fatalf("late event resurrected failed execution: %#v", current)
	}
}
