//go:build linux || darwin

package cli

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
	"github.com/agent0ai/spynel/internal/orchestrator"
)

func TestWorkerConversationNativeUnknownProviderSend(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		name := "ordinary"
		if recovery {
			name = "recovery"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("SPYNEL_TEST_WORKER_UNCERTAIN_START", "1")
			f := newWorkerAgentFixture(t)
			message := core.Message{Channel: "cli", Conversation: "uncertain-start", SourceMessageID: "source", Text: "synthetic request"}
			if recovery {
				f.service.Harness = newHeldCLIHarness()
				f.service.SetPrimaryInstanceID(f.owner.ID())
				f.service.SetRecoveryOwnershipFence(func(action func() error) (bool, error) { return f.owner.RunWhileOwner(f.term, action) })
				if _, err := f.service.History.Append(message.Channel, message.Conversation, history.Entry{Role: "user", SourceMessageID: message.SourceMessageID, Content: message.Text, AcceptedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
				if err := f.service.Start(t.Context()); err != nil {
					t.Fatal(err)
				}
				waitWorkerAgent(t, func() bool { return !f.service.RecoveryStatus().ScannedAt.IsZero() })
				if f.service.RecoveryStatus().Dispatched != 0 {
					t.Fatal("uncertain provider Send was counted as confirmed recovery")
				}
			} else if err := f.service.Handle(t.Context(), message, nil); !errors.Is(err, orchestrator.ErrConversationPending) {
				t.Fatal("uncertain provider Send fabricated an ordinary terminal result", err)
			}
			retained := f.inspect(t)
			if len(retained) != 1 || retained[0].Done || retained[0].Error == "" || !retained[0].SourcesReserved {
				t.Fatal("uncertain provider execution was completed or lost")
			}
			execution, identity := retained[0], f.client.Info()
			t.Logf("uncertain provider worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
			job, ok := f.service.Runtime.JobForSession(execution.Key)
			if !ok || job.WorkID != execution.Conversation.ExecutionID || job.ProviderIterations != 1 {
				t.Fatal("uncertain admission lost its original job or counters")
			}
			f.stop(t)
			if err := f.owner.Release(f.term); err != nil {
				t.Fatal(err)
			}
			f.open(t)
			if f.client.Info().Process != identity.Process || f.client.Info().ID != identity.ID {
				t.Fatal("uncertain admission replacement changed its worker")
			}
			f.start(t)
			waitWorkerAgent(t, func() bool {
				restored, ok := f.service.Runtime.JobForSession(execution.Key)
				return ok && restored.StableID == job.StableID && restored.ProviderIterations == 1 && restored.Execution == "degraded"
			})
			// Repeated ordinary scans may attach but never redispatch or cover it.
			for range 2 {
				if err := f.service.Orchestrator.ScanOnce(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			window, err := f.service.History.RecoveryWindow(message.Channel, message.Conversation, time.Time{}, 20)
			if err != nil || !window.Admitted[message.SourceMessageID] || window.Covered[message.SourceMessageID] != "" && !(recovery && window.Covered[message.SourceMessageID] == "prior_reservation") {
				t.Fatal("uncertain provider Send lost source intent or fabricated coverage", err)
			}
			entries, _, err := f.service.History.RecoveryEntries(message.Channel, message.Conversation, 20)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Terminal || entry.Role == "error" {
					t.Fatal("uncertain Send created a fabricated history result")
				}
			}
			methods, err := os.ReadFile(f.log)
			if err != nil || strings.Count(string(methods), "turn/start\n") != 1 || strings.Count(string(methods), "start-effect\n") != 1 {
				t.Fatal("uncertain provider work was repeated", err)
			}
			if _, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "ack", Sequence: execution.Sequence, After: execution.LastCursor}); err == nil {
				t.Fatal("uncertain execution retired without a provider terminal")
			}
			t.Log("one applied provider effect; uncertainty and exact source intent survive controller replacement without terminal coverage or redispatch")
		})
	}
}
