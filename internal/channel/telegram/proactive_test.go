package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
)

func TestProactiveDeliveryReappliesTelegramAuthorization(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":91}}`))
	}))
	defer server.Close()
	bot := New(config.Telegram{AllowedUsers: []string{"7"}, PollTimeoutSec: 30}, "token")
	bot.baseURL = server.URL
	receipt, err := bot.Deliver(context.Background(), "TG-7", "event-1", "complete")
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.MessageIDs) != 1 || receipt.MessageIDs[0] != "91" {
		t.Fatalf("receipt = %#v", receipt)
	}
	if requests != 1 {
		t.Fatalf("requests = %d", requests)
	}
	if _, err := bot.Deliver(context.Background(), "TG-8", "event-2", "blocked"); err == nil {
		t.Fatal("unauthorized Telegram origin delivered")
	}
}

func TestTelegramInboundCarriesNativeReplyIdentity(t *testing.T) {
	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "token")
	var received core.Message
	bot.processUpdate(context.Background(), func(_ context.Context, message core.Message, _ core.Emit) error {
		received = message
		return nil
	}, telegramUpdate{Message: &telegramMessage{
		MessageID: 303, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 7, Type: "private"}, Date: 1, Text: "answer",
		ReplyToMessage: &telegramMessage{MessageID: 91, From: telegramUser{ID: 99}},
	}})
	if received.NativeMessageID != "303" || received.NativeReplyToID != "91" {
		t.Fatalf("native identity = %#v", received)
	}
}
