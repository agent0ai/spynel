//go:build linux || darwin

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
	"github.com/agent0ai/spynel/internal/instance"
)

func TestWorkerConversationNativeReplayAndSettlement(t *testing.T) {
	for _, scenario := range []struct{ name, mode, channel, partial string }{
		{"provider-error", "1", "cli", "retained partial"},
		{"outbound-validation", "2", "telegram", "retained partial\n[Send attachment](<missing>)"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Setenv("SPYNEL_TEST_WORKER_CONVERSATION_ERROR", scenario.mode)
			completion := filepath.Join(t.TempDir(), "complete")
			if scenario.mode == "2" {
				t.Setenv("SPYNEL_TEST_WORKER_COMPLETIONS", completion)
			}
			f := newWorkerAgentFixture(t)
			metadata := &harness.WorkerConversation{Channel: scenario.channel, Conversation: "retained", SourceMessageID: "second", ExecutionID: "synthetic-execution", Sources: []string{"first", "second"}}
			for _, source := range metadata.Sources {
				if _, err := f.service.History.Append(metadata.Channel, metadata.Conversation, history.Entry{Role: "user", Content: "synthetic", SourceMessageID: source, AcceptedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
			}
			openJob := func() (int, app.Job) {
				t.Helper()
				id, err := f.service.Runtime.TryBeginJobWithDetails("chat:"+metadata.Channel+":"+metadata.Conversation, metadata.Channel, metadata.Conversation, "synthetic conversation", app.JobDetails{Kind: "conversation", WorkID: metadata.ExecutionID})
				if err != nil {
					t.Fatal(err)
				}
				job, _ := f.service.Runtime.Job(id)
				return id, job
			}
			id, originalJob := openJob()
			identity := f.client.Info()
			t.Logf("conversation worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
			request := harness.WorkerRequest{Action: "send", Sequence: 1, Key: "chat:" + metadata.Channel + ":" + metadata.Conversation, Prompt: "synthetic batch", Conversation: metadata}
			if reply, err := f.client.Call(t.Context(), request); err != nil || reply.Executions[0].Error != "" || !reply.Executions[0].SourcesReserved {
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
			if scenario.mode == "2" {
				if err := os.WriteFile(completion+".1", nil, 0600); err != nil {
					t.Fatal(err)
				}
			} else if _, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "interrupt", Key: request.Key}); err != nil {
				t.Fatal(err)
			}
			waitWorkerAgent(t, func() bool { return f.inspect(t)[0].Done })
			before := f.inspect(t)[0].CommittedCursor
			if _, err := f.client.Consume(t.Context(), 1, func(delivery harness.WorkerDelivery) error {
				if scenario.mode == "2" && delivery.Event.Done {
					if delivery.Event.Kind != core.EventFinal || delivery.Streamed != scenario.partial {
						t.Fatal("successful native final lost output before application validation")
					}
				}
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
			entries, _, err := f.service.History.Entries(metadata.Channel, metadata.Conversation)
			partial, result := 0, 0
			for _, entry := range entries {
				if entry.Content == scenario.partial {
					partial++
				}
				if entry.Terminal {
					result++
					if entry.Role != "error" || entry.Outcome != "terminal_error" || entry.ExecutionID != metadata.ExecutionID || strings.Join(entry.Covers, ",") != "first,second" {
						t.Fatal("terminal lost batch source coverage")
					}
				}
			}
			window, windowErr := f.service.History.RecoveryWindow(metadata.Channel, metadata.Conversation, time.Time{}, 2000)
			if err != nil || windowErr != nil || partial != 1 || result != 1 || window.Covered["first"] != "terminal_coverage" || window.Covered["second"] != "terminal_coverage" || f.service.Runtime.Status().Jobs != 0 {
				t.Fatal("replay duplicated or lost history/coverage", err, windowErr)
			}
			recent, _, contextErr := f.service.History.RecentBounded(metadata.Channel, metadata.Conversation, 20, 10000)
			if contextErr != nil || strings.Count(recent, scenario.partial) != 1 {
				t.Fatal("preserved output was lost or duplicated in subsequent chat context", contextErr)
			}
			if _, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "ack", Sequence: 1, After: state.LastCursor}); err != nil {
				t.Fatal(err)
			}
			methods, err := os.ReadFile(f.log)
			if err != nil || strings.Count(string(methods), "turn/start\n") != 1 {
				t.Fatal("attachment dispatched another provider turn", err)
			}
		})
	}
}

func TestWorkerConversationNativeRejectsUnownedSources(t *testing.T) {
	for _, scenario := range []string{"missing", "covered", "already-admitted", "unreserved-recovery", "corrupt", "storage-error"} {
		t.Run(scenario, func(t *testing.T) {
			f := newWorkerAgentFixture(t)
			metadata := &harness.WorkerConversation{Channel: "cli", Conversation: "source-owner", SourceMessageID: "source", ExecutionID: "execution", Sources: []string{"source"}, Recovery: scenario == "unreserved-recovery"}
			if scenario != "missing" {
				if _, err := f.service.History.Append("cli", metadata.Conversation, history.Entry{Role: "user", SourceMessageID: "source", Content: "synthetic", AcceptedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "covered" || scenario == "already-admitted" {
				entry := history.Entry{Role: "correlation", ExecutionID: "other-execution"}
				if scenario == "covered" {
					entry.Covers, entry.Outcome = []string{"source"}, "terminal_assistant"
				} else {
					entry.SourceMessageID, entry.Admission = "source", "new"
				}
				if _, err := f.service.History.Append("cli", metadata.Conversation, entry); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "corrupt" {
				if err := os.WriteFile(f.service.History.Path("cli", metadata.Conversation), []byte("{broken\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "storage-error" {
				path := f.service.History.Path("cli", metadata.Conversation)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			reply, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "send", Sequence: 1, Key: "chat:cli:" + metadata.Conversation, Prompt: "synthetic source gate", Conversation: metadata})
			if err != nil || len(reply.Executions) != 1 || !reply.Executions[0].Done || reply.Executions[0].Error == "" || reply.Executions[0].SourcesReserved {
				t.Fatal("unowned source reached provider admission", err)
			}
			methods, err := os.ReadFile(f.log)
			if err != nil || strings.Contains(string(methods), "turn/start\n") {
				t.Fatal("rejected source dispatched provider work", err)
			}
		})
	}
}

func TestWorkerConversationNativeReservedRecoveryAndLostSendReply(t *testing.T) {
	f := newWorkerAgentFixture(t)
	metadata := &harness.WorkerConversation{Channel: "cli", Conversation: "reserved", SourceMessageID: "first", ExecutionID: "execution", Sources: []string{"first", "second"}, Recovery: true}
	for _, source := range metadata.Sources {
		if _, err := f.service.History.Append("cli", metadata.Conversation, history.Entry{Role: "user", SourceMessageID: source, Content: "synthetic", AcceptedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	if reserved, err := f.service.History.ReserveRetrigger("cli", metadata.Conversation, metadata.Sources, time.Time{}, time.Time{}, 2); err != nil || len(reserved) != 2 {
		t.Fatal("recovery reservation failed", err)
	}
	request := harness.WorkerRequest{Action: "send", Sequence: 1, Key: "chat:cli:" + metadata.Conversation, Prompt: "synthetic recovery", Conversation: metadata}
	f.client.Close()
	restore := dropWorkerReply(t, f.client.Info().Socket, "send", true, false)
	_, err := f.client.Call(t.Context(), request)
	restore()
	f.client.Close()
	if err == nil {
		t.Fatal("lost send reply was reported as confirmed")
	}
	reply, err := f.client.Call(t.Context(), request)
	if err != nil || len(reply.Executions) != 1 || reply.Executions[0].Error != "" || !reply.Executions[0].SourcesReserved {
		t.Fatal("exact retry lost its source reservation", err)
	}
	entries, _, err := f.service.History.RecoveryEntries("cli", metadata.Conversation, 10)
	if err != nil || len(entries) != 4 || strings.Join(entries[3].AdmittedSources, ",") != "first,second" {
		t.Fatal("lost send reply duplicated or lost the source batch", err)
	}
	snapshot, err := f.service.History.RecoveryWindow("cli", metadata.Conversation, time.Time{}, 10)
	if err != nil || !snapshot.Admitted["first"] || !snapshot.Admitted["second"] {
		t.Fatal("recovery admission lost source correlation", err)
	}
	methods, err := os.ReadFile(f.log)
	if err != nil || strings.Count(string(methods), "turn/start\n") != 1 {
		t.Fatal("lost send reply dispatched another provider turn", err)
	}
}
