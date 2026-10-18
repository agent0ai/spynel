package history

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestConversationAdmissionRestorationRead(t *testing.T) {
	store := New(t.TempDir())
	for _, source := range []string{"original", "rejected"} {
		if _, err := store.Append("cli", "owned", Entry{Role: "user", SourceMessageID: source, Content: "synthetic"}); err != nil {
			t.Fatal(err)
		}
		if err := store.AdmitConversation("cli", "owned", "execution", source+":admission", []string{source}, false); err != nil {
			t.Fatal(err)
		}
	}
	// A durable restoration identifies the one rejected batch, not its entire
	// logical execution. Exercise the actual complete-history recovery reader.
	file, err := os.OpenFile(store.Path("cli", "owned"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	err = json.NewEncoder(file).Encode(map[string]any{
		"role": "correlation", "execution_id": "execution", "outcome": "admission_restored",
		"admitted_sources": []string{"rejected"}, "restores_admission": "rejected:admission",
	})
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	snapshot, err := New(store.root).RecoveryWindow("cli", "owned", time.Time{}, 20)
	if err != nil || !snapshot.Admitted["original"] || snapshot.Admitted["rejected"] || len(snapshot.Covered) != 0 {
		t.Fatalf("restored rejected batch remains admitted or changed original coverage: admitted=%v covered=%v error=%v", snapshot.Admitted, snapshot.Covered, err)
	}
	if err := store.AdmitConversation("cli", "owned", "next-execution", "next:admission", []string{"rejected"}, false); err != nil {
		t.Fatal("restored source could not enter ordinary admission", err)
	}
}

func seedAdmissionRestoration(t *testing.T) (*Store, []string) {
	t.Helper()
	store := New(t.TempDir())
	for _, source := range []string{"original", "alpha", "beta"} {
		if _, err := store.Append("cli", "owned", Entry{Role: "user", SourceMessageID: source, Content: "synthetic", AcceptedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AdmitConversation("cli", "owned", "execution", "original-admission", []string{"original"}, false); err != nil {
		t.Fatal(err)
	}
	sources := []string{"alpha", "beta"}
	return store, sources
}

func TestConversationAdmissionRestorationIsExactAndDurable(t *testing.T) {
	for _, recoverSource := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "recovery"}[recoverSource], func(t *testing.T) {
			store, sources := seedAdmissionRestoration(t)
			// A valid admission can use the complete shared receipt-identity bound.
			receipt := strings.Repeat("a", 256)
			if err := store.AdmitConversation("cli", "owned", "execution", receipt, sources, false); err != nil {
				t.Fatal(err)
			}
			restore := func() error {
				return New(store.root).RestoreConversationAdmission("cli", "owned", "execution", receipt, sources)
			}
			if err := restore(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(store.Path("cli", "owned"))
			if err != nil || restore() != nil {
				t.Fatal("restoration retry failed", err)
			}
			// Confirmation of the old admission must not resurrect its sources.
			if err := store.AdmitConversation("cli", "owned", "execution", receipt, sources, false); err != nil {
				t.Fatal("old admission confirmation failed", err)
			}
			after, err := os.ReadFile(store.Path("cli", "owned"))
			if err != nil || string(before) != string(after) {
				t.Fatal("matching retries appended or changed history", err)
			}
			snapshot, err := New(store.root).RecoveryWindow("cli", "owned", time.Time{}, 20)
			if err != nil || !snapshot.Admitted["original"] || snapshot.Admitted["alpha"] || snapshot.Admitted["beta"] || len(snapshot.Covered) != 0 {
				t.Fatal("restoration affected the older batch or created coverage", err)
			}
			if recoverSource {
				err = store.AdmitRecoveryConversation("cli", "owned", "next-execution", "next-admission", sources, time.Now().Add(-time.Hour), time.Now().Add(-24*time.Hour), func(entries []Entry) error {
					if len(entries) != len(sources) {
						t.Fatal("recovery lost a restored source")
					}
					return nil
				})
			} else {
				err = store.AdmitConversation("cli", "owned", "next-execution", "next-admission", sources, false)
			}
			if err != nil || restore() != nil {
				t.Fatal("later admission or old restoration confirmation failed", err)
			}
			snapshot, err = New(store.root).RecoveryWindow("cli", "owned", time.Time{}, 20)
			if err != nil || !snapshot.Admitted["original"] || !snapshot.Admitted["alpha"] || !snapshot.Admitted["beta"] || snapshot.admissions["alpha"].execution != "next-execution" {
				t.Fatal("old restoration cleared a later admission", err)
			}
			context, _, err := store.RecentBounded("cli", "owned", 20, 4096)
			events, _, eventErr := store.EventSnapshot("cli", "owned")
			encoded, marshalErr := json.Marshal(events)
			if err != nil || eventErr != nil || marshalErr != nil || len(events) != 3 || strings.Contains(context, "admission") || strings.Contains(string(encoded), "restores_admission") || strings.Contains(string(encoded), "next-execution") {
				t.Fatal("restoration metadata entered prompt context or committed events", err, eventErr, marshalErr)
			}
		})
	}
}

func TestConversationAdmissionRestorationPreservesCoverage(t *testing.T) {
	store, sources := seedAdmissionRestoration(t)
	if err := store.AdmitConversation("cli", "owned", "execution", "attempt-admission", sources, false); err != nil {
		t.Fatal(err)
	}
	if err := store.SetConversationCancellation("cli", "owned", "execution", "intent", sources, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("cli", "owned", Entry{Role: "correlation", Covers: []string{"beta", "original"}, Outcome: "terminal_assistant"}); err != nil {
		t.Fatal(err)
	}
	if err := store.RestoreConversationAdmission("cli", "owned", "execution", "attempt-admission", sources); err != nil {
		t.Fatal(err)
	}
	snapshot, err := New(store.root).RecoveryWindow("cli", "owned", time.Time{}, 20)
	if err != nil || snapshot.Covered["alpha"] != "cancellation_requested" || snapshot.Covered["beta"] != "terminal_coverage" || snapshot.Covered["original"] != "terminal_coverage" || !snapshot.Admitted["original"] {
		t.Fatal("restoration erased terminal coverage or cancellation intent", err)
	}
	if err := store.AdmitConversation("cli", "owned", "next", "next-admission", sources, false); err == nil {
		t.Fatal("restoration made covered/cancelled sources eligible")
	}
}

func TestConversationAdmissionRestorationRefusesChangedBatch(t *testing.T) {
	for _, scenario := range []string{"subset", "superset", "execution", "receipt", "unsorted", "recovery", "storage", "corrupt"} {
		t.Run(scenario, func(t *testing.T) {
			store, sources := seedAdmissionRestoration(t)
			recovery := scenario == "recovery"
			if recovery {
				if reserved, err := store.ReserveRetrigger("cli", "owned", sources, time.Time{}, time.Time{}, len(sources)); err != nil || len(reserved) != len(sources) {
					t.Fatal("recovery fixture reservation failed", err)
				}
			}
			if err := store.AdmitConversation("cli", "owned", "execution", "attempt-admission", sources, recovery); err != nil {
				t.Fatal(err)
			}
			execution, receipt := "execution", "attempt-admission"
			switch scenario {
			case "subset":
				sources = sources[:1]
			case "superset":
				sources = append(sources, "original")
			case "execution":
				execution = "other-execution"
			case "receipt":
				receipt = "other-admission"
			case "unsorted":
				sources[0], sources[1] = sources[1], sources[0]
			case "corrupt":
				file, err := os.OpenFile(store.Path("cli", "owned"), os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := file.WriteString("{broken\n")
				if closeErr := file.Close(); writeErr != nil || closeErr != nil {
					t.Fatal(writeErr, closeErr)
				}
			}
			path := store.Path("cli", "owned")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "storage" {
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.RestoreConversationAdmission("cli", "owned", execution, receipt, sources); err == nil {
				t.Fatal("changed/unavailable batch was restored")
			}
			if scenario == "storage" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path+".saved", path); err != nil {
					t.Fatal(err)
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatal("refused restoration changed durable history", err)
			}
		})
	}
}

func TestConversationAdmissionRestorationMalformedRecordFailsClosed(t *testing.T) {
	for _, scenario := range []string{"missing-receipt", "changed-receipt", "execution", "subset", "mixed-coverage"} {
		t.Run(scenario, func(t *testing.T) {
			store, sources := seedAdmissionRestoration(t)
			if err := store.AdmitConversation("cli", "owned", "execution", "attempt-admission", sources, false); err != nil {
				t.Fatal(err)
			}
			entry := Entry{Role: "correlation", ExecutionID: "execution", AdmittedSources: sources, RestoresAdmission: "attempt-admission", Outcome: "admission_restored"}
			switch scenario {
			case "missing-receipt":
				entry.RestoresAdmission = ""
			case "changed-receipt":
				entry.RestoresAdmission = "other-admission"
			case "execution":
				entry.ExecutionID = "other-execution"
			case "subset":
				entry.AdmittedSources = sources[:1]
			case "mixed-coverage":
				entry.Covers = sources
			}
			if _, err := store.Append("cli", "owned", entry); err != nil {
				t.Fatal(err)
			}
			if _, err := store.RecoveryWindow("cli", "owned", time.Time{}, 20); err == nil {
				t.Fatal("malformed restoration was accepted")
			}
		})
	}
}

func TestConversationAdmissionRestorationDoesNotCrossBaseline(t *testing.T) {
	store, sources := seedAdmissionRestoration(t)
	if err := store.AdmitConversation("cli", "owned", "execution", "attempt-admission", sources, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("cli", "owned", Entry{Role: "correlation", RecoveryBaseline: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.RestoreConversationAdmission("cli", "owned", "execution", "attempt-admission", sources); err == nil {
		t.Fatal("restoration crossed the branch baseline")
	}
	for _, source := range sources {
		if _, err := store.Append("cli", "owned", Entry{Role: "user", SourceMessageID: source, Content: "synthetic", AcceptedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AdmitConversation("cli", "owned", "next-execution", "next-admission", sources, false); err != nil {
		t.Fatal(err)
	}
	if err := store.RestoreConversationAdmission("cli", "owned", "execution", "attempt-admission", sources); err == nil {
		t.Fatal("restoration cleared reused source identities after a baseline")
	}
	snapshot, err := New(store.root).RecoveryWindow("cli", "owned", time.Time{}, 20)
	if err != nil || len(snapshot.Entries) != 2 || len(snapshot.Admitted) != 2 || snapshot.admissions["alpha"].execution != "next-execution" {
		t.Fatal("old restoration affected the new branch", err)
	}
}
