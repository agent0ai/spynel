package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/channel/telegram"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/history"
	"github.com/agent0ai/spynel/internal/orchestrator"
	"github.com/agent0ai/spynel/internal/workspace"
)

type notificationTelegramRouter struct {
	bot     *telegram.Bot
	started chan struct{}
}

func (r notificationTelegramRouter) Name() string { return "telegram" }

func (r notificationTelegramRouter) Run(ctx context.Context, _ channel.Handler) error {
	close(r.started)
	<-ctx.Done()
	return ctx.Err()
}

func (r notificationTelegramRouter) Deliver(ctx context.Context, conversation, id, text string, attachments []core.OutboundAttachment) error {
	return r.bot.Deliver(ctx, conversation, id, text, attachments)
}

var _ channel.ProactiveDeliverer = notificationTelegramRouter{}

type notificationHTTP func(*http.Request) (*http.Response, error)

func (f notificationHTTP) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNotificationNativeTelegramUploadAndRetry(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	document := filepath.Join(root, "fixture.txt")
	photo := filepath.Join(root, "pixel.png")
	files := map[string]string{"fixture.txt": "harmless document", "pixel.png": "\x89PNG\r\n\x1a\n\x00\x00\x00\x00"}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	cfg.Channels.Telegram.AllowedUsers = []string{"7"}
	service := New(cfg, newServiceHarness())
	defer service.Close()
	started := make(chan struct{})
	supervisor := channel.NewSupervisor(service.Settings, nil, []channel.Managed{{
		Name: "telegram", Enabled: func(config.Config) bool { return true },
		Fingerprint: func(config.Config) string { return "fixture" },
		Build: func(config.Config) (channel.Channel, error) {
			return notificationTelegramRouter{telegram.New(cfg.Channels.Telegram, "mock-token"), started}, nil
		},
	}}, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- supervisor.Run(ctx) }()
	<-started
	defer func() { cancel(); <-stopped }()
	service.DeliveryControl = supervisor
	if _, err := service.History.Append("telegram", "TG-7", history.Entry{Role: "user", Content: "known"}); err != nil {
		t.Fatal(err)
	}
	var methods []string
	failMethod, failure := "", ""
	previous := http.DefaultTransport
	activeTest := t
	http.DefaultTransport = notificationHTTP(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.telegram.org" {
			activeTest.Fatalf("unexpected transport host: %s", r.URL.Host)
		}
		method := filepath.Base(r.URL.Path)
		methods = append(methods, method)
		if method == "sendMessage" {
			var body struct{ Text string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Text != "Ready." {
				activeTest.Fatalf("caption = %q, error = %v", body.Text, err)
			}
		} else {
			reader, err := r.MultipartReader()
			if err != nil {
				activeTest.Fatal(err)
			}
			found := false
			for {
				part, err := reader.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					activeTest.Fatal(err)
				}
				data, err := io.ReadAll(part)
				if err != nil {
					activeTest.Fatal(err)
				}
				if part.FileName() != "" {
					field := "document"
					if method == "sendPhoto" {
						field = "photo"
					}
					if part.FormName() != field || string(data) != files[part.FileName()] {
						activeTest.Fatalf("multipart file %q/%q = %q", part.FormName(), part.FileName(), data)
					}
					found = true
				} else if part.FormName() != "chat_id" || string(data) != "7" {
					activeTest.Fatalf("multipart field %q = %q", part.FormName(), data)
				}
			}
			if !found {
				activeTest.Fatal("native request contained no file")
			}
		}
		status, body := 200, `{"ok":true,"result":{"message_id":91}}`
		if method == failMethod {
			if failure == "transport" {
				return nil, errors.New("synthetic transport rejection")
			}
			body = `{"ok":false,"description":"synthetic rejection"}`
			if failure == "http" {
				status = 500
			}
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	doc := "[Send attachment](<" + document + ">)"
	pic := "[Send photo](<" + photo + ">)"
	for _, test := range []struct {
		name, text string
		want       []string
	}{
		{"document", "Ready.\n" + doc, []string{"sendDocument", "sendMessage"}},
		{"photo", "Ready.\n" + pic, []string{"sendPhoto", "sendMessage"}},
		{"attachment-only", doc, []string{"sendDocument"}},
		{"multiple", "Ready.\n" + doc + "\n" + pic, []string{"sendDocument", "sendPhoto", "sendMessage"}},
		{"text-only", "Ready.", []string{"sendMessage"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			activeTest = t
			methods = nil
			id, err := service.Notify(context.Background(), "telegram/TG-7", test.text)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(methods, test.want) {
				t.Fatalf("methods = %v, want %v", methods, test.want)
			}
			if state, err := service.History.DeliveryState("telegram", "TG-7", id); err != nil || state != "sent" {
				t.Fatalf("delivery state = %s, error = %v", state, err)
			}
			methods = nil
			if err := service.deliverNotification(context.Background(), orchestrator.Origin{Channel: "telegram", Conversation: "TG-7"}, id, test.text); err != nil || len(methods) != 0 {
				t.Fatalf("completed message replayed: %v, %v", methods, err)
			}
		})
	}
	for _, rejection := range []string{"http", "api", "transport", "caption"} {
		t.Run(rejection+"-retry", func(t *testing.T) {
			activeTest = t
			methods = nil
			failMethod, failure = "sendPhoto", rejection
			if rejection == "caption" {
				failMethod = "sendMessage"
			}
			text := "Ready.\n" + doc + "\n" + pic
			id, err := service.Notify(context.Background(), "telegram/TG-7", text)
			if err != nil {
				t.Fatal(err) // Admission succeeds; a transport failure remains pending.
			}
			entry := readNotificationOutbox(t, service, id)
			if entry.State != "pending" || entry.LastError == "" || entry.NextAttemptAt.IsZero() {
				t.Fatalf("failed native send marked successful: %#v", entry)
			}
			if state, _ := service.History.DeliveryState("telegram", "TG-7", id); state != "failed" {
				t.Fatalf("partial delivery state = %s", state)
			}
			if rejection != "caption" && !reflect.DeepEqual(methods, []string{"sendDocument", "sendPhoto"}) {
				t.Fatalf("caption sent after failed upload: %v", methods)
			}
			// Restart replays the whole message under Telegram's at-least-once contract.
			restarted := New(cfg, newServiceHarness())
			defer restarted.Close()
			restarted.DeliveryControl = service.DeliveryControl
			restarted.Orchestrator.Outbox.Now = func() time.Time { return entry.NextAttemptAt }
			failMethod, methods = "", nil
			if err := restarted.Orchestrator.Outbox.Process(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(methods, []string{"sendDocument", "sendPhoto", "sendMessage"}) || readNotificationOutbox(t, restarted, id).State != "delivered" {
				t.Fatalf("retry failed: %v", methods)
			}
		})
	}
	activeTest = t
	for _, invalid := range []string{doc + "\n[Send attachment](<relative.txt>)", doc + "\n[Send attachment](<" + root + ">)", doc + "\n[Send attachment](<" + root + "/missing>)", "[Send photo](<" + document + ">)"} {
		methods = nil
		id, err := service.Notify(context.Background(), "telegram/TG-7", "Ready.\n"+invalid)
		if err != nil || len(methods) != 0 || readNotificationOutbox(t, service, id).State != "pending" {
			t.Fatalf("invalid notification sent: %v, %v", methods, err)
		}
	}
	methods = nil
	if _, err := service.NotifyWithFallback(context.Background(), "telegram/TG-7", "whatsapp/WA-15557654321", doc); err == nil || len(methods) != 0 {
		t.Fatal("remote fallback admitted")
	}
	if _, err := service.History.Append("cli", "local", history.Entry{Role: "user", Content: "known"}); err != nil {
		t.Fatal(err)
	}
	failMethod, failure = "sendDocument", "api"
	id, err := service.NotifyWithFallback(context.Background(), "telegram/TG-7", "cli/local", "Ready.\n"+doc)
	if err != nil || readNotificationOutbox(t, service, id).State != "delivered" {
		t.Fatalf("explicit local fallback failed: %v", err)
	}
	if state, _ := service.History.DeliveryState("telegram", "TG-7", id); state != "failed" {
		t.Fatal("local fallback recorded remote upload success")
	}
	entries, _, err := service.History.RecentEntries("cli", "local", 10, 10000)
	if err != nil || entries[len(entries)-1].Content != "Ready.\n"+doc {
		t.Fatalf("local fallback changed directives: %#v, %v", entries, err)
	}
	entry := readNotificationOutbox(t, service, id)
	if _, err := service.Settings.Update(func(next *config.Config) error {
		next.Channels.Telegram.AllowedUsers = []string{"8"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	methods = nil
	service.Orchestrator.Outbox.Now = func() time.Time { return entry.UpdatedAt.Add(time.Minute) }
	if err := service.Orchestrator.Outbox.Process(context.Background()); err == nil || len(methods) != 0 {
		t.Fatalf("revoked pending messages reached transport: %v, %v", methods, err)
	}
}

func readNotificationOutbox(t *testing.T, service *Service, id string) orchestrator.OutboxEntry {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(service.Orchestrator.Outbox.Directory, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var entry orchestrator.OutboxEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatal(err)
	}
	return entry
}
