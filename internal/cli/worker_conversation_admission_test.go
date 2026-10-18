//go:build linux || darwin

package cli

import (
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
)

func TestWorkerConversationManagerAdmission(t *testing.T) {
	for _, scenario := range []string{"normal", "concurrent", "lost-send", "absent-send", "source-refused", "job-failed", "provider-refused", "obsolete-owner"} {
		t.Run(scenario, func(t *testing.T) {
			if scenario == "provider-refused" {
				t.Setenv("SPYNEL_TEST_WORKER_REJECT_START", "1")
			}
			f := newWorkerAgentFixture(t)
			c := harness.WorkerConversation{Channel: "cli", Conversation: "admission", SourceMessageID: "source", ExecutionID: "execution", Sources: []string{"source"}}
			if scenario != "source-refused" {
				if _, err := f.service.History.Append(c.Channel, c.Conversation, history.Entry{Role: "user", SourceMessageID: "source", Content: "synthetic", AcceptedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
			}
			manager := f.service.Orchestrator
			var admissions atomic.Int32
			start := manager.ConversationStarted
			manager.ConversationStarted = func(c harness.WorkerConversation) (int, error) {
				admissions.Add(1)
				window, err := f.service.History.RecoveryWindow(c.Channel, c.Conversation, time.Time{}, 20)
				if err != nil || window.Admitted["source"] {
					t.Error("source reservation preceded durable job admission", err)
				}
				return start(c)
			}
			if scenario == "job-failed" {
				if err := os.Remove(f.cfg.StatePath("jobs")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f.cfg.StatePath("jobs"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "obsolete-owner" {
				f.stop(t)
				if err := f.owner.Release(f.term); err != nil {
					t.Fatal(err)
				}
				f.open(t)
			}
			restore := func() {}
			if scenario == "lost-send" || scenario == "absent-send" {
				f.client.Close()
				restore = dropWorkerReply(t, f.client.Info().Socket, "send", scenario == "lost-send", false)
			}
			execution, err := manager.AdmitConversation(t.Context(), c, "synthetic complete prompt", "synthetic user message")
			restore()
			f.client.Close()
			wantError := scenario == "lost-send" || scenario == "absent-send" || scenario == "source-refused" || scenario == "job-failed" || scenario == "obsolete-owner"
			if (err != nil) != wantError {
				t.Fatalf("admission error=%v, want error=%v", err, wantError)
			}
			if scenario == "job-failed" || scenario == "obsolete-owner" {
				if len(f.inspect(t)) != 0 || len(f.service.Runtime.Jobs()) != 0 || scenario == "obsolete-owner" && admissions.Load() != 0 {
					t.Fatal("refused application/owner admission dispatched work")
				}
				return
			}
			if scenario == "absent-send" || scenario == "source-refused" {
				f.start(t)
				waitWorkerAgent(t, func() bool { return len(f.service.Runtime.Jobs()) == 0 })
				retained := f.inspect(t)
				if scenario == "source-refused" && (len(retained) != 1 || !retained[0].Done || retained[0].SourcesReserved || retained[0].LastCursor != 0) || scenario == "absent-send" && len(retained) != 0 {
					t.Fatal("reconciliation lost its non-dispatched boundary")
				}
				data, err := os.ReadFile(f.log)
				if err != nil || strings.Contains(string(data), "turn/start\n") {
					t.Fatal("absent/refused Send reached the provider", err)
				}
				return
			}
			identity := f.client.Info()
			t.Logf("manager admission worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
			job, ok := f.service.Runtime.JobForSession(execution.Key)
			if !ok || job.WorkID != c.ExecutionID || job.ProviderIterations != 1 {
				t.Fatal("admission lost its durable job or counters")
			}
			if scenario == "concurrent" {
				var callers sync.WaitGroup
				for range 8 {
					callers.Go(func() {
						if _, err := manager.AdmitConversation(t.Context(), c, "changed prompt", "later message"); err == nil {
							t.Error("retained admission accepted another conversation Send")
						}
					})
				}
				callers.Wait()
				if admissions.Load() != 1 {
					t.Fatal("duplicate requests reached job admission")
				}
			}
			f.stop(t)
			if err := f.owner.Release(f.term); err != nil {
				t.Fatal(err)
			}
			f.open(t)
			if f.client.Info().Process != identity.Process || f.client.Info().ID != identity.ID {
				t.Fatal("controller replacement changed its worker")
			}
			f.start(t)
			if scenario != "provider-refused" {
				waitWorkerAgent(t, func() bool {
					restored, ok := f.service.Runtime.JobForSession(execution.Key)
					return ok && restored.StableID == job.StableID && restored.Number == job.Number && restored.Generation == job.Generation && restored.ProviderIterations == 1
				})
				if _, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "interrupt", Key: execution.Key}); err != nil {
					t.Fatal(err)
				}
			}
			waitWorkerAgent(t, func() bool { return len(f.inspect(t)) == 0 && len(f.service.Runtime.Jobs()) == 0 })
			window, err := f.service.History.RecoveryWindow(c.Channel, c.Conversation, time.Time{}, 20)
			data, readErr := os.ReadFile(f.log)
			if err != nil || readErr != nil || window.Covered["source"] != "terminal_coverage" || strings.Count(string(data), "turn/start\n") != 1 {
				t.Fatal("controller replacement lost coverage or redispatched", err, readErr)
			}
		})
	}
}
