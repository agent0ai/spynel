package app

import (
	"context"
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
	// Pause after the source freeze, then hold final coverage behind the partial.
	service.streamMu.Lock()
	streamLocked := true
	defer func() {
		if streamLocked {
			service.streamMu.Unlock()
		}
	}()
	finished := make(chan error, 1)
	go func() { finished <- consume(event) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		service.recoveryMu.Lock()
		settling := service.recoveryExecution[key].settling
		service.recoveryMu.Unlock()
		if settling {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("terminal did not freeze source admission before reading streamed output")
		}
		time.Sleep(time.Millisecond)
	}
	service.recoveryMu.Lock()
	locked := true
	defer func() {
		if locked {
			service.recoveryMu.Unlock()
		}
	}()
	service.streamMu.Unlock()
	streamLocked = false
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

func TestConversationConsumerPartialWriteFailureFencesSourceAdmission(t *testing.T) {
	service, target, _ := newRecoveryTestService(t)
	message := core.Message{Channel: "cli", Conversation: "partial-failure-fence", SourceMessageID: "original"}
	if _, err := service.History.Append(message.Channel, message.Conversation, history.Entry{Role: "user", Content: "original request", SourceMessageID: message.SourceMessageID, AcceptedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.reserveRecoveryExecution(message); err != nil {
		t.Fatal(err)
	}
	key := sessionKey(message)
	job := service.Runtime.BeginJob(key, message.Channel, message.Conversation, "synthetic result")
	service.streamText[key] = "useful streamed partial"
	deliveries := 0
	consume := service.conversationConsumer(message, job, func(core.Event) { deliveries++ })
	path := service.History.Path(message.Channel, message.Conversation)
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	event := core.Event{Kind: core.EventError, Text: "provider failed", Done: true}
	if err := consume(event); err == nil || !strings.Contains(err.Error(), "save partial") {
		t.Fatalf("partial write did not fail: %v", err)
	}
	if _, exists := service.Runtime.JobForSession(key); !exists || deliveries != 0 || service.streamText[key] != "useful streamed partial" {
		t.Fatal("failed partial persistence released job, stream or delivery state")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".saved", path); err != nil {
		t.Fatal(err)
	}
	later := message
	later.SourceMessageID = "not-yet-dispatched"
	if _, err := service.History.Append(later.Channel, later.Conversation, history.Entry{Role: "user", Content: "later request", SourceMessageID: later.SourceMessageID, AcceptedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.reserveRecoveryExecution(later); err == nil {
		t.Fatal("new source admitted while the previous terminal partial remains unsaved")
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
	partial, terminal := 0, 0
	for _, entry := range entries {
		if entry.Content == "useful streamed partial" {
			partial++
		}
		if entry.Terminal {
			terminal++
			if strings.Join(entry.Covers, ",") != "original" {
				t.Fatalf("original terminal covered an undispatched source: %#v", entry.Covers)
			}
		}
	}
	snapshot, err := service.History.RecoveryWindow(message.Channel, message.Conversation, time.Now().Add(-time.Hour), 2000)
	if err != nil || snapshot.Covered["original"] != "terminal_coverage" || snapshot.Covered[later.SourceMessageID] != "" || snapshot.Admitted[later.SourceMessageID] {
		t.Fatalf("recovery suppressed the undispatched source: %#v, %v", snapshot, err)
	}
	if partial != 1 || terminal != 1 || deliveries != 1 || service.Runtime.Status().Jobs != 0 || service.recoveryExecution[key] != nil || service.streamText[key] != "" {
		t.Fatal("successful retry did not settle exactly once")
	}
	if result := service.scanRecovery(context.Background(), "reconnect"); result.Dispatched != 1 || result.Eligible != 1 || len(target.prompts[key]) != 1 {
		t.Fatalf("later source did not recover once after settlement: %#v", result)
	}
	if result := service.scanRecovery(context.Background(), "reconnect"); result.Dispatched != 0 || len(target.prompts[key]) != 1 {
		t.Fatalf("later source was recovered twice: %#v", result)
	}
}

func TestConversationConsumerNonsettlingFailureKeepsSourceAdmission(t *testing.T) {
	for _, mode := range []string{"continuing", "request-only"} {
		t.Run(mode, func(t *testing.T) {
			service, target, _ := newRecoveryTestService(t)
			message := core.Message{Channel: "cli", Conversation: mode, SourceMessageID: "first"}
			if _, _, err := service.reserveRecoveryExecution(message); err != nil {
				t.Fatal(err)
			}
			key := sessionKey(message)
			target.active[key] = mode == "request-only"
			job := service.Runtime.BeginJob(key, message.Channel, message.Conversation, "synthetic result")
			service.streamText[key] = "active partial"
			consume := service.conversationConsumer(message, job, nil)
			path := service.History.Path(message.Channel, message.Conversation)
			if err := os.Rename(path, path+".saved"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			event := core.Event{Kind: core.EventError, Text: "response failed", Done: true, Continues: mode == "continuing"}
			if err := consume(event); err == nil || service.recoveryExecution[key].settling || service.streamText[key] != "active partial" {
				t.Fatalf("nonsettling persistence failure froze or released the shared turn: %v", err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(path+".saved", path); err != nil {
				t.Fatal(err)
			}
			message.SourceMessageID = "later"
			if _, _, err := service.reserveRecoveryExecution(message); err != nil {
				t.Fatalf("nonsettling response refused follow-up admission: %v", err)
			}
			if err := consume(event); err != nil {
				t.Fatal(err)
			}
			if _, exists := service.Runtime.JobForSession(key); !exists || service.recoveryExecution[key] == nil || len(service.recoveryExecution[key].sources) != 2 {
				t.Fatal("nonsettling retry ended the shared execution")
			}
			entries, _, err := service.History.Entries(message.Channel, message.Conversation)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Terminal && (entry.ExecutionID != "" || len(entry.Covers) != 0 || entry.Outcome != "") {
					t.Fatalf("nonsettling response covered the shared execution: %#v", entry)
				}
			}
		})
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
