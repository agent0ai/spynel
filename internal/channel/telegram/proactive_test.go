package telegram

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/media"
)

func TestNativeDeliveryStopsWhenLiveRecipientsChange(t *testing.T) {
	allowed := []string{"7"}
	bot := New(config.Telegram{AllowedUsers: allowed}, "mock-token")
	bot.SetAllowedUsersSource(func() []string { return allowed })
	path := filepath.Join(t.TempDir(), "fixture.txt")
	if err := os.WriteFile(path, []byte("harmless document"), 0600); err != nil {
		t.Fatal(err)
	}
	_, attachments, err := media.ParseOutbound("[Send attachment](<"+path+">)\n[Send attachment](<"+path+">)", 1024)
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	bot.client.Transport = telegramRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return nil, err
		}
		allowed = []string{"8"}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":1}}`))}, nil
	})
	if err := bot.Deliver(context.Background(), "TG-7", "synthetic-event", "Ready.", attachments); err == nil || requests != 1 {
		t.Fatalf("revoked partial delivery continued: requests=%d, error=%v", requests, err)
	}
}

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
	if err := bot.Deliver(context.Background(), "TG-7", "event-1", "complete", nil); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d", requests)
	}
	if err := bot.Deliver(context.Background(), "TG-8", "event-2", "blocked", nil); err == nil {
		t.Fatal("unauthorized Telegram origin delivered")
	}
	allowed = nil
	if err := bot.Deliver(context.Background(), "TG-group-9", "event-3", "revoked", nil); err == nil {
		t.Fatal("revoked Telegram group origin delivered")
	}
	if requests != 1 {
		t.Fatalf("revoked delivery contacted Telegram: requests=%d", requests)
	}
}
