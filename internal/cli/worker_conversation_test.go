//go:build linux || darwin

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
	"github.com/agent0ai/spynel/internal/instance"
)

type workerConversationRouter struct {
	fail     atomic.Bool
	attempts atomic.Int32
	sent     atomic.Int32
}

func (r *workerConversationRouter) Deliver(context.Context, string, string, string, string, []core.OutboundAttachment) error {
	return errors.New("conversation delivery must use the ordinary event router")
}

func (r *workerConversationRouter) DeliverEvent(_ context.Context, channel, conversation, receipt string, event core.Event) error {
	if channel != "telegram" || conversation != "TG-7" || receipt == "" {
		return errors.New("changed synthetic route or receipt")
	}
	if !event.Done {
		return nil
	}
	r.attempts.Add(1)
	if r.fail.Load() {
		return errors.New("synthetic response outage")
	}
	r.sent.Add(1)
	return nil
}

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
			if scenario.channel == "telegram" {
				metadata.Conversation = "TG-7"
				if _, err := f.service.Settings.Update(func(cfg *config.Config) error {
					cfg.Orchestrator.SemanticHeartbeatMinutes = 5
					cfg.Channels.Telegram.Enabled = true
					cfg.Channels.Telegram.AllowedUsers = []string{"7"}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
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
	for _, scenario := range []string{"missing", "covered", "already-admitted", "prior-reserved-recovery", "corrupt", "storage-error"} {
		t.Run(scenario, func(t *testing.T) {
			f := newWorkerAgentFixture(t)
			metadata := &harness.WorkerConversation{Channel: "cli", Conversation: "source-owner", SourceMessageID: "source", ExecutionID: "execution", Sources: []string{"source"}, Recovery: scenario == "prior-reserved-recovery"}
			if scenario != "missing" {
				if _, err := f.service.History.Append("cli", metadata.Conversation, history.Entry{Role: "user", SourceMessageID: "source", Content: "synthetic", AcceptedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "prior-reserved-recovery" {
				if _, err := f.service.History.ReserveRetrigger("cli", metadata.Conversation, metadata.Sources, time.Time{}, time.Time{}, 1); err != nil {
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

func TestWorkerConversationNativeAtomicRecoveryAndLostSendReply(t *testing.T) {
	f := newWorkerAgentFixture(t)
	metadata := &harness.WorkerConversation{Channel: "cli", Conversation: "reserved", SourceMessageID: "first", ExecutionID: "execution", Sources: []string{"first", "second"}, Recovery: true}
	for _, source := range metadata.Sources {
		if _, err := f.service.History.Append("cli", metadata.Conversation, history.Entry{Role: "user", SourceMessageID: source, Content: "synthetic", AcceptedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
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
	if err != nil || len(entries) != 3 || strings.Join(entries[2].AdmittedSources, ",") != "first,second" || strings.Join(entries[2].RetriggerOf, ",") != "first,second" {
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

func TestWorkerConversationNativeSendFailureSettlesAfterReplacement(t *testing.T) {
	for _, scenario := range []string{"normal", "lost-commit"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("SPYNEL_TEST_WORKER_REJECT_THREAD", "1")
			f := newWorkerAgentFixture(t)
			c := &harness.WorkerConversation{Channel: "cli", Conversation: "send-failure", SourceMessageID: "source", ExecutionID: "execution", Sources: []string{"source"}}
			if _, err := f.service.History.Append(c.Channel, c.Conversation, history.Entry{Role: "user", SourceMessageID: "source", Content: "synthetic", AcceptedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			key := "chat:cli:send-failure"
			id, err := f.service.Runtime.TryBeginJobWithDetails(key, c.Channel, c.Conversation, "synthetic", app.JobDetails{Kind: "conversation", WorkID: c.ExecutionID})
			if err != nil {
				t.Fatal(err)
			}
			original, _ := f.service.Runtime.Job(id)
			identity := f.client.Info()
			t.Logf("failed-send worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
			request := harness.WorkerRequest{Action: "send", Sequence: 1, Key: key, Prompt: "synthetic", Conversation: c}
			reply, err := f.client.Call(t.Context(), request)
			if err != nil || len(reply.Executions) != 1 || !reply.Executions[0].SourcesReserved || !reply.Executions[0].Done || reply.Executions[0].Error == "" {
				t.Fatal("provider refusal did not retain its failed admission", err)
			}
			unsettled, err := f.service.History.RecoveryWindow(c.Channel, c.Conversation, time.Time{}, 20)
			if err != nil || unsettled.Admitted["source"] || unsettled.Covered["source"] != "" {
				t.Fatal("proven provider rejection retained source admission before terminal settlement", err)
			}
			page, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "events", Sequence: 1})
			if err != nil || len(page.Events) == 0 || page.Events[len(page.Events)-1].Event.Kind != core.EventError || !page.Events[len(page.Events)-1].Event.Done {
				t.Fatal("failed provider Send has no durable terminal event", err)
			}
			f.stop(t)
			if err := f.owner.Release(f.term); err != nil {
				t.Fatal(err)
			}
			f.open(t)
			if f.client.Info().ID != identity.ID || f.client.Info().Process != identity.Process {
				t.Fatal("failed admission attachment replaced the worker")
			}
			restored := make(chan app.Job, 1)
			attach := f.service.Orchestrator.ConversationAttached
			f.service.Orchestrator.ConversationAttached = func(execution harness.WorkerExecution) (int, error) {
				id, err := attach(execution)
				if err == nil {
					job, _ := f.service.Runtime.Job(id)
					select {
					case restored <- job:
					default:
					}
				}
				return id, err
			}
			if scenario == "lost-commit" {
				f.client.Close()
				restore := dropWorkerReply(t, f.client.Info().Socket, "commit", true, false)
				defer restore()
			}
			f.cfg.Orchestrator.IntervalSec = 1
			f.service.Orchestrator.ApplyRuntimeConfig(f.cfg)
			f.start(t)
			waitWorkerAgent(t, func() bool { return len(f.inspect(t)) == 0 })
			job := <-restored
			if job.StableID != original.StableID || job.Number != original.Number || job.Generation != original.Generation || job.ProviderIterations != 1 {
				t.Fatal("failed admission attachment changed its job or counters")
			}
			entries, _, err := f.service.History.Entries(c.Channel, c.Conversation)
			terminals := 0
			for _, entry := range entries {
				if entry.Terminal {
					terminals++
					if entry.Role != "error" || entry.Outcome != "terminal_error" || entry.ExecutionID != c.ExecutionID || strings.Join(entry.Covers, ",") != "source" {
						t.Fatal("failed admission lost exact terminal coverage")
					}
				}
			}
			window, windowErr := f.service.History.RecoveryWindow(c.Channel, c.Conversation, time.Time{}, 20)
			methods, readErr := os.ReadFile(f.log)
			if err != nil || windowErr != nil || readErr != nil || terminals != 1 || window.Covered["source"] != "terminal_coverage" || f.service.Runtime.Status().Jobs != 0 || strings.Count(string(methods), "turn/start\n") != 0 || strings.Contains(string(methods), "selection:") {
				t.Fatal("failed admission did not settle once without a new provider attempt", err, windowErr, readErr)
			}
		})
	}
}

func TestWorkerConversationScheduledAttachment(t *testing.T) {
	for _, scenario := range []string{"normal", "lost-commit", "tui", "tui-lost-commit"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("SPYNEL_TEST_WORKER_CONVERSATION_ERROR", "1")
			f := newWorkerAgentFixture(t)
			channelName := "cli"
			if strings.HasPrefix(scenario, "tui") {
				channelName = "tui"
			}
			metadata := &harness.WorkerConversation{Channel: channelName, Conversation: "attached", SourceMessageID: "source", ExecutionID: "execution", Sources: []string{"source"}}
			if _, err := f.service.History.Append(channelName, "attached", history.Entry{Role: "user", SourceMessageID: "source", Content: "synthetic", AcceptedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			key := "chat:" + channelName + ":attached"
			id, err := f.service.Runtime.TryBeginJobWithDetails(key, channelName, "attached", "synthetic", app.JobDetails{Kind: "conversation", WorkID: metadata.ExecutionID})
			if err != nil {
				t.Fatal(err)
			}
			original, _ := f.service.Runtime.Job(id)
			identity := f.client.Info()
			t.Logf("automatic conversation worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
			if reply, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "send", Sequence: 1, Key: key, Prompt: "synthetic", Conversation: metadata}); err != nil || reply.Executions[0].Error != "" {
				t.Fatal("conversation admission failed", err)
			}
			waitWorkerAgent(t, func() bool { return f.inspect(t)[0].LastCursor >= 2 })
			f.stop(t)
			if err := f.owner.Release(f.term); err != nil {
				t.Fatal(err)
			}
			f.open(t)
			if f.client.Info().ID != identity.ID || f.client.Info().Process != identity.Process {
				t.Fatal("replacement changed worker identity")
			}
			f.cfg.Orchestrator.IntervalSec = 1
			f.service.Orchestrator.ApplyRuntimeConfig(f.cfg)
			f.start(t)
			deadline := time.Now().Add(4 * time.Second)
			var restored app.Job
			for time.Now().Before(deadline) {
				if job, ok := f.service.Runtime.JobForSession(key); ok {
					restored = job
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if restored.StableID != original.StableID || restored.Number != original.Number || restored.Generation != original.Generation || restored.ProviderIterations != 1 {
				t.Fatal("scheduled attachment did not restore the same job")
			}
			if channelName == "tui" {
				if err := f.service.RegisterLiveTUI("viewer", "attached", time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				waitWorkerAgent(t, func() bool { return f.service.SharedStateForInstance("viewer").ConversationActivity == 1 })
				if err := f.service.RegisterLiveTUI("other-viewer", "other", time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				if f.service.SharedStateForInstance("other-viewer").ConversationActivity != 0 {
					t.Fatal("retained activity leaked to another selected conversation")
				}
				f.stop(t)
				if f.service.SharedStateForInstance("viewer").ConversationActivity != 0 {
					t.Fatal("controller detach retained local activity")
				}
				if err := f.owner.Release(f.term); err != nil {
					t.Fatal(err)
				}
				f.open(t)
				if err := f.service.RegisterLiveTUI("viewer", "attached", time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				f.start(t)
				waitWorkerAgent(t, func() bool { return f.service.SharedStateForInstance("viewer").ConversationActivity == 1 })
				if f.client.Info().Process != identity.Process {
					t.Fatal("local activity reattachment replaced the worker")
				}
			}
			if strings.HasSuffix(scenario, "lost-commit") {
				f.client.Close()
				restore := dropWorkerReply(t, f.client.Info().Socket, "commit", true, false)
				defer restore()
			}
			if _, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "interrupt", Key: key}); err != nil {
				t.Fatal(err)
			}
			waitWorkerAgent(t, func() bool { return len(f.inspect(t)) == 0 })
			entries, _, err := f.service.History.Entries(channelName, "attached")
			terminals := 0
			for _, entry := range entries {
				if entry.Terminal {
					terminals++
					if entry.ExecutionID != metadata.ExecutionID || strings.Join(entry.Covers, ",") != "source" {
						t.Fatal("automatic attachment lost source coverage")
					}
					if channelName == "tui" && (!entry.Recovery || entry.Sender != "Spy") {
						t.Fatal("reattached TUI terminal is invisible to the live history follower")
					}
				}
			}
			methods, readErr := os.ReadFile(f.log)
			if err != nil || readErr != nil || terminals != 1 || f.service.Runtime.Status().Jobs != 0 || strings.Count(string(methods), "turn/start\n") != 1 {
				t.Fatal("automatic attachment duplicated or lost history/job/provider admission", err, readErr)
			}
			if channelName == "tui" && f.service.SharedStateForInstance("viewer").ConversationActivity != 0 {
				t.Fatal("retained TUI terminal did not stop activity")
			}
		})
	}
}

func TestWorkerConversationScheduledDeliveryFailureAndRevocation(t *testing.T) {
	t.Setenv("SPYNEL_TEST_WORKER_CONVERSATION_ERROR", "1")
	f := newWorkerAgentFixture(t)
	setAuthorized := func(allowed string) {
		t.Helper()
		if _, err := f.service.Settings.Update(func(cfg *config.Config) error {
			cfg.Orchestrator.SemanticHeartbeatMinutes = 5
			cfg.Channels.Telegram.Enabled = true
			cfg.Channels.Telegram.AllowedUsers = []string{allowed}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	setAuthorized("7")
	router := &workerConversationRouter{}
	router.fail.Store(true)
	f.service.SetConversationDelivery(router)
	c := &harness.WorkerConversation{Channel: "telegram", Conversation: "TG-7", SourceMessageID: "source", ExecutionID: "execution", Sources: []string{"source"}}
	if _, err := f.service.History.Append(c.Channel, c.Conversation, history.Entry{Role: "user", SourceMessageID: "source", Content: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	key := "chat:telegram:TG-7"
	identity := f.client.Info()
	t.Logf("automatic remote worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
	if reply, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "send", Sequence: 1, Key: key, Prompt: "synthetic", Conversation: c}); err != nil || reply.Executions[0].Error != "" {
		t.Fatal("conversation admission failed", err)
	}
	f.cfg.Orchestrator.IntervalSec = 1
	f.service.Orchestrator.ApplyRuntimeConfig(f.cfg)
	f.start(t)
	waitWorkerAgent(t, func() bool {
		state := f.inspect(t)[0]
		return state.CommittedCursor >= 2
	})
	original, ok := f.service.Runtime.JobForSession(key)
	if !ok {
		t.Fatal("automatic remote job was not restored")
	}
	if _, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "interrupt", Key: key}); err != nil {
		t.Fatal(err)
	}
	waitWorkerAgent(t, func() bool { return router.attempts.Load() > 0 })
	before := f.inspect(t)[0]
	if before.CommittedCursor == before.LastCursor || router.sent.Load() != 0 {
		t.Fatal("failed remote delivery was acknowledged")
	}
	setAuthorized("8")
	attempts := router.attempts.Load()
	time.Sleep(1300 * time.Millisecond) // Allow the ordinary scheduled retry.
	state := f.inspect(t)[0]
	if router.attempts.Load() != attempts || state.CommittedCursor != before.CommittedCursor {
		t.Fatal("revocation permitted a response or cursor commit")
	}
	pending, ok := f.service.Runtime.JobForSession(key)
	if !ok || pending.StableID != original.StableID || pending.Execution != app.JobDegraded {
		t.Fatal("failed delivery lost its visibly degraded job")
	}
	router.fail.Store(false)
	setAuthorized("7")
	waitWorkerAgent(t, func() bool { return len(f.inspect(t)) == 0 })
	entries, _, err := f.service.History.Entries(c.Channel, c.Conversation)
	terminals := 0
	for _, entry := range entries {
		if entry.Terminal {
			terminals++
		}
	}
	methods, readErr := os.ReadFile(f.log)
	if err != nil || readErr != nil || terminals != 1 || router.sent.Load() != 1 || strings.Count(string(methods), "turn/start\n") != 1 || f.client.Info().Process != identity.Process {
		t.Fatal("scheduled delivery recovery duplicated effects or execution", err, readErr)
	}
}
