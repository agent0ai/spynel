package whatsapp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frdel/spynel/internal/channel"
	"github.com/frdel/spynel/internal/config"
	"github.com/frdel/spynel/internal/core"
	"github.com/frdel/spynel/internal/media"
	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type whatsappTranscriber struct{}

func (whatsappTranscriber) Transcribe(context.Context, string) (string, error) {
	return "voice words", nil
}

type blockingWhatsAppTranscriber struct {
	entered chan struct{}
	release <-chan struct{}
}

func (t blockingWhatsAppTranscriber) Transcribe(ctx context.Context, _ string) (string, error) {
	close(t.entered)
	select {
	case <-t.release:
		return "voice words", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestCGOFreeWhatsAppStoreInitializes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "whatsapp.db")
	container, err := sqlstore.New(context.Background(), "sqlite3", "file:"+filepath.ToSlash(path)+"?_foreign_keys=on", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer container.Close()
	if _, err := container.GetFirstDevice(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSlashCommandIsRoutedToSharedHandler(t *testing.T) {
	client := New(config.WhatsApp{}, filepath.Join(t.TempDir(), "whatsapp.db"))
	var got core.Message
	client.ctx = context.Background()
	client.handler = func(_ context.Context, message core.Message, emit core.Emit) error {
		got = message
		emit(core.Event{Kind: core.EventFinal, Done: true})
		return nil
	}
	chat := types.NewJID("15551234567", types.DefaultUserServer)
	client.handle(time.Unix(10, 0), chat, "15557654321", "/help commands")

	if got.Channel != "whatsapp" || got.Conversation != "WA-15557654321" || got.Text != "/help commands" {
		t.Fatalf("routed message = %#v", got)
	}
}

func TestConnectionEventsAreReported(t *testing.T) {
	client := New(config.WhatsApp{}, filepath.Join(t.TempDir(), "whatsapp.db"))
	var statuses []channel.ConnectionStatus
	client.SetStatusReporter(func(status channel.ConnectionStatus) { statuses = append(statuses, status) })

	client.onEvent(&events.Connected{})
	client.onEvent(&events.Disconnected{})
	if len(statuses) != 2 || statuses[0].State != channel.ConnectionConnected || statuses[1].State != channel.ConnectionError {
		t.Fatalf("connection statuses = %#v", statuses)
	}
}

func TestWhatsAppVoiceIsStreamedToAttachmentStore(t *testing.T) {
	root := t.TempDir()
	client := New(config.WhatsApp{}, filepath.Join(root, "whatsapp.db"))
	store := &media.Store{Directory: filepath.Join(root, "attachments"), MaxBytes: 1024}
	client.SetMedia(store, whatsappTranscriber{})
	client.download = func(_ context.Context, _ whatsmeow.DownloadableMessage, file *os.File) error {
		_, err := file.WriteString("voice bytes")
		return err
	}
	text, err := client.prepareMessage(context.Background(), incomingMessage{
		id: "message-id", message: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
			Mimetype: proto.String("audio/ogg"), FileLength: proto.Uint64(11), PTT: proto.Bool(true), DirectPath: proto.String("/media"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "[Attachment audio-message-id") || !strings.Contains(text, "voice words") || !strings.Contains(text, "Generated voice transcription") {
		t.Fatalf("prepared message = %q", text)
	}
}

func TestWhatsAppTypingRefreshesThroughVoiceTranscriptionAndAgentTurn(t *testing.T) {
	root := t.TempDir()
	client := New(config.WhatsApp{}, filepath.Join(root, "whatsapp.db"))
	transcriptionEntered := make(chan struct{})
	transcriptionRelease := make(chan struct{})
	agentEntered := make(chan struct{})
	agentRelease := make(chan struct{})
	presenceEvents := make(chan types.ChatPresence, 32)
	client.presence = func(_ context.Context, _ types.JID, state types.ChatPresence, media types.ChatPresenceMedia) error {
		if media != types.ChatPresenceMediaText {
			t.Errorf("presence media = %q, want text", media)
		}
		presenceEvents <- state
		return nil
	}
	client.activity = newWhatsAppActivity(client, 10*time.Millisecond)
	client.SetMedia(&media.Store{Directory: filepath.Join(root, "attachments"), MaxBytes: 1024}, blockingWhatsAppTranscriber{
		entered: transcriptionEntered, release: transcriptionRelease,
	})
	client.download = func(_ context.Context, _ whatsmeow.DownloadableMessage, file *os.File) error {
		_, err := file.WriteString("voice bytes")
		return err
	}
	client.handler = func(_ context.Context, _ core.Message, emit core.Emit) error {
		close(agentEntered)
		<-agentRelease
		emit(core.Event{Kind: core.EventFinal, Done: true})
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.ctx = ctx
	go client.messageWorker(ctx)
	chat := types.NewJID("15551234567", types.DefaultUserServer)
	client.incoming <- incomingMessage{
		received: time.Unix(10, 0), chat: chat, sender: "15557654321", id: "message-id",
		message: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
			Mimetype: proto.String("audio/ogg"), FileLength: proto.Uint64(11), PTT: proto.Bool(true), DirectPath: proto.String("/media"),
		}},
	}
	waitWhatsAppClosed(t, transcriptionEntered, "WhatsApp transcription")
	waitWhatsAppPresence(t, presenceEvents, types.ChatPresenceComposing)
	waitWhatsAppPresence(t, presenceEvents, types.ChatPresenceComposing)
	close(transcriptionRelease)
	waitWhatsAppClosed(t, agentEntered, "WhatsApp agent turn")
	waitWhatsAppPresence(t, presenceEvents, types.ChatPresenceComposing)
	close(agentRelease)
	waitWhatsAppPresence(t, presenceEvents, types.ChatPresencePaused)
}

func waitWhatsAppPresence(t *testing.T, events <-chan types.ChatPresence, want types.ChatPresence) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case event := <-events:
			if event == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for WhatsApp presence %q", want)
		}
	}
}

func waitWhatsAppClosed(t *testing.T, done <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}
