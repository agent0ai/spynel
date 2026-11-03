package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/history"
)

type presentationCall struct {
	method, text, id, chat string
	reply                  int64
}

type presentationFixture struct {
	bot   *Bot
	mu    sync.Mutex
	calls []presentationCall
	next  int
	hook  func(*http.Request, presentationCall) error
}

func newPresentationFixture(t *testing.T) *presentationFixture {
	t.Helper()
	f := &presentationFixture{bot: New(config.Telegram{AllowedUsers: []string{"7", "8"}}, "fixture"), next: 100}
	f.bot.SetHistory(history.New(t.TempDir()))
	f.bot.SetVerboseSource(func() string { return "on" })
	f.bot.client.Transport = telegramRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if path.Base(r.URL.Path) == "sendDocument" {
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				return nil, err
			}
			f.mu.Lock()
			f.calls = append(f.calls, presentationCall{method: "sendDocument"})
			f.mu.Unlock()
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
		}
		var p struct {
			Text      string `json:"text"`
			Chat      string `json:"chat_id"`
			MessageID string `json:"message_id"`
			Reply     struct {
				MessageID int64 `json:"message_id"`
			} `json:"reply_parameters"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			return nil, err
		}
		call := presentationCall{method: path.Base(r.URL.Path), text: p.Text, id: p.MessageID, chat: p.Chat, reply: p.Reply.MessageID}
		f.mu.Lock()
		if call.method == "sendMessage" {
			f.next++
			call.id = strconv.Itoa(f.next)
		}
		f.calls = append(f.calls, call)
		f.mu.Unlock()
		if f.hook != nil {
			if err := f.hook(r, call); err != nil {
				return nil, err
			}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"ok":true,"result":{"message_id":%s}}`, call.id)))}, nil
	})
	return f
}

func (f *presentationFixture) snapshot() []presentationCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]presentationCall(nil), f.calls...)
}

func (f *presentationFixture) event(t *testing.T, conversation, request, receipt string, event core.Event) {
	t.Helper()
	event.RequestID = request
	if err := f.bot.DeliverEvent(t.Context(), conversation, receipt, event); err != nil {
		t.Error(err)
	}
}

func presentationTick() { synctest.Wait(); time.Sleep(time.Second); synctest.Wait() }

func TestStreamFinishesWithoutReadyNotice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPresentationFixture(t)
		f.event(t, "TG-7", "telegram:7:10", "", core.Event{Kind: core.EventAccepted})
		f.event(t, "TG-7", "telegram:7:10", "", core.Event{Kind: core.EventDelta, Text: "answer"})
		presentationTick()
		final := core.Event{Kind: core.EventFinal, Text: "answer", Done: true}
		f.event(t, "TG-7", "telegram:7:10", "terminal", final)
		calls := f.snapshot()
		if len(calls) != 2 || calls[0].method != "sendMessage" || calls[1].method != "editMessageText" || calls[1].id != calls[0].id {
			t.Fatalf("unexpected final delivery: %#v", calls)
		}
		restarted := New(f.bot.config, "fixture")
		restarted.SetHistory(f.bot.history)
		restarted.client = f.bot.client
		f.bot = restarted
		f.event(t, "TG-7", "telegram:7:10", "terminal", final)
		if len(f.snapshot()) != 2 {
			t.Fatal("replayed final was delivered again")
		}
	})
}

func TestStreamResumesAfterUserCommand(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPresentationFixture(t)
		f.event(t, "TG-7", "telegram:7:10", "", core.Event{Kind: core.EventAccepted})
		f.event(t, "TG-7", "telegram:7:10", "", core.Event{Kind: core.EventDelta, Text: "Hello"})
		presentationTick()
		f.event(t, "TG-7", "telegram:7:11", "", core.Event{Kind: core.EventAccepted})
		f.event(t, "TG-7", "telegram:7:11", "", core.Event{Kind: core.EventFinal, Text: "status", Done: true, Local: true})
		f.event(t, "TG-7", "telegram:7:10", "", core.Event{Kind: core.EventDelta, Text: " world"})
		presentationTick()
		answer := "Hello world!"
		f.event(t, "TG-7", "telegram:7:10", "terminal", core.Event{Kind: core.EventFinal, Text: answer, FinalText: &answer, Done: true})
		calls := f.snapshot()
		if len(calls) != 4 || calls[0].text != "Hello" || calls[1].text != "status" || calls[2].method != "sendMessage" || calls[2].text != "world" || calls[2].reply != 11 || calls[3].method != "editMessageText" || calls[3].id != calls[2].id || calls[3].text != "world!" {
			t.Fatalf("stream did not move after command: %#v", calls)
		}
		if len(f.bot.responses) != 0 {
			t.Fatal("finished presentation retained")
		}
	})
}

func TestStreamResumesAfterNotificationAndKeepsUnflushedText(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPresentationFixture(t)
		f.event(t, "TG-7", "telegram:7:10", "", core.Event{Kind: core.EventAccepted})
		f.event(t, "TG-7", "telegram:7:10", "", core.Event{Kind: core.EventDelta, Text: "alpha"})
		presentationTick()
		f.event(t, "TG-7", "telegram:7:10", "", core.Event{Kind: core.EventDelta, Text: " pending"})
		if err := f.bot.Deliver(t.Context(), "TG-7", "notification", "notification", nil); err != nil {
			t.Fatal(err)
		}
		f.event(t, "TG-7", "telegram:7:10", "", core.Event{Kind: core.EventDelta, Text: " omega"})
		presentationTick()
		answer := "alpha pending omega!"
		f.event(t, "TG-7", "telegram:7:10", "terminal", core.Event{Kind: core.EventFinal, Text: answer, FinalText: &answer, Done: true})
		calls := f.snapshot()
		if len(calls) != 4 || calls[1].text != "notification" || calls[2].method != "sendMessage" || calls[2].text != "omega" || calls[3].method != "editMessageText" || calls[3].id != calls[2].id || calls[3].text != "pending omega!" {
			t.Fatalf("notification boundary lost or reordered stream: %#v", calls)
		}
	})
}

func TestStreamFinalAfterBoundaryWithNoFurtherDelta(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPresentationFixture(t)
		f.event(t, "TG-7", "telegram:7:10", "", core.Event{Kind: core.EventAccepted})
		f.event(t, "TG-7", "telegram:7:10", "", core.Event{Kind: core.EventDelta, Text: "complete"})
		presentationTick()
		if err := f.bot.Deliver(t.Context(), "TG-7", "notification", "notice", nil); err != nil {
			t.Fatal(err)
		}
		answer := "complete"
		f.event(t, "TG-7", "telegram:7:10", "terminal", core.Event{Kind: core.EventFinal, Text: answer, FinalText: &answer, Done: true})
		if calls := f.snapshot(); len(calls) != 2 || calls[1].text != "notice" {
			t.Fatalf("already displayed final was repeated: %#v", calls)
		}
	})
}

func TestBoundaryCancelsAndJoinsInFlightPreview(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPresentationFixture(t)
		f.event(t, "TG-7", "telegram:7:10", "", core.Event{Kind: core.EventAccepted})
		f.event(t, "TG-7", "telegram:7:10", "", core.Event{Kind: core.EventDelta, Text: "first"})
		presentationTick()
		entered := make(chan struct{})
		f.hook = func(r *http.Request, call presentationCall) error {
			if call.method == "editMessageText" && call.id == "101" {
				close(entered)
				<-r.Context().Done()
				return r.Context().Err()
			}
			return nil
		}
		f.event(t, "TG-7", "telegram:7:10", "", core.Event{Kind: core.EventDelta, Text: " pending"})
		synctest.Wait()
		time.Sleep(time.Second)
		<-entered
		f.event(t, "TG-7", "telegram:7:11", "", core.Event{Kind: core.EventAccepted})
		f.hook = nil
		f.event(t, "TG-7", "telegram:7:10", "", core.Event{Kind: core.EventDelta, Text: " later"})
		presentationTick()
		calls := f.snapshot()
		if len(calls) != 3 || calls[2].method != "sendMessage" || calls[2].id == calls[0].id {
			t.Fatalf("old preview remained editable: %#v", calls)
		}
	})
}

func TestOtherConversationNotificationDoesNotFreezeStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPresentationFixture(t)
		f.event(t, "TG-7", "telegram:7:10", "", core.Event{Kind: core.EventAccepted})
		f.event(t, "TG-7", "telegram:7:10", "", core.Event{Kind: core.EventDelta, Text: "first"})
		presentationTick()
		if err := f.bot.Deliver(t.Context(), "TG-8", "notification", "other chat", nil); err != nil {
			t.Fatal(err)
		}
		f.event(t, "TG-7", "telegram:7:10", "", core.Event{Kind: core.EventDelta, Text: " second"})
		presentationTick()
		calls := f.snapshot()
		if len(calls) != 3 || calls[2].method != "editMessageText" || calls[2].id != calls[0].id || calls[2].text != "first second" {
			t.Fatalf("notification affected another conversation: %#v", calls)
		}
	})
}

func TestStreamSwitchesBetweenRequestsAndInterveningFinals(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPresentationFixture(t)
		f.event(t, "TG-7", "telegram:7:10", "", core.Event{Kind: core.EventAccepted})
		f.event(t, "TG-7", "telegram:7:10", "", core.Event{Kind: core.EventDelta, Text: "old"})
		presentationTick()
		f.event(t, "TG-7", "telegram:7:11", "", core.Event{Kind: core.EventAccepted})
		f.event(t, "TG-7", "telegram:7:11", "", core.Event{Kind: core.EventDelta, Text: "new"})
		presentationTick()
		f.event(t, "TG-7", "telegram:7:10", "old-final", core.Event{Kind: core.EventFinal, Text: "old complete", Done: true})
		f.event(t, "TG-7", "telegram:7:11", "", core.Event{Kind: core.EventDelta, Text: " more"})
		presentationTick()
		f.event(t, "TG-7", "telegram:7:11", "new-final", core.Event{Kind: core.EventFinal, Text: "new more!", Done: true})
		calls := f.snapshot()
		if len(calls) != 5 || calls[1].text != "new" || calls[2].text != "complete" || calls[3].method != "sendMessage" || calls[3].text != "more" || calls[4].id != calls[3].id || calls[4].text != "more!" {
			t.Fatalf("intervening final did not move stream: %#v", calls)
		}
		if len(f.bot.responses) != 0 {
			t.Fatal("finished presentations retained")
		}
	})
}

func TestBoundaryDoesNotDiscardTextFromTruncatedPreview(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPresentationFixture(t)
		text := strings.Repeat("&", 2000)
		chunks := telegramChunks(text, 4096)
		if len(chunks) < 2 {
			t.Fatal("fixture did not exceed Telegram's formatted message limit")
		}
		f.event(t, "TG-7", "telegram:7:10", "", core.Event{Kind: core.EventAccepted})
		f.event(t, "TG-7", "telegram:7:10", "", core.Event{Kind: core.EventDelta, Text: text})
		presentationTick()
		if err := f.bot.Deliver(t.Context(), "TG-7", "notification", "notice", nil); err != nil {
			t.Fatal(err)
		}
		f.event(t, "TG-7", "telegram:7:10", "terminal", core.Event{Kind: core.EventFinal, Text: text, Done: true})
		calls := f.snapshot()
		if len(calls) != 2+len(chunks) {
			t.Fatalf("final omitted text that was not in the truncated preview: %#v", calls)
		}
		for i, chunk := range chunks {
			if calls[i+2].text != chunk {
				t.Fatal("final chunks changed after the boundary")
			}
		}
	})
}

func TestIndependentErrorMovesActiveStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPresentationFixture(t)
		f.event(t, "TG-7", "telegram:7:10", "", core.Event{Kind: core.EventAccepted})
		f.event(t, "TG-7", "telegram:7:10", "", core.Event{Kind: core.EventDelta, Text: "first"})
		presentationTick()
		message := &telegramMessage{MessageID: 11, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 7, Type: "private"}}
		if err := f.bot.sendIndependent(t.Context(), message, "attachment error"); err != nil {
			t.Fatal(err)
		}
		f.event(t, "TG-7", "telegram:7:10", "", core.Event{Kind: core.EventDelta, Text: " next"})
		presentationTick()
		calls := f.snapshot()
		if len(calls) != 3 || calls[1].text != "attachment error" || calls[2].method != "sendMessage" || calls[2].text != "next" {
			t.Fatalf("error did not move the active stream: %#v", calls)
		}
	})
}

func TestNotificationContextDoesNotOwnConcurrentStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPresentationFixture(t)
		noticeCtx, cancel := context.WithCancel(t.Context())
		agentCtx, stopAgent := context.WithCancel(t.Context())
		emit := func(event core.Event) {
			event.RequestID = "telegram:7:10"
			if err := f.bot.DeliverEvent(agentCtx, "TG-7", "", event); err != nil {
				t.Error(err)
			}
		}
		entered, release := make(chan struct{}), make(chan struct{})
		f.hook = func(_ *http.Request, call presentationCall) error {
			if call.text == "notification" {
				close(entered)
				<-release
			}
			return nil
		}
		noticeDone := make(chan error, 1)
		go func() { noticeDone <- f.bot.Deliver(noticeCtx, "TG-7", "notice", "notification", nil) }()
		<-entered
		accepted := make(chan struct{})
		go func() {
			emit(core.Event{Kind: core.EventAccepted})
			close(accepted)
		}()
		for {
			f.bot.responseMu.Lock()
			refs := f.bot.responses["TG-7"].refs
			f.bot.responseMu.Unlock()
			if refs == 2 {
				break
			}
			runtime.Gosched()
		}
		close(release)
		if err := <-noticeDone; err != nil {
			t.Fatal(err)
		}
		<-accepted
		f.hook = nil
		emit(core.Event{Kind: core.EventDelta, Text: "answer"})
		presentationTick()
		cancel()
		synctest.Wait()
		if len(f.bot.responses) == 0 {
			t.Fatal("notification cancellation removed the unrelated active stream")
		}
		emit(core.Event{Kind: core.EventDelta, Text: " continues"})
		presentationTick()
		calls := f.snapshot()
		if calls[len(calls)-1].method != "editMessageText" {
			t.Fatalf("notification cancellation interrupted the active stream: %#v", calls)
		}
		stopAgent()
		synctest.Wait()
		if len(f.bot.responses) != 0 {
			t.Fatal("channel cancellation retained the active stream")
		}
	})
}
