//go:build linux || darwin

package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/orchestrator"
)

func TestWorkerConversationSessionReset(t *testing.T) {
	for _, action := range []string{"clear", "delete"} {
		t.Run(action, func(t *testing.T) {
			t.Setenv("SPYNEL_TEST_WORKER_CONVERSATION_ERROR", "1")
			completion := filepath.Join(t.TempDir(), "complete")
			t.Setenv("SPYNEL_TEST_WORKER_COMPLETIONS", completion)
			f := newWorkerAgentFixture(t)
			f.start(t)
			message := core.Message{Channel: "tui", Conversation: "session-reset", SourceMessageID: "alpha", Text: "synthetic first"}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			events, done := make(chan core.Event, 64), make(chan error, 1)
			go func() { done <- f.service.Handle(ctx, message, func(e core.Event) { events <- e }) }()
			waitFollowupDelta(t, events, done)
			original := f.inspect(t)[0]
			identity := f.client.Info()
			t.Logf("session worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
			reset := func(wantSuccess bool) {
				t.Helper()
				before, err := os.ReadFile(f.service.History.Path(message.Channel, message.Conversation))
				if err != nil {
					t.Fatal(err)
				}
				if action == "clear" {
					command := message
					command.SourceMessageID = ""
					command.Text = "/clear"
					var reply string
					err = f.service.Handle(t.Context(), command, func(e core.Event) { reply += e.Text })
					if err != nil || strings.Contains(reply, "Cannot clear") == wantSuccess {
						t.Fatal("clear selected the wrong session owner", reply, err)
					}
				} else {
					encoded := base64.RawURLEncoding.EncodeToString([]byte(message.Channel + "\x00" + message.Conversation))
					_, err = f.service.ScreenAction(t.Context(), "resume", "delete:"+encoded, nil)
					if (err == nil) != wantSuccess {
						t.Fatal("delete selected the wrong session owner", err)
					}
				}
				after, readErr := os.ReadFile(f.service.History.Path(message.Channel, message.Conversation))
				if !wantSuccess && (readErr != nil || !strings.HasPrefix(string(after), string(before))) {
					t.Fatal("refused reset discarded history", readErr)
				}
				if wantSuccess && readErr == nil && len(after) != 0 {
					t.Fatal("confirmed reset did not clear history")
				}
			}
			reset(false)
			if current := f.inspect(t); len(current) != 1 || current[0].Sequence != original.Sequence || current[0].Done {
				t.Fatal("reset changed the active execution")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, orchestrator.ErrConversationPending) {
					t.Fatal("detach fabricated completion", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("request did not detach")
			}
			f.stop(t)
			if err := f.owner.Release(f.term); err != nil {
				t.Fatal(err)
			}
			old := f.client
			f.open(t)
			defer old.Close()
			if f.client.Info().Process != identity.Process {
				t.Fatal("session commands replaced the worker")
			}
			if err := os.WriteFile(completion+".1", []byte("finish"), 0600); err != nil {
				t.Fatal(err)
			}
			waitWorkerAgent(t, func() bool { current := f.inspect(t); return len(current) == 1 && current[0].Done })
			reset(false) // Provider completion cannot discard unsettled delivery.
			if _, err := old.Call(t.Context(), harness.WorkerRequest{Action: "reset", Key: original.Key}); err == nil {
				t.Fatal("obsolete controller reset a retained session")
			}
			f.start(t)
			waitWorkerAgent(t, func() bool { return len(f.inspect(t)) == 0 })
			reply, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "session", Key: original.Key})
			if err != nil || reply.Session == nil || reply.Session.ThreadID == "" {
				t.Fatal("completed provider thread was not retained", err)
			}
			f.client.Close()
			restore := dropWorkerReply(t, f.client.Info().Socket, "reset", true, false)
			reset(false) // A lost reply cannot authorize local history deletion.
			restore()
			f.client.Close()
			reset(true)
			reply, err = f.client.Call(t.Context(), harness.WorkerRequest{Action: "session", Key: original.Key})
			if err != nil || reply.Session == nil || reply.Session.ThreadID != "" || reply.Session.Active {
				t.Fatal("confirmed reset left the worker thread usable", err)
			}
			methods, err := os.ReadFile(f.log)
			if err != nil || strings.Count(string(methods), "turn/start\n") != 1 {
				t.Fatal("session operations dispatched another provider turn", err)
			}
		})
	}
}

func TestWorkerConversationStrictFollowupRequiresRetainedExecution(t *testing.T) {
	f := newWorkerAgentFixture(t)
	c := harness.WorkerConversation{Channel: "cli", Conversation: "finished", SourceMessageID: "beta", ExecutionID: "finished-execution", Sources: []string{"beta"}}
	err := f.service.Orchestrator.Converse(t.Context(), c, "synthetic stale inspection", "synthetic follow-up", true, func(context.Context, int, harness.WorkerDelivery) error {
		t.Error("strict follow-up delivered an event without an execution")
		return nil
	})
	var refused *harness.SendNotAdmittedError
	if !errors.As(err, &refused) || errors.Is(err, orchestrator.ErrConversationPending) {
		t.Fatal("strict follow-up was not refused before admission", err)
	}
	if len(f.inspect(t)) != 0 || len(f.service.Runtime.Jobs()) != 0 {
		t.Fatal("strict follow-up created a replacement execution/job")
	}
	methods, err := os.ReadFile(f.log)
	if err != nil || strings.Contains(string(methods), "turn/start\n") {
		t.Fatal("strict follow-up reached the provider", err)
	}
}

func TestWorkerConversationStrictFollowupUnknownSession(t *testing.T) {
	f := newWorkerAgentFixture(t)
	f.client.Close()
	restore := dropWorkerReply(t, f.client.Info().Socket, "session", false, false)
	err := f.service.Handle(t.Context(), core.Message{Channel: "cli", Conversation: "unknown-session", SourceMessageID: "alpha", Text: "synthetic follow-up", FollowupOnly: true}, nil)
	restore()
	f.client.Close()
	if err == nil || !strings.Contains(err.Error(), "inspect active conversation") {
		t.Fatal("unknown session was classified as active or inactive", err)
	}
	if _, err := os.Stat(f.service.History.Path("cli", "unknown-session")); !os.IsNotExist(err) {
		t.Fatal("unconfirmed follow-up changed history", err)
	}
	if len(f.inspect(t)) != 0 || len(f.service.Runtime.Jobs()) != 0 {
		t.Fatal("unconfirmed follow-up admitted work")
	}
}
