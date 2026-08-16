package app

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/history"
	"github.com/agent0ai/spynel/internal/workspace"
)

type recentNotificationRouter struct {
	mu    sync.Mutex
	calls []string
}

func (r *recentNotificationRouter) Deliver(_ context.Context, channelName, conversation, eventID, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, channelName+"/"+conversation+"/"+eventID+"/"+text)
	return nil
}

func TestRecentAuthorizedNotificationSelectsLatestUserActivity(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	cfg.Channels.Telegram.Enabled = true
	cfg.Channels.Telegram.AllowedUsers = []string{"7"}
	service := New(cfg, newServiceHarness())
	router := &recentNotificationRouter{}
	service.DeliveryControl = router
	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	_, _ = service.History.Append("tui", "local", history.Entry{At: base, Role: "user", Content: "older local activity"})
	_, _ = service.History.Append("telegram", "TG-7", history.Entry{At: base.Add(time.Minute), Role: "user", Content: "newer remote activity"})
	if _, err := service.NotifyRecentAuthorized(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if len(router.calls) != 1 || !strings.HasPrefix(router.calls[0], "telegram/TG-7/") {
		t.Fatalf("recent delivery = %#v", router.calls)
	}
}

func TestRecentAuthorizedNotificationFailsClosedForAmbiguousOrRevokedRemoteUsers(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	cfg.Channels.Telegram.Enabled = true
	cfg.Channels.Telegram.AllowedUsers = []string{"7", "8"}
	service := New(cfg, newServiceHarness())
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	_, _ = service.History.Append("telegram", "TG-7", history.Entry{At: now, Role: "user", Content: "activity"})
	if _, err := service.NotifyRecentAuthorized(context.Background(), "must not leak"); err == nil || !strings.Contains(err.Error(), "multiple remote users") {
		t.Fatalf("ambiguous routing error = %v", err)
	}
	if _, err := service.Settings.Update(func(next *config.Config) error {
		next.Channels.Telegram.AllowedUsers = []string{"8"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.NotifyRecentAuthorized(context.Background(), "must not leak"); err == nil || !strings.Contains(err.Error(), "no unambiguous") {
		t.Fatalf("revoked routing error = %v", err)
	}
}

func TestRecentAuthorizedNotificationFailsClosedForTiedActivity(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	service := New(cfg, newServiceHarness())
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	_, _ = service.History.Append("tui", "first", history.Entry{At: now, Role: "user", Content: "first"})
	_, _ = service.History.Append("cli", "second", history.Entry{At: now, Role: "user", Content: "second"})
	if _, err := service.NotifyRecentAuthorized(context.Background(), "must not guess"); err == nil || !strings.Contains(err.Error(), "timestamps are tied") {
		t.Fatalf("tied routing error = %v", err)
	}
}

func TestRecentAuthorizedNotificationSelectsSoleWhatsAppUser(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	cfg.Channels.WhatsApp.Enabled = true
	cfg.Channels.WhatsApp.AllowedNumbers = []string{"+1 555 765 4321"}
	service := New(cfg, newServiceHarness())
	router := &recentNotificationRouter{}
	service.DeliveryControl = router
	_, _ = service.History.Append("whatsapp", "WA-15557654321", history.Entry{At: time.Now().UTC(), Role: "user", Content: "recent"})
	if _, err := service.NotifyRecentAuthorized(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if len(router.calls) != 1 || !strings.HasPrefix(router.calls[0], "whatsapp/WA-15557654321/") {
		t.Fatalf("recent WhatsApp delivery = %#v", router.calls)
	}
}
