package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
)

func TestUsernameAuthorizedInboundPersistsIdentityForProactiveDelivery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime", "telegram-identities.json")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,"result":{"message_id":92}}`))
	}))
	defer server.Close()

	bot := NewWithIdentityStore(config.Telegram{AllowedUsers: []string{" @FrD3L "}}, "token", path)
	bot.baseURL = server.URL
	handled := false
	bot.processUpdate(context.Background(), func(_ context.Context, message core.Message, emit core.Emit) error {
		handled = message.Conversation == "TG-518743883"
		emit(core.Event{Kind: core.EventFinal, Done: true})
		return nil
	}, telegramUpdate{Message: &telegramMessage{
		From: telegramUser{ID: 518743883, Username: "frd3l"},
		Chat: telegramChat{ID: 518743883, Type: "private"}, Text: "hello",
	}})
	if !handled {
		t.Fatal("username-authorized inbound message was not handled")
	}

	restarted := NewWithIdentityStore(config.Telegram{AllowedUsers: []string{"FRD3L"}}, "token", path)
	restarted.baseURL = server.URL
	if _, err := restarted.Deliver(context.Background(), "TG-518743883", "event", "complete"); err != nil {
		t.Fatalf("restart delivery using verified username mapping: %v", err)
	}
	if requests.Load() == 0 {
		t.Fatal("proactive Telegram request was not sent")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("identity state permissions = %o", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"token", "hello"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("identity state contains forbidden data %q: %s", forbidden, data)
		}
	}
}

func TestRejectedInboundDoesNotCreateIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telegram-identities.json")
	bot := NewWithIdentityStore(config.Telegram{AllowedUsers: []string{"someone_else"}}, "token", path)
	bot.processUpdate(context.Background(), func(context.Context, core.Message, core.Emit) error {
		t.Fatal("unauthorized inbound message was handled")
		return nil
	}, telegramUpdate{Message: &telegramMessage{
		From: telegramUser{ID: 7, Username: "forged"},
		Chat: telegramChat{ID: 7, Type: "private"}, Text: "hello",
	}})
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unauthorized inbound created identity state: %v", err)
	}
}

func TestPrivateIdentityAuthorizationRevocationCorruptionAndForgery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telegram-identities.json")
	store := NewIdentityStore(path)
	if err := store.RecordVerifiedPrivate(7, 7, "@Alice"); err != nil {
		t.Fatal(err)
	}
	for _, allowed := range [][]string{{"alice"}, {" @ALICE "}, {"7"}, {"@7"}} {
		if !store.AuthorizedPrivate(allowed, "7") {
			t.Fatalf("allowed list %#v rejected", allowed)
		}
	}
	for _, test := range []struct {
		allowed []string
		origin  string
	}{
		{[]string{"bob"}, "7"},
		{nil, "7"},
		{[]string{"alice"}, "8"},
		{[]string{"7"}, "07"},
		{[]string{"alice"}, "not-a-number"},
	} {
		if store.AuthorizedPrivate(test.allowed, test.origin) {
			t.Fatalf("forged or revoked origin authorized: %#v %q", test.allowed, test.origin)
		}
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"identities":{"7":{"conversation_id":7,"user_id":8,"username":"alice"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if store.AuthorizedPrivate([]string{"alice"}, "7") {
		t.Fatal("tampered identity state authorized")
	}
	if !store.AuthorizedPrivate([]string{"7"}, "7") {
		t.Fatal("direct numeric allow-list should not depend on mapping state")
	}
	if err := os.WriteFile(path, []byte(`not-json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if store.AuthorizedPrivate([]string{"alice"}, "7") {
		t.Fatal("corrupt identity state authorized")
	}
	if err := store.RecordVerifiedPrivate(7, 7, "Alice"); err != nil {
		t.Fatalf("verified inbound could not repair corrupt state: %v", err)
	}
	if !NewIdentityStore(path).AuthorizedPrivate([]string{"@alice"}, "7") {
		t.Fatal("repaired identity was not persistent")
	}
}

func TestChannelReconfigurationRechecksCurrentAllowedUsers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telegram-identities.json")
	if err := NewIdentityStore(path).RecordVerifiedPrivate(7, 7, "alice"); err != nil {
		t.Fatal(err)
	}
	allowed := NewWithIdentityStore(config.Telegram{AllowedUsers: []string{"alice"}}, "token", path)
	revoked := NewWithIdentityStore(config.Telegram{AllowedUsers: []string{"bob"}}, "token", path)
	if !allowed.identity.AuthorizedPrivate(allowed.config.AllowedUsers, "7") {
		t.Fatal("mapped username was not authorized before reload")
	}
	if revoked.identity.AuthorizedPrivate(revoked.config.AllowedUsers, "7") {
		t.Fatal("stale mapping preserved access after reload")
	}
}
