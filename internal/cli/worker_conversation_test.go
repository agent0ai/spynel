//go:build linux || darwin

package cli

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
	"github.com/agent0ai/spynel/internal/instance"
)

func TestWorkerConversationNativeReplayAndSettlement(t *testing.T) {
	t.Setenv("SPYNEL_TEST_WORKER_CONVERSATION_ERROR", "1")
	f := newWorkerAgentFixture(t)
	metadata := &harness.WorkerConversation{Channel: "cli", Conversation: "retained", SourceMessageID: "second", ExecutionID: "synthetic-execution", Sources: []string{"first", "second"}}
	for _, source := range metadata.Sources {
		if _, err := f.service.History.Append("cli", "retained", history.Entry{Role: "user", Content: "synthetic", SourceMessageID: source, AcceptedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	openJob := func() (int, app.Job) {
		t.Helper()
		id, err := f.service.Runtime.TryBeginJobWithDetails("chat:cli:retained", "cli", "retained", "synthetic conversation", app.JobDetails{Kind: "conversation", WorkID: metadata.ExecutionID})
		if err != nil {
			t.Fatal(err)
		}
		job, _ := f.service.Runtime.Job(id)
		return id, job
	}
	id, originalJob := openJob()
	identity := f.client.Info()
	t.Logf("conversation worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
	request := harness.WorkerRequest{Action: "send", Sequence: 1, Key: "chat:cli:retained", Prompt: "synthetic batch", Conversation: metadata}
	if reply, err := f.client.Call(t.Context(), request); err != nil || reply.Executions[0].Error != "" {
		t.Fatal("conversation admission failed", err)
	}
	waitWorkerAgent(t, func() bool { return f.inspect(t)[0].LastCursor >= 2 })
	if _, err := f.client.Consume(t.Context(), 1, func(delivery harness.WorkerDelivery) error {
		return f.service.ConsumeConversationDelivery(id, delivery, nil)
	}); err != nil {
		t.Fatal(err)
	}
	f.stop(t)
	if err := f.owner.Release(f.term); err != nil {
		t.Fatal(err)
	}
	var err error
	f.owner, err = instance.New(f.cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	f.term, _ = f.owner.NewToken()
	if _, ok, err := f.owner.TryAcquire("127.0.0.1:10002", f.term); err != nil || !ok {
		t.Fatal("successor owner failed", err)
	}
	if _, err := f.client.Call(t.Context(), request); err == nil {
		t.Fatal("obsolete controller could still admit")
	}
	f.client.Close()
	f.client, err = harness.OpenWorker(t.Context(), f.harnessConfig, f.owner.ID(), f.term)
	if err != nil || f.client.Info().ID != identity.ID || f.client.Info().Process != identity.Process {
		t.Fatal("conversation attachment changed the worker", err)
	}
	f.service = app.New(f.cfg, harness.NewSupervisor(harness.NewBuiltinRegistry(), f.harnessConfig))
	f.service.Orchestrator.Worker = f.client
	id, restoredJob := openJob()
	if restoredJob.Number != originalJob.Number || restoredJob.Generation != originalJob.Generation || restoredJob.StableID != originalJob.StableID || restoredJob.ProviderIterations != 1 {
		t.Fatal("conversation attachment changed its job or counters")
	}
	if _, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "interrupt", Key: request.Key}); err != nil {
		t.Fatal(err)
	}
	waitWorkerAgent(t, func() bool { return f.inspect(t)[0].Done })
	before := f.inspect(t)[0].CommittedCursor
	if _, err := f.client.Consume(t.Context(), 1, func(delivery harness.WorkerDelivery) error {
		return f.service.ConsumeConversationDelivery(id, delivery, func(harness.WorkerDelivery) error { return errors.New("synthetic delivery outage") })
	}); err == nil || f.inspect(t)[0].CommittedCursor != before {
		t.Fatal("failed delivery advanced the durable worker cursor")
	}
	// Persistence succeeded before delivery failed. Reopen a fresh controller's
	// history/archive handles and replay the same worker event without duplicates.
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = app.New(f.cfg, harness.NewSupervisor(harness.NewBuiltinRegistry(), f.harnessConfig))
	f.service.Orchestrator.Worker = f.client
	id, _ = openJob()
	terminals := 0
	f.client.Close()
	restore := dropWorkerReply(t, f.client.Info().Socket, "commit", true, false)
	_, err = f.client.Consume(t.Context(), 1, func(delivery harness.WorkerDelivery) error {
		return f.service.ConsumeConversationDelivery(id, delivery, func(delivery harness.WorkerDelivery) error {
			if delivery.Event.Done {
				terminals++
			}
			return nil
		})
	})
	restore()
	f.client.Close()
	if err == nil || terminals != 1 {
		t.Fatal("lost commit reply was misrepresented", err)
	}
	state, err := f.client.Consume(t.Context(), 1, func(harness.WorkerDelivery) error {
		t.Fatal("confirmed committed terminal was delivered again")
		return nil
	})
	if err != nil || state.CommittedCursor != state.LastCursor {
		t.Fatal("lost commit reply did not reconcile", err)
	}
	entries, _, err := f.service.History.Entries("cli", "retained")
	partial, result := 0, 0
	for _, entry := range entries {
		if entry.Content == "retained partial" {
			partial++
		}
		if entry.Terminal {
			result++
			if entry.ExecutionID != metadata.ExecutionID || strings.Join(entry.Covers, ",") != "first,second" {
				t.Fatal("terminal lost batch source coverage")
			}
		}
	}
	window, windowErr := f.service.History.RecoveryWindow("cli", "retained", time.Time{}, 2000)
	if err != nil || windowErr != nil || partial != 1 || result != 1 || window.Covered["first"] != "terminal_coverage" || window.Covered["second"] != "terminal_coverage" || f.service.Runtime.Status().Jobs != 0 {
		t.Fatal("replay duplicated or lost history/coverage", err, windowErr)
	}
	if _, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "ack", Sequence: 1, After: state.LastCursor}); err != nil {
		t.Fatal(err)
	}
	methods, err := os.ReadFile(f.log)
	if err != nil || strings.Count(string(methods), "turn/start\n") != 1 {
		t.Fatal("attachment dispatched another provider turn", err)
	}
}
