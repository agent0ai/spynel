package harness

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
)

func TestCodexPreservesMultipleAgentMessages(t *testing.T) {
	var events []core.Event
	state := &turnState{
		threadID: "thread-messages",
		turnID:   "turn-messages",
		emit:     func(event core.Event) { events = append(events, event) },
	}
	codex := &Codex{active: map[string]*turnState{state.threadID: state}, deferred: map[string][]wireMessage{}}
	notify := func(method string, params map[string]any) {
		data, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		codex.handleNotification(wireMessage{Method: method, Params: data})
	}
	base := map[string]any{"threadId": state.threadID, "turnId": state.turnID}
	notify("item/agentMessage/delta", mergeNotification(base, map[string]any{"delta": "first message"}))
	notify("item/completed", mergeNotification(base, map[string]any{"item": map[string]any{"type": "agentMessage", "phase": "commentary", "text": "first message"}}))
	notify("item/agentMessage/delta", mergeNotification(base, map[string]any{"delta": "second message"}))
	notify("item/completed", mergeNotification(base, map[string]any{"item": map[string]any{"type": "agentMessage", "phase": "final_answer", "text": "second message"}}))
	notify("turn/completed", map[string]any{"threadId": state.threadID, "turn": map[string]any{"id": state.turnID, "status": "completed"}})

	var streamed strings.Builder
	var final core.Event
	for _, event := range events {
		switch event.Kind {
		case core.EventDelta:
			streamed.WriteString(event.Text)
		case core.EventFinal:
			final = event
		}
	}
	if got, want := streamed.String(), "first message\nsecond message"; got != want {
		t.Fatalf("streamed response = %q, want %q", got, want)
	}
	if want := "first message\nsecond message"; final.Text != want {
		t.Fatalf("final response = %q, want %q", final.Text, want)
	}
	if final.FinalText == nil || *final.FinalText != "second message" {
		t.Fatalf("last assistant item = %#v, want second message", final.FinalText)
	}
}

func mergeNotification(base, extra map[string]any) map[string]any {
	result := make(map[string]any, len(base)+len(extra))
	for key, value := range base {
		result[key] = value
	}
	for key, value := range extra {
		result[key] = value
	}
	return result
}

func TestCodexRetryRecoveryLifecycle(t *testing.T) {
	for _, resumed := range []string{"item/agentMessage/delta", "item/reasoning/summaryTextDelta", "item/reasoning/textDelta", "item/plan/delta"} {
		t.Run(resumed, func(t *testing.T) {
			var events []core.Event
			state := &turnState{threadID: "thread", turnID: "turn", emit: func(event core.Event) { events = append(events, event) }}
			codex := &Codex{active: map[string]*turnState{"thread": state}, deferred: map[string][]wireMessage{}}
			notify := func(method string, extra map[string]any) {
				t.Helper()
				data, err := json.Marshal(mergeNotification(map[string]any{"threadId": "thread", "turnId": "turn"}, extra))
				if err != nil {
					t.Fatal(err)
				}
				codex.handleNotification(wireMessage{Method: method, Params: data})
			}
			check := func(kind, execution string) {
				t.Helper()
				if len(events) == 0 {
					t.Fatal("missing lifecycle event")
				}
				event := events[0]
				if event.Kind != kind || event.Execution == nil || event.Execution.State != execution || event.Done {
					t.Fatalf("event = %#v, want nonterminal %s/%s", event, kind, execution)
				}
			}
			for range 2 {
				events = nil
				notify("error", map[string]any{"willRetry": true, "error": map[string]any{"message": "synthetic connection failure"}})
				check(core.EventStatus, "reconnecting")
				if events[0].Execution.Detail != "synthetic connection failure" {
					t.Fatal("retry diagnostic was lost")
				}
			}
			events = nil
			notify("item/commandExecution/outputDelta", map[string]any{"delta": "still working"})
			notify(resumed, map[string]any{"delta": ""})
			notify(resumed, map[string]any{"turnId": "old-turn", "delta": "late text"})
			notify(resumed, map[string]any{"turnId": "", "delta": "unscoped text"})
			if len(events) != 0 {
				t.Fatalf("unrelated activity cleared reconnect: %#v", events)
			}
			notify(resumed, map[string]any{"delta": "fresh model output"})
			check(core.EventStatus, "running")
			events = nil
			notify(resumed, map[string]any{"delta": "more output"})
			for _, event := range events {
				if event.Execution != nil {
					t.Fatal("ordinary output repeated the recovery transition")
				}
			}

			// Missing, malformed, and false retry flags must never authorize recovery.
			for _, retry := range []any{nil, "true", false} {
				events = nil
				notify("error", map[string]any{"willRetry": retry, "error": map[string]any{"message": "unrecoverable"}})
				check(core.EventError, "error")
				events = nil
				notify(resumed, map[string]any{"delta": "late text"})
				for _, event := range events {
					if event.Execution != nil {
						t.Fatal("output cleared an outstanding error")
					}
				}
			}
		})
	}
}

func TestCodexAdmissionDoesNotOverwriteDeferredRetry(t *testing.T) {
	command, root, _ := portableHarnessFixture(t, "codex-interrupt")
	codex, err := NewCodex(CodexConfig{Command: command, Cwd: root, SessionsFile: filepath.Join(root, "sessions.json")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := codex.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer codex.Close()
	// The server can notify before turn/start returns its ID to Send.
	data := json.RawMessage(`{"threadId":"thr_stop","turnId":"turn_stop","willRetry":true,"error":{"message":"retry pending"}}`)
	codex.handleNotification(wireMessage{Method: "error", Params: data})
	var states []string
	if _, _, err := codex.Send(ctx, "admission", "synthetic work", func(event core.Event) {
		if event.Execution != nil {
			states = append(states, event.Execution.State)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(states, ","); got != "running,reconnecting" {
		t.Fatalf("admission overwrote deferred retry: %s", got)
	}
}

func TestCodexCompletedTurnIgnoresLateNotifications(t *testing.T) {
	for _, status := range []string{"completed", "failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			var events []core.Event
			state := &turnState{threadID: "thread", turnID: "turn", emit: func(event core.Event) { events = append(events, event) }}
			codex := &Codex{active: map[string]*turnState{"thread": state}, deferred: map[string][]wireMessage{}}
			data, _ := json.Marshal(map[string]any{"threadId": "thread", "turn": map[string]any{"id": "turn", "status": status}})
			codex.handleNotification(wireMessage{Method: "turn/completed", Params: data})
			if len(events) != 1 || !events[0].Done {
				t.Fatalf("terminal events = %#v", events)
			}
			// A later turn in the same thread cannot accept the old turn's events.
			codex.active["thread"] = &turnState{threadID: "thread", turnID: "next", emit: state.emit}
			for _, method := range []string{"item/agentMessage/delta", "error", "turn/completed"} {
				data, _ := json.Marshal(map[string]any{"threadId": "thread", "turnId": "turn", "delta": "late", "willRetry": true})
				codex.handleNotification(wireMessage{Method: method, Params: data})
			}
			if len(events) != 1 || codex.active["thread"] == nil {
				t.Fatalf("old turn mutated a new execution: %#v", events)
			}
		})
	}
}

func TestCodexDeferredNotificationsPreserveWireOrder(t *testing.T) {
	codex, err := NewCodex(CodexConfig{})
	if err != nil {
		t.Fatal(err)
	}
	requests, client := io.Pipe()
	responses, server := io.Pipe()
	codex.stdin = client
	codex.session["fixture"] = "thread"
	codex.loaded["thread"] = true
	readDone := make(chan struct{})
	go func() { codex.readLoop(responses); close(readDone) }()
	retryEntered := make(chan struct{})
	releaseRetry := make(chan struct{})
	var release sync.Once
	defer func() {
		release.Do(func() { close(releaseRetry) })
		codex.Close()
		requests.Close()
		server.Close()
		<-readDone
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverDone := make(chan error, 1)
	go func() {
		var request wireMessage
		if err := json.NewDecoder(requests).Decode(&request); err != nil {
			serverDone <- err
			return
		}
		enc := json.NewEncoder(server)
		messages := []wireMessage{
			{Method: "error", Params: json.RawMessage(`{"threadId":"thread","turnId":"turn","willRetry":true,"error":{"message":"retry pending"}}`)},
			{ID: request.ID, Result: json.RawMessage(`{"turn":{"id":"turn"}}`)},
			{Method: "item/reasoning/textDelta", Params: json.RawMessage(`{"threadId":"thread","turnId":"turn","delta":"resumed"}`)},
			{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread","turn":{"id":"turn","status":"completed"}}`)},
		}
		for i, message := range messages {
			if i == 2 {
				select {
				case <-retryEntered:
				case <-ctx.Done():
					serverDone <- ctx.Err()
					return
				}
			}
			if err := enc.Encode(message); err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- nil
	}()
	var mu sync.Mutex
	var states []string
	recovered := make(chan struct{}, 1)
	completed := make(chan struct{}, 1)
	sent := make(chan error, 1)
	go func() {
		_, _, err := codex.Send(ctx, "fixture", "work", func(event core.Event) {
			if event.Execution == nil {
				return
			}
			if event.Execution.State == "reconnecting" {
				close(retryEntered)
				<-releaseRetry // Represent slow lease/archive publication.
			}
			mu.Lock()
			states = append(states, event.Execution.State)
			mu.Unlock()
			if event.Execution.State == "running" && event.Text == "" {
				recovered <- struct{}{}
			}
			if event.Done {
				completed <- struct{}{}
			}
		})
		sent <- err
	}()
	select {
	case <-retryEntered:
	case <-ctx.Done():
		t.Fatal("retry was not delivered")
	}
	// Let an incorrectly concurrent callback finish; correct delivery waits
	// behind the paused retry. Bound the negative check so serialization proceeds.
	select {
	case <-recovered:
	case <-time.After(50 * time.Millisecond):
	}
	release.Do(func() { close(releaseRetry) })
	select {
	case err := <-sent:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("Send did not finish")
	}
	select {
	case <-completed:
	case <-ctx.Done():
		t.Fatal("completion was not delivered")
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(states, ","); got != "running,reconnecting,running,finishing" {
		t.Fatalf("provider sent retry then recovery and completion, application observed %s", got)
	}
}

func TestCodexNormalizesSandboxPoliciesForAppServer(t *testing.T) {
	defaultCodex, err := NewCodex(CodexConfig{SessionsFile: filepath.Join(t.TempDir(), "sessions.json")})
	if err != nil {
		t.Fatal(err)
	}
	if defaultCodex.threadSandbox() != "danger-full-access" {
		t.Fatalf("default Codex sandbox = %q", defaultCodex.threadSandbox())
	}
	tests := []struct {
		configured string
		policyType string
		thread     string
		roots      bool
	}{
		{configured: "read-only", policyType: "readOnly", thread: "read-only"},
		{configured: "workspace-write", policyType: "workspaceWrite", thread: "workspace-write", roots: true},
		{configured: "danger-full-access", policyType: "dangerFullAccess", thread: "danger-full-access"},
	}
	for _, test := range tests {
		codex := &Codex{config: CodexConfig{Sandbox: test.configured, Cwd: "/workspace", Network: true}}
		policy := codex.sandboxPolicy()
		if policy["type"] != test.policyType || codex.threadSandbox() != test.thread {
			t.Fatalf("sandbox %q = policy %#v, thread %q", test.configured, policy, codex.threadSandbox())
		}
		_, hasRoots := policy["writableRoots"]
		if hasRoots != test.roots {
			t.Fatalf("sandbox %q writable roots = %t, want %t", test.configured, hasRoots, test.roots)
		}
	}
}

func TestCodexAppServerStartsThreadsStreamsAndSteers(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "codex-lifecycle")
	sessionsPath := filepath.Join(root, "sessions.json")
	codex, err := NewCodex(CodexConfig{Command: command, Cwd: root, SessionsFile: sessionsPath})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := codex.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer codex.Close()
	var mu sync.Mutex
	var firstEvents []core.Event
	var secondEvents []core.Event
	released := make(chan struct{})
	done := make(chan struct{})
	firstEmit := func(event core.Event) {
		mu.Lock()
		firstEvents = append(firstEvents, event)
		mu.Unlock()
		if event.Done && event.Kind == core.EventStatus {
			select {
			case <-released:
			default:
				close(released)
			}
		}
	}
	secondEmit := func(event core.Event) {
		mu.Lock()
		secondEvents = append(secondEvents, event)
		mu.Unlock()
		if event.Done && event.Kind == core.EventFinal {
			select {
			case <-done:
			default:
				close(done)
			}
		}
	}
	thread, steered, err := codex.Send(ctx, "chat:tui:local", "first", firstEmit)
	if err != nil || steered || thread != "thr_test" {
		t.Fatalf("first send = %q, %t, %v", thread, steered, err)
	}
	_, steered, err = codex.Send(ctx, "chat:tui:local", "follow-up", secondEmit)
	if err != nil || !steered {
		t.Fatalf("active follow-up was not steered: %t, %v", steered, err)
	}
	select {
	case <-released:
	case <-ctx.Done():
		t.Fatal("timed out waiting for previous emitter release")
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("timed out waiting for streamed completion")
	}
	mu.Lock()
	for _, event := range firstEvents {
		if event.Kind == core.EventFinal {
			t.Fatalf("previous emitter received the steered final: %#v", firstEvents)
		}
	}
	foundStart, foundFinal := false, false
	for _, event := range firstEvents {
		foundStart = foundStart || event.Execution != nil && event.Execution.State == "running"
	}
	for _, event := range secondEvents {
		if event.Kind == core.EventFinal && strings.Contains(event.Text, "hello world") {
			foundFinal = event.Execution != nil && event.Execution.State == "finishing"
		}
	}
	if !foundStart || !foundFinal {
		t.Fatalf("unexpected latest-emitter events: %#v", secondEvents)
	}
	if codex.ThreadID("chat:tui:local") != "thr_test" {
		t.Fatal("thread session was not persisted")
	}
	mu.Unlock()
	if err := codex.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewCodex(CodexConfig{Command: command, Cwd: root, SessionsFile: sessionsPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Start(ctx); err != nil {
		t.Fatal(err)
	}
	restartDone := make(chan struct{})
	if thread, steered, err := restarted.Send(ctx, "chat:tui:local", "after restart", func(event core.Event) {
		if event.Done {
			close(restartDone)
		}
	}); err != nil || steered || thread != "thr_test" {
		t.Fatalf("restart Send() = %q, %t, %v", thread, steered, err)
	}
	select {
	case <-restartDone:
	case <-ctx.Done():
		t.Fatal("timed out waiting for resumed Codex turn")
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
	started, resumed, portable := false, false, false
	for _, record := range readFixtureRecords(t, logPath) {
		started = started || record.Method == "thread/start"
		resumed = resumed || record.Method == "thread/resume"
		portable = portable || record.Kind == "invocation" && record.Cwd == root && record.Executable == command
	}
	if !started || !resumed || !portable {
		t.Fatalf("portable Codex fixture evidence = start %t, resume %t, paths %t", started, resumed, portable)
	}
}

func TestCodexTransportLossEmitsStructuredFatalFailure(t *testing.T) {
	events := make(chan core.Event, 1)
	codex := &Codex{pending: map[int]chan rpcResponse{}, active: map[string]*turnState{
		"thread": {threadID: "thread", turnID: "turn", emit: func(event core.Event) { events <- event }},
	}}
	codex.failAll(errors.New("connection lost"))
	event := <-events
	if !event.Done || event.Kind != core.EventError || event.Execution == nil || event.Execution.State != "error" || !strings.Contains(event.Execution.Detail, "connection lost") {
		t.Fatalf("transport loss event = %#v", event)
	}
}

// Deliver a successful turn/start reply and EOF before the request write
// returns, so admission necessarily observes an already-exited provider.
type codexExitAfterReply struct{ *Codex }

func (c codexExitAfterReply) Write(data []byte) (int, error) {
	var request wireMessage
	if err := json.Unmarshal(data, &request); err != nil {
		return 0, err
	}
	c.readLoop(strings.NewReader(`{"id":` + string(request.ID) + `,"result":{"turn":{"id":"turn"}}}` + "\n"))
	return len(data), nil
}

func (codexExitAfterReply) Close() error { return nil }

func TestCodexTransportLossFencesPendingAdmission(t *testing.T) {
	codex, err := NewCodex(CodexConfig{})
	if err != nil {
		t.Fatal(err)
	}
	codex.stdin = codexExitAfterReply{codex}
	codex.session["fixture"] = "thread"
	codex.loaded["thread"] = true
	defer codex.Close()
	var events []core.Event
	for range 2 {
		_, _, err := codex.Send(context.Background(), "fixture", "work", func(event core.Event) { events = append(events, event) })
		if err == nil || !strings.Contains(err.Error(), "stream closed") || codex.IsActive("fixture") || len(events) != 0 {
			t.Fatalf("exited provider admitted work: err=%v, active=%t, events=%#v", err, codex.IsActive("fixture"), events)
		}
	}
}

func TestCodexInterruptsActiveTurn(t *testing.T) {
	command, root, _ := portableHarnessFixture(t, "codex-interrupt")
	codex, err := NewCodex(CodexConfig{Command: command, Cwd: root, SessionsFile: filepath.Join(root, "sessions.json")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := codex.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer codex.Close()
	done := make(chan struct{})
	if _, _, err := codex.Send(ctx, "chat:tui:local", "long task", func(event core.Event) {
		if event.Done {
			select {
			case <-done:
			default:
				close(done)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	interrupted, err := codex.Interrupt(ctx, "chat:tui:local")
	if err != nil || !interrupted {
		t.Fatalf("Interrupt() = %t, %v", interrupted, err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("timed out waiting for interrupted turn completion")
	}
	deadline := time.Now().Add(time.Second)
	for codex.IsActive("chat:tui:local") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if codex.IsActive("chat:tui:local") {
		t.Fatal("turn remained active after interruption")
	}
	interrupted, err = codex.Interrupt(ctx, "chat:tui:local")
	if err != nil || interrupted {
		t.Fatalf("inactive Interrupt() = %t, %v", interrupted, err)
	}
}

func TestCodexDiscoversPickerVisibleModels(t *testing.T) {
	command, root, _ := portableHarnessFixture(t, "codex-models")
	codex, err := NewCodex(CodexConfig{Command: command, Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := codex.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer codex.Close()
	models, err := codex.Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "model-a" || models[0].DisplayName != "Model A" || !models[0].Default || strings.Join(models[0].Efforts, ",") != "low,medium,ultra" || len(models[0].ServiceModes) != 1 || models[0].ServiceModes[0].ID != "fast" {
		t.Fatalf("Models() = %#v", models)
	}
}

func TestCodexPassesCapturedEffortAndServiceTier(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "codex-lifecycle")
	codex, err := NewCodex(CodexConfig{Command: command, Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := codex.Start(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{}, 1)
	selection := InferenceSelection{Model: "model-a", Effort: "medium", ServiceMode: "fast"}
	if _, _, err := codex.SendWithInference(ctx, "chat", "test", selection, func(event core.Event) {
		if event.Done {
			done <- struct{}{}
		}
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("timed out")
	}
	_ = codex.Close()
	for _, record := range readFixtureRecords(t, logPath) {
		if record.Method != "turn/start" {
			continue
		}
		var params map[string]any
		if err := json.Unmarshal(record.Params, &params); err != nil {
			t.Fatal(err)
		}
		if params["model"] != "model-a" || params["effort"] != "medium" || params["serviceTier"] != "fast" {
			t.Fatalf("turn/start params = %#v", params)
		}
		return
	}
	t.Fatal("turn/start request not recorded")
}
