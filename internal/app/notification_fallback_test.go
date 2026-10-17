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
