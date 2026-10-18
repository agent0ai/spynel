//go:build linux || darwin

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/orchestrator"
)

func TestWorkerConversationManagerFollowup(t *testing.T) {
	for _, channel := range []string{"cli", "tui"} {
		for _, detach := range []bool{false, true} {
			name := channel + map[bool]string{false: "/complete", true: "/detach-replace"}[detach]
			t.Run(name, func(t *testing.T) {
				t.Setenv("SPYNEL_TEST_WORKER_CONVERSATION_ERROR", "1")
				completion := filepath.Join(t.TempDir(), "complete")
				t.Setenv("SPYNEL_TEST_WORKER_COMPLETIONS", completion)
				f := newWorkerAgentFixture(t)
				f.start(t)
				first := core.Message{Channel: channel, Conversation: "manager-followup", SourceMessageID: "alpha", Text: "synthetic first"}
				second := first
				second.SourceMessageID, second.Text = "beta", "synthetic follow-up"
				second.FollowupOnly = true
				a, b := make(chan core.Event, 64), make(chan core.Event, 64)
				firstDone, secondDone := make(chan error, 1), make(chan error, 1)
				go func() { firstDone <- f.service.Handle(t.Context(), first, func(e core.Event) { a <- e }) }()
				waitFollowupDelta(t, a, firstDone)
				original := f.inspect(t)[0]
				job, ok := f.service.Runtime.JobForSession(original.Key)
				if !ok {
					t.Fatal("initial job absent")
				}
				identity := f.client.Info()
				t.Logf("worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				go func() { secondDone <- f.service.Handle(ctx, second, func(e core.Event) { b <- e }) }()
				select {
				case err := <-firstDone:
					if err != nil {
						t.Fatal("handoff did not release preceding request", err)
					}
				case err := <-secondDone:
					t.Fatal("follow-up ended before provider completion", err)
				case <-time.After(10 * time.Second):
					t.Fatal("follow-up did not hand off response ownership")
				}
				current := f.inspect(t)[0]
				if current.Sequence != original.Sequence || current.Conversation.ExecutionID != original.Conversation.ExecutionID || current.ConversationTurns != 1 || !reflect.DeepEqual(current.Conversation.Sources, []string{"alpha", "beta"}) {
					t.Fatal("follow-up replaced execution, sources or native turn accounting")
				}
				if updated, ok := f.service.Runtime.JobForSession(original.Key); !ok || updated.StableID != job.StableID || updated.Number != job.Number || updated.ProviderIterations != 1 {
					t.Fatal("follow-up replaced its job")
				}
				if detach {
					cancel()
					select {
					case err := <-secondDone:
						if !errors.Is(err, orchestrator.ErrConversationPending) {
							t.Fatal("detach manufactured completion", err)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("follow-up did not detach")
					}
					if f.inspect(t)[0].Done {
						t.Fatal("detaching request interrupted provider")
					}
					f.stop(t)
					if err := f.owner.Release(f.term); err != nil {
						t.Fatal(err)
					}
					old := f.client
					f.open(t)
					defer old.Close()
					if f.client.Info().Process != identity.Process {
						t.Fatal("replacement changed worker")
					}
					f.start(t)
				}
				if err := os.WriteFile(completion+".1", []byte("finish"), 0600); err != nil {
					t.Fatal(err)
				}
				if !detach {
					select {
					case err := <-secondDone:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(10 * time.Second):
						t.Fatal("follow-up request never received its result")
					}
				}
				waitWorkerAgent(t, func() bool { return len(f.inspect(t)) == 0 })
				for _, pair := range []struct {
					source string
					events chan core.Event
				}{{"alpha", a}, {"beta", b}} {
					for len(pair.events) > 0 {
						if e := <-pair.events; e.RequestID != pair.source {
							t.Fatal("journal consumer routed an event to another request")
						}
					}
				}
				entries, _, err := f.service.History.Entries(channel, first.Conversation)
				if err != nil {
					t.Fatal(err)
				}
				terminals := 0
				for _, e := range entries {
					if e.ExecutionID == original.Conversation.ExecutionID && len(e.Covers) > 0 {
						terminals++
						if e.SourceMessageID != "beta" || !reflect.DeepEqual(e.Covers, []string{"alpha", "beta"}) {
							t.Fatal("terminal lost response identity or coverage")
						}
					}
				}
				if terminals != 1 {
					t.Fatal("terminal was lost or duplicated")
				}
				archived, _, err := f.service.Runtime.ArchivedJob(job.Number)
				if err != nil || archived.Generation != job.Generation || !followupArchiveTurns(t, archived.Path, "1") {
					t.Fatal("retained job identity/accounting changed", err)
				}
				data, err := os.ReadFile(f.log)
				if err != nil || strings.Count(string(data), "turn/start\n") != 1 || strings.Count(string(data), "turn/steer\n") != 1 {
					t.Fatal("follow-up repeated provider admission", err)
				}
			})
		}
	}
}

func waitFollowupDelta(t *testing.T, events <-chan core.Event, done <-chan error) {
	t.Helper()
	for {
		select {
		case e := <-events:
			if e.Kind == core.EventDelta {
				return
			}
		case err := <-done:
			t.Fatal("request ended before progress", err)
		case <-time.After(10 * time.Second):
			t.Fatal("request never progressed")
		}
	}
}

func TestWorkerConversationManagerUnknownFollowup(t *testing.T) {
	for _, scenario := range []string{"accepted", "absent", "provider-error"} {
		t.Run(scenario, func(t *testing.T) {
			accepted := scenario != "absent"
			if scenario == "provider-error" {
				t.Setenv("SPYNEL_TEST_WORKER_UNCERTAIN_STEER", "1")
			}
			t.Setenv("SPYNEL_TEST_WORKER_CONVERSATION_ERROR", "1")
			completion := filepath.Join(t.TempDir(), "complete")
			t.Setenv("SPYNEL_TEST_WORKER_COMPLETIONS", completion)
			f := newWorkerAgentFixture(t)
			f.start(t)
			message := core.Message{Channel: "cli", Conversation: "unknown-followup", SourceMessageID: "alpha", Text: "synthetic first"}
			events, done := make(chan core.Event, 64), make(chan error, 1)
			go func() { done <- f.service.Handle(t.Context(), message, func(e core.Event) { events <- e }) }()
			waitFollowupDelta(t, events, done)
			original := f.inspect(t)[0]
			job, _ := f.service.Runtime.JobForSession(original.Key)
			f.client.Close()
			restore := func() {}
			if scenario != "provider-error" {
				restore = dropWorkerReply(t, f.client.Info().Socket, "followup", accepted, false)
			}
			message.SourceMessageID, message.Text = "beta", "synthetic uncertain follow-up"
			if err := f.service.Handle(t.Context(), message, nil); !errors.Is(err, orchestrator.ErrConversationPending) {
				t.Fatal("unknown follow-up did not retain pending result", err)
			}
			restore()
			f.client.Close()
			entries, _, err := f.service.History.Entries(message.Channel, message.Conversation)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Terminal {
					t.Fatal("unknown follow-up fabricated terminal history")
				}
			}
			f.stop(t)
			if err := f.owner.Release(f.term); err != nil {
				t.Fatal(err)
			}
			old := f.client
			f.open(t)
			defer old.Close()
			f.start(t)
			if err := os.WriteFile(completion+".1", []byte("finish"), 0600); err != nil {
				t.Fatal(err)
			}
			waitWorkerAgent(t, func() bool { return len(f.inspect(t)) == 0 })
			entries, _, err = f.service.History.Entries(message.Channel, message.Conversation)
			if err != nil {
				t.Fatal(err)
			}
			expected := []string{"alpha"}
			if accepted {
				expected = append(expected, "beta")
			}
			terminals := 0
			for _, entry := range entries {
				if entry.ExecutionID == original.Conversation.ExecutionID && len(entry.Covers) > 0 {
					terminals++
					if !reflect.DeepEqual(entry.Covers, expected) {
						t.Fatal("unknown delivery changed exact terminal coverage")
					}
				}
			}
			if terminals != 1 {
				t.Fatal("terminal lost or duplicated")
			}
			archived, _, err := f.service.Runtime.ArchivedJob(job.Number)
			if err != nil || !followupArchiveTurns(t, archived.Path, "1") {
				t.Fatal("unknown reply changed job accounting", err)
			}
			data, err := os.ReadFile(f.log)
			if err != nil || strings.Count(string(data), "turn/start\n") != 1 || strings.Count(string(data), "turn/steer\n") != map[bool]int{true: 1, false: 0}[accepted] {
				t.Fatal("unknown delivery repeated provider work", err)
			}
		})
	}
}

func TestWorkerConversationManagerRemoteFollowup(t *testing.T) {
	for _, name := range []string{"telegram", "whatsapp"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("SPYNEL_TEST_WORKER_CONVERSATION_ERROR", "1")
			f := newWorkerAgentFixture(t)
			message, router := configureRemoteRequest(t, f, name)
			message.SourceMessageID = "alpha"
			f.start(t)
			if err := f.service.Handle(t.Context(), message, nil); err != nil {
				t.Fatal(err)
			}
			original := f.inspect(t)[0]
			job, _ := f.service.Runtime.JobForSession(original.Key)
			message.SourceMessageID, message.Text = "beta", "synthetic second"
			if err := f.service.Handle(t.Context(), message, func(core.Event) { t.Error("remote provider output used inbound emitter") }); err != nil {
				t.Fatal(err)
			}
			current := f.inspect(t)[0]
			if current.Sequence != original.Sequence || current.Conversation.ExecutionID != original.Conversation.ExecutionID || !reflect.DeepEqual(current.Conversation.Sources, []string{"alpha", "beta"}) {
				t.Fatal("remote follow-up replaced execution")
			}
			batch := harness.WorkerConversation{Channel: message.Channel, Conversation: message.Conversation, ExecutionID: "unused-new-execution", SourceMessageID: "absent", Sources: []string{"absent"}}
			if _, err := f.service.Orchestrator.AdmitConversation(t.Context(), batch, "synthetic refused", "refused"); err == nil || errors.Is(err, orchestrator.ErrConversationPending) {
				t.Fatal("proven source refusal was classified as unknown", err)
			}
			current = f.inspect(t)[0]
			if len(current.Followups) != 2 || current.Followups[1].Reserved || current.Done || !reflect.DeepEqual(current.Conversation.Sources, []string{"alpha", "beta"}) {
				t.Fatal("refusal changed original execution")
			}
			if _, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "interrupt", Key: original.Key}); err != nil {
				t.Fatal(err)
			}
			waitWorkerAgent(t, func() bool { return len(f.inspect(t)) == 0 })
			archived, _, err := f.service.Runtime.ArchivedJob(job.Number)
			if err != nil || archived.Generation != job.Generation || !followupArchiveTurns(t, archived.Path, "1") {
				t.Fatal("remote follow-up replaced job", err)
			}
			entries, _, err := f.service.History.Entries(message.Channel, message.Conversation)
			if err != nil {
				t.Fatal(err)
			}
			terminals := 0
			for _, entry := range entries {
				if entry.ExecutionID == original.Conversation.ExecutionID && len(entry.Covers) > 0 {
					terminals++
					if !reflect.DeepEqual(entry.Covers, []string{"alpha", "beta"}) {
						t.Fatal("refusal changed terminal coverage")
					}
				}
			}
			if terminals != 1 {
				t.Fatal("canonical remote terminal was not delivered")
			}
			router.mu.Lock()
			defer router.mu.Unlock()
			if len(router.sent) != 1 || router.active != 0 {
				t.Fatal("remote final was duplicated or activity remained live")
			}

		})
	}
}

func followupArchiveTurns(t *testing.T, path, count string) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(data), "provider_iterations: "+strconv.Quote(count)+"\n")
}

func TestWorkerConversationManagerQueuedFollowup(t *testing.T) {
	completion := filepath.Join(t.TempDir(), "complete")
	t.Setenv("SPYNEL_TEST_WORKER_COMPLETIONS", completion)
	f := newWorkerAgentFixture(t, "acp")
	f.start(t)
	message := core.Message{Channel: "cli", Conversation: "queued-followup", SourceMessageID: "alpha", Text: "synthetic first"}
	events, done := make(chan core.Event, 64), make(chan error, 1)
	go func() { done <- f.service.Handle(t.Context(), message, func(e core.Event) { events <- e }) }()
	waitFollowupDelta(t, events, done)
	original := f.inspect(t)[0]
	job, _ := f.service.Runtime.JobForSession(original.Key)
	identity := f.client.Info()
	t.Logf("queued worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
	waiting := map[string]chan error{}
	streams := map[string]chan core.Event{}
	for index, source := range []string{"beta", "gamma"} {
		next := message
		next.SourceMessageID, next.Text = source, "synthetic "+source
		finished, stream := make(chan error, 1), make(chan core.Event, 64)
		waiting[source], streams[source] = finished, stream
		go func() { finished <- f.service.Handle(t.Context(), next, func(e core.Event) { stream <- e }) }()
		waitWorkerAgent(t, func() bool { return len(f.inspect(t)[0].Followups) == index+1 })
	}
	if f.inspect(t)[0].ConversationTurns != 1 {
		t.Fatal("queue admission inflated provider turns")
	}
	if err := os.WriteFile(completion+".1", nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, finished := range []chan error{done, waiting["beta"]} {
		select {
		case err := <-finished:
			if err != nil {
				t.Fatal("batched response handoff failed", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("batched predecessor request did not release")
		}
	}
	waitFollowupDelta(t, streams["gamma"], waiting["gamma"])
	if current := f.inspect(t)[0]; current.ConversationTurns != 2 || current.Sequence != original.Sequence || current.Conversation.ExecutionID != original.Conversation.ExecutionID {
		t.Fatal("batched dispatch lost execution identity or actual turn count")
	}
	if current, ok := f.service.Runtime.JobForSession(original.Key); !ok || current.StableID != job.StableID || current.ProviderIterations != 2 {
		t.Fatal("running job did not receive queued dispatch reservation")
	}
	f.stop(t)
	select {
	case err := <-waiting["gamma"]:
		if !errors.Is(err, orchestrator.ErrConversationPending) {
			t.Fatal("controller detach fabricated completion", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request did not release on controller detach")
	}
	if err := f.owner.Release(f.term); err != nil {
		t.Fatal(err)
	}
	old := f.client
	f.open(t)
	defer old.Close()
	if f.client.Info().Process != identity.Process {
		t.Fatal("replacement changed queued worker")
	}
	f.start(t)
	waitWorkerAgent(t, func() bool {
		current, ok := f.service.Runtime.JobForSession(original.Key)
		return ok && current.StableID == job.StableID && current.ProviderIterations == 2
	})
	if err := os.WriteFile(completion+".2", nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitWorkerAgent(t, func() bool { return len(f.inspect(t)) == 0 })
	archived, _, err := f.service.Runtime.ArchivedJob(job.Number)
	if err != nil || archived.Generation != job.Generation || !followupArchiveTurns(t, archived.Path, "2") {
		t.Fatal("attachment/replay changed persisted queued count", err)
	}
	entries, _, err := f.service.History.Entries(message.Channel, message.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	terminals := 0
	for _, entry := range entries {
		if entry.ExecutionID == original.Conversation.ExecutionID && len(entry.Covers) > 0 {
			terminals++
			if entry.SourceMessageID != "gamma" || !reflect.DeepEqual(entry.Covers, []string{"alpha", "beta", "gamma"}) {
				t.Fatal("batched terminal lost exact source ownership")
			}
		}
	}
	if terminals != 1 {
		t.Fatal("batched terminal was lost or duplicated")
	}
	data, err := os.ReadFile(f.log)
	if err != nil || strings.Count(string(data), "session/prompt\n") != 2 {
		t.Fatal("queued follow-ups did not use exactly two native provider turns", err)
	}
}
