package telegram_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/channel/telegram"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/media"
	"github.com/agent0ai/spynel/internal/workspace"
)

type heldVoice struct{ entered, release chan struct{} }

func (h heldVoice) Transcribe(context.Context, string) (string, error) {
	close(h.entered)
	<-h.release // Deliberately return a late result even after cancellation.
	return "/restart", nil
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("blocked before transcription release")
		var zero T
		return zero
	}
}

func TestTelegramCommandsDuringTranscription(t *testing.T) {
	for _, mode := range []string{"polling", "webhook"} {
		for _, finish := range []string{"ordered", "cancel", "revoke"} {
			t.Run(mode+"/"+finish, func(t *testing.T) {
				root := t.TempDir()
				if err := workspace.Init(root, false); err != nil {
					t.Fatal(err)
				}
				cfg, err := config.Load(config.PathForRoot(root))
				if err != nil {
					t.Fatal(err)
				}
				cfg.Extensions.Enabled = false
				cfg.Channels.Telegram = config.Telegram{Enabled: true, AllowedUsers: []string{"7"}, Mode: mode, GroupMode: "mentions", WebhookSecret: "fixture"}
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				address := listener.Addr().String()
				listener.Close()
				cfg.Channels.Telegram.WebhookListen = address
				cfg.Channels.Telegram.WebhookURL = "http://" + address
				service := app.New(cfg, harness.NewSupervisor(harness.NewBuiltinRegistry(), harness.HarnessConfig{Name: "codex", Command: "missing-fixture-provider", Cwd: root}))
				defer service.Close()
				updates := make(chan []json.RawMessage, 8)
				sent := make(chan map[string]any, 32)
				webhook := make(chan string, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					result := any(true)
					switch {
					case strings.HasSuffix(r.URL.Path, "/getMe"):
						result = map[string]any{"id": 99, "username": "spy"}
					case strings.HasSuffix(r.URL.Path, "/getUpdates"):
						select {
						case batch := <-updates:
							result = batch
						case <-r.Context().Done():
							return
						}
					case strings.HasSuffix(r.URL.Path, "/getFile"):
						result = map[string]string{"file_path": "voice.ogg"}
					case strings.Contains(r.URL.Path, "/file/"):
						fmt.Fprint(w, "voice")
						return
					case strings.HasSuffix(r.URL.Path, "/setWebhook"):
						var p struct {
							URL string `json:"url"`
						}
						json.NewDecoder(r.Body).Decode(&p)
						webhook <- p.URL
					case strings.HasSuffix(r.URL.Path, "/sendMessage"):
						var p map[string]any
						json.NewDecoder(r.Body).Decode(&p)
						sent <- p
					}
					json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
				}))
				defer server.Close()
				bot := telegram.New(cfg.Channels.Telegram, "fixture")
				bot.SetTestEndpoint(server.URL)
				voice := heldVoice{make(chan struct{}), make(chan struct{})}
				var release sync.Once
				defer release.Do(func() { close(voice.release) })
				bot.SetMedia(&media.Store{Directory: filepath.Join(root, "attachments"), MaxBytes: 1024}, voice)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				handled := make(chan core.Message, 32)
				done := make(chan error, 1)
				go func() {
					done <- bot.Run(ctx, func(ctx context.Context, m core.Message, emit core.Emit) error {
						err := service.Handle(ctx, m, emit)
						handled <- m
						return err
					})
				}()
				defer func() { cancel(); release.Do(func() { close(voice.release) }); receive(t, done) }()
				url := ""
				if mode == "webhook" {
					url = receive(t, webhook)
				}
				submit := func(id int, sender int, text, extra string) {
					t.Helper()
					data := json.RawMessage(fmt.Sprintf(`{"update_id":%d,"message":{"message_id":%d,"date":10,"from":{"id":%d,"username":"alice"},"chat":{"id":7,"type":"private"},"text":%q%s}}`, id, id, sender, text, extra))
					if mode == "polling" {
						updates <- []json.RawMessage{data}
						return
					}
					req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(string(data)))
					req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "fixture")
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					resp.Body.Close()
					if resp.StatusCode != 200 {
						t.Fatalf("webhook: %d", resp.StatusCode)
					}
				}
				submit(1, 7, "", `,"voice":{"file_id":"voice"},"forward_origin":{"type":"hidden_user","sender_user_name":"Bob"},"reply_to_message":{"message_id":9,"text":"quote"}`)
				receive(t, voice.entered)
				submit(2, 7, "second", "")
				submit(3, 7, "third", "")
				submit(4, 8, "/restart", `,"forward_origin":{"type":"user","sender_user":{"id":7}}`)
				submit(5, 7, "/status@spy", "")
				response := receive(t, sent)
				if response["chat_id"] != "7" || !strings.Contains(response["text"].(string), "Version") || response["reply_parameters"].(map[string]any)["message_id"] != float64(5) {
					t.Fatalf("status route: %#v", response)
				}
				if m := receive(t, handled); m.Text != "/status@spy" {
					t.Fatalf("unexpected admission: %#v", m)
				}
				select {
				case <-service.RestartRequests():
					t.Fatal("denied sender restarted")
				default:
				}
				submit(6, 7, "/restart", "")
				response = receive(t, sent)
				if !strings.Contains(response["text"].(string), "Restarting") {
					t.Fatalf("restart response: %#v", response)
				}
				receive(t, service.RestartRequests())
				receive(t, handled)
				submit(6, 7, "/restart", "")
				receive(t, handled)
				select {
				case <-service.RestartRequests():
					t.Fatal("redelivery restarted twice")
				default:
				}
				if finish == "cancel" {
					cancel()
				}
				if finish == "revoke" {
					bot.RevokeRuntimeAuthorization()
				}
				release.Do(func() { close(voice.release) })
				if finish == "ordered" {
					for i, want := range []string{"[Attachment ", "second", "third"} {
						m := receive(t, handled)
						if !strings.Contains(m.Text, want) || m.SourceMessageID != fmt.Sprintf("telegram:7:%d", i+1) {
							t.Fatalf("ordered admission: %#v", m)
						}
						if i == 0 && (m.ForwardedFrom != "Bob" || m.ReplyTo != "9 quote" || !strings.Contains(m.Text, "[Generated voice transcription — may contain errors]\n/restart")) {
							t.Fatalf("voice provenance: %#v", m)
						}
					}
					select {
					case <-service.RestartRequests():
						t.Fatal("speech became command")
					default:
					}
				} else {
					// Wait for the owned workers, then assert no late handler or reply.
					cancel()
					receive(t, done)
					done <- nil
					select {
					case m := <-handled:
						t.Fatalf("late admission: %#v", m)
					default:
					}
					select {
					case m := <-sent:
						t.Fatalf("late response: %#v", m)
					default:
					}
				}
				entries, _, err := service.History.Entries("telegram", "TG-7")
				if err != nil {
					t.Fatal(err)
				}
				users := map[string]int{}
				for _, entry := range entries {
					if entry.Role == "user" {
						users[entry.SourceMessageID]++
					}
				}
				want := 2
				if finish == "ordered" {
					want = 5
				}
				if len(users) != want {
					t.Fatalf("user entries: %#v", users)
				}
				for _, count := range users {
					if count != 1 {
						t.Fatalf("duplicate history: %#v", users)
					}
				}
			})
		}
	}
}
