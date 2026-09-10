package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/history"
)

func TestConversationSubscriptionSendNotifyReconnectAndRestart(t *testing.T) {
	root := t.TempDir()
	election, server, _, stop, done := startTestServer(t, root)
	client := NewClient(election)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	received := make(chan EventEnvelope, 16)
	finished := make(chan error, 1)
	watchCtx, stopWatch := context.WithCancel(ctx)
	go func() {
		finished <- client.Events(watchCtx, "adapter", "", func(e EventEnvelope) error { received <- e; return nil })
	}()
	initial := receiveEnvelope(t, received)
	if initial.Cursor == "" {
		t.Fatalf("missing initial checkpoint: %#v", initial)
	}
	message := core.Message{Channel: "cli", Conversation: "adapter", Text: "request", SourceMessageID: "request-1"}
	var final bool
	if err := client.Handle(ctx, message, func(e core.Event) { final = final || e.Kind == core.EventFinal && e.Done && e.RequestID == "request-1" }); err != nil || !final {
		t.Fatalf("send final=%t: %v", final, err)
	}
	if err := client.Handle(ctx, message, nil); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("retry: %v", err)
	}
	id, err := client.Notify(ctx, "cli/adapter", "later ordinary notification")
	if err != nil {
		t.Fatal(err)
	}
	var notification history.ConversationEvent
	for notification.ID == "" {
		e := receiveEnvelope(t, received)
		if e.Event != nil && e.Event.Kind == "notification" {
			notification = *e.Event
		}
	}
	if notification.NotificationID != id {
		t.Fatal("outbox identity was lost")
	}
	stopWatch()
	<-finished
	stop()
	<-done
	_ = server.Service.Close()
	_ = election.Release(server.Token)
	election2, server2, target2, stop2, done2 := startTestServer(t, root)
	defer func() { stop2(); <-done2; _ = server2.Service.Close(); _ = election2.Release(server2.Token) }()
	client2 := NewClient(election2)
	replayCtx, stopReplay := context.WithCancel(ctx)
	go func() {
		finished <- client2.Events(replayCtx, "adapter", initial.Cursor, func(e EventEnvelope) error { received <- e; return nil })
	}()
	var replayed history.ConversationEvent
	for replayed.ID == "" {
		e := receiveEnvelope(t, received)
		if e.Event != nil && e.Event.Kind == "notification" {
			replayed = *e.Event
		}
	}
	stopReplay()
	<-finished
	if replayed.ID != notification.ID {
		t.Fatal("event identity changed across restart")
	}
	if err := client2.Handle(ctx, message, nil); err == nil {
		t.Fatal("restart retry dispatched")
	}
	target2.mu.Lock()
	sends := len(target2.keys)
	target2.mu.Unlock()
	if sends != 0 {
		t.Fatal("duplicate provider execution after restart")
	}
	response, err := client2.request(ctx, http.MethodGet, "/v1/conversation?conversation=adapter", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var snapshot ConversationSnapshot
	if err := json.NewDecoder(response.Body).Decode(&snapshot); err != nil || len(snapshot.Events) != 3 || snapshot.Cursor == "" || !snapshot.Bounded {
		t.Fatalf("resync: %#v %v", snapshot, err)
	}
}

func receiveEnvelope(t *testing.T, events <-chan EventEnvelope) EventEnvelope {
	t.Helper()
	select {
	case e := <-events:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("event timeout")
		return EventEnvelope{}
	}
}

type burstHarness struct{ *apiHarness }

func TestReturnedCommandErrorIsDurableAcrossTUIClientReconnect(t *testing.T) {
	election, server, _, stop, done := startTestServer(t, t.TempDir())
	defer func() { stop(); <-done; _ = server.Service.Close(); _ = election.Release(server.Token) }()
	ctx := context.Background()
	message := core.Message{Channel: "tui", Conversation: "errors", Text: "/extension remove ../escape", SourceMessageID: "error-request"}
	client := NewClient(election)
	if err := client.Handle(ctx, message, nil); err == nil || err.Error() != "invalid extension name" {
		t.Fatalf("TUI client error = %v", err)
	}
	// A new client reads the same committed error without sending it again.
	client = NewClient(election)
	var screen core.Event
	message.Text, message.SourceMessageID = "/resume", "resume-request"
	if err := client.Handle(ctx, message, func(event core.Event) { screen = event }); err != nil {
		t.Fatal(err)
	}
	if screen.Screen == nil {
		t.Fatal("missing resume picker")
	}
	for _, control := range screen.Screen.Controls {
		if strings.Contains(control.Value, "errors") && strings.HasPrefix(control.Key, "resume:") {
			branch, err := client.ScreenAction(ctx, "resume", control.Key, nil)
			if err != nil || branch == nil {
				t.Fatalf("resume = %#v, %v", branch, err)
			}
			var count int
			for _, entry := range branch.Transcript {
				if entry.Role == "error" && entry.Text == "invalid extension name" {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("resumed TUI contains %d error messages", count)
			}
			return
		}
	}
	t.Fatal("saved error conversation missing from resume picker")
}

func (h burstHarness) Send(_ context.Context, _, _ string, emit core.Emit) (string, bool, error) {
	for i := 0; i < 300; i++ {
		emit(core.Event{Kind: core.EventDelta, Text: "x"})
	}
	emit(core.Event{Kind: core.EventFinal, Text: "continuing", Done: true, Continues: true})
	emit(core.Event{Kind: core.EventFinal, Text: "finished", Done: true})
	return "fixture", false, nil
}

type disconnectHarness struct {
	*apiHarness
	cancelled chan struct{}
}

func (h disconnectHarness) Send(ctx context.Context, key, _ string, emit core.Emit) (string, bool, error) {
	h.mu.Lock()
	h.active[key] = true
	h.mu.Unlock()
	emit(core.Event{Kind: core.EventDelta, Text: "partial"})
	go func() {
		<-ctx.Done()
		h.mu.Lock()
		delete(h.active, key)
		h.mu.Unlock()
		emit(core.Event{Kind: core.EventError, Text: "disconnected", Done: true})
		close(h.cancelled)
	}()
	return "fixture", false, nil
}

func TestMessageDisconnectCancelsProviderButSubscriptionDoesNot(t *testing.T) {
	election, server, target, stop, done := startTestServer(t, t.TempDir())
	defer func() { stop(); <-done; _ = server.Service.Close() }()
	disconnected := make(chan struct{})
	server.Service.Harness = disconnectHarness{target, disconnected}
	client := NewClient(election)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	started := make(chan struct{}, 1)
	go func() {
		result <- client.Handle(ctx, core.Message{Channel: "cli", Conversation: "disconnect", Text: "hello"}, func(e core.Event) {
			if e.Kind == core.EventDelta {
				started <- struct{}{}
			}
		})
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("message did not start")
	}
	watchCtx, stopWatch := context.WithCancel(context.Background())
	watchResult := make(chan error, 1)
	go func() {
		watchResult <- client.Events(watchCtx, "disconnect", "", func(EventEnvelope) error { stopWatch(); return context.Canceled })
	}()
	select {
	case <-watchResult:
	case <-time.After(5 * time.Second):
		stopWatch()
		cancel()
		t.Fatal("subscription did not close")
	}
	if !target.IsActive("chat:cli:disconnect") {
		t.Fatal("subscription disconnect stopped provider")
	}
	cancel()
	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("HTTP disconnect did not cancel provider context")
	}
	if err := <-result; err == nil {
		t.Fatal("disconnect claimed completion")
	}
	events, _, err := server.Service.History.EventSnapshot("cli", "disconnect")
	if err != nil || events[len(events)-1].Kind != "error" {
		t.Fatalf("disconnect terminal not retained: %#v %v", events, err)
	}
}

func TestMessageDrainsSynchronousProviderAndRejectsInvalidBoundary(t *testing.T) {
	election, server, target, cancel, done := startTestServer(t, t.TempDir())
	defer func() { cancel(); <-done; _ = server.Service.Close() }()
	// No requests are in flight while replacing this fixture.
	server.Service.Harness = burstHarness{target}
	client := NewClient(election)
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	deltas := 0
	if err := client.Handle(ctx, core.Message{Channel: "cli", Conversation: "burst", Text: "synthetic"}, func(e core.Event) {
		if e.Kind == core.EventDelta {
			deltas++
		}
	}); err != nil || deltas != 300 {
		t.Fatalf("sync stream: %d %v", deltas, err)
	}
	for _, body := range []string{
		`{"channel":"telegram","conversation":"TG-1","text":"spoof"}`,
		`{"channel":"cli","conversation":"../escape","text":"bad"}`,
		`{"channel":"cli","conversation":"a","text":"","unknown":1}`,
		`{"channel":"cli","conversation":"a","text":"ok"} {}`,
	} {
		response, err := client.request(ctx, http.MethodPost, "/v1/message", []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid boundary: %d", response.StatusCode)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/events?conversation=burst", nil)
	w := httptest.NewRecorder()
	server.authorize(server.events)(w, request)
	if w.Code != http.StatusUnauthorized {
		t.Fatal("missing event authentication")
	}
	server.subscribers.Store(maxSubscribers)
	w = httptest.NewRecorder()
	server.events(w, request)
	if w.Code != http.StatusTooManyRequests || server.subscribers.Load() != maxSubscribers {
		t.Fatal("subscriber capacity was not bounded")
	}
}

type blockedWriter struct {
	header   http.Header
	deadline time.Time
}

func (w *blockedWriter) Header() http.Header { return w.header }
func (w *blockedWriter) WriteHeader(int)     {}
func (w *blockedWriter) Write([]byte) (int, error) {
	if w.deadline.IsZero() {
		panic("write without a deadline")
	}
	return 0, context.DeadlineExceeded
}
func (w *blockedWriter) SetWriteDeadline(deadline time.Time) error { w.deadline = deadline; return nil }

func TestSlowEventConsumerReleasesSlotAndCursorErrorsAreExplicit(t *testing.T) {
	_, server, _, cancel, done := startTestServer(t, t.TempDir())
	defer func() { cancel(); <-done; _ = server.Service.Close() }()
	w := &blockedWriter{header: make(http.Header)}
	server.events(w, httptest.NewRequest(http.MethodGet, "/v1/events?conversation=slow", nil))
	if server.subscribers.Load() != 0 {
		t.Fatal("slow subscriber leaked a slot")
	}
	recorder := httptest.NewRecorder()
	server.events(recorder, httptest.NewRequest(http.MethodGet, "/v1/events?conversation=slow&after=invalid", nil))
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "invalid_cursor") {
		t.Fatalf("cursor error: %d %s", recorder.Code, recorder.Body)
	}
}

func TestMessageClientReportsTruncatedResponse(t *testing.T) {
	election, _, _, cancel, done := startTestServer(t, t.TempDir())
	defer func() { cancel(); <-done }()
	client := NewClient(election)
	client.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"event":{"kind":"delta","text":"partial"}}`))}, nil
	})
	if err := client.Handle(context.Background(), core.Message{}, nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated response silently succeeded: %v", err)
	}
}
