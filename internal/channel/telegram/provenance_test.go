package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/media"
)

func TestForwardOriginUsesOnlyTopLevelProviderMetadata(t *testing.T) {
	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	for _, test := range []struct{ name, fields, want string }{
		{"direct", ``, ""},
		{"handle", `,"forward_origin":{"type":"user","date":1,"sender_user":{"id":8,"username":"bob","first_name":"Bob"}}`, "@bob"},
		{"name", `,"forward_origin":{"type":"user","sender_user":{"id":8,"first_name":"Bob"}}`, "Bob"},
		{"identifier", `,"forward_origin":{"type":"user","sender_user":{"id":8}}`, "8"},
		{"hidden", `,"forward_origin":{"type":"hidden_user","sender_user_name":"Žofie\nNová"}`, "Žofie Nová"},
		{"chat", `,"forward_origin":{"type":"chat","sender_chat":{"id":-9,"title":"Study group"},"author_signature":"not the source"}`, "Study group"},
		{"channel", `,"forward_origin":{"type":"channel","chat":{"id":-9,"username":"news","title":"News"}}`, "@news"},
		{"channel identifier", `,"forward_origin":{"type":"channel","chat":{"id":-9}}`, "-9"},
		{"missing user", `,"forward_origin":{"type":"user"}`, "unknown"},
		{"unknown type", `,"forward_origin":{"type":"future"}`, "unknown"},
		{"automatic", `,"is_automatic_forward":true`, "unknown"},
		{"quoted forward", `,"reply_to_message":{"message_id":5,"text":"quoted","forward_origin":{"type":"hidden_user","sender_user_name":"Bob"}}`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var update telegramUpdate
			data := `{"message":{"message_id":3,"date":10,"from":{"id":7,"username":"alice"},"chat":{"id":7,"type":"private"},"text":"forwarded from fake"` + test.fields + `}}`
			if err := json.Unmarshal([]byte(data), &update); err != nil {
				t.Fatal(err)
			}
			var got core.Message
			bot.processUpdate(context.Background(), func(_ context.Context, m core.Message, _ core.Emit) error { got = m; return nil }, update)
			if got.ForwardedFrom != test.want || got.Sender != "@alice" || got.Conversation != "TG-7" || got.SourceMessageID != "telegram:7:3" || !got.ReceivedAt.Equal(time.Unix(10, 0)) {
				t.Fatalf("message = %#v, want provenance %q with actual sender identity", got, test.want)
			}
		})
	}
	// An allowed source cannot authorize an unlisted actual sender.
	bot.processUpdate(context.Background(), func(context.Context, core.Message, core.Emit) error { t.Fatal("source authorized sender"); return nil }, telegramUpdate{Message: &telegramMessage{
		From: telegramUser{ID: 8}, Chat: telegramChat{ID: 8, Type: "private"}, Text: "forward",
		ForwardOrigin: &telegramMessageOrigin{Type: "user", SenderUser: &telegramUser{ID: 7}},
	}})
}

func TestForwardedTelegramMediaPreservesCaptionVoiceReplyAndRouting(t *testing.T) {
	for _, kind := range []string{"document", "voice"} {
		t.Run(kind, func(t *testing.T) {
			sent := make(chan map[string]any, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/sendMessage") {
					var payload map[string]any
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					sent <- payload
				}
				if strings.Contains(r.URL.Path, "/file/") {
					_, _ = w.Write([]byte("media bytes"))
					return
				}
				_, _ = w.Write([]byte(`{"ok":true,"result":{"file_path":"media/file.ogg"}}`))
			}))
			defer server.Close()
			bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
			bot.baseURL = server.URL
			bot.SetMedia(&media.Store{Directory: filepath.Join(t.TempDir(), "attachments"), MaxBytes: 1024}, fixedTranscriber{text: "spoken words"})
			var update telegramUpdate
			data := fmt.Sprintf(`{"message":{"message_id":3,"date":10,"from":{"id":7,"username":"alice"},"chat":{"id":7,"type":"private"},"caption":"caption","%s":{"file_id":"file","file_unique_id":"unique","file_name":"file.ogg"},"reply_to_message":{"message_id":2,"text":"previous"},"forward_origin":{"type":"hidden_user","sender_user_name":"Bob"}}}`, kind)
			if err := json.Unmarshal([]byte(data), &update); err != nil {
				t.Fatal(err)
			}
			var got core.Message
			bot.processUpdate(context.Background(), func(_ context.Context, m core.Message, emit core.Emit) error {
				got = m
				emit(core.Event{Kind: core.EventFinal, Text: "reply", Done: true})
				return nil
			}, update)
			if got.ForwardedFrom != "Bob" || got.Sender != "@alice" || got.Conversation != "TG-7" || got.ReplyTo != "2 previous" || !strings.Contains(got.Text, "caption") || !strings.Contains(got.Text, "[Attachment ") {
				t.Fatalf("media message = %#v", got)
			}
			if kind == "voice" && !strings.Contains(got.Text, "[Generated voice transcription — may contain errors]\nspoken words") {
				t.Fatalf("voice text = %q", got.Text)
			}
			select {
			case payload := <-sent:
				if payload["chat_id"] != "7" || payload["reply_parameters"].(map[string]any)["message_id"] != float64(3) {
					t.Fatalf("reply route = %#v", payload)
				}
			default:
				t.Fatal("reply was not sent to the actual chat")
			}
		})
	}
}
