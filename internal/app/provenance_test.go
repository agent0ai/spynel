package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
	"github.com/agent0ai/spynel/internal/orchestrator"
)

func TestForwardedProvenanceReachesHistoryChatHookAndRecovery(t *testing.T) {
	for _, channel := range []string{"telegram", "whatsapp"} {
		t.Run(channel, func(t *testing.T) {
			service, _, _ := newRecoveryTestService(t)
			message := core.Message{Channel: channel, Conversation: "person", Sender: "@alice", ForwardedFrom: "@bob", Text: "/tasks active", ReplyTo: "42 caption", ReceivedAt: time.Unix(10, 0).UTC()}
			if err := service.Handle(context.Background(), message, func(core.Event) {}); err != nil {
				t.Fatal(err)
			}
			service.History = history.New(service.Config.StatePath("history"))
			entries, _, err := service.History.Entries(channel, "person")
			if err != nil || len(entries) == 0 || entries[0].ForwardedFrom != "@bob" || entries[0].Sender != "@alice" || entries[0].ReplyTo != "42 caption" {
				t.Fatalf("persisted = %#v, %v", entries, err)
			}
			header := "[1970-01-01T00:00:10Z] user (@alice) forwarded from (@bob): [reply_to: 42 caption] /tasks active"
			prompt, err := service.chatPrompt(message)
			if err != nil || !strings.Contains(prompt, header) {
				t.Fatalf("chat prompt missing attribution: %v", err)
			}
			payload := messageHookPayload(message)
			if payload["forwarded_from"] != "@bob" || payload["sender"] != "@alice" || payload["conversation"] != "person" {
				t.Fatalf("hook = %#v", payload)
			}
			// The selected source may be outside the recent-history window.
			service.Config.Workspace.HistoryMaxMessages = 0
			_, recovery, err := service.recoveryPrompt(orchestrator.Origin{Channel: channel, Conversation: "person"}, entries[:1])
			if err != nil || !strings.Contains(recovery, "excerpt=\"false\">\n"+header+"\n</stalled_message>") {
				t.Fatalf("recovery lost source attribution: %v\n%s", err, recovery)
			}
		})
	}
}

func TestQueuedForwardedMessagesRetainAttributionOutsideHistoryWindow(t *testing.T) {
	initial, _, cfg := newRecoveryTestService(t)
	_ = initial.Close()
	cfg.Workspace.HistoryMaxMessages = 1
	target := newHeldServiceHarness()
	registry := harness.NewRegistry()
	registry.Register("fixture", func(harness.HarnessConfig) (harness.Harness, error) { return target, nil })
	supervisor := harness.NewSupervisor(registry, harness.HarnessConfig{Name: "fixture"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	service := New(cfg, supervisor)
	defer service.Close()
	message := core.Message{Channel: "telegram", Conversation: "person", Sender: "@alice", Text: "initial", ReceivedAt: time.Unix(10, 0).UTC()}
	if conversationMessageText(message) != message.Text {
		t.Fatal("direct batch text changed")
	}
	key := sessionKey(message)
	if err := service.Handle(ctx, message, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	message.Text, message.ForwardedFrom = "first forwarded body", "@bob"
	if err := service.Handle(ctx, message, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	message.Text, message.ForwardedFrom = "second forwarded body", "unknown"
	started := make(chan struct{}, 1)
	if err := service.Handle(ctx, message, func(event core.Event) {
		if event.Kind == core.EventStatus && event.Text == "Queued follow-up started" {
			started <- struct{}{}
		}
	}); err != nil {
		t.Fatal(err)
	}
	target.mu.Lock()
	emit := target.emits[key]
	target.active[key] = false
	target.mu.Unlock()
	emit(core.Event{Kind: core.EventFinal, Text: "initial result", Done: true})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("queued batch did not start")
	}
	target.mu.Lock()
	prompts := append([]string(nil), target.prompts[key]...)
	target.mu.Unlock()
	if len(prompts) != 2 {
		t.Fatalf("provider sends = %d", len(prompts))
	}
	batch := prompts[1]
	first := strings.Index(batch, "<queued_followup index=\"1\">\n[1970-01-01T00:00:10Z] user (@alice) forwarded from (@bob): first forwarded body")
	second := strings.Index(batch, "<queued_followup index=\"2\">\n[1970-01-01T00:00:10Z] user (@alice) forwarded from (unknown): second forwarded body")
	if first < 0 || second < first {
		t.Fatalf("queued attribution or order lost: %s", batch)
	}
}

func TestLocalProvenanceMetadataIsBounded(t *testing.T) {
	message := core.Message{Channel: "cli", Conversation: "local", Text: "hello", ForwardedFrom: strings.Repeat("a", 513)}
	if ValidateLocalMessage(message) == nil {
		t.Fatal("unbounded provenance accepted")
	}
	message.ForwardedFrom = string([]byte{0xff})
	if ValidateLocalMessage(message) == nil {
		t.Fatal("invalid UTF-8 provenance accepted")
	}
}
