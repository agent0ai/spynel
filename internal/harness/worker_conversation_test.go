//go:build linux || darwin

package harness

import (
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
