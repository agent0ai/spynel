//go:build linux || darwin

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/orchestrator"
	"github.com/agent0ai/spynel/internal/updater"
)

func TestWorkerCrashRecoveryPreservesCancellation(t *testing.T) {
	for _, scenario := range []string{"recover", "cancelled", "unacknowledged-cancellation"} {
		t.Run(scenario, func(t *testing.T) {
			if scenario == "unacknowledged-cancellation" {
				release := filepath.Join(t.TempDir(), "release-interrupt")
				t.Setenv("SPYNEL_TEST_WORKER_DELAY_INTERRUPT", release)
			}
			t.Setenv("SPYNEL_TEST_WORKER_CONVERSATION_ERROR", "1")
			f := newWorkerAgentFixture(t)
			if _, err := orchestrator.Create(f.cfg, "tasks", "synthetic worker crash", ""); err != nil {
				t.Fatal(err)
			}
			f.start(t)
			waitWorkerAgent(t, func() bool {
				return len(f.inspect(t)) == 1 && len(f.service.Runtime.Jobs()) == 1 && f.inspect(t)[0].LastCursor >= 2
			})
			execution, job, identity := f.inspect(t)[0], f.service.Runtime.Jobs()[0], f.client.Info()
			t.Logf("admitted worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
			// Keep the terminal unconsumed: cancellation must already be durable
			// when the ordinary operator command returns, before worker death.
			f.cancel()
			<-f.done
			f.cancel = nil
			f.service.Orchestrator.Wait()
			if scenario != "recover" {
				var response string
				err := f.service.Handle(t.Context(), core.Message{Channel: "cli", Conversation: "synthetic-operator", Text: fmt.Sprintf("/job kill %d", job.Number)}, func(e core.Event) { response += e.Text })
				if err != nil || scenario == "cancelled" && !strings.Contains(response, "Kill requested") || scenario != "cancelled" && !strings.Contains(response, context.DeadlineExceeded.Error()) {
					t.Fatalf("unexpected cancellation response %q: %v", response, err)
				}
				if retained := f.inspect(t)[0]; !retained.Cancelled || scenario == "cancelled" && (!retained.Done || retained.CommittedCursor == retained.LastCursor) || scenario == "unacknowledged-cancellation" && retained.Done {
					t.Fatal("cancellation fixture did not retain unsettled intent/events")
				}
				if lease, ok := f.service.Orchestrator.LeaseForSession(execution.Key); !ok || !lease.CancellationRequested {
					t.Fatal("interruption returned without durable workflow cancellation intent")
				}
			}
			f.stop(t)
			guardian := workerCrashGuardian(t)
			t.Logf("crash worker pid=%d start=%s generation=%s; guardian pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation, guardian.PID, guardian.StartTime, guardian.Generation)
			process, err := os.FindProcess(identity.PID)
			if err != nil {
				t.Fatal(err)
			}
			defer process.Release()
			if err := updater.VerifyProcess(identity.Process); err != nil {
				t.Fatal(err)
			}
			if err := process.Kill(); err != nil {
				t.Fatal(err)
			}
			waitWorkerAgent(t, func() bool {
				return errors.Is(updater.VerifyProcess(identity.Process), syscall.ESRCH) && errors.Is(updater.VerifyProcess(guardian), syscall.ESRCH)
			})
			// SIGKILL cannot execute worker-owned endpoint cleanup. This exact
			// disposable directory is removed only after both owners are dead.
			t.Cleanup(func() {
				if err := os.RemoveAll(filepath.Dir(identity.Socket)); err != nil {
					t.Error(err)
				}
			})
			if err := f.owner.Release(f.term); err != nil {
				t.Fatal(err)
			}
			f.client.Close()
			f.open(t)
			if f.client.Info().ID == identity.ID || f.client.Info().Process == identity.Process {
				t.Fatal("dead worker was adopted as a live execution")
			}
			t.Logf("replacement worker pid=%d start=%s generation=%s", f.client.Info().PID, f.client.Info().Process.StartTime, f.client.Info().Process.Generation)
			if scenario == "recover" {
				f.start(t)
				waitWorkerAgent(t, func() bool { return len(f.inspect(t)) == 1 && len(f.service.Runtime.Jobs()) == 1 })
				current, restored := f.inspect(t)[0], f.service.Runtime.Jobs()[0]
				if current.Workflow.LeaseID != execution.Workflow.LeaseID || restored.StableID != job.StableID || restored.Generation != job.Generation || restored.Number != job.Number || current.Reservation.ProviderIterations != 2 || restored.ProviderIterations != 2 {
					t.Fatal("dead-worker recovery lost its claim/job or reserved more than one replacement")
				}
				if _, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "interrupt", Key: current.Key}); err != nil {
					t.Fatal(err)
				}
				waitWorkerAgent(t, func() bool { return len(f.inspect(t)) == 0 && len(f.service.Runtime.Jobs()) == 0 })
			}
			ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
			defer cancel()
			for range 3 {
				if err := f.service.Orchestrator.ScanOnce(ctx); err != nil {
					t.Fatal(err)
				}
			}
			<-ctx.Done()
			f.service.Orchestrator.Wait()
			lease, ok := f.service.Orchestrator.LeaseForSession(execution.Key)
			document, err := orchestrator.ReadDocument(execution.Workflow.File)
			methods, readErr := os.ReadFile(f.log)
			want := 1
			if scenario == "recover" {
				want = 2
			}
			if !ok || lease.State != "cancelled" || err != nil || readErr != nil || document.FrontMatter["provider_iterations"] != want || strings.Count(string(methods), "turn/start\n") != want || len(f.inspect(t)) != 0 {
				t.Fatalf("crash recovery repeated paused work: state=%s iterations=%v starts=%d retained=%d err=%v read=%v", lease.State, document.FrontMatter["provider_iterations"], strings.Count(string(methods), "turn/start\n"), len(f.inspect(t)), err, readErr)
			}
		})
	}
}

func workerCrashGuardian(t *testing.T) updater.ProcessRegistration {
	t.Helper()
	configDirectory, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(configDirectory, "spynel", "processes")
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	var guardian updater.ProcessRegistration
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		var record updater.ProcessRegistration
		if err != nil || json.Unmarshal(data, &record) != nil {
			t.Fatal("unreadable isolated fixture registry", err)
		}
		if record.ProviderGuardian && updater.VerifyProcess(record) == nil {
			if guardian.PID != 0 {
				t.Fatal("isolated fixture has multiple guardians")
			}
			guardian = record
		}
	}
	if guardian.PID <= 1 || guardian.StartTime == "" {
		t.Fatal("isolated fixture has no verified live guardian")
	}
	return guardian
}

func TestWorkerCancellationPersistenceBeforeInterruption(t *testing.T) {
	for _, scenario := range []string{"inactive", "lease-unavailable", "moved-document"} {
		t.Run(scenario, func(t *testing.T) {
			completion := filepath.Join(t.TempDir(), "complete")
			t.Setenv("SPYNEL_TEST_WORKER_COMPLETIONS", completion)
			f := newWorkerAgentFixture(t)
			if _, err := orchestrator.Create(f.cfg, "tasks", "synthetic cancellation persistence", ""); err != nil {
				t.Fatal(err)
			}
			f.start(t)
			waitWorkerAgent(t, func() bool { return len(f.inspect(t)) == 1 && len(f.service.Runtime.Jobs()) == 1 })
			execution := f.inspect(t)[0]
			f.cancel()
			<-f.done
			f.cancel = nil
			f.service.Orchestrator.Wait()
			if scenario == "inactive" {
				if err := os.WriteFile(completion+".1", nil, 0600); err != nil {
					t.Fatal(err)
				}
				waitWorkerAgent(t, func() bool { return f.inspect(t)[0].Done })
			} else if scenario == "lease-unavailable" {
				path := f.cfg.StatePath("runtime", "leases", execution.Workflow.LeaseID+".json")
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Rename(path+".saved", path) }()
			} else {
				if err := os.Rename(execution.Workflow.File, execution.Workflow.File+".moved"); err != nil {
					t.Fatal(err)
				}
			}
			interrupted, err := f.service.Orchestrator.Interrupt(t.Context(), execution.Key)
			if interrupted != (scenario == "moved-document") || (err != nil) != (scenario == "lease-unavailable") {
				t.Fatal("incorrect interruption result", interrupted, err)
			}
			if f.inspect(t)[0].Cancelled != (scenario == "moved-document") {
				t.Fatal("refused interruption changed worker intent")
			}
			if scenario == "inactive" {
				lease, ok := f.service.Orchestrator.LeaseForSession(execution.Key)
				if !ok || lease.CancellationRequested {
					t.Fatal("clean inactive result retained a new cancellation pause")
				}
			} else if scenario == "lease-unavailable" {
				data, err := os.ReadFile(f.log)
				if err != nil || strings.Contains(string(data), "turn/interrupt\n") {
					t.Fatal("failed durable reservation still reached the provider", err)
				}
			} else if lease, ok := f.service.Orchestrator.LeaseForSession(execution.Key); !ok || !lease.CancellationRequested {
				t.Fatal("moved document lost cancellation of its still-retained execution")
			}
		})
	}
}
