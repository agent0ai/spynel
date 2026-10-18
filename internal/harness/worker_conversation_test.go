//go:build linux || darwin

package harness

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
)

func TestWorkerConversationCapturesTerminalOutput(t *testing.T) {
	for _, kind := range []string{core.EventFinal, core.EventError} {
		t.Run(kind, func(t *testing.T) {
			host := &workerHost{}
			execution := &workerExecution{WorkerExecution: WorkerExecution{Conversation: &WorkerConversation{ExecutionID: "execution"}}}
			for _, text := range []string{"useful ", "output 界"} {
				if _, streamed := host.captureConversationLocked(execution, core.Event{Kind: core.EventDelta, Text: text}, false); streamed != "" {
					t.Fatal("delta duplicated private terminal output")
				}
			}
			if _, streamed := host.captureConversationLocked(execution, core.Event{Kind: core.EventError, Done: true}, true); streamed != "" || execution.streamed != "useful output 界" {
				t.Fatal("request-only terminal consumed the shared stream")
			}
			_, streamed := host.captureConversationLocked(execution, core.Event{Kind: kind, Done: true}, false)
			if streamed != "useful output 界" || execution.streamed != "" {
				t.Fatal("terminal did not retain and release its complete output")
			}
		})
	}
}

func TestWorkerConversationTerminalOutputSharesJournalLimit(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "journal-")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	metadata := &WorkerConversation{ExecutionID: "execution"}
	execution := &workerExecution{WorkerExecution: WorkerExecution{Sequence: 1, Conversation: metadata}, file: file, streamed: "useful output"}
	final := core.Event{Kind: core.EventFinal, Text: "synthetic final", Done: true}
	withoutStream, err := json.Marshal(WorkerEvent{Cursor: 1, Conversation: metadata, Event: final})
	if err != nil {
		t.Fatal(err)
	}
	host := &workerHost{supervisor: &Supervisor{}, stop: make(chan struct{}), bytes: workerJournalBytes - int64(len(withoutStream)+1)}
	host.capture(execution, final)
	if host.failure == nil || execution.Done || execution.LastCursor != 0 {
		t.Fatal("retained output bypassed the journal limit or acknowledged an unrecorded final")
	}
	select {
	case <-host.stop:
	default:
		t.Fatal("journal overflow did not stop the worker")
	}
}

func TestWorkerConversationDescriptorSurvivesControllerTerms(t *testing.T) {
	for _, scenario := range []struct{ name, mode string }{
		{"codex", "codex-worker"}, {"claude-code", "claude-interrupt"}, {"pi", "pi-interrupt"}, {"acp", "acp-interrupt"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newWorkerFixture(t, scenario.name, scenario.mode)
			conversation := &WorkerConversation{Channel: "cli", Conversation: "synthetic", ExecutionID: "execution", SourceMessageID: "second", Sources: []string{"first", "second"}}
			request := WorkerRequest{Action: "send", Sequence: 1, Key: "chat:cli:synthetic", Prompt: "synthetic conversation", Conversation: conversation}
			first := fixture.mustCall(t, request).Executions[0]
			if first.Error != "" || first.Done || !reflect.DeepEqual(first.Conversation, conversation) {
				t.Fatal("conversation metadata was not admitted")
			}
			for term := 0; term < 2; term++ {
				fixture.replaceOwner(t)
				retained := fixture.mustCall(t, request).Executions[0]
				if !reflect.DeepEqual(retained.Conversation, conversation) || retained.ThreadID != first.ThreadID {
					t.Fatal("replacement changed the retained conversation")
				}
			}
			changed := *conversation
			changed.Sources = []string{"first", "later", "second"}
			request.Conversation = &changed
			if reply, _ := fixture.call(request); reply.Error == "" {
				t.Fatal("changed sources retried the original admission")
			}
			request.Sequence = 2
			if reply, _ := fixture.call(request); reply.Error == "" {
				t.Fatal("a second admission bypassed retained source ownership")
			}
			fixture.mustCall(t, WorkerRequest{Action: "interrupt", Key: request.Key})
			deadline := time.Now().Add(5 * time.Second)
			for {
				state := fixture.mustCall(t, WorkerRequest{Action: "inspect"}).Executions[0]
				if state.Done {
					page := fixture.mustCall(t, WorkerRequest{Action: "events", Sequence: 1})
					for _, event := range page.Events {
						if !reflect.DeepEqual(event.Conversation, conversation) || event.RequestOnly {
							t.Fatal("journal lost frozen source/activity metadata")
						}
					}
					fixture.mustCall(t, WorkerRequest{Action: "commit", Sequence: 1, After: state.LastCursor})
					fixture.mustCall(t, WorkerRequest{Action: "ack", Sequence: 1, After: state.LastCursor})
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("conversation did not release")
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

func TestWorkerConversationRejectsInvalidDescriptorsBeforeAdmission(t *testing.T) {
	host := &workerHost{next: 1, executions: map[uint64]*workerExecution{}}
	if reply := host.send(WorkerRequest{Sequence: 1, Key: "chat:cli:synthetic", Prompt: "synthetic"}); reply.Error == "" || host.next != 1 || len(host.executions) != 0 {
		t.Fatal("untyped chat admission bypassed source ownership")
	}
	base := WorkerConversation{Channel: "cli", Conversation: "synthetic", ExecutionID: "execution", SourceMessageID: "source", Sources: []string{"source"}}
	for _, change := range []func(*WorkerConversation){
		func(c *WorkerConversation) { c.Channel = "unknown" },
		func(c *WorkerConversation) { c.Conversation = "../other" },
		func(c *WorkerConversation) { c.Conversation = "trailing." },
		func(c *WorkerConversation) { c.SourceMessageID = "missing" },
		func(c *WorkerConversation) { c.Sources = []string{"z", "source"} },
		func(c *WorkerConversation) { c.Sources = []string{"source", "source"} },
		func(c *WorkerConversation) { c.ExecutionID = "invalid\nidentity" },
	} {
		value := base
		change(&value)
		reply := host.send(WorkerRequest{Sequence: 1, Key: "chat:cli:synthetic", Prompt: "synthetic", Conversation: &value})
		if reply.Error == "" || host.next != 1 || len(host.executions) != 0 {
			t.Fatal("invalid conversation crossed admission")
		}
	}
}

func TestWorkerConversationReservationFailureNeverDispatches(t *testing.T) {
	for _, failure := range []string{"missing-owner", "refused", "checkpoint", "refusal-journal", "refusal-checkpoint"} {
		t.Run(failure, func(t *testing.T) {
			target := &supervisorHarness{name: "synthetic", active: map[string]bool{}, emits: map[string]core.Emit{}}
			supervisor := NewSupervisor(NewRegistry(), HarnessConfig{Cwd: t.TempDir()})
			supervisor.current, supervisor.ctx = target, context.Background()
			host := &workerHost{supervisor: supervisor, directory: t.TempDir(), next: 1, executions: map[uint64]*workerExecution{}, stop: make(chan struct{})}
			defer func() {
				for _, execution := range host.executions {
					_ = execution.file.Close()
				}
			}()
			calls := 0
			if failure != "missing-owner" {
				host.conversation = func(root, key string, conversation WorkerConversation, receipt string, sendErr error) error {
					calls++
					if failure == "refused" {
						return os.ErrPermission
					}
					if failure == "refusal-journal" {
						_ = host.executions[1].file.Close()
						return os.ErrPermission
					}
					path := host.executions[1].file.Name() + ".admission"
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(path, 0700); err != nil {
						return err
					}
					if failure == "refusal-checkpoint" {
						return os.ErrPermission
					}
					return nil
				}
			}
			metadata := &WorkerConversation{Channel: "cli", Conversation: "synthetic", ExecutionID: "execution", SourceMessageID: "source", Sources: []string{"source"}}
			request := WorkerRequest{Action: "send", Sequence: 1, Key: "chat:cli:synthetic", Prompt: "synthetic", Conversation: metadata}
			reply := host.send(request)
			if len(target.prompts[request.Key]) != 0 {
				t.Fatal("failed source reservation dispatched provider work")
			}
			if failure == "checkpoint" || failure == "refusal-journal" || failure == "refusal-checkpoint" {
				if reply.Error == "" || host.failure == nil {
					t.Fatal("failed reservation checkpoint did not stop the worker")
				}
				return
			}
			if len(reply.Executions) != 1 || !reply.Executions[0].Done || reply.Executions[0].Error == "" || reply.Executions[0].SourcesReserved {
				t.Fatal("refused source reservation was admitted")
			}
			if !reply.Executions[0].ConversationRefused() {
				t.Fatal("refusal has no durable non-provider outcome")
			}
			page := host.events(WorkerRequest{Sequence: 1})
			if len(page.Events) != 1 || page.Events[0].Event.Kind != core.EventStatus || !page.Events[0].Event.Done {
				t.Fatal("refusal fabricated a provider terminal")
			}
			_ = host.send(request)
			if calls > 1 || len(target.prompts[request.Key]) != 0 {
				t.Fatal("matching retry repeated a refused reservation")
			}
		})
	}
}

func TestWorkerConversationRefusalClassification(t *testing.T) {
	c := &WorkerConversation{Channel: "cli", Conversation: "synthetic", ExecutionID: "execution", SourceMessageID: "source", Sources: []string{"source"}}
	base := WorkerExecution{Sequence: 1, Key: "chat:cli:synthetic", Conversation: c, Done: true, Error: "refused", LastCursor: 1}
	if !base.ConversationRefused() {
		t.Fatal("journaled source refusal was not recognized")
	}
	for _, change := range []func(*WorkerExecution){
		func(e *WorkerExecution) { e.SourcesReserved = true },
		func(e *WorkerExecution) { e.Done = false },
		func(e *WorkerExecution) { e.Error = "" },
		func(e *WorkerExecution) { e.LastCursor = 0 },
		func(e *WorkerExecution) { e.LastCursor = 2 },
		func(e *WorkerExecution) { e.CommittedCursor = 2 },
		func(e *WorkerExecution) { e.ThreadID = "provider-thread" },
		func(e *WorkerExecution) { e.Steered = true },
		func(e *WorkerExecution) { e.Cancelled = true },
		func(e *WorkerExecution) { e.Workflow = &WorkerWorkflow{} },
		func(e *WorkerExecution) { e.Agent = &WorkerAgent{} },
		func(e *WorkerExecution) { e.Reservation = &WorkerReservation{} },
		func(e *WorkerExecution) { e.Followups = []WorkerConversationFollowup{{}} },
		func(e *WorkerExecution) { e.ConversationTurns = 1 },
		func(e *WorkerExecution) { e.Key = "chat:cli:other" },
		func(e *WorkerExecution) { e.Conversation = nil },
		func(e *WorkerExecution) { e.Sequence = 0 },
	} {
		value := base
		change(&value)
		if value.ConversationRefused() {
			t.Fatalf("ambiguous or dispatched admission classified as a source refusal: %+v", value)
		}
	}
}
