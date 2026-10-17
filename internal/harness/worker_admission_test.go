//go:build linux || darwin

package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
)

type workerStorageHarness struct {
	*supervisorHarness
	afterSend func()
}

func (h *workerStorageHarness) SendWithModel(ctx context.Context, key, prompt, model string, emit core.Emit) (string, bool, error) {
	thread, steered, err := h.supervisorHarness.SendWithModel(ctx, key, prompt, model, emit)
	if h.afterSend != nil {
		h.afterSend()
	}
	return thread, steered, err
}

func TestWorkerWorkflowAdmissionSurvivesControllerTerms(t *testing.T) {
	for _, scenario := range []struct{ name, mode string }{
		{"codex", "codex-worker"}, {"claude-code", "claude-interrupt"}, {"pi", "pi-interrupt"}, {"acp", "acp-interrupt"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newWorkerFixture(t, scenario.name, scenario.mode)
			work := &WorkerWorkflow{WorkID: "synthetic", File: "private", ClaimID: "claim", OwnerID: "owner", Route: "tasks", Phase: "task_implementation", ClaimAttempt: 2}
			send := WorkerRequest{Action: "send", Sequence: 1, Key: "synthetic/workflow", Prompt: "synthetic workflow", Workflow: work}
			first := fixture.mustCall(t, send).Executions[0]
			if first.Error != "" || first.Done || !reflect.DeepEqual(first.Workflow, work) || first.Reservation == nil || first.Reservation.ProviderIterations != 7 {
				t.Fatalf("workflow not admitted with its reservation: %+v", first)
			}
			for term := 0; term < 2; term++ {
				fixture.replaceOwner(t)
				retained := fixture.mustCall(t, WorkerRequest{Action: "inspect"}).Executions[0]
				if !reflect.DeepEqual(retained.Workflow, first.Workflow) || !reflect.DeepEqual(retained.Reservation, first.Reservation) || retained.ThreadID != first.ThreadID {
					t.Fatal("adoption lost the original workflow identity/reservation")
				}
				fixture.mustCall(t, send)
			}
			changed := *work
			changed.ClaimAttempt++
			send.Workflow = &changed
			if reply, _ := fixture.call(send); reply.Error == "" {
				t.Fatal("changed claim retried an existing admission")
			}
			data, err := os.ReadFile(filepath.Join(fixture.config.Cwd, "fixture-reservations"))
			if err != nil || string(data) != send.Key+"\n" {
				t.Fatalf("reattachment reserved again: %q, %v", data, err)
			}
			path := filepath.Join(filepath.Dir(fixture.info.Socket), "1.jsonl.admission")
			data, err = os.ReadFile(path)
			var checkpoint struct{ Execution WorkerExecution }
			if err != nil || json.Unmarshal(data, &checkpoint) != nil || !reflect.DeepEqual(checkpoint.Execution.Workflow, work) || !reflect.DeepEqual(checkpoint.Execution.Reservation, first.Reservation) {
				t.Fatal("synced admission did not retain its descriptor/reservation")
			}
			if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("workflow checkpoint is not private")
			}
			fixture.mustCall(t, WorkerRequest{Action: "interrupt", Key: send.Key})
			deadline := time.Now().Add(5 * time.Second)
			for {
				state := fixture.mustCall(t, WorkerRequest{Action: "inspect"}).Executions[0]
				if state.Done {
					fixture.mustCall(t, WorkerRequest{Action: "commit", Sequence: 1, After: state.LastCursor})
					fixture.mustCall(t, WorkerRequest{Action: "ack", Sequence: 1, After: state.LastCursor})
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("provider did not release")
				}
				time.Sleep(time.Millisecond)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("retirement retained workflow metadata")
			}
		})
	}
}

func TestWorkerAdmissionStorageFailureFencesReservationAndSend(t *testing.T) {
	for _, stage := range []string{"before-reservation", "after-reservation", "after-send"} {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			target := &workerStorageHarness{supervisorHarness: &supervisorHarness{name: "synthetic", active: map[string]bool{}, emits: map[string]core.Emit{}}}
			supervisor := NewSupervisor(NewRegistry(), HarnessConfig{Cwd: root})
			supervisor.current, supervisor.ctx = target, context.Background()
			host := &workerHost{supervisor: supervisor, directory: root, next: 1, stop: make(chan struct{}), executions: map[uint64]*workerExecution{}}
			path := filepath.Join(root, "1.jsonl.admission")
			block := func() {
				t.Helper()
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			reserved := 0
			host.workflow = func(string, string, WorkerWorkflow, string) (WorkerReservation, error) {
				reserved++
				if stage == "after-reservation" {
					block()
				}
				return WorkerReservation{ProviderIterations: 1}, nil
			}
			if stage == "before-reservation" {
				block()
			}
			if stage == "after-send" {
				target.afterSend = block
			}
			reply := host.send(WorkerRequest{Action: "send", Sequence: 1, Key: "synthetic/storage", Prompt: "synthetic", Workflow: &WorkerWorkflow{WorkID: "synthetic"}})
			if reply.Error == "" || host.failure == nil {
				t.Fatal("storage failure was acknowledged")
			}
			wantReserved, wantSends := 1, 0
			if stage == "before-reservation" {
				wantReserved = 0
			}
			if stage == "after-send" {
				wantSends = 1
			}
			if reserved != wantReserved || len(target.prompts["synthetic/storage"]) != wantSends {
				t.Fatalf("failure crossed admission: reservations=%d sends=%d", reserved, len(target.prompts["synthetic/storage"]))
			}
			for sequence, execution := range host.executions {
				_ = execution.file.Close()
				_ = os.Remove(filepath.Join(root, strconv.FormatUint(sequence, 10)+".jsonl"))
			}
		})
	}
}
