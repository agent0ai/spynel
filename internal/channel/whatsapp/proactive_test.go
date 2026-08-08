package whatsapp

import (
	"context"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

func TestProactiveDeliveryReappliesWhatsAppAuthorization(t *testing.T) {
	var delivered types.JID
	var firstID types.MessageID
	client := New(config.WhatsApp{AllowedNumbers: []string{"+1 555 765 4321"}}, t.TempDir()+"/wa.db")
	client.deliverID = func(_ context.Context, jid types.JID, _ *waE2E.Message, id types.MessageID) (whatsmeow.SendResponse, error) {
		delivered = jid
		firstID = id
		return whatsmeow.SendResponse{}, nil
	}
	receipt, err := client.Deliver(context.Background(), "WA-15557654321", "event-1", "complete")
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.MessageIDs) != 1 || receipt.MessageIDs[0] != string(firstID) {
		t.Fatalf("receipt = %#v", receipt)
	}
	if delivered.User != "15557654321" {
		t.Fatalf("delivered to %s", delivered.String())
	}
	if firstID == "" || firstID != stableWhatsAppMessageID("event-1", 0) {
		t.Fatalf("unstable WhatsApp event ID %q", firstID)
	}
	if _, err := client.Deliver(context.Background(), "WA-1999", "event-2", "blocked"); err == nil {
		t.Fatal("unauthorized WhatsApp origin delivered")
	}
}

func TestWhatsAppInboundCarriesNativeReplyIdentity(t *testing.T) {
	client := New(config.WhatsApp{}, t.TempDir()+"/wa.db")
	client.ctx = context.Background()
	var received core.Message
	client.handler = func(_ context.Context, message core.Message, _ core.Emit) error {
		received = message
		return nil
	}
	client.handleWithNativeActivity(time.Now(), types.NewJID("1555", types.DefaultUserServer), "1555", "answer", "message-2", "message-1", func() {})
	if received.NativeMessageID != "message-2" || received.NativeReplyToID != "message-1" {
		t.Fatalf("native identity = %#v", received)
	}
}
