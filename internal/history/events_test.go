package history

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestEventsReplayRestartAndReset(t *testing.T) {
	root := t.TempDir()
	store := New(root)
	_, start, err := store.Events("cli", "adapter", "")
	if err != nil || start == "" {
		t.Fatalf("start: %q %v", start, err)
	}
	for _, entry := range []Entry{
		{Role: "user", Content: "hello", SourceMessageID: "req-1"},
		{Role: "correlation", ExecutionID: "private-execution"},
		{Role: "assistant", Content: "reply", SourceMessageID: "req-1", Terminal: true},
		{Role: "assistant", Content: "later notification", EventID: "notification-1"},
	} {
		if _, err := store.Append("cli", "adapter", entry); err != nil {
			t.Fatal(err)
		}
	}
	events, cursor, err := store.Events("cli", "adapter", start)
	if err != nil || len(events) != 3 || events[1].RequestID != "req-1" || !events[1].Done || events[2].NotificationID != "notification-1" {
		t.Fatalf("events: %#v %v", events, err)
	}
	restarted := New(root)
	replay, _, err := restarted.Events("cli", "adapter", events[1].Cursor)
	if err != nil || len(replay) != 1 || replay[0].ID != events[2].ID {
		t.Fatalf("restart replay: %#v %v", replay, err)
	}
	if replay, _, err = restarted.Events("cli", "adapter", cursor); err != nil || len(replay) != 0 {
		t.Fatalf("duplicate replay: %#v %v", replay, err)
	}
	for _, name := range []string{"other", "adapter"} {
		if name == "adapter" {
			if err := store.Clear("cli", name); err != nil {
				t.Fatal(err)
			}
		}
		_, _, _ = store.Events("cli", name, "")
		if _, _, err := store.Events("cli", name, cursor); !errors.Is(err, ErrEventExpired) {
			t.Fatalf("scope/reset %s: %v", name, err)
		}
	}
	other := New(t.TempDir())
	_, _, _ = other.Events("cli", "adapter", "")
	if _, _, err := other.Events("cli", "adapter", cursor); !errors.Is(err, ErrEventExpired) {
		t.Fatalf("workspace fence: %v", err)
	}
}

func TestEventsFailClosedOnBoundsCorruptionAndInvalidCursor(t *testing.T) {
	for _, mode := range []string{"records", "bytes", "partial", "invalid", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			s := New(t.TempDir())
			_, cursor, err := s.Events("cli", "bounded", "")
			if err != nil {
				t.Fatal(err)
			}
			want := ErrEventLag
			switch mode {
			case "records":
				for i := 0; i <= EventReplayRecords; i++ {
					_, _ = s.Append("cli", "bounded", Entry{Role: "assistant", Content: "record"})
				}
			case "bytes":
				_, _ = s.Append("cli", "bounded", Entry{Role: "assistant", Content: strings.Repeat("x", EventReplayBytes)})
			case "partial":
				f, _ := os.OpenFile(s.Path("cli", "bounded"), os.O_APPEND|os.O_WRONLY, 0o600)
				_, _ = f.WriteString("{\"partial\"")
				_ = f.Close()
				want = ErrEventExpired
			case "invalid":
				cursor = "not-a-cursor"
				want = ErrEventCursor
			case "symlink":
				target := filepath.Join(t.TempDir(), "history")
				_ = os.WriteFile(target, []byte("{}\n"), 0o600)
				_ = os.Remove(s.Path("cli", "bounded"))
				if err := os.Symlink(target, s.Path("cli", "bounded")); err != nil {
					t.Skip(err)
				}
				want = ErrEventExpired
			}
			if events, _, err := s.Events("cli", "bounded", cursor); !errors.Is(err, want) || len(events) != 0 {
				t.Fatalf("partial replay: %#v %v, want %v", events, err, want)
			}
		})
	}
}

func TestEventSnapshotConcurrentAppendBoundary(t *testing.T) {
	s := New(t.TempDir())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 40; i++ {
			_, _ = s.Append("cli", "snapshot", Entry{Role: "assistant", Content: "reply"})
		}
	}()
	entries, cursor, err := s.EventSnapshot("cli", "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	replay, _, err := s.Events("cli", "snapshot", cursor)
	if err != nil || len(entries)+len(replay) != 40 {
		t.Fatalf("snapshot gap: %d + %d, %v", len(entries), len(replay), err)
	}
}
