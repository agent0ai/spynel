package history

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestConversationCancellationPreservesExactIntent(t *testing.T) {
	root := t.TempDir()
	store := New(root)
	sources := []string{"first", "second"}
	for _, source := range sources {
		if _, err := store.Append("cli", "owned", Entry{Role: "user", SourceMessageID: source, Content: "synthetic", AcceptedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AdmitConversation("cli", "owned", "execution", "admission", sources, false); err != nil {
		t.Fatal(err)
	}
	set := func(intent string, restore bool) {
		t.Helper()
		if err := New(root).SetConversationCancellation("cli", "owned", "execution", intent, sources, restore); err != nil {
			t.Fatal(err)
		}
	}
	check := func(want string) {
		t.Helper()
		snapshot, err := New(root).RecoveryWindow("cli", "owned", time.Time{}, 20)
		if err != nil || snapshot.Covered["first"] != want || snapshot.Covered["second"] != want || !snapshot.Admitted["first"] {
			t.Fatalf("cancellation intent = %v, want %q: %v", snapshot.Covered, want, err)
		}
	}
	set("first-intent", false)
	set("first-intent", false)
	check("cancellation_requested")
	if err := store.AdmitRecoveryConversation("cli", "owned", "recovery", "recovery-admission", sources, time.Now().Add(-time.Hour), time.Now().Add(-24*time.Hour), func([]Entry) error {
		t.Fatal("cancelled sources reached recovery policy")
		return nil
	}); err == nil {
		t.Fatal("pending cancellation admitted a recovery provider turn")
	}
	set("later-intent", false)
	set("later-intent", true)
	check("cancellation_requested") // A clean later rejection cannot erase the earlier uncertainty.
	set("first-intent", true)
	check("")
	set("new-intent", false)
	set("first-intent", true)
	check("cancellation_requested")
	if _, err := store.AppendOnce("cli", "owned", "terminal", Entry{Role: "assistant", Content: "done", Covers: sources, Outcome: "terminal_assistant"}); err != nil {
		t.Fatal(err)
	}
	set("new-intent", true)
	check("terminal_coverage")
	recent, _, err := store.RecentBounded("cli", "owned", 20, 4096)
	events, _, eventErr := store.EventSnapshot("cli", "owned")
	if err != nil || eventErr != nil || len(events) != 3 || strings.Contains(recent, "intent") || strings.Contains(recent, "cancellation") {
		t.Fatal("private cancellation correlation entered visible context/events", err, eventErr, len(events))
	}
	if _, err := store.Append("cli", "owned", Entry{Role: "correlation", RecoveryBaseline: true}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := New(root).RecoveryWindow("cli", "owned", time.Time{}, 20)
	if err != nil || len(snapshot.Entries) != 0 || len(snapshot.Covered) != 0 {
		t.Fatal("branch baseline retained an earlier cancellation", err)
	}
}

func TestConversationCancellationMalformedRecordFailsClosed(t *testing.T) {
	for _, scenario := range []string{"identity", "outcome", "role", "sources"} {
		t.Run(scenario, func(t *testing.T) {
			store := New(t.TempDir())
			entry := Entry{Role: "correlation", ExecutionID: "execution", CancellationID: "intent", Covers: []string{"source"}, Outcome: "cancellation_requested"}
			switch scenario {
			case "identity":
				entry.CancellationID = ""
			case "outcome":
				entry.Outcome = "unknown"
			case "role":
				entry.Role = "assistant"
			case "sources":
				entry.Covers = []string{"source", "source"}
			}
			if _, err := store.Append("cli", "owned", entry); err != nil {
				t.Fatal(err)
			}
			if _, err := store.RecoveryWindow("cli", "owned", time.Time{}, 20); err == nil {
				t.Fatal("malformed cancellation correlation was accepted")
			}
		})
	}
}

func TestConversationCancellationRefusesChangedAdmission(t *testing.T) {
	for _, scenario := range []string{"missing", "unadmitted", "changed-execution", "changed-receipt", "corrupt-tail", "invalid"} {
		t.Run(scenario, func(t *testing.T) {
			store := New(t.TempDir())
			if _, err := store.Append("cli", "owned", Entry{Role: "user", SourceMessageID: "source", Content: "synthetic"}); err != nil {
				t.Fatal(err)
			}
			sources, execution := []string{"source"}, "execution"
			if scenario != "unadmitted" {
				if err := store.AdmitConversation("cli", "owned", execution, "admission", sources, false); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "missing":
				sources = []string{"absent", "source"}
			case "changed-execution":
				execution = "other"
			case "invalid":
				sources = []string{"source", "source"}
			case "changed-receipt", "corrupt-tail":
				if err := store.SetConversationCancellation("cli", "owned", execution, "intent", sources, false); err != nil {
					t.Fatal(err)
				}
				if scenario == "changed-receipt" {
					execution = "other"
				} else {
					file, err := os.OpenFile(store.Path("cli", "owned"), os.O_APPEND|os.O_WRONLY, 0600)
					if err != nil {
						t.Fatal(err)
					}
					_, writeErr := file.WriteString("{\n")
					closeErr := file.Close()
					if writeErr != nil || closeErr != nil {
						t.Fatal(writeErr, closeErr)
					}
				}
			}
			before, err := os.ReadFile(store.Path("cli", "owned"))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SetConversationCancellation("cli", "owned", execution, "intent", sources, false); err == nil {
				t.Fatal("unavailable/changed admission was cancelled")
			}
			after, err := os.ReadFile(store.Path("cli", "owned"))
			if err != nil || string(before) != string(after) {
				t.Fatal("refused cancellation changed history", err)
			}
		})
	}
}
