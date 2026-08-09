package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/media"
	"go.mau.fi/whatsmeow"
	waCompanionReg "go.mau.fi/whatsmeow/proto/waCompanionReg"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
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

func TestQRPairingIdentifiesSpynelAsDesktopClient(t *testing.T) {
	originalOS := store.DeviceProps.Os
	originalPlatform := store.DeviceProps.PlatformType
	t.Cleanup(func() {
		store.DeviceProps.Os = originalOS
		store.DeviceProps.PlatformType = originalPlatform
	})

	client := whatsmeow.NewClient(&store.Device{}, nil)
	configurePairingIdentity(client)

	if got := store.DeviceProps.GetOs(); got != whatsAppDeviceName {
		t.Fatalf("QR device name = %q, want %q", got, whatsAppDeviceName)
	}
	if got := store.DeviceProps.GetPlatformType(); got != waCompanionReg.DeviceProps_DESKTOP {
		t.Fatalf("QR platform type = %v, want DESKTOP", got)
	}
	if got := client.QRClientType; got != whatsmeow.PairClientElectron {
		t.Fatalf("QR client type = %q, want Electron", got)
	}
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

func TestInteractivePairingControlsRequireAnActiveReadySession(t *testing.T) {
	client := New(config.WhatsApp{AllowedNumbers: []string{"15551234567"}}, filepath.Join(t.TempDir(), "whatsapp.db"))
	if err := client.RetryPairing(); err == nil {
		t.Fatal("retry succeeded without an active pairing session")
	}
	if _, err := client.PairPhone(context.Background(), "15551234567"); err == nil {
		t.Fatal("phone pairing succeeded without an active pairing session")
	}

	client.setPairingState(true, false)
	if _, err := client.PairPhone(context.Background(), "15551234567"); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("phone pairing before first QR = %v", err)
	}
	if err := client.RetryPairing(); err != nil {
		t.Fatalf("retry active pairing: %v", err)
	}
	select {
	case <-client.pairingRetry:
	default:
		t.Fatal("retry did not signal the pairing loop")
	}

	var pairedPhone string
	var event channel.PairingEvent
	client.pairPhone = func(_ context.Context, phone string) (string, error) {
		pairedPhone = phone
		return "ABCD-EFGH", nil
	}
	client.SetPairingReporter(func(value channel.PairingEvent) { event = value })
	client.setPairingQR("CURRENT-QR")
	code, err := client.PairPhone(context.Background(), "+1 (555) 123-4567")
	if err != nil || code != "ABCD-EFGH" || pairedPhone != "+1 (555) 123-4567" {
		t.Fatalf("phone pairing = code %q phone %q err %v", code, pairedPhone, err)
	}
	if event.State != "phone-code" || event.Code != code || event.Rendered != "CURRENT-QR" || !strings.Contains(event.Detail, code) {
		t.Fatalf("phone pairing event = %#v", event)
	}
}

func TestPairingFailureAutomaticallyRestartsAfterShortDelay(t *testing.T) {
	client := New(config.WhatsApp{AllowedNumbers: []string{"15551234567"}}, filepath.Join(t.TempDir(), "whatsapp.db"))
	client.log = io.Discard
	client.pairingDelay = 5 * time.Millisecond
	var event channel.PairingEvent
	client.SetPairingReporter(func(value channel.PairingEvent) { event = value })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	started := time.Now()
	if err := client.waitForPairingRestart(ctx, "timeout", "WhatsApp pairing: timeout"); err != nil {
		t.Fatalf("automatic pairing restart: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("automatic pairing restart took %s", elapsed)
	}
	if event.State != "timeout" || !strings.Contains(event.Detail, "retrying automatically") {
		t.Fatalf("automatic retry event = %#v", event)
	}
}

func TestWorkflowSlashCommandIsRoutedToSharedHandler(t *testing.T) {
	client := New(config.WhatsApp{AllowedNumbers: []string{"15551234567"}}, filepath.Join(t.TempDir(), "whatsapp.db"))
	var got core.Message
	client.ctx = context.Background()
	client.handler = func(_ context.Context, message core.Message, emit core.Emit) error {
		got = message
		emit(core.Event{Kind: core.EventFinal, Done: true})
		return nil
	}
	chat := types.NewJID("15551234567", types.DefaultUserServer)
	client.handle(time.Unix(10, 0), chat, "15557654321", "/tasks failed --limit 5")

	if got.Channel != "whatsapp" || got.Conversation != "WA-15557654321" || got.Text != "/tasks failed --limit 5" {
		t.Fatalf("routed message = %#v", got)
	}
}

func TestWhatsAppReplyContextAcrossWrappersAndMedia(t *testing.T) {
	quotedText := &waE2E.Message{Conversation: proto.String(" quoted\n text ")}
	quotedImage := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String(" image caption ")}}
	quotedDocument := &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String("private.pdf")}}
	quotedPTV := &waE2E.Message{PtvMessage: &waE2E.VideoMessage{Caption: proto.String(" video note ")}}
	ephemeral := &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: quotedImage}}
	documentWrapper := &waE2E.Message{DocumentWithCaptionMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Caption: proto.String(" wrapped caption "), FileName: proto.String("private.pdf")}}}}

	tests := []struct {
		name, id, want string
		quoted         *waE2E.Message
	}{
		{"text", "text-id", "text-id quoted text", quotedText},
		{"ephemeral image", "image-id", "image-id image caption", ephemeral},
		{"document wrapper", "doc-id", "doc-id wrapped caption", documentWrapper},
		{"captionless document", "file-id", "file-id", quotedDocument},
		{"ptv", "ptv-id", "ptv-id video note", quotedPTV},
		{"missing quoted payload", "only-id", "only-id", nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			contextInfo := &waE2E.ContextInfo{StanzaID: proto.String(test.id), QuotedMessage: test.quoted}
			message := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("reply"), ContextInfo: contextInfo}}
			if got := whatsappReplyTo(message); got != test.want {
				t.Fatalf("reply_to = %q, want %q", got, test.want)
			}
		})
	}

	sticker := &waE2E.Message{StickerMessage: &waE2E.StickerMessage{ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("sticker-reply"), QuotedMessage: quotedText}}}
	if got := whatsappReplyTo(sticker); got != "sticker-reply quoted text" {
		t.Fatalf("sticker reply_to = %q", got)
	}
	if got := whatsappReplyTo(&waE2E.Message{Conversation: proto.String("ordinary")}); got != "" {
		t.Fatalf("ordinary message reply_to = %q", got)
	}
}

func TestWhatsAppWorkerReplyValueReachesProviderNeutralMessage(t *testing.T) {
	client := New(config.WhatsApp{AllowedNumbers: []string{"15551234567"}}, filepath.Join(t.TempDir(), "whatsapp.db"))
	client.ctx = context.Background()
	var got core.Message
	client.handler = func(_ context.Context, message core.Message, _ core.Emit) error {
		got = message
		return nil
	}
	chat := types.NewJID("15551234567", types.DefaultUserServer)
	client.handleWithReply(time.Unix(10, 0), chat, "15557654321", "/tasks", "quoted-id referenced text", func() {})
	if got.ReplyTo != "quoted-id referenced text" || got.Text != "/tasks" {
		t.Fatalf("provider-neutral message = %#v", got)
	}
}

func TestWhatsAppSendsOnlyLastTerminalResponse(t *testing.T) {
	client := New(config.WhatsApp{AllowedNumbers: []string{"15551234567"}}, filepath.Join(t.TempDir(), "whatsapp.db"))
	client.ctx = context.Background()
	client.presence = func(context.Context, types.JID, types.ChatPresence, types.ChatPresenceMedia) error { return nil }
	sent := make(chan string, 8)
	client.deliver = func(_ context.Context, _ types.JID, message *waE2E.Message) (whatsmeow.SendResponse, error) {
		sent <- message.GetConversation()
		return whatsmeow.SendResponse{ID: types.MessageID("sent")}, nil
	}
	client.handler = func(_ context.Context, _ core.Message, emit core.Emit) error {
		lastResponse := "last response"
		emit(core.Event{Kind: core.EventDelta, Text: "streamed progress"})
		emit(core.Event{Kind: core.EventStatus, Text: "transport handoff", Done: true})
		emit(core.Event{Kind: core.EventFinal, Text: "intermediate response", Done: true, Continues: true})
		emit(core.Event{Kind: core.EventFinal, Text: "progress update\nlast response", FinalText: &lastResponse, Done: true})
		return nil
	}
	client.handle(time.Unix(10, 0), types.NewJID("15551234567", types.DefaultUserServer), "15557654321", "hello")

	select {
	case got := <-sent:
		if got != "last response" {
			t.Fatalf("WhatsApp sent %q, want only the last response", got)
		}
	default:
		t.Fatal("WhatsApp did not send the last response")
	}
	select {
	case extra := <-sent:
		t.Fatalf("WhatsApp sent an intermediate response: %q", extra)
	default:
	}
}

func TestWhatsAppFormatsErrorsAsOrdinaryUnindentedResponses(t *testing.T) {
	client := New(config.WhatsApp{AllowedNumbers: []string{"15551234567"}}, filepath.Join(t.TempDir(), "whatsapp.db"))
	client.ctx = context.Background()
	client.presence = func(context.Context, types.JID, types.ChatPresence, types.ChatPresenceMedia) error { return nil }
	sent := make(chan string, 4)
	client.deliver = func(_ context.Context, _ types.JID, message *waE2E.Message) (whatsmeow.SendResponse, error) {
		sent <- message.GetConversation()
		return whatsmeow.SendResponse{ID: types.MessageID("sent")}, nil
	}
	partial := "partial"
	client.handler = func(_ context.Context, _ core.Message, emit core.Emit) error {
		emit(core.Event{Kind: core.EventError, Text: "first line\nsecond line", FinalText: &partial, Done: true})
		return nil
	}
	chat := types.NewJID("15551234567", types.DefaultUserServer)
	client.handle(time.Unix(10, 0), chat, "15557654321", "hello")
	client.handler = func(context.Context, core.Message, core.Emit) error { return errors.New("handler failed") }
	client.handle(time.Unix(11, 0), chat, "15557654321", "hello")

	for _, want := range []string{"Error first line\nsecond line", "Error handler failed"} {
		select {
		case got := <-sent:
			if got != want {
				t.Fatalf("WhatsApp error reply = %q, want %q", got, want)
			}
		default:
			t.Fatalf("WhatsApp did not send error reply %q", want)
		}
	}
}

func TestSendAttachmentUsesNativeWhatsAppMediaMessage(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "photo.png")
	if err := os.WriteFile(path, []byte("png bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := New(config.WhatsApp{AllowedNumbers: []string{"15551234567"}}, filepath.Join(root, "whatsapp.db"))
	client.upload = func(_ context.Context, source io.Reader, _ io.ReadWriteSeeker, mediaType whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
		body, err := io.ReadAll(source)
		if err != nil {
			return whatsmeow.UploadResponse{}, err
		}
		if string(body) != "png bytes" || mediaType != whatsmeow.MediaImage {
			t.Fatalf("upload body = %q, media type = %q", body, mediaType)
		}
		return whatsmeow.UploadResponse{URL: "https://media", DirectPath: "/media", FileLength: uint64(len(body))}, nil
	}
	var sent *waE2E.Message
	client.deliver = func(_ context.Context, _ types.JID, message *waE2E.Message) (whatsmeow.SendResponse, error) {
		sent = message
		return whatsmeow.SendResponse{ID: "sent-photo"}, nil
	}
	chat := types.NewJID("15551234567", types.DefaultUserServer)
	if err := client.sendAttachment(context.Background(), chat, core.OutboundAttachment{
		Kind: "photo", Name: "photo.png", Path: path, MediaType: "image/png", MaxBytes: 1024,
	}); err != nil {
		t.Fatal(err)
	}
	if sent == nil || sent.ImageMessage == nil || sent.ImageMessage.GetMimetype() != "image/png" || sent.DocumentMessage != nil {
		t.Fatalf("sent message = %#v", sent)
	}
}

func TestConnectionEventRequiresPairedDeviceIdentity(t *testing.T) {
	client := New(config.WhatsApp{AllowedNumbers: []string{"15551234567"}}, filepath.Join(t.TempDir(), "whatsapp.db"))
	var statuses []channel.ConnectionStatus
	var pairings []channel.PairingEvent
	client.SetStatusReporter(func(status channel.ConnectionStatus) { statuses = append(statuses, status) })
	client.SetPairingReporter(func(event channel.PairingEvent) { pairings = append(pairings, event) })
	client.client = whatsmeow.NewClient(&store.Device{}, nil)

	client.onEvent(&events.Connected{})
	if len(statuses) != 1 || statuses[0].State != channel.ConnectionConnecting || statuses[0].Detail != "waiting for pairing" || len(pairings) != 0 {
		t.Fatalf("unpaired socket status = %#v, pairings %#v", statuses, pairings)
	}
	pairedID := types.NewJID("15551234567", types.DefaultUserServer)
	client.client.Store.ID = &pairedID
	client.onEvent(&events.Connected{})
	client.onEvent(&events.Disconnected{})
	if len(statuses) != 3 || statuses[1].State != channel.ConnectionConnected || statuses[2].State != channel.ConnectionError {
		t.Fatalf("connection statuses = %#v", statuses)
	}
	if statuses[1].Identity != "+15551234567" || statuses[1].Link != "https://wa.me/15551234567" {
		t.Fatalf("paired WhatsApp identity = %#v", statuses[1])
	}
	if statuses[2].Identity != "" || statuses[2].Link != "" {
		t.Fatalf("disconnected WhatsApp status exposes stale identity: %#v", statuses[2])
	}
	if len(pairings) != 1 || pairings[0].State != "connected" {
		t.Fatalf("paired events = %#v", pairings)
	}
}

func TestEmptyWhitelistReportsConnectionErrorAndRejectsNumbers(t *testing.T) {
	client := New(config.WhatsApp{AllowedNumbers: []string{" + "}}, filepath.Join(t.TempDir(), "whatsapp.db"))
	var got channel.ConnectionStatus
	client.SetStatusReporter(func(status channel.ConnectionStatus) { got = status })

	if err := client.Run(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "allowed_numbers") {
		t.Fatalf("Run() accepted an empty whitelist: %v", err)
	}
	if got.State != channel.ConnectionError || !strings.Contains(got.Detail, "allowed_numbers") {
		t.Fatalf("connection status = %#v", got)
	}
	if client.allowed("15551234567") {
		t.Fatal("empty whitelist accepted a WhatsApp number")
	}
}

func TestInvalidRuntimeAllowListsHaveZeroDatabaseOrPairingSideEffects(t *testing.T) {
	invalid := [][]string{nil, {}, {"  "}, {" + "}, {"phone"}, {"12x34"}, {"1234567890123456"}}
	for index, allowed := range invalid {
		t.Run(fmt.Sprintf("invalid-%d", index), func(t *testing.T) {
			root := t.TempDir()
			database := filepath.Join(root, "session", "whatsapp.db")
			client := New(config.WhatsApp{AllowedNumbers: allowed}, database)
			var statuses []channel.ConnectionStatus
			var pairings []channel.PairingEvent
			client.SetStatusReporter(func(status channel.ConnectionStatus) { statuses = append(statuses, status) })
			client.SetPairingReporter(func(event channel.PairingEvent) { pairings = append(pairings, event) })
			if err := client.Run(context.Background(), nil); !errors.Is(err, errWhatsAppRuntimeAuthorization) {
				t.Fatalf("Run() error = %v", err)
			}
			if _, err := os.Stat(filepath.Dir(database)); !os.IsNotExist(err) {
				t.Fatalf("invalid runtime touched session directory: %v", err)
			}
			if len(pairings) != 0 || len(statuses) != 1 || statuses[0].State != channel.ConnectionError {
				t.Fatalf("statuses=%#v pairings=%#v", statuses, pairings)
			}
		})
	}
}

func TestInvalidRuntimeDoesNotOpenPersistedWhatsAppSessionArtifact(t *testing.T) {
	root := t.TempDir()
	database := filepath.Join(root, "whatsapp.db")
	want := []byte("persisted-session-sentinel")
	if err := os.WriteFile(database, want, 0o600); err != nil {
		t.Fatal(err)
	}
	client := New(config.WhatsApp{AllowedNumbers: []string{"phone"}}, database)
	if err := client.Run(context.Background(), nil); !errors.Is(err, errWhatsAppRuntimeAuthorization) {
		t.Fatalf("Run() error = %v", err)
	}
	got, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("persisted session artifact changed: %q", got)
	}
}

func TestNextInboundEventRevalidatesLiveAllowListBeforeSideEffects(t *testing.T) {
	allowed := []string{"15551234567"}
	client := New(config.WhatsApp{Mode: "dedicated", AllowedNumbers: allowed}, filepath.Join(t.TempDir(), "whatsapp.db"))
	client.SetAllowedNumbersSource(func() []string { return allowed })
	pairedID := types.NewJID("15550000000", types.DefaultUserServer)
	client.client = whatsmeow.NewClient(&store.Device{ID: &pairedID}, nil)
	providerCalls := 0
	client.download = func(context.Context, whatsmeow.DownloadableMessage, *os.File) error { providerCalls++; return nil }
	client.presence = func(context.Context, types.JID, types.ChatPresence, types.ChatPresenceMedia) error {
		providerCalls++
		return nil
	}
	client.deliver = func(context.Context, types.JID, *waE2E.Message) (whatsmeow.SendResponse, error) {
		providerCalls++
		return whatsmeow.SendResponse{}, nil
	}
	var status channel.ConnectionStatus
	client.SetStatusReporter(func(next channel.ConnectionStatus) { status = next })
	allowed = []string{"phone"}
	client.onEvent(&events.Message{
		Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: types.NewJID("15551234567", types.DefaultUserServer), Sender: types.NewJID("15551234567", types.DefaultUserServer)}, ID: "message", Timestamp: time.Unix(10, 0)},
		Message: &waE2E.Message{Conversation: proto.String("hello")},
	})
	select {
	case incoming := <-client.incoming:
		t.Fatalf("revoked event reached downstream queue: %#v", incoming)
	default:
	}
	if providerCalls != 0 {
		t.Fatalf("revoked event attempted %d provider side effects", providerCalls)
	}
	if status.State != channel.ConnectionError || !strings.Contains(status.Detail, "allowed_numbers") {
		t.Fatalf("status = %#v", status)
	}
}

func TestAuthorizationLossWakesActiveWhatsAppRuntime(t *testing.T) {
	allowed := []string{"15551234567"}
	client := New(config.WhatsApp{AllowedNumbers: allowed, PollIntervalSec: 60}, filepath.Join(t.TempDir(), "whatsapp.db"))
	client.SetAllowedNumbersSource(func() []string { return allowed })
	var disconnects atomic.Int32
	client.disconnect = func() { disconnects.Add(1) }
	done := make(chan error, 1)
	go func() { done <- client.waitForRuntime(context.Background()) }()
	allowed = nil
	client.onEvent(&events.Message{})
	select {
	case err := <-done:
		if !errors.Is(err, errWhatsAppRuntimeAuthorization) {
			t.Fatalf("active runtime error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("authorization loss did not wake the active WhatsApp runtime")
	}
	if disconnects.Load() != 1 {
		t.Fatalf("authorization loss disconnects = %d, want 1", disconnects.Load())
	}
}

func TestQueuedInboundEventRevalidatesAfterEarlierAuthorization(t *testing.T) {
	allowed := []string{"15551234567"}
	client := New(config.WhatsApp{Mode: "dedicated", AllowedNumbers: allowed}, filepath.Join(t.TempDir(), "whatsapp.db"))
	client.SetAllowedNumbersSource(func() []string { return allowed })
	pairedID := types.NewJID("15550000000", types.DefaultUserServer)
	client.client = whatsmeow.NewClient(&store.Device{ID: &pairedID}, nil)
	sender := types.NewJID("15551234567", types.DefaultUserServer)
	client.onEvent(&events.Message{
		Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: sender, Sender: sender}, ID: "queued", Timestamp: time.Unix(10, 0)},
		Message: &waE2E.Message{Conversation: proto.String("hello")},
	})
	if len(client.incoming) != 1 {
		t.Fatal("valid event was not queued for the revocation fixture")
	}
	var providerCalls, handlerCalls atomic.Int32
	client.presence = func(context.Context, types.JID, types.ChatPresence, types.ChatPresenceMedia) error {
		providerCalls.Add(1)
		return nil
	}
	client.handler = func(context.Context, core.Message, core.Emit) error { handlerCalls.Add(1); return nil }
	errorStatus := make(chan struct{}, 1)
	client.SetStatusReporter(func(status channel.ConnectionStatus) {
		if status.State == channel.ConnectionError {
			errorStatus <- struct{}{}
		}
	})
	allowed = nil
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { client.messageWorker(ctx); close(done) }()
	select {
	case <-errorStatus:
	case <-time.After(time.Second):
		t.Fatal("queued event did not revalidate live authorization")
	}
	cancel()
	<-done
	if providerCalls.Load() != 0 || handlerCalls.Load() != 0 {
		t.Fatalf("queued revoked event attempted provider=%d handler=%d", providerCalls.Load(), handlerCalls.Load())
	}
}

func TestLiveAllowListReplacementRejectsPreviouslyAllowedWhatsAppSender(t *testing.T) {
	allowed := []string{"15551234567"}
	client := New(config.WhatsApp{Mode: "dedicated", AllowedNumbers: allowed}, filepath.Join(t.TempDir(), "whatsapp.db"))
	client.SetAllowedNumbersSource(func() []string { return allowed })
	pairedID := types.NewJID("15550000000", types.DefaultUserServer)
	client.client = whatsmeow.NewClient(&store.Device{ID: &pairedID}, nil)
	allowed = []string{"15557654321"}
	sender := types.NewJID("15551234567", types.DefaultUserServer)
	client.onEvent(&events.Message{
		Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: sender, Sender: sender}, ID: "replaced", Timestamp: time.Unix(10, 0)},
		Message: &waE2E.Message{Conversation: proto.String("hello")},
	})
	select {
	case incoming := <-client.incoming:
		t.Fatalf("replaced live WhatsApp allow-list retained old sender: %#v", incoming)
	default:
	}
}

func TestAllowedNumbersNormalizePunctuation(t *testing.T) {
	client := New(config.WhatsApp{AllowedNumbers: []string{"+1 (555) 123-4567"}}, filepath.Join(t.TempDir(), "whatsapp.db"))
	if !client.allowed("15551234567") {
		t.Fatal("normalized WhatsApp number was rejected")
	}
	if !client.allowed("001 555 123 4567") {
		t.Fatal("00-prefixed WhatsApp number was rejected")
	}
	if client.allowed("15557654321") {
		t.Fatal("unlisted WhatsApp number was accepted")
	}
}

func TestSelfChatLIDUsesPairedPhoneIdentityForWhitelist(t *testing.T) {
	client := New(config.WhatsApp{
		Mode: "self-chat", AllowedNumbers: []string{"00 420 123 456 789"},
	}, filepath.Join(t.TempDir(), "whatsapp.db"))
	phoneID := types.NewJID("420123456789", types.DefaultUserServer)
	lid := types.NewJID("987654321", types.HiddenUserServer)
	client.client = whatsmeow.NewClient(&store.Device{ID: &phoneID, LID: lid}, nil)
	client.onEvent(&events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: lid, Sender: lid, IsFromMe: true},
			ID:            "self-message", Timestamp: time.Unix(10, 0),
		},
		Message: &waE2E.Message{Conversation: proto.String("hello Spy")},
	})

	select {
	case incoming := <-client.incoming:
		if incoming.sender != phoneID.User || incoming.chat != lid || messageBody(incoming.message) != "hello Spy" {
			t.Fatalf("self-chat message = %#v", incoming)
		}
	default:
		t.Fatal("self-chat message addressed by LID was rejected")
	}
}

func TestDedicatedChatPrefersPhoneNumberOverAlternativeLID(t *testing.T) {
	client := New(config.WhatsApp{
		Mode: "dedicated", AllowedNumbers: []string{"+420 123 456 789"},
	}, filepath.Join(t.TempDir(), "whatsapp.db"))
	phoneID := types.NewJID("420999999999", types.DefaultUserServer)
	senderPhone := types.NewJID("420123456789", types.DefaultUserServer)
	senderLID := types.NewJID("987654321", types.HiddenUserServer)
	client.client = whatsmeow.NewClient(&store.Device{ID: &phoneID}, nil)
	client.onEvent(&events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: senderPhone, Sender: senderPhone, SenderAlt: senderLID},
			ID:            "direct-message", Timestamp: time.Unix(10, 0),
		},
		Message: &waE2E.Message{Conversation: proto.String("hello Spy")},
	})

	select {
	case incoming := <-client.incoming:
		if incoming.sender != senderPhone.User {
			t.Fatalf("dedicated sender = %q, want phone number %q", incoming.sender, senderPhone.User)
		}
	default:
		t.Fatal("dedicated message with an alternative LID was rejected")
	}
}

func TestWhatsAppVoiceIsStreamedToAttachmentStore(t *testing.T) {
	root := t.TempDir()
	client := New(config.WhatsApp{AllowedNumbers: []string{"15551234567"}}, filepath.Join(root, "whatsapp.db"))
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
	client := New(config.WhatsApp{AllowedNumbers: []string{"15551234567"}}, filepath.Join(root, "whatsapp.db"))
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
