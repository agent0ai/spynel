//go:build linux || darwin

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
	"github.com/agent0ai/spynel/internal/updater"
)

func TestCompletedWorkerJournalAfterNativeDeath(t *testing.T) {
	for _, persisted := range []bool{false, true} {
		t.Run(fmt.Sprintf("history-persisted-%v", persisted), func(t *testing.T) { completedWorkerJournalAfterNativeDeath(t, persisted) })
	}
}

func completedWorkerJournalAfterNativeDeath(t *testing.T, persisted bool) {
	completion := filepath.Join(t.TempDir(), "complete")
	t.Setenv("SPYNEL_TEST_WORKER_COMPLETIONS", completion)
	t.Setenv("SPYNEL_TEST_WORKER_CONVERSATION_ERROR", "3")
	f := newWorkerAgentFixture(t)
	c := harness.WorkerConversation{Channel: "cli", Conversation: "journal-crash", SourceMessageID: "source", ExecutionID: "synthetic-journal-execution", Sources: []string{"source"}}
	if _, err := f.service.History.Append(c.Channel, c.Conversation, history.Entry{Role: "user", Content: "synthetic request", SourceMessageID: "source", AcceptedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	execution, err := f.service.Orchestrator.AdmitConversation(t.Context(), c, "synthetic completed journal", "synthetic request")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(completion+".1", nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitWorkerAgent(t, func() bool { return len(f.inspect(t)) == 1 && f.inspect(t)[0].Done })
	retained := f.inspect(t)[0]
	if retained.CommittedCursor != 0 || retained.LastCursor == 0 {
		t.Fatal("fixture did not retain an unconsumed actual provider terminal")
	}
	old := f.client.Info()
	journal := filepath.Join(filepath.Dir(old.Socket), fmt.Sprintf("%d.jsonl", execution.Sequence))
	data, err := os.ReadFile(journal)
	if err != nil || !strings.Contains(string(data), `"kind":"final"`) {
		t.Fatal("provider terminal was not durably journaled", err)
	}
	if persisted {
		job, ok := f.service.Runtime.JobForSession(execution.Key)
		if !ok {
			t.Fatal("missing admitted job")
		}
		_, err := f.client.Consume(t.Context(), execution.Sequence, func(delivery harness.WorkerDelivery) error {
			return f.service.ConsumeConversationDelivery(job.ID, delivery, func(delivery harness.WorkerDelivery) error {
				if delivery.Event.Done {
					return errors.New("synthetic failure after durable terminal before cursor commit")
				}
				return nil
			})
		})
		if err == nil || f.inspect(t)[0].CommittedCursor != 0 {
			t.Fatal("fixture did not leave a durable effect before commit")
		}
	}

	f.stop(t)
	guardian := workerCrashGuardian(t)
	t.Logf("completed worker pid=%d start=%s generation=%s endpoint=%s", old.PID, old.Process.StartTime, old.Process.Generation, old.Socket)
	t.Logf("guardian pid=%d start=%s generation=%s", guardian.PID, guardian.StartTime, guardian.Generation)
	if err := updater.VerifyProcess(old.Process); err != nil {
		t.Fatal(err)
	}
	process, err := os.FindProcess(old.PID)
	if err != nil {
		t.Fatal(err)
	}
	defer process.Release()
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitWorkerAgent(t, func() bool {
		return errors.Is(updater.VerifyProcess(old.Process), syscall.ESRCH) && errors.Is(updater.VerifyProcess(guardian), syscall.ESRCH)
	})
	// Retain the exact old directory until the successor has had a chance to reconcile it.
	t.Cleanup(func() {
		if err := os.RemoveAll(filepath.Dir(old.Socket)); err != nil {
			t.Error(err)
		}
	})
	if err := f.owner.Release(f.term); err != nil {
		t.Fatal(err)
	}
	f.client.Close()
	f.open(t)
	t.Logf("replacement worker pid=%d start=%s generation=%s endpoint=%s", f.client.Info().PID, f.client.Info().Process.StartTime, f.client.Info().Process.Generation, f.client.Info().Socket)
	f.service.SetPrimaryInstanceID(f.owner.ID())
	f.service.SetRecoveryOwnershipFence(func(action func() error) (bool, error) { return f.owner.RunWhileOwner(f.term, action) })
	if err := f.service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	waitWorkerAgent(t, func() bool { return !f.service.RecoveryStatus().ScannedAt.IsZero() })
	waitWorkerAgent(t, func() bool {
		window, err := f.service.History.RecoveryWindow(c.Channel, c.Conversation, time.Time{}, 20)
		return err == nil && window.Covered["source"] == "terminal_coverage"
	})
	waitWorkerAgent(t, func() bool { return len(f.inspect(t)) == 0 })
	entries, _, err := f.service.History.Entries(c.Channel, c.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	terminals := 0
	for _, entry := range entries {
		if entry.Terminal {
			terminals++
		}
	}
	if terminals != 1 {
		t.Fatalf("terminal receipt replay count = %d, want 1", terminals)
	}
	methods, err := os.ReadFile(f.log)
	if err != nil {
		t.Fatal(err)
	}
	starts := strings.Count(string(methods), "turn/start\n")
	t.Logf("ASSERT crash-left original terminal present; original committed cursor=%d; provider admissions after replacement=%d", retained.CommittedCursor, starts)
	if starts != 1 {
		t.Fatalf("completed crash-left journal caused another provider turn: got %d admissions, want 1", starts)
	}
}
