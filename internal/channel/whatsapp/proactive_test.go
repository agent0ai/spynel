package whatsapp

import (
	"context"
	"testing"

	"github.com/agent0ai/spynel/internal/config"
	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

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
	if err := client.Deliver(context.Background(), "WA-15557654321", "event-1", "complete"); err != nil {
		t.Fatal(err)
	}
	if delivered.User != "15557654321" {
		t.Fatalf("delivered to %s", delivered.String())
	}
	if firstID == "" || firstID != stableWhatsAppMessageID("event-1", 0) {
		t.Fatalf("unstable WhatsApp event ID %q", firstID)
	}
	if err := client.Deliver(context.Background(), "WA-1999", "event-2", "blocked"); err == nil {
		t.Fatal("unauthorized WhatsApp origin delivered")
	}
	allowed = []string{" + "}
	if err := client.Deliver(context.Background(), "WA-group-7", "event-3", "revoked"); err == nil {
		t.Fatal("revoked WhatsApp group origin delivered")
	}
	if delivered.User != "15557654321" {
		t.Fatalf("revoked delivery changed provider target to %s", delivered.String())
	}
}
