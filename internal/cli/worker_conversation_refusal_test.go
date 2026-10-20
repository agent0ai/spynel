//go:build linux || darwin

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
)

func TestWorkerConversationRefusalRetirement(t *testing.T) {
	for _, scenario := range []string{"normal", "replacement", "archive-write", "archive-failed", "lost-commit", "lost-ack", "absent-ack"} {
		t.Run(scenario, func(t *testing.T) {
			f := newWorkerAgentFixture(t)
			c := harness.WorkerConversation{Channel: "cli", Conversation: "refused", SourceMessageID: "first", ExecutionID: "refused-execution", Sources: []string{"first", "second"}, Recovery: true}
			for _, source := range c.Sources {
				if _, err := f.service.History.Append(c.Channel, c.Conversation, history.Entry{Role: "user", SourceMessageID: source, Content: "synthetic request", AcceptedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
			}
			start := f.service.Orchestrator.ConversationStarted
			f.service.Orchestrator.ConversationStarted = func(c harness.WorkerConversation) (int, error) {
				if _, err := f.service.History.Append(c.Channel, c.Conversation, history.Entry{Role: "correlation", Covers: []string{"second"}, Outcome: "terminal_assistant"}); err != nil {
					return 0, err
				}
				return start(c)
			}
			refused, err := f.service.Orchestrator.AdmitConversation(t.Context(), c, "synthetic prompt", "synthetic message")
			if err == nil || !refused.Done || refused.SourcesReserved || refused.Error == "" {
				t.Fatal("late source coverage did not refuse the whole admission", err)
			}
			job, ok := f.service.Runtime.JobForSession(refused.Key)
			if !ok {
				t.Fatal("refusal lost its provisional job")
			}
			identity := f.client.Info()
			if scenario != "normal" {
				obsolete := f.service.Orchestrator
				f.stop(t)
				if err := f.owner.Release(f.term); err != nil {
					t.Fatal(err)
				}
				f.open(t)
				if err := obsolete.ScanOnce(t.Context()); err == nil || len(f.inspect(t)) != 1 {
					t.Fatal("obsolete controller retired the refusal", err)
				}
				if f.client.Info().Process != identity.Process || f.client.Info().ID != identity.ID {
					t.Fatal("replacement changed the refused worker")
				}
			}
			manager := f.service.Orchestrator
			manager.ConversationDelivery = func(context.Context, int, harness.WorkerDelivery) error {
				t.Error("source refusal entered conversation effects")
				return errors.New("source refusal must only enter the job archive")
			}
			restore := func() {}
			if scenario == "lost-commit" || scenario == "lost-ack" || scenario == "absent-ack" {
				action := "ack"
				if scenario == "lost-commit" {
					action = "commit"
				}
				f.client.Close()
				restore = dropWorkerReply(t, f.client.Info().Socket, action, scenario != "absent-ack", false)
			}
			deliver := manager.JobDelivery
			if scenario == "archive-write" || scenario == "archive-failed" {
				manager.JobDelivery = func(id int, d harness.WorkerDelivery) error {
					if scenario == "archive-write" {
						path := filepath.Join(f.cfg.StatePath("jobs"), job.StableID+".md")
						if err := os.Rename(path, path+".saved"); err != nil {
							return err
						}
						if err := os.Mkdir(path, 0700); err != nil {
							t.Error(err)
						}
						err := deliver(id, d)
						if removeErr := os.Remove(path); removeErr != nil {
							t.Error(removeErr)
						}
						if restoreErr := os.Rename(path+".saved", path); restoreErr != nil {
							t.Error(restoreErr)
						}
						return err
					}
					if err := deliver(id, d); err != nil {
						return err
					}
					return errors.New("synthetic lost archive acknowledgement")
				}
			}
			if err := manager.ScanOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			manager.Wait()
			restore()
			f.client.Close()
			if scenario == "archive-write" || scenario == "archive-failed" {
				retained := f.inspect(t)
				if len(retained) != 1 || retained[0].CommittedCursor != 0 {
					t.Fatal("failed archive acknowledgement consumed the refusal")
				}
				manager.JobDelivery = deliver
			}
			if err := manager.ScanOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			manager.Wait()
			if len(f.inspect(t)) != 0 || len(f.service.Runtime.Jobs()) != 0 {
				t.Fatal("non-dispatched conversation refusal remained retained")
			}
			archived, output, err := f.service.Runtime.ArchivedJob(job.StableID)
			if err != nil || archived.Number != job.Number || archived.Generation != job.Generation || strings.Count(output, "Conversation source reservation refused") != 1 {
				t.Fatal("refusal did not preserve its original archive once", err)
			}
			window, err := f.service.History.RecoveryWindow(c.Channel, c.Conversation, time.Time{}, 20)
			if err != nil || window.Covered["first"] != "" || window.Admitted["first"] {
				t.Fatal("refusal fabricated source admission or terminal coverage", err)
			}
			entries, _, err := f.service.History.Entries(c.Channel, c.Conversation)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Role == "assistant" || entry.Role == "error" {
					t.Fatal("source refusal fabricated a provider response")
				}
			}
			// Ordinary startup recovery, rather than a retry of the rejected Send,
			// must now select and settle only the still-uncovered source.
			f.stop(t)
			if err := f.owner.Release(f.term); err != nil {
				t.Fatal(err)
			}
			f.open(t)
			f.service.Harness = newHeldCLIHarness()
			f.service.SetPrimaryInstanceID(f.owner.ID())
			f.service.SetRecoveryOwnershipFence(func(action func() error) (bool, error) { return f.owner.RunWhileOwner(f.term, action) })
			if err := f.service.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			waitWorkerAgent(t, func() bool { return !f.service.RecoveryStatus().ScannedAt.IsZero() })
			retained := f.inspect(t)
			if len(retained) != 1 || !retained[0].SourcesReserved || retained[0].Sequence <= refused.Sequence || strings.Join(retained[0].Conversation.Sources, ",") != "first" || f.client.Info().Process != identity.Process {
				t.Fatal("ordinary recovery did not progress the later eligible source on the same worker")
			}
			f.start(t)
			if _, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "interrupt", Key: refused.Key}); err != nil {
				t.Fatal(err)
			}
			waitWorkerAgent(t, func() bool { return len(f.inspect(t)) == 0 && len(f.service.Runtime.Jobs()) == 0 })
			window, err = f.service.History.RecoveryWindow(c.Channel, c.Conversation, time.Time{}, 20)
			methods, readErr := os.ReadFile(f.log)
			if err != nil || readErr != nil || window.Covered["first"] != "terminal_coverage" || strings.Count(string(methods), "turn/start\n") != 1 {
				t.Fatal("later eligible source did not settle exactly once", err, readErr)
			}
			t.Logf("refusal worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
		})
	}
}
