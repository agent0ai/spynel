package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agent0ai/spynel/internal/config"
)

func TestProactiveDeliveryReappliesTelegramAuthorization(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":91}}`))
	}))
	defer server.Close()
	bot := New(config.Telegram{AllowedUsers: []string{"7"}, PollTimeoutSec: 30}, "token")
	allowed := []string{"7"}
	bot.SetAllowedUsersSource(func() []string { return allowed })
	bot.baseURL = server.URL
	if err := bot.Deliver(context.Background(), "TG-7", "event-1", "complete"); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d", requests)
	}
	if err := bot.Deliver(context.Background(), "TG-8", "event-2", "blocked"); err == nil {
		t.Fatal("unauthorized Telegram origin delivered")
	}
	allowed = nil
	if err := bot.Deliver(context.Background(), "TG-group-9", "event-3", "revoked"); err == nil {
		t.Fatal("revoked Telegram group origin delivered")
	}
	if requests != 1 {
		t.Fatalf("revoked delivery contacted Telegram: requests=%d", requests)
	}
}
