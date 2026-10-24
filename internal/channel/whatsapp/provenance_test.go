package whatsapp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/media"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestWhatsAppForwardProvenanceIgnoresQuotedIdentities(t *testing.T) {
	for _, test := range []struct {
		name string
		info *waE2E.ContextInfo
		want string
	}{
		{"direct", nil, ""},
		{"flag", &waE2E.ContextInfo{IsForwarded: proto.Bool(true), Participant: proto.String("15557654321@s.whatsapp.net")}, "unknown"},
		{"score", &waE2E.ContextInfo{ForwardingScore: proto.Uint32(3)}, "unknown"},
		{"origin", &waE2E.ContextInfo{ForwardOrigin: waE2E.ContextInfo_CHAT.Enum()}, "unknown"},
		{"newsletter", &waE2E.ContextInfo{ForwardedNewsletterMessageInfo: &waE2E.ContextInfo_ForwardedNewsletterMessageInfo{NewsletterName: proto.String("Daily\nNews"), NewsletterJID: proto.String("opaque@newsletter")}}, "Daily News"},
		{"unnamed newsletter", &waE2E.ContextInfo{ForwardedNewsletterMessageInfo: &waE2E.ContextInfo_ForwardedNewsletterMessageInfo{}}, "unknown"},
		{"quoted forward", &waE2E.ContextInfo{Participant: proto.String("15557654321@s.whatsapp.net"), QuotedMessage: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{ContextInfo: &waE2E.ContextInfo{IsForwarded: proto.Bool(true)}}}}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("forwarded from fake"), ContextInfo: test.info}}
			if got := whatsappForwardedFrom(message); got != test.want {
				t.Fatalf("provenance = %q, want %q", got, test.want)
			}
		})
	}
}

func TestWhatsAppForwardSurvivesAuthorizedIntakeMediaAndVoice(t *testing.T) {
	for _, kind := range []string{"text", "document", "voice"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			client := New(config.WhatsApp{Mode: "dedicated", AllowedNumbers: []string{"15551234567"}}, filepath.Join(root, "whatsapp.db"))
			client.SetMedia(&media.Store{Directory: filepath.Join(root, "attachments"), MaxBytes: 1024}, whatsappTranscriber{})
			client.download = func(_ context.Context, _ whatsmeow.DownloadableMessage, file *os.File) error {
				_, err := file.WriteString("media bytes")
				return err
			}
			info := &waE2E.ContextInfo{IsForwarded: proto.Bool(true), StanzaID: proto.String("previous"), Participant: proto.String("15551234567@s.whatsapp.net"), QuotedMessage: &waE2E.Message{Conversation: proto.String("quote")}}
			message := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("text"), ContextInfo: info}}
			if kind == "document" {
				message = &waE2E.Message{DocumentWithCaptionMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Caption: proto.String("caption"), FileName: proto.String("file.txt"), DirectPath: proto.String("/media"), ContextInfo: info}}}}
			}
			if kind == "voice" {
				message = &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{Mimetype: proto.String("audio/ogg"), PTT: proto.Bool(true), DirectPath: proto.String("/media"), ContextInfo: info}}}}
			}
			chat := types.NewJID("15551234567", types.DefaultUserServer)
			event := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: types.NewJID("15557654321", types.DefaultUserServer)}, ID: "current", Timestamp: time.Unix(10, 0)}, Message: message}
			client.onEvent(event)
			if len(client.incoming) != 0 {
				t.Fatal("quoted participant authorized unlisted actual sender")
			}
			event.Info.Sender = chat
			client.onEvent(event)
			if len(client.incoming) != 1 {
				t.Fatal("authorized forward was dropped")
			}
			ctx, cancel := context.WithCancel(context.Background())
			client.ctx = ctx
			defer cancel()
			messages := make(chan core.Message, 1)
			sent := make(chan types.JID, 1)
			client.deliver = func(_ context.Context, destination types.JID, _ *waE2E.Message) (whatsmeow.SendResponse, error) {
				sent <- destination
				return whatsmeow.SendResponse{ID: "response"}, nil
			}
			client.handler = func(_ context.Context, m core.Message, emit core.Emit) error {
				emit(core.Event{Kind: core.EventFinal, Text: "reply", Done: true})
				messages <- m
				return nil
			}
			done := make(chan struct{})
			go func() { client.messageWorker(ctx); close(done) }()
			defer func() { cancel(); <-done }()
			select {
			case got := <-messages:
				if got.ForwardedFrom != "unknown" || got.Sender != chat.User || got.Conversation != "WA-15551234567" || got.SourceMessageID != "whatsapp:"+chat.String()+":current" || got.ReplyTo != "previous quote" || !got.ReceivedAt.Equal(time.Unix(10, 0)) {
					t.Fatalf("message = %#v", got)
				}
				if kind != "text" && !strings.Contains(got.Text, "[Attachment ") {
					t.Fatalf("missing media: %q", got.Text)
				}
				if kind == "document" && !strings.Contains(got.Text, "caption") {
					t.Fatalf("missing caption: %q", got.Text)
				}
				if kind == "voice" && !strings.Contains(got.Text, "[Generated voice transcription — may contain errors]\nvoice words") {
					t.Fatalf("missing transcript: %q", got.Text)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("intake did not reach handler")
			}
			select {
			case destination := <-sent:
				if destination != chat {
					t.Fatalf("reply destination = %v", destination)
				}
			default:
				t.Fatal("reply was not sent to the actual chat")
			}
		})
	}
}
