package localapi

import (
	"testing"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
)

func TestVerboseAcrossAttachedClientsAndPersistence(t *testing.T) {
	election, server, target, cancel, done := startTestServer(t, t.TempDir())
	defer func() { cancel(); <-done; _ = election.Release(server.Token) }()
	first, second := NewClient(election), NewClient(election)
	for _, mode := range []string{"off", "on"} {
		var reply core.Event
		if err := first.Handle(t.Context(), core.Message{Channel: "tui", Conversation: "first", Text: "/verbose " + mode}, func(e core.Event) { reply = e }); err != nil {
			t.Fatal(err)
		}
		if !reply.Local || reply.Verbose != mode {
			t.Fatal(reply)
		}
		state, err := second.State(t.Context())
		if err != nil || state.Verbose != mode {
			t.Fatalf("secondary state: %#v %v", state, err)
		}
		if err := second.Handle(t.Context(), core.Message{Channel: "tui", Conversation: "second", Text: "/verbose"}, func(e core.Event) { reply = e }); err != nil {
			t.Fatal(err)
		}
		loaded, err := config.Load(server.Service.Config.Path)
		if err != nil || loaded.Channels.Verbose != mode {
			t.Fatal("restart persistence", err)
		}
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	if len(target.keys) != 0 {
		t.Fatal("verbose dispatched a provider")
	}
}
