package app

import (
	"strings"
	"testing"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/workspace"
)

func TestVerboseSharedCommandsAndUnfilteredConsumer(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Extensions.Enabled = false
	s := New(cfg, newServiceHarness())
	defer s.Close()
	for i, channel := range []string{"tui", "telegram", "whatsapp", "cli"} {
		mode := []string{"off", "on", "auto", "off"}[i]
		var reply core.Event
		if err := s.Handle(t.Context(), core.Message{Channel: channel, Conversation: "global", Text: "/verbose " + mode}, func(e core.Event) { reply = e }); err != nil {
			t.Fatal(err)
		}
		if !reply.Local || !reply.Done || reply.Verbose != mode || s.SharedState().Verbose != mode {
			t.Fatalf("command failed: %#v", reply)
		}
		if err := s.Handle(t.Context(), core.Message{Channel: "cli", Conversation: "other", Text: "/verbose"}, func(e core.Event) { reply = e }); err != nil || !strings.Contains(reply.Text, mode) {
			t.Fatalf("global read: %#v %v", reply, err)
		}
	}
	if err := s.Handle(t.Context(), core.Message{Channel: "cli", Conversation: "other", Text: "/verbose yes"}, nil); err == nil {
		t.Fatal("invalid accepted")
	}
	loaded, err := config.Load(cfg.Path)
	if err != nil || loaded.Channels.Verbose != "off" {
		t.Fatal("persistence", err)
	}
	// Presentation hints leave explicit CLI/API streaming and aggregate history intact.
	message := core.Message{Channel: "cli", Conversation: "stream"}
	var events []core.Event
	consume := s.conversationConsumer(message, 0, func(e core.Event) { events = append(events, e) })
	final := "answer"
	for _, event := range []core.Event{{Kind: core.EventDelta, Text: "progress\n"}, {Kind: core.EventFinal, Text: "progress\nanswer", FinalText: &final, Done: true}} {
		if err := consume(event); err != nil {
			t.Fatal(err)
		}
	}
	if len(events) != 2 || events[0].Text != "progress\n" || events[1].Text != "progress\nanswer" || events[1].Verbose != "off" {
		t.Fatal(events)
	}
}
