package history

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRecoveryConversationAdmissionReservesExactBatchAtomically(t *testing.T) {
	for _, scenario := range []string{"accepted", "covered", "reserved", "legacy", "stale", "missing", "validation"} {
		t.Run(scenario, func(t *testing.T) {
			store := New(t.TempDir())
			now := time.Now().UTC()
			for _, source := range []string{"first", "second"} {
				entry := Entry{At: now, AcceptedAt: now, Role: "user", SourceMessageID: source, Content: "synthetic"}
				if source == "second" {
					switch scenario {
					case "missing":
						continue
					case "legacy":
						entry.AcceptedAt = now.Add(-time.Hour)
					case "stale":
						entry.At = now.Add(-48 * time.Hour)
					}
				}
				if _, err := store.Append("cli", "owned", entry); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "covered" || scenario == "reserved" {
				entry := Entry{Role: "correlation", Covers: []string{"second"}, Outcome: "terminal_assistant"}
				if scenario == "reserved" {
					entry.Covers, entry.RetriggerOf = nil, []string{"second"}
				}
				if _, err := store.Append("cli", "owned", entry); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(store.Path("cli", "owned"))
			if err != nil {
				t.Fatal(err)
			}
			admit := func(receipt string) error {
				return store.AdmitRecoveryConversation("cli", "owned", "execution", receipt, []string{"first", "second"}, now.Add(-time.Minute), now.Add(-24*time.Hour), func(entries []Entry) error {
					if len(entries) != 2 {
						t.Fatal("validation did not receive the exact source set")
					}
					if scenario == "validation" {
						return errors.New("source policy refused")
					}
					return nil
				})
			}
			err = admit("worker:1:admission")
			after, readErr := os.ReadFile(store.Path("cli", "owned"))
			if scenario != "accepted" {
				if err == nil || readErr != nil || string(before) != string(after) {
					t.Fatal("failed recovery partially reserved its batch", err, readErr)
				}
				return
			}
			if err != nil || readErr != nil || admit("worker:1:admission") != nil || admit("worker:2:admission") == nil {
				t.Fatal("recovery receipt was not idempotent and exclusive", err, readErr)
			}
			entries, _, err := store.RecoveryEntries("cli", "owned", 10)
			if err != nil || len(entries) != 3 || !reflect.DeepEqual(entries[2].AdmittedSources, entries[2].RetriggerOf) || !entries[2].Recovery {
				t.Fatal("recovery reservation and admission were not one record", err)
			}
		})
	}
}

func TestConversationAdmissionCommitsExactBatchAndPrivateReceipt(t *testing.T) {
	root := t.TempDir()
	store := New(root)
	sources := []string{"first", "second"}
	for _, id := range sources {
		if _, err := store.Append("cli", "owned", Entry{Role: "user", SourceMessageID: id, Content: "synthetic", AcceptedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	admit := func(receipt string, ids []string) error {
		return New(root).AdmitConversation("cli", "owned", "execution", receipt, ids, false)
	}
	if err := admit("worker:1:admission", sources); err != nil {
		t.Fatal(err)
	}
	if err := admit("worker:1:admission", sources); err != nil {
		t.Fatal("replacement repeated the reservation", err)
	}
	if err := admit("worker:1:admission", sources[:1]); err == nil {
		t.Fatal("changed batch reused the receipt")
	}
	if err := admit("worker:2:admission", sources); err == nil {
		t.Fatal("another execution reserved an admitted batch")
	}
	entries, _, err := store.RecoveryEntries("cli", "owned", 10)
	if err != nil || len(entries) != 3 || !reflect.DeepEqual(entries[2].AdmittedSources, sources) {
		t.Fatal("batch admission was not one exact record", err)
	}
	snapshot, err := store.RecoveryWindow("cli", "owned", time.Time{}, 2000)
	if err != nil || !snapshot.Admitted["first"] || !snapshot.Admitted["second"] || len(snapshot.Covered) != 0 {
		t.Fatal("admission became coverage or lost a batch source", err)
	}
	context, _, err := store.RecentBounded("cli", "owned", 20, 10000)
	if err != nil || strings.Contains(context, "execution") || strings.Contains(context, "worker:1:admission") {
		t.Fatal("private admission entered prompt context", err)
	}
	events, _, err := store.EventSnapshot("cli", "owned")
	data, marshalErr := json.Marshal(events)
	if err != nil || marshalErr != nil || len(events) != 2 || strings.Contains(string(data), "admitted_sources") || strings.Contains(string(data), "worker:1:admission") {
		t.Fatal("private admission entered committed events", err)
	}
	// Later settlement does not invalidate confirmation of an existing receipt.
	if _, err := store.Append("cli", "owned", Entry{Role: "correlation", Covers: sources, Outcome: "terminal_assistant"}); err != nil {
		t.Fatal(err)
	}
	if err := admit("worker:1:admission", sources); err != nil {
		t.Fatal("matching receipt confirmation depended on later settlement", err)
	}
}

func TestConversationAdmissionRejectsEntireInvalidOrCoveredBatch(t *testing.T) {
	for _, scenario := range []string{"missing", "duplicate", "unsorted", "empty", "invalid", "covered", "cancelled", "reserved", "admitted", "corrupt"} {
		t.Run(scenario, func(t *testing.T) {
			store := New(t.TempDir())
			for _, source := range []string{"first", "second"} {
				if _, err := store.Append("cli", "owned", Entry{Role: "user", SourceMessageID: source, Content: "synthetic"}); err != nil {
					t.Fatal(err)
				}
			}
			sources := []string{"first", "second"}
			entry := Entry{Role: "correlation"}
			switch scenario {
			case "missing":
				sources = []string{"first", "missing"}
			case "duplicate":
				sources = []string{"first", "first"}
			case "unsorted":
				sources = []string{"second", "first"}
			case "empty":
				sources = nil
			case "invalid":
				sources = []string{"bad\nidentity", "first"}
			case "covered", "cancelled":
				entry.Covers, entry.Outcome = []string{"second"}, "terminal_assistant"
				if scenario == "cancelled" {
					entry.Outcome = "intentional_cancellation"
				}
			case "reserved":
				entry.RetriggerOf = []string{"second"}
			case "admitted":
				entry.SourceMessageID, entry.Admission = "second", "new"
			}
			if _, err := store.Append("cli", "owned", entry); err != nil {
				t.Fatal(err)
			}
			if scenario == "corrupt" {
				file, err := os.OpenFile(store.Path("cli", "owned"), os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.WriteString("{broken\n")
				if closeErr := file.Close(); err != nil || closeErr != nil {
					t.Fatal(err, closeErr)
				}
			}
			before, err := os.ReadFile(store.Path("cli", "owned"))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.AdmitConversation("cli", "owned", "execution", "worker:1:admission", sources, false); err == nil {
				t.Fatal("invalid batch was admitted")
			}
			after, err := os.ReadFile(store.Path("cli", "owned"))
			if err != nil || string(before) != string(after) {
				t.Fatal("failed reservation admitted part of a batch", err)
			}
		})
	}
}

func TestConversationRecoveryAdmissionRequiresExactUncoveredReservation(t *testing.T) {
	for _, covered := range []bool{false, true} {
		store := New(t.TempDir())
		if _, err := store.Append("cli", "owned", Entry{Role: "user", SourceMessageID: "source", AcceptedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if err := store.AdmitConversation("cli", "owned", "execution", "worker:1:admission", []string{"source"}, true); err == nil {
			t.Fatal("recovery invented a reservation")
		}
		if reserved, err := store.ReserveRetrigger("cli", "owned", []string{"source"}, time.Time{}, time.Time{}, 1); err != nil || len(reserved) != 1 {
			t.Fatal("could not reserve recovery source", err)
		}
		if covered {
			if _, err := store.Append("cli", "owned", Entry{Role: "correlation", Covers: []string{"source"}, Outcome: "intentional_cancellation"}); err != nil {
				t.Fatal(err)
			}
		}
		err := store.AdmitConversation("cli", "owned", "execution", "worker:1:admission", []string{"source"}, true)
		if (err != nil) != covered {
			t.Fatal("recovery did not recheck exact coverage", err)
		}
		if !covered {
			if err := store.AdmitConversation("cli", "owned", "execution", "worker:1:admission", []string{"source"}, true); err != nil {
				t.Fatal("matching recovery receipt did not confirm", err)
			}
			if err := store.AdmitConversation("cli", "owned", "another", "worker:2:admission", []string{"source"}, true); err == nil {
				t.Fatal("a second recovery admission reused the same reservation")
			}
		}
	}
}
