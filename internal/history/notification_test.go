package history

import "testing"

func TestNotificationHistoryRestoresSafeInterleave(t *testing.T) {
	entries := []Entry{{Role: "notification_pending", Sender: "Spy", Content: "done", EventID: "n1"}, {Role: "notification_ack", EventID: "n1", AfterChars: 6}, {Role: "assistant", Content: "Hello world", Terminal: true}}
	got := resolveNotificationOrder(entries)
	if len(got) != 3 || got[0].Content != "Hello " || got[1].Content != "done" || got[2].Content != "world" {
		t.Fatalf("restored = %#v", got)
	}
}

func TestNotificationHistoryFallsBackAfterTerminalResponse(t *testing.T) {
	entries := []Entry{{Role: "notification_pending", Sender: "Spy", Content: "done", EventID: "n1"}, {Role: "assistant", Content: "hello", Terminal: true}}
	got := resolveNotificationOrder(entries)
	if len(got) != 2 || got[0].Content != "hello" || got[1].Content != "done" {
		t.Fatalf("restored = %#v", got)
	}
}

func TestNotificationHistoryPreservesCancelledStream(t *testing.T) {
	entries := []Entry{{Role: "notification_pending", Sender: "Spy", Content: "done", EventID: "n1"}, {Role: "assistant", Content: "partial"}, {Role: "error", Content: "cancelled", Terminal: true}}
	got := resolveNotificationOrder(entries)
	if len(got) != 3 || got[0].Content != "partial" || got[1].Content != "cancelled" || got[2].Content != "done" {
		t.Fatalf("restored = %#v", got)
	}
}
