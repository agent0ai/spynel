//go:build linux || darwin

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/orchestrator"
	"github.com/agent0ai/spynel/internal/updater"
)

func TestWorkerWorkflowControlsAcrossManagerReplacement(t *testing.T) {
	for _, scenario := range []string{"successor-control", "obsolete-continuation", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			completion := filepath.Join(t.TempDir(), "complete")
			t.Setenv("SPYNEL_TEST_WORKER_COMPLETIONS", completion)
			f := newWorkerAgentFixture(t)
			if _, err := orchestrator.Create(f.cfg, "tasks", "synthetic retained control", ""); err != nil {
				t.Fatal(err)
			}
			f.start(t)
			call := func(request harness.WorkerRequest) harness.WorkerReply {
				t.Helper()
				reply, err := f.client.Call(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				return reply
			}
			waitWorkerAgent(t, func() bool {
				return len(call(harness.WorkerRequest{Action: "inspect"}).Executions) == 1 && len(f.service.Runtime.Jobs()) == 1
			})
			execution := call(harness.WorkerRequest{Action: "inspect"}).Executions[0]
			firstJob := f.service.Runtime.Jobs()[0]
			identity := f.client.Info()
			command := func(text string) string {
				t.Helper()
				var response string
				if err := f.service.Handle(t.Context(), core.Message{Channel: "cli", Conversation: "synthetic-operator", Text: text}, func(event core.Event) { response += event.Text }); err != nil {
					t.Fatal(err)
				}
				return response
			}
			iterations := func() int {
				t.Helper()
				doc, err := orchestrator.ReadDocument(execution.Workflow.File)
				if err != nil {
					t.Fatal(err)
				}
				return doc.FrontMatter["provider_iterations"].(int)
			}
			if scenario == "cancelled" {
				if reply := command(fmt.Sprintf("/job kill %d", firstJob.Number)); !strings.Contains(reply, "Kill requested") {
					t.Fatal(reply)
				}
				waitWorkerAgent(t, func() bool { return len(call(harness.WorkerRequest{Action: "inspect"}).Executions) == 0 })
			} else {
				ping := fmt.Sprintf("/job ping %d", firstJob.Number)
				if reply := command(ping); !strings.Contains(reply, "Delivered") {
					t.Fatal(reply)
				}
				if reply := command(ping); !strings.Contains(reply, "already accepted") || iterations() != 2 {
					t.Fatal("duplicate control was not retained", reply, iterations())
				}
			}
			obsolete := f.client
			f.stop(t)
			if err := f.owner.Release(f.term); err != nil {
				t.Fatal(err)
			}
			f.open(t)
			if scenario != "obsolete-continuation" {
				f.start(t)
			}
			defer obsolete.Close()
			if f.client.Info().PID != identity.PID || f.client.Info().Process.StartTime != identity.Process.StartTime || f.client.Info().Process.Generation != identity.Process.Generation || updater.VerifyProcess(identity.Process) != nil {
				t.Fatal("controller replacement changed the native worker")
			}
			if _, err := obsolete.Call(t.Context(), harness.WorkerRequest{Action: "interrupt", Key: execution.Key}); err == nil {
				t.Fatal("obsolete controller could cancel")
			}
			if scenario == "cancelled" {
				for scan := 0; scan < 2; scan++ {
					if err := f.service.Orchestrator.ScanOnce(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				settled := make(chan struct{})
				go func() { f.service.Orchestrator.Wait(); close(settled) }()
				select {
				case <-settled:
				case <-time.After(time.Second):
					t.Fatal("cancelled execution started replacement work")
				}
				lease, ok := f.service.Orchestrator.LeaseForSession(execution.Key)
				if !ok || lease.State != "cancelled" || len(call(harness.WorkerRequest{Action: "inspect"}).Executions) != 0 || len(f.service.Runtime.Jobs()) != 0 || iterations() != 1 {
					t.Fatal("accepted cancellation recovered or lost its durable fence")
				}
			} else {
				if scenario == "obsolete-continuation" {
					// Complete before successor lease attachment; the old durable
					// claim still matches, so only the election fence can refuse.
					if err := os.WriteFile(completion+".1", nil, 0600); err != nil {
						t.Fatal(err)
					}
					waitWorkerAgent(t, func() bool { return call(harness.WorkerRequest{Action: "inspect"}).Executions[0].Done })
					f.start(t)
				}
				waitWorkerAgent(t, func() bool { return len(f.service.Runtime.Jobs()) == 1 })
				job := f.service.Runtime.Jobs()[0]
				if job.Number != firstJob.Number || job.Generation != firstJob.Generation || job.ProviderIterations != 2 {
					t.Fatal("control attachment lost job identity/counters")
				}
				if scenario == "successor-control" {
					if reply := command(fmt.Sprintf("/job ping %d", job.Number)); !strings.Contains(reply, "already accepted") {
						t.Fatal("replacement lost control deduplication", reply)
					}
					if reply := command(fmt.Sprintf("/job message %d synthetic successor guidance", job.Number)); !strings.Contains(reply, "Delivered") {
						t.Fatal(reply)
					}
				}
				if err := os.WriteFile(completion+".1", nil, 0600); err != nil {
					t.Fatal(err)
				}
				if scenario == "successor-control" {
					waitWorkerAgent(t, func() bool { return iterations() == 4 })
					if err := os.WriteFile(completion+".2", nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
				waitWorkerAgent(t, func() bool { return len(call(harness.WorkerRequest{Action: "inspect"}).Executions) == 0 })
				want := 2
				if scenario == "successor-control" {
					want = 4
				}
				if iterations() != want {
					t.Fatal("continuation reserved again or crossed its obsolete term", iterations())
				}
			}
			methods, err := os.ReadFile(f.log)
			wantStarts := 1
			if scenario == "successor-control" {
				wantStarts = 2
			}
			if err != nil || strings.Count(string(methods), "turn/start\n") != wantStarts {
				t.Fatal("unexpected provider dispatch count", err)
			}
			t.Logf("same native worker PID=%d start=%s generation=%s; control/cancellation scenario=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation, scenario)
		})
	}
}
