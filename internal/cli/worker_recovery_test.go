//go:build linux || darwin

package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
)

func TestWorkerConversationRecoveryScanner(t *testing.T) {
	for _, scenario := range []string{"normal", "lost-send", "absent-send", "late-coverage"} {
		t.Run(scenario, func(t *testing.T) {
			f := newWorkerAgentFixture(t)
			local := newHeldCLIHarness()
			f.service.Harness = local
			f.service.SetPrimaryInstanceID(f.owner.ID())
			f.service.SetRecoveryOwnershipFence(func(action func() error) (bool, error) {
				return f.owner.RunWhileOwner(f.term, action)
			})
			for _, source := range []string{"first", "second"} {
				if _, err := f.service.History.Append("cli", "recovered", history.Entry{Role: "user", SourceMessageID: source, Content: "synthetic request", AcceptedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
			}
			restore := func() {}
			if scenario == "lost-send" || scenario == "absent-send" {
				f.client.Close()
				restore = dropWorkerReply(t, f.client.Info().Socket, "send", scenario == "lost-send", false)
			}
			if scenario == "late-coverage" {
				start := f.service.Orchestrator.ConversationStarted
				f.service.Orchestrator.ConversationStarted = func(c harness.WorkerConversation) (int, error) {
					if _, err := f.service.History.Append("cli", "recovered", history.Entry{Role: "correlation", Covers: []string{"second"}, Outcome: "terminal_assistant"}); err != nil {
						return 0, err
					}
					return start(c)
				}
			}
			if err := f.service.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			waitWorkerAgent(t, func() bool { return !f.service.RecoveryStatus().ScannedAt.IsZero() })
			restore()
			f.client.Close()
			retained := f.inspect(t)
			if scenario == "absent-send" || scenario == "late-coverage" {
				window, err := f.service.History.RecoveryWindow("cli", "recovered", time.Time{}, 20)
				methods, readErr := os.ReadFile(f.log)
				if err != nil || readErr != nil || window.Covered["first"] != "" || window.Admitted["first"] || strings.Contains(string(methods), "turn/start\n") {
					t.Fatal("failed recovery partially reserved or dispatched its batch", err, readErr)
				}
				f.start(t)
				waitWorkerAgent(t, func() bool { return len(f.service.Runtime.Jobs()) == 0 })
				return
			}
			if scenario == "lost-send" && f.service.RecoveryStatus().Dispatched != 0 {
				t.Fatal("lost Send reply was counted as confirmed admission")
			}
			retained = f.inspect(t)
			if len(retained) != 1 || retained[0].Conversation == nil || !retained[0].Conversation.Recovery || !retained[0].SourcesReserved {
				t.Fatal("ordinary recovery scanner did not admit the surviving worker")
			}
			local.mu.Lock()
			localSends := len(local.prompts)
			local.mu.Unlock()
			if localSends != 0 {
				t.Fatal("worker recovery also dispatched the controller harness")
			}
			execution := retained[0]
			identity := f.client.Info()
			job, ok := f.service.Runtime.JobForSession(execution.Key)
			if !ok || job.WorkID != execution.Conversation.ExecutionID || job.ProviderIterations != 1 {
				t.Fatal("recovery lost its admitted job or counters")
			}
			f.stop(t)
			if err := f.owner.Release(f.term); err != nil {
				t.Fatal(err)
			}
			f.open(t)
			if f.client.Info().Process != identity.Process || f.client.Info().ID != identity.ID {
				t.Fatal("recovery controller replacement changed the worker")
			}
			t.Logf("recovery worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
			f.start(t)
			waitWorkerAgent(t, func() bool {
				restored, ok := f.service.Runtime.JobForSession(execution.Key)
				return ok && restored.StableID == job.StableID && restored.ProviderIterations == 1
			})
			if _, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "interrupt", Key: execution.Key}); err != nil {
				t.Fatal(err)
			}
			waitWorkerAgent(t, func() bool { return len(f.inspect(t)) == 0 && len(f.service.Runtime.Jobs()) == 0 })
			window, err := f.service.History.RecoveryWindow("cli", "recovered", time.Time{}, 20)
			methods, readErr := os.ReadFile(f.log)
			if err != nil || readErr != nil || window.Covered["first"] != "terminal_coverage" || window.Covered["second"] != "terminal_coverage" || strings.Count(string(methods), "turn/start\n") != 1 || !strings.Contains(string(methods), "Provider admission may have changed since message selection. Reconcile first") {
				t.Fatal("scanner recovery did not settle the exact batch once", err, readErr)
			}

		})
	}
}
