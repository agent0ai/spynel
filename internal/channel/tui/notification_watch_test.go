package tui

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
)

func TestWatchRetainedWorkerResponsesPreservesActivityAndRetryOrder(t *testing.T) {
	cfg := config.Default()
	cfg.Root = t.TempDir()
	cfg.Extensions.Enabled = false
	s := app.New(cfg, harness.NewSupervisor(harness.NewBuiltinRegistry(), harness.HarnessConfig{}))
	defer s.Close()
	c := &harness.WorkerConversation{Channel: "tui", Conversation: "selected", SourceMessageID: "source", ExecutionID: "execution", Sources: []string{"source"}}
	if _, err := s.History.Append(c.Channel, c.Conversation, history.Entry{Role: "user", SourceMessageID: "source", Content: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	id, err := s.Runtime.TryBeginJobWithDetails("chat:tui:selected", c.Channel, c.Conversation, "synthetic", app.JobDetails{Kind: "conversation", WorkID: c.ExecutionID})
	if err != nil {
		t.Fatal(err)
	}
	_, path, offset, err := s.History.RecentEntriesSnapshot(c.Channel, c.Conversation, 10, 1000)
	if err != nil {
		t.Fatal(err)
	}
	events := watchTaskNotifications(t.Context(), path, offset)
	d := harness.WorkerDelivery{WorkerID: strings.Repeat("f", 32), Sequence: 1, WorkerEvent: harness.WorkerEvent{Cursor: 1, Conversation: c,
		Event: core.Event{Kind: core.EventFinal, Text: "intermediate", Done: true, Continues: true}}}
	if err := s.ConsumeConversationDelivery(id, d, nil); err != nil {
		t.Fatal(err)
	}
	d.Cursor, d.Streamed = 2, "useful partial"
	d.Event = core.Event{Kind: core.EventError, Text: "retained error", Done: true}
	if err := s.ConsumeConversationDelivery(id, d, func(harness.WorkerDelivery) error { return errors.New("synthetic lost delivery") }); err == nil {
		t.Fatal("fixture did not retain its pending delivery")
	}
	if err := s.ConsumeConversationDelivery(id, d, nil); err != nil {
		t.Fatal(err)
	}
	m := testModel()
	for _, event := range []uiEvent{
		{event: core.Event{Kind: core.EventActivity, Active: true}, conversation: true},
		{event: core.Event{Kind: core.EventActivity, Active: true}},
	} {
		next, _ := m.Update(event)
		m = next.(model)
	}
	m.streaming, m.responseText = "newer stream", "newer stream"
	for i, text := range []string{"intermediate", "useful partial", "retained error"} {
		select {
		case event := <-events:
			if event.Text != text || !event.Recovery || event.ID != "" || event.Continues != (i < 2) || event.Error != (i == 2) {
				t.Fatalf("retained response %d = %#v", i, event)
			}
			next, _ := m.Update(taskNotificationEvent{notification: event})
			m = next.(model)
			wantActivity := 2
			if i == 2 {
				wantActivity = 1
			}
			if m.mainAgentActivity != wantActivity || m.streaming != "newer stream" || m.responseText != "newer stream" || m.notificationAckBusy || len(m.pendingNotifications) != 0 {
				t.Fatal("retained history disturbed activity, foreground stream or notification acknowledgement")
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("retained response %d never reached the selected TUI", i)
		}
	}
	if len(m.transcript) != 3 || m.transcript[2].role != "error" {
		t.Fatal("retained transcript lost order or error role")
	}
	next, _ := m.Update(uiEvent{event: core.Event{Kind: core.EventActivity}, conversation: true})
	m = next.(model)
	if m.mainAgentActivity != 1 || !m.working {
		t.Fatal("delayed inactive edge stopped the newer foreground turn")
	}
	select {
	case extra := <-events:
		t.Fatalf("receipt replay duplicated the live transcript: %#v", extra)
	case <-time.After(250 * time.Millisecond):
	}
}

func TestWatchTaskNotificationsRetriesPartialRecoveryLine(t *testing.T) {
	store := history.New(t.TempDir())
	if _, err := store.Ensure("tui", "partial"); err != nil {
		t.Fatal(err)
	}
	_, path, offset, err := store.RecentEntriesSnapshot("tui", "partial", 10, 1000)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := watchTaskNotifications(ctx, path, offset)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	line := []byte(`{"role":"assistant","sender":"Spy","content":"partial recovered","recovery":true}` + "\n")
	if _, err := file.Write(line[:len(line)/2]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if _, err := file.Write(line[len(line)/2:]); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.Text != "partial recovered" || !event.Recovery {
			t.Fatalf("partial recovery event = %#v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("partial recovery line was skipped")
	}
}

func TestWatchTaskNotificationsSurfacesLiveRecoveryTerminal(t *testing.T) {
	store := history.New(t.TempDir())
	if _, err := store.Ensure("tui", "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("tui", "local", history.Entry{Role: "assistant", Content: "already displayed"}); err != nil {
		t.Fatal(err)
	}
	_, path, offset, err := store.RecentEntriesSnapshot("tui", "local", 10, 1000)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := watchTaskNotifications(ctx, path, offset)
	if _, err := store.Append("tui", "local", history.Entry{Role: "assistant", Sender: "Spy", Content: "recovered visibly", Terminal: true, Recovery: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.Text != "recovered visibly" || event.ID != "" || !event.Recovery || event.Error {
			t.Fatalf("recovery event = %#v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("live TUI watcher did not surface recovery terminal")
	}
}

func TestWatchTaskNotificationsMarksRecoveryErrorsWithoutAcknowledgementIdentity(t *testing.T) {
	store := history.New(t.TempDir())
	if _, err := store.Ensure("tui", "local"); err != nil {
		t.Fatal(err)
	}
	_, path, offset, err := store.RecentEntriesSnapshot("tui", "local", 10, 1000)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := watchTaskNotifications(ctx, path, offset)
	if _, err := store.Append("tui", "local", history.Entry{Role: "error", Sender: "Spy", Content: "provider interrupted", Terminal: true, Recovery: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.Text != "provider interrupted" || event.ID != "" || !event.Recovery || !event.Error {
			t.Fatalf("recovery error = %#v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("live TUI watcher did not surface recovery error")
	}
}

func TestRestartHistorySnapshotAndRecoveredTailHaveOneStableOrder(t *testing.T) {
	store := history.New(t.TempDir())
	for _, entry := range []history.Entry{
		{Role: "user", Content: "ordinary question"},
		{Role: "user", Content: "/restart"},
		{Role: "assistant", Content: "Restarting Spynel..."},
		{Role: "user", Content: "/restart"},
		{Role: "assistant", Content: "Restarting Spynel..."},
	} {
		if _, err := store.Append("tui", "restart", entry); err != nil {
			t.Fatal(err)
		}
	}
	initial, path, offset, err := store.RecentEntriesSnapshot("tui", "restart", 20, 4000)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := watchTaskNotifications(ctx, path, offset)
	if _, err := store.Append("tui", "restart", history.Entry{Role: "assistant", Sender: "Spy", Content: "recovered answer", Terminal: true, Recovery: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if len(initial) != 5 || initial[0].Content != "ordinary question" || initial[1].Content != "/restart" || initial[2].Content != "Restarting Spynel..." || initial[3].Content != "/restart" || initial[4].Content != "Restarting Spynel..." || event.Text != "recovered answer" || !event.Recovery {
			t.Fatalf("restart/recovery live sequence = initial %#v event %#v", initial, event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recovered terminal did not cross the startup snapshot boundary")
	}
	reopened, _, _, err := store.RecentEntriesSnapshot("tui", "restart", 20, 4000)
	if err != nil || len(reopened) != 6 || reopened[5].Content != "recovered answer" {
		t.Fatalf("reopened sequence = %#v, %v", reopened, err)
	}
}
