//go:build linux || darwin

package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/localapi"
	"github.com/agent0ai/spynel/internal/orchestrator"
)

func TestWorkerConversationOrdinaryCLIStream(t *testing.T) {
	for _, name := range []string{"complete", "detached", "lost-ack"} {
		t.Run(name, func(t *testing.T) {
			detached := name == "detached"
			t.Setenv("SPYNEL_TEST_WORKER_CONVERSATION_ERROR", "1")
			completion := filepath.Join(t.TempDir(), "complete")
			t.Setenv("SPYNEL_TEST_WORKER_COMPLETIONS", completion)
			f := newWorkerAgentFixture(t)
			handle := f.service.Handle
			if !detached {
				f.start(t)
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				serverContext, stopServer := context.WithCancel(t.Context())
				serverDone := make(chan error, 1)
				server := &localapi.Server{Service: f.service, Token: f.term}
				go func() { serverDone <- server.Serve(serverContext, listener) }()
				client := localapi.NewClient(f.owner)
				client.HTTP.Transport.(*http.Transport).DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
				}
				t.Cleanup(func() {
					stopServer()
					if err := <-serverDone; err != nil {
						t.Error(err)
					}
					client.HTTP.CloseIdleConnections()
				})
				handle = client.Handle
			}
			identity := f.client.Info()
			t.Logf("ordinary request worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
			message := core.Message{Channel: "cli", Conversation: "ordinary", SourceMessageID: "request:one", Text: "synthetic request"}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			events := make(chan core.Event, 64)
			done := make(chan error, 1)
			go func() { done <- handle(ctx, message, func(e core.Event) { events <- e }) }()
			var seen []core.Event
			for {
				select {
				case err := <-done:
					t.Fatal("request ended before worker progress", err)
				case e := <-events:
					seen = append(seen, e)
					if e.Kind == core.EventDelta {
						goto active
					}
				case <-time.After(10 * time.Second):
					t.Fatal("ordinary request never streamed worker progress")
				}
			}
		active:
			executions := f.inspect(t)
			if len(executions) != 1 || !executions[0].SourcesReserved || executions[0].Conversation.SourceMessageID != message.SourceMessageID {
				t.Fatal("ordinary request lost source ownership")
			}
			original, ok := f.service.Runtime.JobForSession("chat:cli:ordinary")
			if !ok || original.WorkID != executions[0].Conversation.ExecutionID || original.ProviderIterations != 1 {
				t.Fatal("ordinary request lost job identity")
			}
			if detached {
				cancel()
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("detached request reported completion")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("request did not detach")
				}
				if current := f.inspect(t); len(current) != 1 || current[0].Done {
					t.Fatal("request detach interrupted the provider")
				}
				entries, _, err := f.service.History.RecoveryEntries("cli", "ordinary", 10)
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if entry.Terminal {
						t.Fatal("detach manufactured terminal history")
					}
				}
				f.stop(t)
				if err := f.owner.Release(f.term); err != nil {
					t.Fatal(err)
				}
				f.open(t)
				if f.client.Info().Process != identity.Process {
					t.Fatal("controller replacement changed worker identity")
				}
				f.start(t)
				waitWorkerAgent(t, func() bool {
					job, ok := f.service.Runtime.JobForSession("chat:cli:ordinary")
					return ok && job.StableID == original.StableID && job.Number == original.Number &&
						job.Generation == original.Generation && job.ProviderIterations == 1
				})
			}
			var restore func()
			if name == "lost-ack" {
				f.client.Close()
				restore = dropWorkerReply(t, f.client.Info().Socket, "ack", true, false)
			}
			if err := os.WriteFile(completion+".1", []byte("finish"), 0600); err != nil {
				t.Fatal(err)
			}
			if !detached {
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("ordinary request did not finish")
				}
				for len(events) > 0 {
					seen = append(seen, <-events)
				}
				active, terminal := false, 0
				for _, e := range seen {
					if e.RequestID != message.SourceMessageID {
						t.Fatal("request event lost correlation")
					}
					if e.Kind == core.EventActivity {
						active = e.Active
					}
					if e.Done && (e.Kind == core.EventFinal || e.Kind == core.EventError) {
						if active {
							t.Fatal("activity remained active at terminal delivery")
						}
						terminal++
					}
				}
				if terminal != 1 {
					t.Fatal("ordinary request did not receive one terminal")
				}
			}
			if restore != nil {
				restore()
				f.client.Close()
			}
			waitWorkerAgent(t, func() bool { return len(f.inspect(t)) == 0 })
			entries, _, err := f.service.History.RecoveryEntries("cli", "ordinary", 20)
			terminals := 0
			for _, entry := range entries {
				if entry.Terminal {
					terminals++
					if entry.SourceMessageID != message.SourceMessageID || len(entry.Covers) != 1 || entry.Covers[0] != message.SourceMessageID {
						t.Fatal("terminal lost exact request coverage")
					}
				}
			}
			methods, readErr := os.ReadFile(f.log)
			if err != nil || readErr != nil || terminals != 1 || strings.Count(string(methods), "turn/start\n") != 1 {
				t.Fatal("request replay duplicated execution/history", err, readErr)
			}
		})
	}
}

func TestWorkerConversationOrdinaryCLIUnknownSend(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		name := "absent"
		if accepted {
			name = "accepted"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("SPYNEL_TEST_WORKER_CONVERSATION_ERROR", "1")
			completion := filepath.Join(t.TempDir(), "complete")
			t.Setenv("SPYNEL_TEST_WORKER_COMPLETIONS", completion)
			f := newWorkerAgentFixture(t)
			identity := f.client.Info()
			t.Logf("uncertain request worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
			message := core.Message{Channel: "cli", Conversation: "uncertain", SourceMessageID: "request:unknown", Text: "synthetic request"}
			f.client.Close()
			restore := dropWorkerReply(t, f.client.Info().Socket, "send", accepted, false)
			err := f.service.Handle(t.Context(), message, nil)
			restore()
			f.client.Close()
			if !errors.Is(err, orchestrator.ErrConversationPending) {
				t.Fatal("unknown Send was not left pending", err)
			}
			entries, _, err := f.service.History.RecoveryEntries("cli", "uncertain", 10)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Terminal {
					t.Fatal("unknown Send fabricated terminal history")
				}
			}
			if _, ok := f.service.Runtime.JobForSession("chat:cli:uncertain"); !ok {
				t.Fatal("unknown Send discarded its provisional job")
			}
			f.start(t)
			if accepted {
				if err := os.WriteFile(completion+".1", []byte("finish"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			waitWorkerAgent(t, func() bool {
				_, ok := f.service.Runtime.JobForSession("chat:cli:uncertain")
				return !ok && len(f.inspect(t)) == 0
			})
			entries, _, err = f.service.History.RecoveryEntries("cli", "uncertain", 10)
			terminals := 0
			for _, entry := range entries {
				if entry.Terminal {
					terminals++
				}
			}
			methods, readErr := os.ReadFile(f.log)
			want := 0
			if accepted {
				want = 1
			}
			if err != nil || readErr != nil || terminals != want || strings.Count(string(methods), "turn/start\n") != want {
				t.Fatal("unknown Send reconciliation fabricated or repeated execution", err, readErr)
			}
		})
	}
}
