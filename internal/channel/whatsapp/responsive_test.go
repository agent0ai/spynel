package whatsapp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/media"
	"github.com/agent0ai/spynel/internal/workspace"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type heldVoice struct{ entered, release chan struct{} }

func (h heldVoice) Transcribe(context.Context, string) (string, error) {
	close(h.entered)
	<-h.release
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

func TestWhatsAppCommandsDuringTranscription(t *testing.T) {
	for _, finish := range []string{"ordered", "cancel", "revoke"} {
		t.Run(finish, func(t *testing.T) {
			root := t.TempDir()
			if err := workspace.Init(root, false); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(config.PathForRoot(root))
			if err != nil {
				t.Fatal(err)
			}
			cfg.Extensions.Enabled = false
			cfg.Channels.WhatsApp = config.WhatsApp{Enabled: true, Mode: "dedicated", PollIntervalSec: 3, AllowedNumbers: []string{"15551234567"}}
			service := app.New(cfg, harness.NewSupervisor(harness.NewBuiltinRegistry(), harness.HarnessConfig{Name: "codex", Command: "missing-fixture-provider", Cwd: root}))
			defer service.Close()
			client := New(cfg.Channels.WhatsApp, filepath.Join(root, "whatsapp.db"))
			voice := heldVoice{make(chan struct{}), make(chan struct{})}
			var release sync.Once
			defer release.Do(func() { close(voice.release) })
			client.SetMedia(&media.Store{Directory: filepath.Join(root, "attachments"), MaxBytes: 1024}, voice)
			client.download = func(_ context.Context, _ whatsmeow.DownloadableMessage, file *os.File) error {
				_, err := file.WriteString("voice")
				return err
			}
			chat := types.NewJID("15551234567", types.DefaultUserServer)
			type reply struct {
				chat types.JID
				text string
			}
			sent := make(chan reply, 32)
			client.deliver = func(_ context.Context, to types.JID, m *waE2E.Message) (whatsmeow.SendResponse, error) {
				sent <- reply{to, messageBody(m)}
				return whatsmeow.SendResponse{ID: "response"}, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client.ctx = ctx
			handled := make(chan core.Message, 32)
			client.handler = func(ctx context.Context, m core.Message, emit core.Emit) error {
				err := service.Handle(ctx, m, emit)
				handled <- m
				return err
			}
			done := make(chan struct{}, 1)
			go func() { client.messageWorker(ctx); done <- struct{}{} }()
			defer func() { cancel(); release.Do(func() { close(voice.release) }); receive(t, done) }()
			submit := func(id int, sender types.JID, m *waE2E.Message) {
				client.onEvent(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: sender}, ID: fmt.Sprint(id), Timestamp: time.Unix(10, 0)}, Message: m})
			}
			text := func(s string) *waE2E.Message { return &waE2E.Message{Conversation: proto.String(s)} }
			info := &waE2E.ContextInfo{IsForwarded: proto.Bool(true), StanzaID: proto.String("previous"), QuotedMessage: text("quote")}
			submit(1, chat, &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true), Mimetype: proto.String("audio/ogg"), DirectPath: proto.String("/voice"), ContextInfo: info}})
			receive(t, voice.entered)
			submit(2, chat, text("second"))
			submit(3, chat, text("third"))
			submit(4, types.NewJID("15557654321", types.DefaultUserServer), &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("/restart"), ContextInfo: &waE2E.ContextInfo{IsForwarded: proto.Bool(true), Participant: proto.String(chat.String())}}})
			submit(5, chat, text("/status"))
			response := receive(t, sent)
			if response.chat != chat || !strings.Contains(response.text, "Version") {
				t.Fatalf("status: %#v", response)
			}
			if m := receive(t, handled); m.Text != "/status" {
				t.Fatalf("unexpected admission: %#v", m)
			}
			select {
			case <-service.RestartRequests():
				t.Fatal("denied sender restarted")
			default:
			}
			submit(50, chat, text("/verbose off"))
			response = receive(t, sent)
			if !strings.Contains(response.text, "channels.verbose") || service.Settings.Snapshot().Channels.Verbose != "off" {
				t.Fatalf("verbose failed during transcription: %#v", response)
			}
			if m := receive(t, handled); m.Text != "/verbose off" {
				t.Fatal("wrong verbose admission")
			}
			submit(6, chat, text("/restart"))
			response = receive(t, sent)
			if !strings.Contains(response.text, "Restarting") {
				t.Fatalf("restart: %#v", response)
			}
			receive(t, service.RestartRequests())
			receive(t, handled)
			submit(6, chat, text("/restart"))
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
				client.RevokeRuntimeAuthorization()
			}
			release.Do(func() { close(voice.release) })
			if finish == "ordered" {
				for i, want := range []string{"[Attachment ", "second", "third"} {
					m := receive(t, handled)
					if !strings.Contains(m.Text, want) || m.SourceMessageID != fmt.Sprintf("whatsapp:%s:%d", chat, i+1) {
						t.Fatalf("order: %#v", m)
					}
					if i == 0 && (m.ForwardedFrom != "unknown" || m.ReplyTo != "previous quote" || !strings.Contains(m.Text, "[Generated voice transcription — may contain errors]\n/restart")) {
						t.Fatalf("voice provenance: %#v", m)
					}
				}
				select {
				case <-service.RestartRequests():
					t.Fatal("speech became command")
				default:
				}
			} else {
				cancel()
				receive(t, done)
				done <- struct{}{}
				select {
				case m := <-handled:
					t.Fatalf("late admission: %#v", m)
				default:
				}
				select {
				case m := <-sent:
					t.Fatalf("late reply: %#v", m)
				default:
				}
			}
			entries, _, err := service.History.Entries("whatsapp", "WA-15551234567")
			if err != nil {
				t.Fatal(err)
			}
			users := map[string]int{}
			for _, entry := range entries {
				if entry.Role == "user" {
					users[entry.SourceMessageID]++
				}
			}
			want := 3
			if finish == "ordered" {
				want = 6
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

func TestWhatsAppInboxCommandSourcesAndBounds(t *testing.T) {
	client := New(config.WhatsApp{Mode: "dedicated", AllowedNumbers: []string{"15551234567"}}, "")
	client.SetLogWriter(nil)
	chat := types.NewJID("15551234567", types.DefaultUserServer)
	event := func(text string) *events.Message {
		return &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: chat}, ID: "test"}, Message: &waE2E.Message{Conversation: proto.String(text)}}
	}
	for range cap(client.incoming) {
		client.onEvent(event("ordinary"))
	}
	client.onEvent(event("/status"))
	if len(client.commands) != 1 || len(client.incoming) != 64 {
		t.Fatal("full ordinary queue blocked command")
	}
	<-client.commands
	for _, text := range []string{"/unknown", "quote /restart"} {
		// Test classification without overflow diagnostics.
		<-client.incoming
		client.onEvent(event(text))
		if len(client.commands) != 0 || len(client.incoming) != 64 {
			t.Fatal("non-command selected command lane")
		}
	}
	group := event("/restart")
	group.Info.IsGroup = true
	client.onEvent(group)
	denied := event("/restart")
	denied.Info.Sender = types.NewJID("15557654321", types.DefaultUserServer)
	client.onEvent(denied)
	if len(client.commands) != 0 {
		t.Fatal("group/sender checks bypassed")
	}
}
