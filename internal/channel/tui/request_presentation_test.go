package tui

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
)

func TestRequestCompletionFollowsCallbacksAndReturnedError(t *testing.T) {
	m := notificationTestModel()
	m.handler = func(_ context.Context, _ core.Message, emit core.Emit) error {
		emit(core.Event{Kind: core.EventDelta, Text: "useful partial"})
		return errors.New("synthetic request loss")
	}
	commands := m.dispatchMessage("synthetic", "synthetic")
	source := m.responseRequestID
	commands[0]()
	for _, kind := range []string{core.EventDelta, core.EventError, requestFinished} {
		select {
		case event := <-m.events:
			if event.Kind != kind || event.RequestID != source {
				t.Fatal("local request completion overtook its callbacks or lost identity")
			}
			next, _ := m.Update(uiEvent{event: event})
			m = next.(model)
		default:
			t.Fatal("missing ordered local request callback")
		}
	}
	if m.requests[source].active || m.streaming != "" || len(m.transcript) != 3 {
		t.Fatal("returned error did not settle its presentation")
	}
}

func TestPresentationRetentionKeepsAsynchronousProvider(t *testing.T) {
	m := notificationTestModel()
	var provider core.Emit
	m.handler = func(_ context.Context, _ core.Message, emit core.Emit) error {
		provider = emit
		emit(core.Event{Kind: core.EventActivity, Active: true})
		return nil
	}
	applyQueued := func() {
		for len(m.events) > 0 {
			next, _ := m.Update(uiEvent{event: <-m.events})
			m = next.(model)
		}
	}
	commands := m.dispatchMessage("synthetic", "synthetic")
	source := m.responseRequestID
	commands[0]()
	applyQueued()
	m.handler = func(_ context.Context, _ core.Message, emit core.Emit) error {
		emit(core.Event{Kind: core.EventFinal, Text: "status", Done: true, Local: true})
		return nil
	}
	for range maxTranscriptRows + 1 {
		commands = m.dispatchMessage("/status", "/status")
		commands[0]()
		applyQueued()
	}
	provider(core.Event{Kind: core.EventDelta, Text: "still active"})
	provider(core.Event{Kind: core.EventActivity})
	provider(core.Event{Kind: core.EventFinal, Text: "still active", Done: true})
	applyQueued()
	if _, known := m.requests[source]; !known || m.mainAgentActivity != 0 || m.working || m.transcript[len(m.transcript)-1].text != "still active" {
		t.Fatal("bounded presentation retention dropped a still-active provider")
	}
}

func TestWorkerRequestHistoryAndStreamPresentOneResponse(t *testing.T) {
	for _, order := range []string{"history-first", "stream-first", "lost-terminal", "newer-request"} {
		t.Run(order, func(t *testing.T) {
			s, _ := notificationWorkflowService(t)
			m := notificationTestModel()
			m.conversation = "selected"
			m.dispatchMessage("synthetic request", "synthetic request")
			var source string
			for id := range m.requests {
				source = id
			}
			if source == "" {
				t.Fatal("submission lost its request identity")
			}
			c := &harness.WorkerConversation{Channel: "tui", Conversation: m.conversation, SourceMessageID: source, ExecutionID: "execution", Sources: []string{source}}
			if _, err := s.History.Append(c.Channel, c.Conversation, history.Entry{Role: "user", Content: "synthetic", SourceMessageID: source}); err != nil {
				t.Fatal(err)
			}
			_, path, offset, err := s.History.RecentEntriesSnapshot(c.Channel, c.Conversation, 10, 1000)
			if err != nil {
				t.Fatal(err)
			}
			followed := watchTaskNotifications(t.Context(), path, offset)
			job, err := s.Runtime.TryBeginJobWithDetails("chat:tui:selected", c.Channel, c.Conversation, "synthetic", app.JobDetails{Kind: "conversation", WorkID: c.ExecutionID})
			if err != nil {
				t.Fatal(err)
			}
			var stream []core.Event
			for i, event := range []core.Event{
				{Kind: core.EventDelta, Text: "intermediate"},
				{Kind: core.EventFinal, Text: "intermediate", Done: true, Continues: true},
				{Kind: core.EventDelta, Text: "useful partial"},
				{Kind: core.EventError, Text: "terminal error", Done: true},
			} {
				d := harness.WorkerDelivery{WorkerID: strings.Repeat("f", 32), Sequence: 1, WorkerEvent: harness.WorkerEvent{Cursor: uint64(i + 1), Conversation: c, Event: event}}
				if i == 3 {
					d.Streamed = "useful partial"
				}
				if err := s.ConsumeConversationDelivery(job, d, func(delivery harness.WorkerDelivery) error {
					stream = append(stream, delivery.Event)
					if (order == "lost-terminal" || order == "newer-request") && i == 3 {
						stream = stream[:len(stream)-1]
						return errors.New("synthetic lost request delivery")
					}
					return nil
				}); err != nil && !((order == "lost-terminal" || order == "newer-request") && i == 3) {
					t.Fatal(err)
				}
			}
			var recovered []channel.Notification
			for range 3 {
				select {
				case n := <-followed:
					if n.RequestID != source || n.ResponseID == "" {
						t.Fatal("history follower lost response correlation")
					}
					recovered = append(recovered, n)
				case <-time.After(3 * time.Second):
					t.Fatal("history follower did not deliver the durable responses")
				}
			}
			applyStream := func() {
				next, _ := m.Update(uiEvent{event: core.Event{Kind: core.EventActivity, RequestID: source, Active: true}})
				m = next.(model)
				for _, e := range stream {
					next, _ := m.Update(uiEvent{event: e})
					m = next.(model)
				}
				next, _ = m.Update(uiEvent{event: core.Event{Kind: core.EventActivity, RequestID: source}})
				m = next.(model)
			}
			applyHistory := func() {
				for _, n := range recovered {
					next, _ := m.Update(taskNotificationEvent{conversation: m.conversation, notification: n})
					m = next.(model)
				}
			}
			if order == "stream-first" {
				applyStream()
				applyHistory()
			} else {
				applyHistory()
				if len(m.transcript) != 1 {
					t.Fatal("history overtook the active request stream")
				}
				applyStream()
			}
			if order == "newer-request" {
				m.working = true
				m.dispatchMessage("newer request", "newer request")
				m.streaming, m.responseText = "newer stream", "newer stream"
			}
			next, _ := m.Update(uiEvent{event: core.Event{Kind: requestFinished, RequestID: source}})
			m = next.(model)
			if order == "newer-request" {
				if len(m.transcript) != 5 || m.transcript[2].text != "useful partial" || m.transcript[4].role != "error" || m.streaming != "newer stream" || m.responseText != "newer stream" || !m.working {
					t.Fatalf("old missing terminal disturbed newer response or duplicated partial: %#v stream=%q", m.transcript, m.streaming)
				}
				return
			}
			if len(m.transcript) != 4 || m.transcript[1].text != "intermediate" || m.transcript[2].text != "useful partial" || m.transcript[3].role != "error" || m.transcript[3].text != "terminal error" || m.streaming != "" || m.working || m.mainAgentActivity != 0 || m.notificationAckBusy || len(m.pendingNotifications) != 0 || len(m.pendingRecovery) != 0 {
				t.Fatalf("request/history presentation duplicated or lost output: %#v stream=%q", m.transcript, m.streaming)
			}
		})
	}
}

func TestRequestPresentationFencesSwitchedConversation(t *testing.T) {
	m := notificationTestModel()
	m.conversation = "old"
	m.dispatchMessage("synthetic", "synthetic")
	source := m.responseRequestID
	m.selectConversation("new", 0)
	m.transcript = nil
	for _, event := range []core.Event{
		{Kind: core.EventActivity, RequestID: source, Active: true},
		{Kind: core.EventDelta, RequestID: source, Text: "old reply"},
		{Kind: core.EventFinal, RequestID: source, ResponseID: "old-response", Text: "old reply", Done: true},
		{Kind: requestFinished, RequestID: source},
	} {
		next, _ := m.Update(uiEvent{event: event})
		m = next.(model)
	}
	if len(m.transcript) != 0 || m.streaming != "" || m.mainAgentActivity != 0 || len(m.requests) != 0 {
		t.Fatal("old request crossed the conversation switch")
	}
}

func TestVerboseWorkerHandoffPreservesTurnPresentation(t *testing.T) {
	for _, handoff := range []string{"native", "native-final-only", "queued", "new-turn"} {
		for _, route := range []string{"stream", "history"} {
			t.Run(handoff+"/"+route, func(t *testing.T) {
				s, _ := notificationWorkflowService(t)
				m := notificationTestModel()
				m.conversation = "selected"
				apply := func(event core.Event) { next, _ := m.Update(uiEvent{event: event}); m = next.(model) }
				var sources []string
				submit := func(text string) string {
					m.dispatchMessage(text, text)
					id := m.responseRequestID
					sources = append(sources, id)
					slices.Sort(sources)
					if _, err := s.History.Append("tui", m.conversation, history.Entry{Role: "user", Content: text, SourceMessageID: id}); err != nil {
						t.Fatal(err)
					}
					return id
				}
				first := submit("Initial request")
				_, path, offset, err := s.History.RecentEntriesSnapshot("tui", m.conversation, 10, 1000)
				if err != nil {
					t.Fatal(err)
				}
				followed := watchTaskNotifications(t.Context(), path, offset)
				job, err := s.Runtime.TryBeginJobWithDetails("chat:tui:selected", "tui", m.conversation, "synthetic", app.JobDetails{Kind: "conversation", WorkID: "execution"})
				if err != nil {
					t.Fatal(err)
				}
				var cursor uint64
				execution, sequence := "execution", uint64(1)
				deliver := func(id string, event core.Event, loseFinal bool) {
					cursor++
					c := &harness.WorkerConversation{Channel: "tui", Conversation: m.conversation, SourceMessageID: id, ExecutionID: execution, Sources: slices.Clone(sources)}
					d := harness.WorkerDelivery{WorkerID: strings.Repeat("f", 32), Sequence: sequence, WorkerEvent: harness.WorkerEvent{Cursor: cursor, Conversation: c, Event: event}}
					if event.Kind == core.EventFinal {
						d.Streamed = event.Text
					}
					lost := errors.New("synthetic lost terminal callback")
					err := s.ConsumeConversationDelivery(job, d, func(v harness.WorkerDelivery) error {
						if loseFinal {
							return lost
						}
						apply(v.Event)
						return nil
					})
					if loseFinal && !errors.Is(err, lost) || !loseFinal && err != nil {
						t.Fatal(err)
					}
				}
				mode := func(value string) {
					if _, err := s.ApplySettings(map[string]string{"channels.verbose": value}); err != nil {
						t.Fatal(err)
					}
					m.setVerbose(value)
				}
				mode("on")
				deliver(first, core.Event{Kind: core.EventDelta, Text: "The "}, false)
				mode("off")
				deliver(first, core.Event{Kind: core.EventDelta, Text: "result "}, false)
				final := "The result is 42."
				want := final
				responses := 1
				second := ""
				if handoff == "queued" {
					second = submit("Please include the number.")
				}
				if handoff == "queued" || handoff == "new-turn" {
					firstFinal := "The result "
					deliver(first, core.Event{Kind: core.EventFinal, Text: firstFinal, FinalText: &firstFinal, Done: true, Continues: handoff == "queued"}, false)
					responses++
					if handoff == "queued" {
						want = "The " + final
					} else {
						want = firstFinal + final
					}
				}
				if handoff == "new-turn" {
					sources = nil
					execution, sequence, cursor = "next-execution", 2, 0
					job, err = s.Runtime.TryBeginJobWithDetails("chat:tui:selected", "tui", m.conversation, "synthetic", app.JobDetails{Kind: "conversation", WorkID: execution})
					if err != nil {
						t.Fatal(err)
					}
				}
				if second == "" {
					second = submit("Please include the number.")
				}
				if handoff != "new-turn" {
					deliver(first, core.Event{Kind: core.EventStatus, Done: true}, false)
				}
				apply(core.Event{Kind: requestFinished, RequestID: first})
				mode("on")
				if handoff == "queued" || handoff == "new-turn" {
					deliver(second, core.Event{Kind: core.EventDelta, Text: "The "}, false)
					mode("off")
					deliver(second, core.Event{Kind: core.EventDelta, Text: "result "}, false)
					mode("on")
				}
				if handoff != "native-final-only" {
					deliver(second, core.Event{Kind: core.EventDelta, Text: "is 42."}, false)
				}
				m.handler = func(_ context.Context, _ core.Message, emit core.Emit) error {
					emit(core.Event{Kind: core.EventFinal, Text: "Command reply.", Done: true, Local: true})
					return nil
				}
				m.dispatchMessage("/status", "/status")[0]()
				for len(m.events) > 0 {
					apply(<-m.events)
				}
				deliver(second, core.Event{Kind: core.EventFinal, Text: final, FinalText: &final, Done: true}, route == "history")
				for range responses {
					select {
					case n := <-followed:
						m.receiveRecoveredResponse(n)
					case <-time.After(3 * time.Second):
						t.Fatal("missing durable response")
					}
				}
				apply(core.Event{Kind: requestFinished, RequestID: second})
				var answer string
				for _, entry := range m.transcript {
					if entry.role == "assistant" && entry.text != "Command reply." {
						answer += entry.text
					}
					if entry.streamEnd != 0 {
						t.Fatal("completed turn retained fragment positions")
					}
				}
				if answer != want {
					t.Fatalf("handoff duplicated, replayed or lost text: got %q, want %q", answer, want)
				}
				if m.requests[first].deltaBytes != 0 || m.requests[second].deltaBytes != 0 {
					t.Fatal("completed turn retained observed position")
				}
			})
		}
	}
}

func TestVerboseHarnessCommandRecoversHandoffFinal(t *testing.T) {
	m := notificationTestModel()
	m.requests = map[string]requestPresentation{"first": {active: true}}
	apply := func(event core.Event) { next, _ := m.Update(uiEvent{event: event}); m = next.(model) }
	m.setVerbose("on")
	apply(core.Event{Kind: core.EventDelta, RequestID: "first", Text: "The "})
	m.setVerbose("off")
	apply(core.Event{Kind: core.EventDelta, RequestID: "first", Text: "result "})
	var second string
	m.handler = func(_ context.Context, message core.Message, emit core.Emit) error {
		second = message.SourceMessageID
		emit(core.Event{Kind: core.EventActivity, Active: true})
		return nil
	}
	m.dispatchMessage("/task Include the number", "/task Include the number")[0]()
	for len(m.events) > 0 {
		apply(<-m.events)
	}
	m.setVerbose("on")
	final := "The result is 42."
	m.receiveRecoveredResponse(channel.Notification{Recovery: true, RequestID: second, ResponseID: "terminal", Text: final, FinalText: &final})
	var answer string
	for _, entry := range m.transcript {
		if entry.role == "assistant" {
			answer += entry.text
		}
	}
	if answer != final {
		t.Fatalf("harness command duplicated final fragments: %q", answer)
	}
}
