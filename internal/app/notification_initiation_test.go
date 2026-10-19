package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/orchestrator"
	"github.com/agent0ai/spynel/internal/workspace"
)

func newInitiationService(t *testing.T) *Service {
	t.Helper()
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Channels.Telegram.Enabled = true
	cfg.Channels.Telegram.Token = "fixture"
	cfg.Channels.Telegram.AllowedUsers = []string{"7", "8"}
	cfg.Channels.Telegram.GroupMode = "all"
	cfg.Channels.WhatsApp.AllowGroups = true
	cfg.Channels.WhatsApp.Enabled = true
	cfg.Channels.WhatsApp.AllowedNumbers = []string{"15557654321", "15557654322"}
	s := New(cfg, newServiceHarness())
	t.Cleanup(func() { _ = s.Close() })
	for _, name := range []string{"telegram", "whatsapp"} {
		s.SetConnectionStatus(channel.ConnectionStatus{Name: name, State: channel.ConnectionConnected})
	}
	return s
}

func TestNotificationInitiatesAuthorizedRemoteConversation(t *testing.T) {
	for _, route := range []string{"telegram/TG-7", "whatsapp/WA-15557654321"} {
		for _, concurrent := range []bool{false, true} {
			mode := "direct"
			if concurrent {
				mode = "concurrent-fallback"
			}
			t.Run(route+"/"+mode, func(t *testing.T) {
				s := newInitiationService(t)
				router := &recentNotificationRouter{}
				s.DeliveryControl = router
				name, conversation, _ := strings.Cut(route, "/")
				origin := orchestrator.Origin{Channel: name, Conversation: conversation}
				if err := s.validateOrigin(origin); err == nil {
					t.Fatal("read/inbound origin must remain unknown")
				}
				contextText, err := s.notificationContext(origin)
				if err != nil || contextText != "" {
					t.Fatalf("absent context: %q %v", contextText, err)
				}
				if _, err := os.Stat(s.History.Path(name, conversation)); !os.IsNotExist(err) {
					t.Fatal("context fabricated registration")
				}
				var id string
				if concurrent {
					if _, err := s.History.Ensure("tui", "original"); err != nil {
						t.Fatal(err)
					}
					var wg sync.WaitGroup
					ids := make(chan string, 8)
					for range 8 {
						wg.Add(1)
						go func() {
							defer wg.Done()
							event, err := s.NotifyEscalation(context.Background(), "tui/original", route, "one-question", "initiated outbound")
							if err != nil {
								t.Error(err)
							}
							ids <- event
						}()
					}
					wg.Wait()
					close(ids)
					for event := range ids {
						if id == "" {
							id = event
						}
						if event == "" || event != id {
							t.Fatal("keyed admission was duplicated")
						}
					}
				} else {
					id, err = s.Notify(context.Background(), route, "initiated outbound")
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := s.validateOrigin(origin); err != nil {
					t.Fatal(err)
				}
				followup, err := s.Notify(context.Background(), route, "followup outbound")
				if err != nil {
					t.Fatal(err)
				}
				entries, _, err := s.History.RecentEntries(name, conversation, 20, 12000)
				if err != nil || len(entries) != 2 || entries[0].EventID != id || entries[1].EventID != followup || len(router.calls) != 2 {
					t.Fatalf("history=%#v calls=%d err=%v", entries, len(router.calls), err)
				}
				prompt, err := s.chatPrompt(core.Message{Channel: name, Conversation: conversation})
				if err != nil || strings.Count(prompt, "initiated outbound") != 1 || strings.Count(prompt, "followup outbound") != 1 {
					t.Fatalf("next chat missed actual delivery: %v", err)
				}
				restarted := New(s.Settings.Snapshot(), newServiceHarness())
				defer restarted.Close()
				restarted.DeliveryControl = router
				if err := restarted.Orchestrator.Outbox.Process(context.Background()); err != nil {
					t.Fatal(err)
				}
				if len(router.calls) != 2 {
					t.Fatal("restart duplicated delivery")
				}
			})
		}
	}
}

func TestNotificationInitiationRefusesUnknownUnauthorizedAndDisconnected(t *testing.T) {
	for _, route := range []string{"cli/unknown", "tui/unknown", "telegram/TG-9", "telegram/TG-alice", "telegram/TG-group-9", "whatsapp/WA-15550000000", "whatsapp/WA-group-9", "whatsapp/WA-0015557654321", "whatsapp/WA-1-5557654321", "whatsapp/WA-self15557654321"} {
		t.Run(route, func(t *testing.T) {
			s := newInitiationService(t)
			router := &recentNotificationRouter{}
			s.DeliveryControl = router
			if _, err := s.Notify(context.Background(), route, "must not send"); err == nil {
				t.Fatal("invalid destination admitted")
			}
			if len(router.calls) != 0 {
				t.Fatal("refused destination reached transport")
			}
			if entries, err := os.ReadDir(s.Config.StatePath("runtime", "outbox")); err == nil && len(entries) != 0 {
				t.Fatal("refused destination entered outbox")
			}
		})
	}
	for _, name := range []string{"telegram", "whatsapp"} {
		t.Run(name+"/disconnected", func(t *testing.T) {
			s := newInitiationService(t)
			s.DeliveryControl = &recentNotificationRouter{}
			s.SetConnectionStatus(channel.ConnectionStatus{Name: name, State: channel.ConnectionConnecting})
			route := "telegram/TG-7"
			if name == "whatsapp" {
				route = "whatsapp/WA-15557654321"
			}
			if _, err := s.Notify(context.Background(), route, "must not send"); err == nil {
				t.Fatal("disconnected initiation admitted")
			}
		})
	}
	s := newInitiationService(t)
	s.DeliveryControl = &recentNotificationRouter{}
	if _, err := s.NotifyRecentAuthorized(context.Background(), "must not infer recipient"); err == nil {
		t.Fatal("ambiguous recent recipient admitted")
	}
}

func TestNotificationInitiationPreservesProviderFailureAndRevocation(t *testing.T) {
	for _, route := range []string{"telegram/TG-7", "whatsapp/WA-15557654321"} {
		t.Run(route, func(t *testing.T) {
			s := newInitiationService(t)
			router := &fallibleNotificationRouter{err: errors.New("provider refuses initiation")}
			s.DeliveryControl = router
			id, err := s.Notify(context.Background(), route, "must remain undelivered")
			if err != nil {
				t.Fatal(err)
			}
			name, conversation, _ := strings.Cut(route, "/")
			entries, _, err := s.History.RecentEntries(name, conversation, 20, 12000)
			if err != nil || len(entries) != 0 {
				t.Fatalf("invented delivered history: %#v %v", entries, err)
			}
			data, err := os.ReadFile(s.Config.StatePath("runtime", "outbox", id+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var entry orchestrator.OutboxEntry
			if err := json.Unmarshal(data, &entry); err != nil {
				t.Fatal(err)
			}
			if entry.State != "pending" || entry.Attempts != 1 || entry.LastError == "" || !entry.DeliveredAt.IsZero() {
				t.Fatalf("lost failed send: %#v", entry)
			}
			cfg := s.Settings.Snapshot()
			cfg.Channels.Telegram.AllowedUsers = []string{"8"}
			cfg.Channels.WhatsApp.AllowedNumbers = []string{"15557654322"}
			restarted := New(cfg, newServiceHarness())
			defer restarted.Close()
			restarted.DeliveryControl = router
			restarted.Orchestrator.Outbox.Now = func() time.Time { return time.Now().Add(time.Hour) }
			router.err = nil
			_ = restarted.Orchestrator.Outbox.Process(context.Background())
			if router.calls != 1 {
				t.Fatal("revoked queued delivery sent after restart")
			}
		})
	}
}
