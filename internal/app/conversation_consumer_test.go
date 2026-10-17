package app

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/history"
)

func TestConversationConsumerKeepsFailedResultPending(t *testing.T) {
	service, _, _ := newRecoveryTestService(t)
	message := core.Message{Channel: "cli", Conversation: "failed-result", SourceMessageID: "first"}
	for _, source := range []string{"first", "second"} {
		if _, err := service.History.Append(message.Channel, message.Conversation, history.Entry{
			Role: "user", Content: source, SourceMessageID: source, AcceptedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := service.reserveRecoveryExecution(message, "second"); err != nil {
		t.Fatal(err)
	}
	key := sessionKey(message)
	job := service.Runtime.BeginJob(key, message.Channel, message.Conversation, "synthetic result")
	service.streamText[key] = "streamed result"
	deliveries := 0
	consume := service.conversationConsumer(message, job, func(core.Event) { deliveries++ })
	path := service.History.Path(message.Channel, message.Conversation)
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	event := core.Event{Kind: core.EventFinal, Text: "one result", Done: true,
		Execution: &core.ExecutionStatus{State: "finishing"}}
	if err := consume(event); err == nil {
		t.Fatal("storage failure was consumed")
	}
	if job, exists := service.Runtime.JobForSession(key); !exists || job.Execution == JobFinishing || deliveries != 0 || service.recoveryExecution[key] == nil || service.streamText[key] != "streamed result" {
		t.Fatal("failed persistence delivered or released pending execution state")
	}
	if _, _, err := service.reserveRecoveryExecution(core.Message{Channel: message.Channel, Conversation: message.Conversation, SourceMessageID: "later"}); err == nil {
		t.Fatal("a new admission changed the unsettled result's source coverage")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".saved", path); err != nil {
		t.Fatal(err)
	}
	if err := consume(event); err != nil {
		t.Fatal(err)
	}
	if err := consume(event); err != nil {
		t.Fatal(err)
	}
	entries, _, err := service.History.Entries(message.Channel, message.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	results := 0
	for _, entry := range entries {
		if entry.Terminal {
			results++
			if entry.Content != event.Text || entry.ExecutionID == "" || strings.Join(entry.Covers, ",") != "first,second" || entry.Outcome != "terminal_assistant" || entry.ReceiptID == "" {
				t.Fatalf("reply and coverage were not committed together: %#v", entry)
			}
		}
	}
	snapshot, err := service.History.RecoveryWindow(message.Channel, message.Conversation, time.Now().Add(-time.Hour), 2000)
	if err != nil || snapshot.Covered["first"] != "terminal_coverage" || snapshot.Covered["second"] != "terminal_coverage" {
		t.Fatalf("committed batch remained recoverable: %#v, %v", snapshot.Covered, err)
	}
	if results != 1 || deliveries != 1 || service.Runtime.Status().Jobs != 0 || service.recoveryExecution[key] != nil || service.streamText[key] != "" {
		t.Fatal("successful retry did not settle once")
	}
}

func TestConversationConsumerDeduplicatesPartialAfterResultFailure(t *testing.T) {
	service, _, _ := newRecoveryTestService(t)
	message := core.Message{Channel: "cli", Conversation: "partial-retry", SourceMessageID: "source"}
	if _, _, err := service.reserveRecoveryExecution(message); err != nil {
		t.Fatal(err)
	}
	key := sessionKey(message)
	service.streamText[key] = "useful partial"
	deliveries := 0
	consume := service.conversationConsumer(message, 0, func(core.Event) { deliveries++ })
	event := core.Event{Kind: core.EventError, Text: "synthetic provider failure", Done: true}
	path := service.History.Path(message.Channel, message.Conversation)
	// Hold final coverage admission while the independent partial write finishes.
	service.recoveryMu.Lock()
	locked := true
	defer func() {
		if locked {
			service.recoveryMu.Unlock()
		}
	}()
	finished := make(chan error, 1)
	go func() { finished <- consume(event) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), ":partial\"") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("partial write did not reach the final coverage boundary")
		}
		time.Sleep(time.Millisecond)
	}
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	service.recoveryMu.Unlock()
	locked = false
	if err := <-finished; err == nil || deliveries != 0 || service.streamText[key] != "useful partial" || service.recoveryExecution[key] == nil {
		t.Fatal("partial success hid final failure or discarded retry state")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".saved", path); err != nil {
		t.Fatal(err)
	}
	if err := consume(event); err != nil {
		t.Fatal(err)
	}
	entries, _, err := service.History.Entries(message.Channel, message.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	partial, terminal := 0, 0
	for _, entry := range entries {
		if entry.Content == "useful partial" {
			partial++
		}
		if entry.Terminal && entry.Role == "error" && entry.Outcome == "terminal_error" {
			terminal++
		}
	}
	if partial != 1 || terminal != 1 || deliveries != 1 {
		t.Fatalf("partial/result retry counts: %d/%d/%d", partial, terminal, deliveries)
	}
}

func TestConversationConsumerSerializesTerminalAndReentrantDelivery(t *testing.T) {
	service, _, _ := newRecoveryTestService(t)
	message := core.Message{Channel: "cli", Conversation: "concurrent-terminal", SourceMessageID: "source"}
	var consume func(core.Event) error
	deliveries := 0
	consume = service.conversationConsumer(message, 0, func(core.Event) {
		deliveries++
		if err := consume(core.Event{Kind: core.EventStatus, Text: "late status"}); err != nil {
			t.Error(err)
		}
	})
	var group sync.WaitGroup
	for range 20 {
		group.Go(func() {
			if err := consume(core.Event{Kind: core.EventFinal, Text: "one result", Done: true}); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	entries, _, err := service.History.Entries(message.Channel, message.Conversation)
	if err != nil || len(entries) != 1 || deliveries != 1 {
		t.Fatalf("concurrent result delivery: entries=%d deliveries=%d err=%v", len(entries), deliveries, err)
	}
}

func TestConversationEmitterReportsUnsavedResult(t *testing.T) {
	service, _, _ := newRecoveryTestService(t)
	message := core.Message{Channel: "cli", Conversation: "unsaved"}
	job := service.Runtime.BeginJob(sessionKey(message), message.Channel, message.Conversation, "synthetic result")
	path := service.History.Path(message.Channel, message.Conversation)
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	var got core.Event
	service.wrapEmit(message, job, func(event core.Event) { got = event })(core.Event{Kind: core.EventFinal, Text: "must not claim saved success", Done: true})
	if got.Kind != core.EventError || !got.Done || strings.Contains(got.Text, path) {
		t.Fatalf("unsaved result was not an explicit content-free failure: %#v", got)
	}
	if current, exists := service.Runtime.JobForSession(sessionKey(message)); !exists || current.Execution != JobDegraded {
		t.Fatal("unsaved job was settled as successful")
	}
}
