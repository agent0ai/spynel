package whatsapp

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/media"
	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

func TestNativeDeliveryStopsWhenLiveRecipientsChange(t *testing.T) {
	allowed := []string{"15557654321"}
	client := New(config.WhatsApp{AllowedNumbers: allowed}, filepath.Join(t.TempDir(), "fixture.db"))
	client.SetAllowedNumbersSource(func() []string { return allowed })
	path := filepath.Join(t.TempDir(), "fixture.txt")
	if err := os.WriteFile(path, []byte("harmless document"), 0600); err != nil {
		t.Fatal(err)
	}
	_, attachments, err := media.ParseOutbound("[Send attachment](<"+path+">)", 1024)
	if err != nil {
		t.Fatal(err)
	}
	uploads, sends := 0, 0
	client.upload = func(_ context.Context, r io.Reader, _ io.ReadWriteSeeker, _ whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
		uploads++
		_, err := io.Copy(io.Discard, r)
		allowed = []string{"15551234567"}
		return whatsmeow.UploadResponse{}, err
	}
	client.deliverID = func(context.Context, types.JID, *waE2E.Message, types.MessageID) (whatsmeow.SendResponse, error) {
		sends++
		return whatsmeow.SendResponse{}, nil
	}
	if err := client.Deliver(context.Background(), "WA-15557654321", "synthetic-event", "Ready.", attachments); err == nil || uploads != 1 || sends != 0 {
		t.Fatalf("revoked upload continued to send: uploads=%d, sends=%d, error=%v", uploads, sends, err)
	}
}

func TestProactiveDeliveryReappliesWhatsAppAuthorization(t *testing.T) {
	var delivered types.JID
	var firstID types.MessageID
	client := New(config.WhatsApp{AllowedNumbers: []string{"+1 555 765 4321"}}, t.TempDir()+"/wa.db")
	allowed := []string{"+1 555 765 4321"}
	client.SetAllowedNumbersSource(func() []string { return allowed })
	client.deliverID = func(_ context.Context, jid types.JID, _ *waE2E.Message, id types.MessageID) (whatsmeow.SendResponse, error) {
		delivered = jid
		firstID = id
		return whatsmeow.SendResponse{}, nil
	}
	if err := client.Deliver(context.Background(), "WA-15557654321", "event-1", "complete", nil); err != nil {
		t.Fatal(err)
	}
	if delivered.User != "15557654321" {
		t.Fatalf("delivered to %s", delivered.String())
	}
	if firstID == "" || firstID != stableWhatsAppMessageID("event-1", 0) {
		t.Fatalf("unstable WhatsApp event ID %q", firstID)
	}
	if err := client.Deliver(context.Background(), "WA-1999", "event-2", "blocked", nil); err == nil {
		t.Fatal("unauthorized WhatsApp origin delivered")
	}
	allowed = []string{" + "}
	if err := client.Deliver(context.Background(), "WA-group-7", "event-3", "revoked", nil); err == nil {
		t.Fatal("revoked WhatsApp group origin delivered")
	}
	if delivered.User != "15557654321" {
		t.Fatalf("revoked delivery changed provider target to %s", delivered.String())
	}
}

func TestProactiveWhatsAppMediaPreservesEncryptionAndRetryIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.txt")
	if err := os.WriteFile(path, []byte("harmless document"), 0600); err != nil {
		t.Fatal(err)
	}
	photoPath := filepath.Join(filepath.Dir(path), "pixel.png")
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\x00"
	if err := os.WriteFile(photoPath, []byte(png), 0600); err != nil {
		t.Fatal(err)
	}
	_, attachments, err := media.ParseOutbound("[Send attachment](<"+path+">)\n[Send photo](<"+photoPath+">)", 1024)
	if err != nil {
		t.Fatal(err)
	}
	client := New(config.WhatsApp{AllowedNumbers: []string{"15557654321"}}, filepath.Join(t.TempDir(), "wa.db"))
	var uploaded []whatsmeow.MediaType
	var ids []types.MessageID
	var messages []*waE2E.Message
	failUpload, failSend := false, false
	client.upload = func(_ context.Context, reader io.Reader, _ io.ReadWriteSeeker, kind whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
		data, err := io.ReadAll(reader)
		want := "harmless document"
		if kind == whatsmeow.MediaImage {
			want = png
		}
		if err != nil || string(data) != want {
			t.Fatalf("upload bytes = %q, error = %v", data, err)
		}
		uploaded = append(uploaded, kind)
		if failUpload {
			return whatsmeow.UploadResponse{}, errors.New("synthetic upload rejection")
		}
		return whatsmeow.UploadResponse{URL: "https://example.invalid/media", DirectPath: "/fixture", MediaKey: []byte("fixture-key"), FileSHA256: []byte("plain-hash"), FileEncSHA256: []byte("encrypted-hash"), FileLength: uint64(len(data))}, nil
	}
	client.deliverID = func(_ context.Context, _ types.JID, message *waE2E.Message, id types.MessageID) (whatsmeow.SendResponse, error) {
		ids, messages = append(ids, id), append(messages, message)
		if failSend {
			return whatsmeow.SendResponse{}, errors.New("synthetic transport rejection")
		}
		return whatsmeow.SendResponse{ID: id}, nil
	}
	client.presence = func(context.Context, types.JID, types.ChatPresence, types.ChatPresenceMedia) error {
		t.Fatal("proactive notification changed composing presence")
		return nil
	}
	event := core.Event{Kind: core.EventFinal, Done: true, Text: "Ready.", Attachments: attachments}
	wantIDs := []types.MessageID{stableWhatsAppMessageID("event:attachment", 0), stableWhatsAppMessageID("event:attachment", 1), stableWhatsAppMessageID("event", 0)}
	for range 2 {
		ids, messages, uploaded = nil, nil, nil
		if err := client.Deliver(context.Background(), "WA-15557654321", "event", event.Text, event.Attachments); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ids, wantIDs) || !reflect.DeepEqual(uploaded, []whatsmeow.MediaType{whatsmeow.MediaDocument, whatsmeow.MediaImage}) {
			t.Fatalf("media IDs = %v, uploads = %v", ids, uploaded)
		}
		doc, img := messages[0].GetDocumentMessage(), messages[1].GetImageMessage()
		if doc.GetFileName() != "fixture.txt" || string(doc.GetMediaKey()) != "fixture-key" || string(doc.GetFileEncSHA256()) != "encrypted-hash" || string(doc.GetFileSHA256()) != "plain-hash" || string(img.GetMediaKey()) != "fixture-key" || img.GetMimetype() != "image/png" || messages[2].GetConversation() != "Ready." {
			t.Fatal("native media message lost encrypted upload metadata or caption")
		}
	}
	for _, uploadFailure := range []bool{true, false} {
		failUpload, failSend = uploadFailure, !uploadFailure
		ids, messages = nil, nil
		if err := client.Deliver(context.Background(), "WA-15557654321", "event", event.Text, event.Attachments); err == nil {
			t.Fatal("native rejection reported success")
		}
		if len(messages) > 1 || len(messages) == 1 && messages[0].GetConversation() != "" {
			t.Fatal("caption sent after rejected media")
		}
	}
	failUpload, failSend = false, false
	event.Text = ""
	event.Attachments = attachments[:1]
	ids, messages = nil, nil
	if err := client.Deliver(context.Background(), "WA-15557654321", "event", event.Text, event.Attachments); err != nil || len(messages) != 1 || messages[0].GetDocumentMessage() == nil {
		t.Fatalf("attachment-only delivery = %v, error = %v", ids, err)
	}
	client.RevokeRuntimeAuthorization()
	ids, uploaded = nil, nil
	if err := client.Deliver(context.Background(), "WA-15557654321", "event", event.Text, event.Attachments); err == nil || len(ids) != 0 || len(uploaded) != 0 {
		t.Fatal("revoked media reached transport")
	}
}
