//go:build linux || darwin

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/orchestrator"
)

type remoteRequestRouter struct {
	mu                    sync.Mutex
	channel, conversation string
	active                int
	fail                  bool
	attempts, sent        []string
}

func (r *remoteRequestRouter) Deliver(context.Context, string, string, string, string, []core.OutboundAttachment) error {
	return errors.New("ordinary worker replies require acknowledged events")
}

func (r *remoteRequestRouter) DeliverEvent(_ context.Context, name, conversation, receipt string, e core.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if name != r.channel || conversation != r.conversation || receipt == "" {
		return errors.New("unexpected remote route or receipt")
	}
	if e.Kind == core.EventActivity {
		if e.Active {
			r.active++
		} else {
			r.active--
		}
	}
	if e.Done && !e.Continues && (e.Kind == core.EventFinal || e.Kind == core.EventError) {
		if r.active != 0 {
			return errors.New("remote activity was not released before delivery")
		}
		r.attempts = append(r.attempts, receipt)
		if r.fail {
			return errors.New("synthetic remote send failure")
		}
		r.sent = append(r.sent, receipt)
	}
	return nil
}

func configureRemoteRequest(t *testing.T, f *workerAgentFixture, name string) (core.Message, *remoteRequestRouter) {
	t.Helper()
	var err error
	f.cfg, err = f.service.Settings.Update(func(cfg *config.Config) error {
		cfg.Orchestrator.SemanticHeartbeatMinutes = 5
		cfg.Channels.Telegram.Enabled = true
		cfg.Channels.Telegram.AllowedUsers = []string{"7"}
		cfg.Channels.WhatsApp.Enabled = true
		cfg.Channels.WhatsApp.AllowedNumbers = []string{"15551234567"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	conversation := "TG-7"
	if name == "whatsapp" {
		conversation = "WA-15551234567"
	}
	router := &remoteRequestRouter{channel: name, conversation: conversation}
	f.service.SetConversationDelivery(router)
	f.cfg.Orchestrator.IntervalSec = 1
	f.service.Orchestrator.ApplyRuntimeConfig(f.cfg)
	return core.Message{Channel: name, Conversation: conversation, SourceMessageID: "request:remote", Text: "synthetic remote request"}, router
}

func TestWorkerConversationOrdinaryRemoteRequest(t *testing.T) {
	for _, name := range []string{"telegram", "whatsapp"} {
		for _, scenario := range []string{"ordinary", "creation", "replacement", "delivery-failure"} {
			t.Run(name+"/"+scenario, func(t *testing.T) {
				t.Setenv("SPYNEL_TEST_WORKER_CONVERSATION_ERROR", "3")
				completion := filepath.Join(t.TempDir(), "complete")
				t.Setenv("SPYNEL_TEST_WORKER_COMPLETIONS", completion)
				f := newWorkerAgentFixture(t)
				message, router := configureRemoteRequest(t, f, name)
				if scenario == "creation" {
					message.Text = "/task synthetic finite request"
				}
				identity := f.client.Info()
				t.Logf("ordinary remote worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
				ctx, cancel := context.WithCancel(t.Context())
				err := f.service.Handle(ctx, message, func(core.Event) { t.Error("worker reply used the unacknowledged inbound emitter") })
				cancel() // Returning from intake must not own the consumer or provider.
				if err != nil {
					t.Fatal("remote request admission failed", err)
				}
				key := "chat:" + name + ":" + message.Conversation
				original, ok := f.service.Runtime.JobForSession(key)
				if !ok || original.ProviderIterations != 1 {
					t.Fatal("remote admission lost its durable job")
				}
				f.start(t)
				waitWorkerAgent(t, func() bool {
					state := f.inspect(t)[0]
					router.mu.Lock()
					defer router.mu.Unlock()
					return router.active == 1 && state.CommittedCursor >= 2
				})
				if scenario == "replacement" {
					f.stop(t)
					router.mu.Lock()
					if router.active != 0 {
						t.Error("controller detach retained remote activity")
					}
					router.mu.Unlock()
					if err := f.owner.Release(f.term); err != nil {
						t.Fatal(err)
					}
					f.client.Close()
					f.open(t)
					f.service.SetConversationDelivery(router)
					f.start(t)
					waitWorkerAgent(t, func() bool {
						job, ok := f.service.Runtime.JobForSession(key)
						router.mu.Lock()
						defer router.mu.Unlock()
						return ok && job.StableID == original.StableID && job.Number == original.Number && job.Generation == original.Generation && job.ProviderIterations == 1 && router.active == 1
					})
				}
				if scenario == "delivery-failure" {
					router.mu.Lock()
					router.fail = true
					router.mu.Unlock()
				}
				if err := os.WriteFile(completion+".1", nil, 0600); err != nil {
					t.Fatal(err)
				}
				if scenario == "delivery-failure" {
					waitWorkerAgent(t, func() bool {
						router.mu.Lock()
						defer router.mu.Unlock()
						job, ok := f.service.Runtime.JobForSession(key)
						return len(router.attempts) > 0 && ok && job.Execution == app.JobDegraded
					})
					state := f.inspect(t)[0]
					if state.CommittedCursor == state.LastCursor {
						t.Fatal("failed transport delivery committed the terminal")
					}
					router.mu.Lock()
					router.fail = false
					router.mu.Unlock()
				}
				waitWorkerAgent(t, func() bool { return len(f.inspect(t)) == 0 })
				router.mu.Lock()
				defer router.mu.Unlock()
				if router.active != 0 || len(router.sent) != 1 || router.sent[0] != router.attempts[0] {
					t.Fatal("remote delivery/activity/receipt did not settle once")
				}
				entries, _, err := f.service.History.RecoveryEntries(name, message.Conversation, 20)
				terminals := 0
				for _, e := range entries {
					if e.Terminal {
						terminals++
						if len(e.Covers) != 1 || e.Covers[0] != message.SourceMessageID {
							t.Fatal("remote terminal lost exact source coverage")
						}
					}
				}
				methods, readErr := os.ReadFile(f.log)
				if err != nil || readErr != nil || terminals != 1 || strings.Count(string(methods), "turn/start\n") != 1 || f.client.Info().Process != identity.Process || f.service.Runtime.Status().Jobs != 0 {
					t.Fatal("remote settlement duplicated execution/history or lost native identity", err, readErr)
				}
			})
		}
	}
}

func TestWorkerConversationOrdinaryRemoteUnknownSend(t *testing.T) {
	for _, name := range []string{"telegram", "whatsapp"} {
		for _, accepted := range []bool{false, true} {
			lane := "absent"
			if accepted {
				lane = "accepted"
			}
			t.Run(name+"/"+lane, func(t *testing.T) {
				f := newWorkerAgentFixture(t)
				message, router := configureRemoteRequest(t, f, name)
				identity := f.client.Info()
				t.Logf("unknown remote worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
				f.client.Close()
				restore := dropWorkerReply(t, f.client.Info().Socket, "send", accepted, false)
				err := f.service.Handle(t.Context(), message, nil)
				restore()
				f.client.Close()
				if !errors.Is(err, orchestrator.ErrConversationPending) {
					t.Fatal("unknown remote Send did not remain pending", err)
				}
				entries, _, err := f.service.History.RecoveryEntries(name, message.Conversation, 10)
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range entries {
					if e.Terminal {
						t.Fatal("unknown remote Send invented terminal history")
					}
				}
				f.start(t)
				if accepted {
					if _, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "interrupt", Key: "chat:" + name + ":" + message.Conversation}); err != nil {
						t.Fatal(err)
					}
				}
				waitWorkerAgent(t, func() bool {
					_, ok := f.service.Runtime.JobForSession("chat:" + name + ":" + message.Conversation)
					return !ok && len(f.inspect(t)) == 0
				})
				router.mu.Lock()
				defer router.mu.Unlock()
				want := 0
				if accepted {
					want = 1
				}
				methods, err := os.ReadFile(f.log)
				if err != nil || len(router.sent) != want || router.active != 0 || strings.Count(string(methods), "turn/start\n") != want || f.client.Info().Process != identity.Process {
					t.Fatal("unknown remote Send repeated/fabricated delivery or execution", err)
				}
			})
		}
	}
}
