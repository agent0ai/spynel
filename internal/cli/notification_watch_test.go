package cli

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/history"
)

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
