package localapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/channel/telegram"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/orchestrator"
)

type initiatingTelegramRouter struct{ bot *telegram.Bot }

func (r initiatingTelegramRouter) Deliver(ctx context.Context, _ string, conversation, id, text string, attachments []core.OutboundAttachment) error {
	return r.bot.Deliver(ctx, conversation, id, text, attachments)
}

func TestNotificationInitiationThroughAuthenticatedOwnerAndTelegram(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		name := "permitted"
		if rejected {
			name = "provider-refused"
		}
		t.Run(name, func(t *testing.T) {
			election, server, _, cancel, done := startTestServer(t, t.TempDir())
			defer func() { cancel(); <-done; _ = election.Release(server.Token) }()
			cfg, err := server.Service.Settings.Update(func(cfg *config.Config) error {
				cfg.Channels.Telegram.Enabled = true
				cfg.Channels.Telegram.Token = "fixture"
				cfg.Channels.Telegram.AllowedUsers = []string{"7"}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			bot := telegram.New(cfg.Channels.Telegram, "fixture")
			bot.SetAllowedUsersSource(func() []string { return server.Service.Settings.Snapshot().Channels.Telegram.AllowedUsers })
			server.Service.DeliveryControl = initiatingTelegramRouter{bot}
			server.Service.SetConnectionStatus(channel.ConnectionStatus{Name: "telegram", State: channel.ConnectionConnected})
			client := NewClient(election)
			var sends atomic.Int32
			previous := http.DefaultTransport
			http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "api.telegram.org" {
					return previous.RoundTrip(r)
				}
				var body struct {
					ChatID string `json:"chat_id"`
					Text   string `json:"text"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ChatID != "7" || body.Text != "initiation fixture" || !strings.HasSuffix(r.URL.Path, "/sendMessage") {
					t.Errorf("unexpected Telegram request: %#v %v", body, err)
				}
				sends.Add(1)
				status, bodyText := http.StatusOK, `{"ok":true,"result":{"message_id":1}}`
				if rejected {
					status, bodyText = http.StatusForbidden, `{"ok":false,"error_code":403,"description":"Forbidden: bot can't initiate conversation with a user"}`
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(bodyText))}, nil
			})
			defer func() { http.DefaultTransport = previous }()
			id, err := client.Notify(context.Background(), "telegram/TG-7", "initiation fixture")
			if err != nil || id == "" {
				t.Fatalf("admission: %s %v", id, err)
			}
			entries, _, err := server.Service.History.RecentEntries("telegram", "TG-7", 10, 10000)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(server.Service.Config.StatePath("runtime", "outbox", id+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var entry orchestrator.OutboxEntry
			if err := json.Unmarshal(data, &entry); err != nil {
				t.Fatal(err)
			}
			if sends.Load() != 1 {
				t.Fatalf("sends=%d", sends.Load())
			}
			if rejected {
				if len(entries) != 0 || entry.State != "pending" || !strings.Contains(entry.LastError, "bot can't initiate conversation") || !entry.DeliveredAt.IsZero() {
					t.Fatalf("refusal evidence: %#v %#v", entries, entry)
				}
				if state, err := client.CancelNotification(context.Background(), entry.Origin, id); err != nil || state != "cancelled" {
					t.Fatalf("settlement: %s %v", state, err)
				}
			} else if len(entries) != 1 || entries[0].EventID != id || entries[0].Content != "initiation fixture" || entry.State != "delivered" {
				t.Fatalf("delivery: %#v %#v", entries, entry)
			}
		})
	}
}
