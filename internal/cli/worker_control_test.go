//go:build linux || darwin

package cli

import (
	"context"
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

func TestWorkerWorkflowInterruptTimeoutStaysCancelledAcrossReplacement(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release-interrupt")
	t.Setenv("SPYNEL_TEST_WORKER_DELAY_INTERRUPT", release)
	f := newWorkerAgentFixture(t)
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
	if _, err := orchestrator.Create(f.cfg, "tasks", "synthetic delayed cancellation", ""); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	waitWorkerAgent(t, func() bool { return len(f.inspect(t)) == 1 && len(f.service.Runtime.Jobs()) == 1 })
	execution, job, identity := f.inspect(t)[0], f.service.Runtime.Jobs()[0], f.client.Info()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	started := time.Now()
	var response string
	err := f.service.Handle(ctx, core.Message{Channel: "cli", Conversation: "synthetic-operator", Text: fmt.Sprintf("/job kill %d", job.Number)}, func(e core.Event) { response += e.Text })
	if err != nil || !strings.Contains(response, context.DeadlineExceeded.Error()) || strings.Contains(response, "Kill requested") || time.Since(started) > 7*time.Second {
		t.Fatalf("uncertain job kill was not bounded and honest: response=%q err=%v elapsed=%s", response, err, time.Since(started))
	}
	retained := f.inspect(t)[0]
	if retained.Done || !retained.Cancelled || retained.Sequence != execution.Sequence {
		t.Fatal("unacknowledged cancellation released its live execution")
	}
	f.stop(t)
	if err := f.owner.Release(f.term); err != nil {
		t.Fatal(err)
	}
	f.open(t)
	if f.client.Info().Process != identity.Process || updater.VerifyProcess(identity.Process) != nil {
		t.Fatal("replacement changed worker identity")
	}
	f.start(t)
	waitWorkerAgent(t, func() bool { return len(f.service.Runtime.Jobs()) == 1 })
	current := f.service.Runtime.Jobs()[0]
	if current.Number != job.Number || current.Generation != job.Generation || current.StableID != job.StableID || current.ProviderIterations != 1 {
		t.Fatal("replacement lost original job identity/counters")
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitWorkerAgent(t, func() bool { return len(f.inspect(t)) == 0 && len(f.service.Runtime.Jobs()) == 0 })
	for scan := 0; scan < 2; scan++ {
		if err := f.service.Orchestrator.ScanOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	f.service.Orchestrator.Wait()
	lease, ok := f.service.Orchestrator.LeaseForSession(execution.Key)
	doc, err := orchestrator.ReadDocument(execution.Workflow.File)
	methods, readErr := os.ReadFile(f.log)
	if !ok || lease.State != "cancelled" || err != nil || doc.FrontMatter["provider_iterations"] != 1 ||
		readErr != nil || strings.Count(string(methods), "turn/start\n") != 1 || strings.Count(string(methods), "turn/interrupt\n") != 1 || len(f.inspect(t)) != 0 {
		t.Fatal("late interruption recovered cancelled work or repeated provider admission", lease.State, err, readErr)
	}
	t.Logf("job kill timeout and late terminal preserved same job/counters and cancelled lease across replacement; worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
}

func TestWorkerWorkflowInterruptTransportFailureStaysCancelledAcrossReplacement(t *testing.T) {
	t.Setenv("SPYNEL_TEST_WORKER_DISCONNECT_INTERRUPT", "1")
	f := newWorkerAgentFixture(t)
	if _, err := orchestrator.Create(f.cfg, "tasks", "synthetic disconnected cancellation", ""); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	waitWorkerAgent(t, func() bool { return len(f.inspect(t)) == 1 && len(f.service.Runtime.Jobs()) == 1 })
	execution, job, identity := f.inspect(t)[0], f.service.Runtime.Jobs()[0], f.client.Info()
	// Detach consumption so the terminal remains available for successor attachment.
	f.cancel()
	<-f.done
	f.cancel = nil
	f.service.Orchestrator.Wait()
	started := time.Now()
	var response string
	err := f.service.Handle(t.Context(), core.Message{Channel: "cli", Conversation: "synthetic-operator", Text: fmt.Sprintf("/job kill %d", job.Number)}, func(e core.Event) { response += e.Text })
	if err != nil || !strings.Contains(response, "Cannot kill job") || strings.Contains(response, "Kill requested") || time.Since(started) >= 5*time.Second {
		t.Fatalf("disconnected job kill was not an honest pre-deadline error: response=%q err=%v", response, err)
	}
	retained := f.inspect(t)[0]
	if !retained.Done || !retained.Cancelled || retained.Sequence != execution.Sequence {
		t.Fatal("disconnected cancellation lost its terminal intent")
	}
	f.stop(t)
	if err := f.owner.Release(f.term); err != nil {
		t.Fatal(err)
	}
	f.open(t)
	if f.client.Info().Process != identity.Process || updater.VerifyProcess(identity.Process) != nil {
		t.Fatal("replacement changed worker identity")
	}
	f.start(t)
	waitWorkerAgent(t, func() bool { return len(f.inspect(t)) == 0 && len(f.service.Runtime.Jobs()) == 0 })
	for scan := 0; scan < 2; scan++ {
		if err := f.service.Orchestrator.ScanOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	f.service.Orchestrator.Wait()
	lease, ok := f.service.Orchestrator.LeaseForSession(execution.Key)
	doc, err := orchestrator.ReadDocument(execution.Workflow.File)
	archived, _, archiveErr := f.service.Runtime.ArchivedJob(job.Number)
	methods, readErr := os.ReadFile(f.log)
	if !ok || lease.State != "cancelled" || err != nil || doc.FrontMatter["provider_iterations"] != 1 ||
		archiveErr != nil || archived.ID != job.StableID || archived.Number != job.Number || archived.Generation != job.Generation ||
		readErr != nil || strings.Count(string(methods), "turn/start\n") != 1 || strings.Count(string(methods), "turn/interrupt\n") != 1 {
		t.Fatal("transport failure recovered cancelled work or changed its job/counters", lease.State, err, archiveErr, readErr)
	}
	t.Logf("disconnected job kill preserved same job/counters and cancelled lease across replacement; worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
}

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
