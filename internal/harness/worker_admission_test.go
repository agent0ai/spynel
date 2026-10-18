//go:build linux || darwin

package harness

import (
	"context"
	"encoding/json"
	"errors"
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

type workerSendErrorHarness struct {
	*supervisorHarness
	beforeReturn func(core.Emit)
	returnErr    error
	sends        int
}

func (h *workerSendErrorHarness) SendWithModel(_ context.Context, _, _, _ string, emit core.Emit) (string, bool, error) {
	h.sends++
	h.beforeReturn(emit)
	if h.returnErr != nil {
		return "synthetic-thread", false, h.returnErr
	}
	return "synthetic-thread", false, errors.New("synthetic provider admission refused")
}

func TestWorkerUnknownSendRetainsExecutionUntilProviderTerminal(t *testing.T) {
	for _, role := range []string{"conversation", "workflow", "agent"} {
		t.Run(role, func(t *testing.T) {
			root := t.TempDir()
			target := &workerSendErrorHarness{supervisorHarness: &supervisorHarness{active: map[string]bool{}, emits: map[string]core.Emit{}}, returnErr: errors.Join(errors.New("synthetic acknowledgement failed after effect"), context.DeadlineExceeded)}
			supervisor := NewSupervisor(NewRegistry(), HarnessConfig{Cwd: root})
			supervisor.current, supervisor.ctx = target, context.Background()
			host := &workerHost{supervisor: supervisor, directory: root, next: 1, executions: map[uint64]*workerExecution{}, stop: make(chan struct{})}
			t.Cleanup(func() {
				for _, execution := range host.executions {
					_ = execution.file.Close()
				}
			})
			reservations := 0
			host.conversation = func(_, _ string, _ WorkerConversation, _ string, sendErr error) error {
				if sendErr == nil {
					reservations++
				}
				return nil
			}
			host.workflow = func(string, string, WorkerWorkflow, string) (WorkerReservation, error) {
				reservations++
				return WorkerReservation{ProviderIterations: 1}, nil
			}
			request := WorkerRequest{Sequence: 1, Key: "synthetic", Prompt: "synthetic"}
			switch role {
			case "conversation":
				request.Key = "chat:cli:synthetic"
				request.Conversation = &WorkerConversation{Channel: "cli", Conversation: "synthetic", ExecutionID: "execution", SourceMessageID: "source", Sources: []string{"source"}}
			case "workflow":
				request.Workflow = &WorkerWorkflow{WorkID: "synthetic"}
			case "agent":
				request.Key = "orchestrator:semantic-heartbeat"
				now := time.Now().UTC()
				request.Agent = &WorkerAgent{ID: "synthetic-agent", Kind: "heartbeat", StartedAt: now, Deadline: now.Add(time.Minute)}
			}
			var provider core.Emit
			target.beforeReturn = func(emit core.Emit) {
				provider = emit
				emit(core.Event{Kind: core.EventDelta, Text: "applied before acknowledgement"})
			}
			reply := host.send(request)
			execution := host.executions[1]
			if reply.Error == "" || execution.Done || execution.Error == "" {
				t.Fatal("uncertain Send manufactured terminal completion")
			}
			page := host.events(WorkerRequest{Sequence: 1})
			for _, event := range page.Events {
				if event.Event.Done {
					t.Fatal("uncertain Send journaled fabricated terminal coverage")
				}
			}
			if retry := host.send(request); retry.Error == "" || target.sends != 1 || role != "agent" && reservations != 1 {
				t.Fatal("uncertain retry confirmed or repeated provider admission")
			}
			if ack := host.apply(WorkerRequest{Action: "ack", Sequence: 1, After: execution.LastCursor}); ack.Error == "" {
				t.Fatal("uncertain execution retired before a provider terminal")
			}
			provider(core.Event{Kind: core.EventFinal, Text: "actual provider result", Done: true})
			if !execution.Done || execution.Error != "" {
				t.Fatal("actual terminal did not resolve admission uncertainty")
			}
			page = host.events(WorkerRequest{Sequence: 1})
			terminal := page.Events[len(page.Events)-1]
			if terminal.Event.Kind != core.EventFinal || terminal.Event.Text != "actual provider result" || terminal.RequestOnly {
				t.Fatal("actual provider terminal was lost or rewritten")
			}
			if role == "conversation" && (terminal.Streamed != "applied before acknowledgement" || !reflect.DeepEqual(terminal.Conversation, request.Conversation)) {
				t.Fatal("late provider terminal lost retained output or exact sources")
			}
			if reply := host.commit(WorkerRequest{Sequence: 1, After: execution.LastCursor}); reply.Error != "" {
				t.Fatal(reply.Error)
			}
			if reply := host.apply(WorkerRequest{Action: "ack", Sequence: 1, After: execution.LastCursor}); reply.Error != "" || len(host.executions) != 0 {
				t.Fatal("actual committed terminal did not retire", reply.Error)
			}
		})
	}
}

func TestWorkerSendFailureHasOneDurableTerminal(t *testing.T) {
	for _, role := range []string{"conversation", "workflow", "agent"} {
		for _, mode := range []string{"return-only", "already-terminal", "journal-failure"} {
			t.Run(role+"/"+mode, func(t *testing.T) {
				root := t.TempDir()
				target := &workerSendErrorHarness{supervisorHarness: &supervisorHarness{active: map[string]bool{}, emits: map[string]core.Emit{}}}
				if mode != "already-terminal" {
					target.returnErr = &SendNotAdmittedError{Err: errors.New("synthetic provider admission refused")}
				}
				supervisor := NewSupervisor(NewRegistry(), HarnessConfig{Cwd: root})
				supervisor.current, supervisor.ctx = target, context.Background()
				host := &workerHost{supervisor: supervisor, directory: root, next: 1, executions: map[uint64]*workerExecution{}, stop: make(chan struct{})}
				defer func() {
					for _, execution := range host.executions {
						_ = execution.file.Close()
					}
				}()
				reservations := 0
				host.conversation = func(_, _ string, _ WorkerConversation, _ string, sendErr error) error {
					if sendErr == nil {
						reservations++
					}
					return nil
				}
				host.workflow = func(string, string, WorkerWorkflow, string) (WorkerReservation, error) {
					reservations++
					return WorkerReservation{ProviderIterations: 1}, nil
				}
				request := WorkerRequest{Sequence: 1, Key: "synthetic", Prompt: "synthetic"}
				switch role {
				case "conversation":
					request.Key = "chat:cli:synthetic"
					request.Conversation = &WorkerConversation{Channel: "cli", Conversation: "synthetic", ExecutionID: "execution", SourceMessageID: "source", Sources: []string{"source"}}
				case "workflow":
					request.Workflow = &WorkerWorkflow{WorkID: "synthetic"}
				case "agent":
					request.Key = "orchestrator:semantic-heartbeat"
					now := time.Now().UTC()
					request.Agent = &WorkerAgent{ID: "synthetic-agent", Kind: "heartbeat", StartedAt: now, Deadline: now.Add(time.Minute)}
				}
				target.beforeReturn = func(emit core.Emit) {
					if mode == "already-terminal" {
						emit(core.Event{Kind: core.EventDelta, Text: "useful output 界"})
						emit(core.Event{Kind: core.EventError, Text: "adapter terminal", Done: true})
					}
					if mode == "journal-failure" {
						if err := host.executions[1].file.Close(); err != nil {
							t.Fatal(err)
						}
					}
				}
				reply := host.send(request)
				execution := host.executions[1]
				if mode == "journal-failure" {
					if reply.Error == "" || host.failure == nil || execution.Done || execution.LastCursor != 0 {
						t.Fatal("unrecorded Send failure was acknowledged as completion")
					}
					select {
					case <-host.stop:
					default:
						t.Fatal("failed terminal persistence did not stop the worker")
					}
					return
				}
				wantError, wantEvents, wantStream := "synthetic provider admission refused", uint64(1), ""
				if mode == "already-terminal" {
					wantError, wantEvents, wantStream = "", 2, "useful output 界"
				}
				if reply.Error != "" || len(reply.Executions) != 1 || !execution.Done || execution.Error != wantError || execution.LastCursor != wantEvents {
					t.Fatal("failed provider Send has no single durable terminal")
				}
				page := host.events(WorkerRequest{Sequence: 1})
				if page.Error != "" || uint64(len(page.Events)) != wantEvents {
					t.Fatal("failed dispatch journal cannot be replayed")
				}
				terminal := page.Events[len(page.Events)-1]
				want := "synthetic provider admission refused"
				if mode == "already-terminal" {
					want = "adapter terminal"
				}
				if terminal.Event.Kind != core.EventError || !terminal.Event.Done || terminal.Event.Text != want || terminal.RequestOnly {
					t.Fatal("failed Send changed or duplicated the terminal")
				}
				if role == "conversation" && (!execution.SourcesReserved || terminal.Streamed != wantStream || !reflect.DeepEqual(terminal.Conversation, request.Conversation)) {
					t.Fatal("failed Send lost reserved sources or useful output")
				}
				if retry := host.send(request); retry.Error != "" || target.sends != 1 || role != "agent" && reservations != 1 || execution.LastCursor != wantEvents {
					t.Fatal("matching retry repeated dispatch, reservation or terminal capture")
				}
				if reply := host.apply(WorkerRequest{Action: "ack", Sequence: 1, After: wantEvents}); reply.Error == "" {
					t.Fatal("failed dispatch retired before downstream commit")
				}
				if reply := host.commit(WorkerRequest{Sequence: 1, Expected: 0, After: wantEvents}); reply.Error != "" {
					t.Fatal(reply.Error)
				}
				if reply := host.apply(WorkerRequest{Action: "ack", Sequence: 1, After: wantEvents}); reply.Error != "" || len(host.executions) != 0 {
					t.Fatal("committed failed dispatch did not retire", reply.Error)
				}
			})
		}
	}
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
