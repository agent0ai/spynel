package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/orchestrator"
	"github.com/agent0ai/spynel/internal/workspace"
)

type fallibleNotificationRouter struct {
	calls int
	err   error
}

func (r *fallibleNotificationRouter) Deliver(context.Context, string, string, string, string, []core.OutboundAttachment) error {
	r.calls++
	return r.err
}

func TestExplicitRemoteFallbackUsesLivePresentationAndStableHistory(t *testing.T) {
	for _, fallback := range []string{"telegram/TG-7", "whatsapp/WA-15557654321"} {
		for _, live := range []bool{false, true} {
			t.Run(fallback+map[bool]string{true: "/live", false: "/offline"}[live], func(t *testing.T) {
				root := t.TempDir()
				if err := workspace.Init(root, false); err != nil {
					t.Fatal(err)
				}
				cfg, _ := config.Load(config.PathForRoot(root))
				cfg.Channels.Telegram.AllowedUsers = []string{"7", "8"}
				cfg.Channels.WhatsApp.AllowedNumbers = []string{"15557654321", "15557654322"}
				s := New(cfg, newServiceHarness())
				t.Cleanup(func() { _ = s.Close() })
				router := &fallibleNotificationRouter{}
				s.DeliveryControl = router
				_, _ = s.History.Ensure("tui", "original")
				channel, conversation, _ := strings.Cut(fallback, "/")
				_, _ = s.History.Ensure(channel, conversation)
				if live {
					if err := s.RegisterLiveTUI("fixture", "original", time.Now()); err != nil {
						t.Fatal(err)
					}
				}
				entry, err := s.Orchestrator.Outbox.EnqueueWithFallback("stable", "waiting", "tui/original", fallback, "controlled update")
				if err != nil {
					t.Fatal(err)
				}
				if err := s.Orchestrator.Outbox.Process(context.Background()); err != nil {
					t.Fatal(err)
				}
				wantChannel, wantConversation, wantRemoteCalls := "tui", "original", 0
				if !live {
					wantChannel, wantConversation, wantRemoteCalls = channel, conversation, 1
				}
				// Simulate a crash after history commit but before the outbox's
				// delivered write, then change primary presentation availability.
				entry.State, entry.NextAttemptAt = "pending", time.Time{}
				data, _ := json.Marshal(entry)
				if err := os.WriteFile(cfg.StatePath("runtime", "outbox", entry.ID+".json"), data, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := s.RegisterLiveTUI("fixture", "original", time.Now()); err != nil {
					t.Fatal(err)
				}
				if err := s.Orchestrator.Outbox.Process(context.Background()); err != nil {
					t.Fatal(err)
				}
				entries, _, err := s.History.RecentEntries(wantChannel, wantConversation, 20, 12000)
				if err != nil || len(entries) != 1 || entries[0].EventID != entry.ID || entries[0].Role != "assistant" || router.calls != wantRemoteCalls {
					t.Fatalf("stable original/fallback delivery = %#v calls=%d err=%v", entries, router.calls, err)
				}
				otherChannel, otherConversation := channel, conversation
				if !live {
					otherChannel, otherConversation = "tui", "original"
				}
				other, _, _ := s.History.RecentEntries(otherChannel, otherConversation, 20, 12000)
				if len(other) != 0 {
					t.Fatal("retry switched to a second destination or invented original delivery")
				}
				chat, err := s.chatPrompt(core.Message{Channel: wantChannel, Conversation: wantConversation})
				if err != nil || strings.Count(chat, "controlled update") != 1 {
					t.Fatalf("actual destination chat visibility: %v", err)
				}
			})
		}
	}
}

func TestFallbackTransportFailureRemainsPendingUntilActualSend(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	cfg.Channels.Telegram.AllowedUsers = []string{"7"}
	s := New(cfg, newServiceHarness())
	t.Cleanup(func() { _ = s.Close() })
	_, _ = s.History.Ensure("tui", "original")
	_, _ = s.History.Ensure("telegram", "TG-7")
	router := &fallibleNotificationRouter{err: errors.New("controlled transport unavailable")}
	s.DeliveryControl = router
	now := time.Now().UTC()
	s.Orchestrator.Outbox.Now = func() time.Time { return now }
	id, err := s.NotifyWithFallback(context.Background(), "tui/original", "telegram/TG-7", "actual result")
	if err != nil || id == "" {
		t.Fatalf("queue admission = %q, %v", id, err)
	}
	for _, origin := range []orchestrator.Origin{{Channel: "tui", Conversation: "original"}, {Channel: "telegram", Conversation: "TG-7"}} {
		entries, _, err := s.History.RecentEntries(origin.Channel, origin.Conversation, 20, 12000)
		if err != nil || len(entries) != 0 {
			t.Fatalf("failed send invented a delivered message: %#v, %v", entries, err)
		}
	}
	data, err := os.ReadFile(cfg.StatePath("runtime", "outbox", id+".json"))
	var entry orchestrator.OutboxEntry
	if err != nil || json.Unmarshal(data, &entry) != nil || entry.State != "pending" || entry.LastError == "" || !entry.DeliveredAt.IsZero() {
		t.Fatalf("pending admission evidence: %v", err)
	}
	router.err = nil
	now = now.Add(time.Minute)
	if err := s.Orchestrator.Outbox.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Orchestrator.Outbox.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	entries, _, _ := s.History.RecentEntries("telegram", "TG-7", 20, 12000)
	if len(entries) != 1 || entries[0].Content != "actual result" || router.calls != 2 {
		t.Fatalf("retry history = %#v calls=%d", entries, router.calls)
	}
}

func TestFallbackRevocationAndUnknownOrAliasedOriginsFailClosed(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	cfg.Channels.Telegram.AllowedUsers = []string{"7"}
	s := New(cfg, newServiceHarness())
	t.Cleanup(func() { _ = s.Close() })
	router := &fallibleNotificationRouter{}
	s.DeliveryControl = router
	_, _ = s.History.Ensure("tui", "original")
	_, _ = s.History.Ensure("tui", "aliased_origin")
	_, _ = s.History.Ensure("telegram", "TG-7")
	for _, origin := range []string{"tui/missing", "tui/aliased/origin"} {
		if _, err := s.NotifyWithFallback(context.Background(), origin, "telegram/TG-7", "must not leak"); err == nil {
			t.Fatalf("unsafe origin %q accepted", origin)
		}
	}
	for _, fallback := range []string{"telegram/TG-8", "telegram/TG-group-7"} {
		if _, err := s.NotifyWithFallback(context.Background(), "tui/original", fallback, "must not leak"); err == nil {
			t.Fatal("unknown, unauthorized or group fallback accepted")
		}
	}
	_, err := s.Orchestrator.Outbox.EnqueueWithFallback("stable-revocation", "waiting", "telegram/TG-7", "tui/original", "must not leak")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Settings.Update(func(next *config.Config) error {
		next.Channels.Telegram.AllowedUsers = []string{"8"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Orchestrator.Outbox.Process(context.Background()); err == nil || router.calls != 0 {
		t.Fatalf("revoked primary escaped through fallback: calls=%d err=%v", router.calls, err)
	}
	entries, _, _ := s.History.RecentEntries("tui", "original", 20, 12000)
	if len(entries) != 0 {
		t.Fatal("revoked primary leaked to fallback")
	}
	// The fallback itself is revalidated at delivery, not trusted from admission.
	_, _ = s.Orchestrator.Outbox.EnqueueWithFallback("fallback-revoked", "waiting", "tui/original", "telegram/TG-7", "must not leak")
	if err := s.Orchestrator.Outbox.Process(context.Background()); err == nil || router.calls != 0 {
		t.Fatal("revoked fallback was sent")
	}
}

func TestSwitchedTUIFallbackRequiresAnActuallySelectedOriginal(t *testing.T) {
	for _, mode := range []string{"fallback", "another-client", "no-fallback"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			if err := workspace.Init(root, false); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(config.PathForRoot(root))
			if err != nil {
				t.Fatal(err)
			}
			cfg.Channels.Telegram.AllowedUsers = []string{"7"}
			s := New(cfg, newServiceHarness())
			t.Cleanup(func() { _ = s.Close() })
			router := &fallibleNotificationRouter{}
			s.DeliveryControl = router
			for _, origin := range []orchestrator.Origin{{Channel: "tui", Conversation: "original"}, {Channel: "tui", Conversation: "selected"}, {Channel: "telegram", Conversation: "TG-7"}} {
				if _, err := s.History.Ensure(origin.Channel, origin.Conversation); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now().UTC()
			for _, selected := range []string{"original", "selected"} {
				if err := s.RegisterLiveTUI("client", selected, now); err != nil {
					t.Fatal(err)
				}
			}
			if !s.liveTUIConversations(now)["original"] {
				t.Fatal("switch lost cleanup protection")
			}
			if mode == "another-client" {
				if err := s.RegisterLiveTUI("other-client", "original", now); err != nil {
					t.Fatal(err)
				}
			}
			fallback := "telegram/TG-7"
			if mode == "no-fallback" {
				fallback = ""
			}
			if _, err := s.NotifyWithFallback(context.Background(), "tui/original", fallback, "controlled update"); err != nil {
				t.Fatal(err)
			}
			local, _, err := s.History.RecentEntries("tui", "original", 20, 12000)
			if err != nil {
				t.Fatal(err)
			}
			remote, _, err := s.History.RecentEntries("telegram", "TG-7", 20, 12000)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "fallback" {
				if router.calls != 1 || len(remote) != 1 || len(local) != 0 {
					t.Fatalf("fallback calls=%d local=%d remote=%d", router.calls, len(local), len(remote))
				}
			} else if router.calls != 0 || len(local) != 1 || len(remote) != 0 {
				t.Fatalf("saved/live original calls=%d local=%d remote=%d", router.calls, len(local), len(remote))
			}
		})
	}
}

func TestUnansweredEscalationUsesAssociatedRouteAndDeduplicates(t *testing.T) {
	for _, fallback := range []string{"telegram/TG-7", "whatsapp/WA-15557654321"} {
		t.Run(fallback, func(t *testing.T) {
			root := t.TempDir()
			if err := workspace.Init(root, false); err != nil {
				t.Fatal(err)
			}
			cfg, _ := config.Load(config.PathForRoot(root))
			cfg.Channels.Telegram.AllowedUsers = []string{"7"}
			cfg.Channels.WhatsApp.AllowedNumbers = []string{"15557654321"}
			s := New(cfg, newServiceHarness())
			t.Cleanup(func() { _ = s.Close() })
			router := &fallibleNotificationRouter{}
			s.DeliveryControl = router
			_, _ = s.History.Ensure("tui", "original")
			channel, conversation, _ := strings.Cut(fallback, "/")
			_, _ = s.History.Ensure(channel, conversation)
			if err := s.RegisterLiveTUI("fixture", "original", time.Now()); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				if _, err := s.Notify(context.Background(), "tui/original", "Needs your answer: GREEN or RED?"); err != nil {
					t.Fatal(err)
				}
			}
			ids := make(chan string, 8)
			errs := make(chan error, 8)
			for i := 0; i < 8; i++ {
				go func() {
					id, err := s.NotifyEscalation(context.Background(), "tui/original", fallback, "choice:green-or-red", "Needs your answer: choose GREEN or RED to resume the task.")
					ids <- id
					errs <- err
				}()
			}
			var id string
			for i := 0; i < 8; i++ {
				got := <-ids
				if err := <-errs; err != nil {
					t.Fatal(err)
				}
				if id != "" && id != got {
					t.Fatal("duplicate escalation identities")
				}
				id = got
			}
			if router.calls != 1 {
				t.Fatalf("simultaneous sends=%d", router.calls)
			}
			local, _, _ := s.History.RecentEntries("tui", "original", 20, 12000)
			if len(local) != 3 {
				t.Fatal("escalation repeated the unattended TUI")
			}
			restarted := New(cfg, newServiceHarness())
			t.Cleanup(func() { _ = restarted.Close() })
			restarted.DeliveryControl = router
			if got, err := restarted.NotifyEscalation(context.Background(), "tui/original", fallback, "choice:green-or-red", "Needs your answer: choose GREEN or RED to resume the task."); err != nil || got != id {
				t.Fatalf("restart replay %q %v", got, err)
			}
			if router.calls != 1 {
				t.Fatal("restart duplicated send")
			}
			remote, _, _ := s.History.RecentEntries(channel, conversation, 20, 12000)
			if len(remote) != 1 || remote[0].EventID != id {
				t.Fatal("escalation missing from destination history")
			}
			if _, err := s.NotifyEscalation(context.Background(), "tui/original", fallback, "choice:green-or-red", "changed retry"); err == nil {
				t.Fatal("changed delivery reused a decision key")
			}
			for _, bad := range []string{"", "telegram/TG-8", "telegram/TG-group-7", "whatsapp/WA-group-7"} {
				if _, err := s.NotifyEscalation(context.Background(), "tui/original", bad, "bad-route", "must not leak"); err == nil {
					t.Fatal("invalid/unassociated escalation accepted")
				}
			}
			router.err = errors.New("synthetic unavailable transport")
			failedID, err := s.NotifyEscalation(context.Background(), "tui/original", fallback, "second-decision", "another question")
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(cfg.StatePath("runtime", "outbox", failedID+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var pending orchestrator.OutboxEntry
			if err := json.Unmarshal(data, &pending); err != nil {
				t.Fatal(err)
			}
			if pending.State != "pending" || !strings.Contains(pending.LastError, "unavailable") {
				t.Fatal("failed send lost retry evidence")
			}
			// Revocation after admission must prevent the scheduled retry from sending.
			_, err = s.Settings.Update(func(next *config.Config) error {
				next.Channels.Telegram.AllowedUsers = []string{"8"}
				next.Channels.WhatsApp.AllowedNumbers = []string{"15557654322"}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			router.err = nil
			before := router.calls
			s.Orchestrator.Outbox.Now = func() time.Time { return time.Now().Add(time.Minute) }
			if err := s.Orchestrator.Outbox.Process(context.Background()); err == nil {
				t.Fatal("revoked queued escalation succeeded")
			}
			if router.calls != before {
				t.Fatal("revoked route leaked at retry")
			}
		})
	}
}
