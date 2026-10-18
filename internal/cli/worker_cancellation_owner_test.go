//go:build linux || darwin

package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
)

func TestWorkerCancellationSelectsSessionOwner(t *testing.T) {
	for _, command := range []string{"manager", "stop", "kill"} {
		for _, owner := range []string{"local", "worker"} {
			t.Run(command+"/"+owner, func(t *testing.T) {
				f := newWorkerAgentFixture(t)
				identity := f.client.Info()
				t.Logf("cancellation worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
				local := newHeldCLIHarness()
				f.service.Harness, f.service.Orchestrator.Harness = local, local
				if err := f.service.Handle(t.Context(), core.Message{Channel: "cli", Conversation: "local", Text: "synthetic local request"}, nil); err != nil {
					t.Fatal(err)
				}
				c := harness.WorkerConversation{Channel: "cli", Conversation: "worker", SourceMessageID: "source", ExecutionID: "execution", Sources: []string{"source"}}
				if _, err := f.service.History.Append(c.Channel, c.Conversation, history.Entry{Role: "user", SourceMessageID: "source", Content: "synthetic worker request", AcceptedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.Orchestrator.AdmitConversation(t.Context(), c, "synthetic prompt", "synthetic request"); err != nil {
					t.Fatal(err)
				}
				f.start(t)
				key := "chat:cli:" + owner
				job, ok := f.service.Runtime.JobForSession(key)
				if !ok {
					t.Fatal("admitted job unavailable")
				}
				if command == "manager" {
					if stopped, err := f.service.Orchestrator.Interrupt(t.Context(), key); err != nil || !stopped {
						t.Fatal("shared owner interrupt failed", stopped, err)
					}
				} else {
					text := "/stop"
					if command == "kill" {
						text = fmt.Sprintf("/job kill %d", job.Number)
					}
					var result strings.Builder
					if err := f.service.Handle(t.Context(), core.Message{Channel: "cli", Conversation: owner, Text: text}, func(event core.Event) { result.WriteString(event.Text) }); err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(result.String(), "requested") {
						t.Fatal("command did not request its actual execution owner:", result.String())
					}
				}
				waitWorkerAgent(t, func() bool {
					retained := f.inspect(t)
					if owner == "local" {
						return !local.IsActive("chat:cli:local") && len(retained) == 1 && !retained[0].Done && !retained[0].Cancelled
					}
					return local.IsActive("chat:cli:local") && len(retained) == 0
				})
				methods, err := os.ReadFile(f.log)
				if err != nil || strings.Count(string(methods), "turn/start\n") != 1 {
					t.Fatal("cancellation dispatched another provider turn", err)
				}
			})
		}
	}
}

func TestWorkerCancellationInspectionFailurePreservesLocalJob(t *testing.T) {
	f := newWorkerAgentFixture(t)
	local := newHeldCLIHarness()
	f.service.Harness, f.service.Orchestrator.Harness = local, local
	if err := f.service.Handle(t.Context(), core.Message{Channel: "cli", Conversation: "local", Text: "synthetic local request"}, nil); err != nil {
		t.Fatal(err)
	}
	job, _ := f.service.Runtime.JobForSession("chat:cli:local")
	f.client.Close()
	restore := dropWorkerReply(t, f.client.Info().Socket, "inspect", false, false)
	stopped, interruptErr := f.service.Orchestrator.Interrupt(t.Context(), job.SessionKey)
	var result strings.Builder
	err := f.service.Handle(t.Context(), core.Message{Channel: "cli", Conversation: "operator", Text: fmt.Sprintf("/job kill %d", job.Number)}, func(event core.Event) { result.WriteString(event.Text) })
	restore()
	f.client.Close()
	current, ok := f.service.Runtime.Job(job.ID)
	if stopped || interruptErr == nil || err != nil || !strings.Contains(result.String(), "Cannot kill") || !local.IsActive(job.SessionKey) || !ok || current.Execution != job.Execution {
		t.Fatal("unknown worker inspection cancelled or retired the local job", stopped, interruptErr, err, result.String())
	}
	if _, err := f.client.Call(context.Background(), harness.WorkerRequest{Action: "inspect"}); err != nil {
		t.Fatal("inspection did not recover after fixture restoration", err)
	}
}

func TestWorkerCancellationRefusesConflictingOwners(t *testing.T) {
	f := newWorkerAgentFixture(t)
	local := newHeldCLIHarness()
	f.service.Harness, f.service.Orchestrator.Harness = local, local
	c := harness.WorkerConversation{Channel: "cli", Conversation: "conflicting", SourceMessageID: "source", ExecutionID: "execution", Sources: []string{"source"}}
	if _, err := f.service.History.Append(c.Channel, c.Conversation, history.Entry{Role: "user", SourceMessageID: "source", Content: "synthetic", AcceptedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	execution, err := f.service.Orchestrator.AdmitConversation(t.Context(), c, "synthetic", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := local.Send(t.Context(), execution.Key, "synthetic conflicting local execution", func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if stopped, err := f.service.Orchestrator.Interrupt(t.Context(), execution.Key); stopped || err == nil {
		t.Fatal("conflicting ownership was treated as safe to interrupt", stopped, err)
	}
	job, _ := f.service.Runtime.JobForSession(execution.Key)
	for _, command := range []string{"/stop", fmt.Sprintf("/job kill %d", job.Number)} {
		var result strings.Builder
		if err := f.service.Handle(t.Context(), core.Message{Channel: c.Channel, Conversation: c.Conversation, Text: command}, func(event core.Event) { result.WriteString(event.Text) }); err != nil || !strings.Contains(result.String(), "conflicting execution owners") {
			t.Fatal("command did not report conflicting ownership", err, result.String())
		}
	}
	retained := f.inspect(t)
	current, ok := f.service.Runtime.Job(job.ID)
	if !local.IsActive(execution.Key) || len(retained) != 1 || retained[0].Done || retained[0].Cancelled || !ok || current.Execution != job.Execution {
		t.Fatal("conflicting owners were cancelled or their job was retired")
	}
}

func TestWorkerCancellationObsoleteOwnerCannotInterruptLocal(t *testing.T) {
	f := newWorkerAgentFixture(t)
	local := newHeldCLIHarness()
	f.service.Harness, f.service.Orchestrator.Harness = local, local
	if _, _, err := local.Send(t.Context(), "chat:cli:local", "synthetic", func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	obsolete := f.service.Orchestrator
	oldClient := f.client
	defer oldClient.Close()
	f.stop(t)
	if err := f.owner.Release(f.term); err != nil {
		t.Fatal(err)
	}
	f.open(t)
	if stopped, err := obsolete.Interrupt(t.Context(), "chat:cli:local"); err == nil || stopped || !local.IsActive("chat:cli:local") {
		t.Fatal("obsolete controller interrupted a local execution", stopped, err)
	}
}
