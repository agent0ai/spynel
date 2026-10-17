//go:build linux || darwin

package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
)

type deadlineAgentHarness struct{ *supervisorHarness }

func (h *deadlineAgentHarness) Send(ctx context.Context, _, _ string, _ core.Emit) (string, bool, error) {
	<-ctx.Done()
	return "", false, ctx.Err()
}

func (h *deadlineAgentHarness) SendWithModel(ctx context.Context, key, prompt, _ string, emit core.Emit) (string, bool, error) {
	return h.Send(ctx, key, prompt, emit)
}

func TestWorkerAgentDeadlineBoundsInitialSend(t *testing.T) {
	registry := NewRegistry()
	registry.Register("codex", func(HarnessConfig) (Harness, error) { return &deadlineAgentHarness{&supervisorHarness{}}, nil })
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "codex"})
	if err := supervisor.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	host := &workerHost{supervisor: supervisor, directory: t.TempDir(), executions: map[uint64]*workerExecution{}, next: 1, stop: make(chan struct{})}
	now := time.Now().UTC()
	finished := make(chan WorkerReply, 1)
	go func() {
		finished <- host.send(WorkerRequest{Sequence: 1, Key: "orchestrator:semantic-heartbeat", Prompt: "synthetic", Agent: &WorkerAgent{ID: "synthetic-agent", Kind: "heartbeat", StartedAt: now, Deadline: now.Add(50 * time.Millisecond)}})
	}()
	select {
	case reply := <-finished:
		if len(reply.Executions) != 1 || !reply.Executions[0].Done || reply.Executions[0].Error != context.DeadlineExceeded.Error() {
			t.Fatal("initial send escaped its original deadline")
		}
	case <-time.After(time.Second):
		_ = supervisor.Close()
		t.Fatal("initial Send did not receive the agent deadline")
	}
	_ = host.executions[1].file.Close()
}

func TestWorkerAgentDeadlineSurvivesControllerReplacement(t *testing.T) {
	for _, kind := range []string{"heartbeat", "notification"} {
		t.Run(kind, func(t *testing.T) {
			f := newWorkerFixture(t, "codex", "codex-worker")
			now := time.Now().UTC()
			key := "orchestrator:semantic-heartbeat"
			if kind == "notification" {
				key = "orchestrator:notification:synthetic"
			}
			request := WorkerRequest{Action: "send", Sequence: 1, Key: key, Prompt: "synthetic deadline", Agent: &WorkerAgent{ID: "synthetic-agent", Kind: kind, StartedAt: now, Deadline: now.Add(1500 * time.Millisecond)}}
			first := f.mustCall(t, request).Executions[0]
			if first.Error != "" {
				t.Fatal(first.Error)
			}
			f.replaceOwner(t)
			deadline := time.Now().Add(5 * time.Second)
			var current WorkerExecution
			for {
				current = f.mustCall(t, WorkerRequest{Action: "inspect"}).Executions[0]
				if current.Done {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("original worker deadline did not interrupt")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if *current.Agent != *first.Agent || current.Error != context.DeadlineExceeded.Error() {
				t.Fatal("deadline/error changed during attachment")
			}
			if f.mustCall(t, WorkerRequest{Action: "session", Key: key}).Session.Active {
				t.Fatal("deadline left the actual provider turn active")
			}
			// A retry after expiry confirms the retained admission; it never extends it.
			if f.mustCall(t, request).Executions[0].Sequence != first.Sequence {
				t.Fatal("expired retry created a turn")
			}
			for {
				page := f.mustCall(t, WorkerRequest{Action: "events", Sequence: 1, After: current.CommittedCursor})
				after := current.CommittedCursor
				for _, event := range page.Events {
					after = event.Cursor
				}
				current = f.mustCall(t, WorkerRequest{Action: "commit", Sequence: 1, Expected: current.CommittedCursor, After: after}).Executions[0]
				if current.CommittedCursor == current.LastCursor {
					break
				}
			}
			f.mustCall(t, WorkerRequest{Action: "ack", Sequence: 1, After: current.LastCursor})
		})
	}
}

func TestWorkerAgentRejectsInvalidMetadataBeforeAdmission(t *testing.T) {
	f := newWorkerFixture(t, "codex", "codex-worker")
	for _, test := range []string{"short-id", "unknown-kind", "wrong-session", "expired", "future-start", "long-deadline", "mixed-workflow"} {
		now := time.Now().UTC()
		request := WorkerRequest{Action: "send", Sequence: 1, Key: "orchestrator:semantic-heartbeat", Prompt: "synthetic", Agent: &WorkerAgent{ID: "synthetic-agent", Kind: "heartbeat", StartedAt: now, Deadline: now.Add(time.Minute)}}
		switch test {
		case "short-id":
			request.Agent.ID = "short"
		case "unknown-kind":
			request.Agent.Kind = "other"
		case "wrong-session":
			request.Key = "synthetic"
		case "expired":
			request.Agent.Deadline = now.Add(-time.Second)
		case "future-start":
			request.Agent.StartedAt = now.Add(2 * time.Minute)
		case "long-deadline":
			request.Agent.Deadline = now.Add(25 * time.Hour)
		case "mixed-workflow":
			request.Workflow = &WorkerWorkflow{}
		}
		if reply, _ := f.call(request); reply.Error == "" {
			t.Fatalf("accepted %s", test)
		}
		if reply := f.mustCall(t, WorkerRequest{Action: "inspect"}); reply.NextSequence != 1 || len(reply.Executions) != 0 {
			t.Fatal("rejection retained an admission")
		}
	}
}

func TestWorkerAgentDeadlineInterruptionFailureKeepsFenceAndEvidence(t *testing.T) {
	target := &supervisorHarness{name: "codex", active: map[string]bool{}, emits: map[string]core.Emit{}, interruptErr: errors.New("synthetic interruption failure")}
	registry := NewRegistry()
	registry.Register("codex", func(HarnessConfig) (Harness, error) { return target, nil })
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "codex"})
	if err := supervisor.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	host := &workerHost{supervisor: supervisor, directory: t.TempDir(), executions: map[uint64]*workerExecution{}, next: 1, stop: make(chan struct{})}
	now := time.Now().UTC()
	request := WorkerRequest{Action: "send", Sequence: 1, Key: "orchestrator:semantic-heartbeat", Prompt: "synthetic", Agent: &WorkerAgent{ID: "synthetic-agent", Kind: "heartbeat", StartedAt: now, Deadline: now.Add(50 * time.Millisecond)}}
	if reply := host.send(request); reply.Error != "" {
		t.Fatal(reply.Error)
	}
	defer host.executions[1].file.Close()
	time.Sleep(time.Until(request.Agent.Deadline))
	host.expireAgents()
	current := host.executions[1]
	if current.Done || !supervisor.IsActive(request.Key) || current.Error != context.DeadlineExceeded.Error() {
		t.Fatal("failed interrupt falsely settled execution")
	}
	page := host.events(WorkerRequest{Sequence: 1})
	last := page.Events[len(page.Events)-1].Event
	if !strings.Contains(last.Text, "interruption failed") || last.Execution == nil || last.Execution.State != "degraded" {
		t.Fatal("deadline failure was discarded")
	}
	cursor := current.LastCursor
	host.expireAgents()
	if host.executions[1].LastCursor != cursor {
		t.Fatal("deadline failure caused immediate retry churn")
	}
	data, err := os.ReadFile(filepath.Join(host.directory, "1.jsonl.admission"))
	if err != nil || !strings.Contains(string(data), context.DeadlineExceeded.Error()) {
		t.Fatal("deadline outcome not checkpointed", err)
	}
	if reply := host.send(WorkerRequest{Action: "send", Sequence: 2, Key: request.Key, Prompt: request.Prompt, Agent: request.Agent}); reply.Error == "" {
		t.Fatal("failed interruption admitted an overlapping turn")
	}
}
