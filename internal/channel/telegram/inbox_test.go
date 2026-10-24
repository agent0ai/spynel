package telegram

import (
	"context"
	"testing"

	"github.com/agent0ai/spynel/internal/config"
)

func TestTelegramInboxBoundsAndCommandSources(t *testing.T) {
	b := New(config.Telegram{AllowedUsers: []string{"7"}, GroupMode: "mentions"}, "test")
	b.me = telegramUser{ID: 99, Username: "spy"}
	messages, commands := make(chan telegramUpdate, 1), make(chan telegramUpdate, 1)
	base := telegramMessage{From: telegramUser{ID: 7}, Chat: telegramChat{ID: 7, Type: "private"}, Text: "ordinary"}
	if !b.enqueueUpdate(context.Background(), messages, commands, telegramUpdate{Message: &base}) {
		t.Fatal("ordinary rejected")
	}
	for _, text := range []string{"/unknown", "quoted /restart"} {
		m := base
		m.Text = text
		if b.enqueueUpdate(context.Background(), messages, commands, telegramUpdate{Message: &m}) {
			t.Fatal("unrecognized text bypassed full ordinary queue")
		}
	}
	m := base
	m.Text = "/status@spy"
	m.Chat = telegramChat{ID: -1, Type: "supergroup"}
	if !b.enqueueUpdate(context.Background(), messages, commands, telegramUpdate{Message: &m}) || len(commands) != 1 {
		t.Fatal("addressed command blocked by ordinary queue")
	}
	if b.enqueueUpdate(context.Background(), messages, commands, telegramUpdate{Message: &m}) {
		t.Fatal("command queue exceeded bound")
	}
	<-commands
	m.Text = "/restart" // Group command without addressing must still be ignored.
	b.enqueueUpdate(context.Background(), messages, commands, telegramUpdate{Message: &m})
	m.Chat = base.Chat
	m.From.ID = 8
	b.enqueueUpdate(context.Background(), messages, commands, telegramUpdate{Message: &m})
	if len(commands) != 0 {
		t.Fatal("addressing or sender check bypassed")
	}
}
