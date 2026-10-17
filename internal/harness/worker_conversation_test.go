//go:build linux || darwin

package harness

import (
	"reflect"
	"testing"
	"time"
)

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
