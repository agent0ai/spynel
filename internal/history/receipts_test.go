package history

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAppendOncePersistsEffectReceiptAcrossStores(t *testing.T) {
	root := t.TempDir()
	store := New(root)
	entry := Entry{Role: "assistant", Content: strings.Repeat("useful 界 progress ", 5000), Terminal: true, Covers: []string{"source"}, Outcome: "terminal_assistant"}
	if added, err := store.AppendOnce("cli", "owned", "worker:1:3", entry); err != nil || !added {
		t.Fatalf("first effect: added=%v err=%v", added, err)
	}
	entry.At = time.Now().UTC().Add(time.Minute)
	if added, err := New(root).AppendOnce("cli", "owned", "worker:1:3", entry); err != nil || added {
		t.Fatalf("replayed effect: added=%v err=%v", added, err)
	}
	changed := entry
	changed.Content += "changed beyond the bounded body projection"
	if _, err := New(root).AppendOnce("cli", "owned", "worker:1:3", changed); err == nil {
		t.Fatal("receipt accepted a different effect")
	}
	changed = entry
	changed.Covers = []string{"another-source"}
	if _, err := store.AppendOnce("cli", "owned", "worker:1:3", changed); err == nil {
		t.Fatal("receipt accepted different terminal coverage")
	}
	entries, _, err := store.RecentEntries("cli", "owned", 10, 200000)
	if err != nil || len(entries) != 1 || len(entries[0].ReceiptHash) != 64 || len(entries[0].Covers) != 1 {
		t.Fatalf("effect/receipt/coverage lost: count=%d err=%v", len(entries), err)
	}
	context, _, err := store.Recent("cli", "owned", 200000)
	if err != nil || strings.Contains(context, "worker:1:3") || strings.Contains(context, entries[0].ReceiptHash) {
		t.Fatal("private receipt entered rendered context")
	}
	events, _, err := store.EventSnapshot("cli", "owned")
	data, marshalErr := json.Marshal(events)
	if err != nil || marshalErr != nil || strings.Contains(string(data), "receipt_") || strings.Contains(string(data), "worker:1:3") {
		t.Fatal("private receipt entered the committed event projection")
	}
	if added, err := store.AppendOnce("cli", "another", "worker:1:3", entry); err != nil || !added {
		t.Fatal("receipt crossed conversation boundary")
	}
}

func TestAppendOnceSerializesLocalConsumersAndFailsClosed(t *testing.T) {
	store := New(t.TempDir())
	entry := Entry{Role: "error", Content: "synthetic terminal error", Terminal: true}
	var added atomic.Int32
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			inserted, err := store.AppendOnce("cli", "owned", "worker:1:1", entry)
			if err != nil {
				t.Error(err)
			}
			if inserted {
				added.Add(1)
			}
		})
	}
	group.Wait()
	if added.Load() != 1 {
		t.Fatal("concurrent consumers repeated the history effect")
	}
	for _, receipt := range []string{"", "worker\n1", strings.Repeat("x", 257), string([]byte{0xff})} {
		if _, err := store.AppendOnce("cli", "owned", receipt, entry); err == nil {
			t.Fatal("invalid receipt accepted")
		}
	}
	if _, err := store.AppendOnce("cli", "owned", "user", Entry{Role: "user"}); err == nil {
		t.Fatal("receipted append accepted a user-activity side effect")
	}
	path := store.Path("cli", "owned")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("{broken\n")
	if closeErr := file.Close(); err != nil || closeErr != nil {
		t.Fatal("could not seed corruption")
	}
	if _, err := store.AppendOnce("cli", "owned", "worker:1:1", entry); !errors.Is(err, ErrRecoveryCorrupt) {
		t.Fatalf("receipt hid later corruption: %v", err)
	}
	if _, err := store.AppendOnce("cli", "owned", "worker:1:2", entry); !errors.Is(err, ErrRecoveryCorrupt) {
		t.Fatalf("new receipt appended after corruption: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"role":"assistant","content":"valid JSON with no record boundary"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendOnce("cli", "owned", "worker:1:2", entry); !errors.Is(err, ErrRecoveryCorrupt) {
		t.Fatalf("append joined an incomplete tail: %v", err)
	}
}
