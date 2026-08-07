package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frdel/spynel/internal/core"
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
	var final string
	for _, event := range events {
		switch event.Kind {
		case core.EventDelta:
			streamed.WriteString(event.Text)
		case core.EventFinal:
			final = event.Text
		}
	}
	if got, want := streamed.String(), "first message\nsecond message"; got != want {
		t.Fatalf("streamed response = %q, want %q", got, want)
	}
	if want := "first message\nsecond message"; final != want {
		t.Fatalf("final response = %q, want %q", final, want)
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
	if runtime.GOOS == "windows" {
		t.Skip("shell app-server fixture")
	}
	root := t.TempDir()
	script := filepath.Join(root, "fake-codex")
	fixture := `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*) printf '{"id":%s,"result":{}}\n' "$id" ;;
    *'"method":"initialized"'*) ;;
    *'"method":"thread/start"'*) printf '{"id":%s,"result":{"thread":{"id":"thr_test"}}}\n' "$id" ;;
    *'"method":"thread/resume"'*) printf '{"id":%s,"result":{"thread":{"id":"thr_test"}}}\n' "$id" ;;
    *'"method":"turn/start"'*)
      printf '{"id":%s,"result":{"turn":{"id":"turn_test","status":"inProgress"}}}\n' "$id"
      printf '{"method":"item/agentMessage/delta","params":{"threadId":"thr_test","turnId":"turn_test","delta":"hello "}}\n'
      sleep 0.1
      printf '{"method":"item/agentMessage/delta","params":{"threadId":"thr_test","turnId":"turn_test","delta":"world"}}\n'
      printf '{"method":"turn/completed","params":{"threadId":"thr_test","turn":{"id":"turn_test","status":"completed"}}}\n'
      ;;
    *'"method":"turn/steer"'*) printf '{"id":%s,"result":{"turnId":"turn_test"}}\n' "$id" ;;
  esac
done
`
	if err := os.WriteFile(script, []byte(fixture), 0o700); err != nil {
		t.Fatal(err)
	}
	codex, err := NewCodex(CodexConfig{Command: script, Cwd: root, SessionsFile: filepath.Join(root, "sessions.json")})
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
	var events []core.Event
	done := make(chan struct{})
	emit := func(event core.Event) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
		if event.Done {
			select {
			case <-done:
			default:
				close(done)
			}
		}
	}
	thread, steered, err := codex.Send(ctx, "chat:tui:local", "first", emit)
	if err != nil || steered || thread != "thr_test" {
		t.Fatalf("first send = %q, %t, %v", thread, steered, err)
	}
	_, steered, err = codex.Send(ctx, "chat:tui:local", "follow-up", emit)
	if err != nil || !steered {
		t.Fatalf("active follow-up was not steered: %t, %v", steered, err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("timed out waiting for streamed completion")
	}
	mu.Lock()
	defer mu.Unlock()
	foundFinal := false
	for _, event := range events {
		if event.Kind == core.EventFinal && strings.Contains(event.Text, "hello world") {
			foundFinal = true
		}
	}
	if !foundFinal {
		t.Fatalf("unexpected events: %#v", events)
	}
	if codex.ThreadID("chat:tui:local") != "thr_test" {
		t.Fatal("thread session was not persisted")
	}
}

func TestCodexInterruptsActiveTurn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell app-server fixture")
	}
	root := t.TempDir()
	script := filepath.Join(root, "fake-codex")
	fixture := `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*) printf '{"id":%s,"result":{}}\n' "$id" ;;
    *'"method":"initialized"'*) ;;
    *'"method":"thread/start"'*) printf '{"id":%s,"result":{"thread":{"id":"thr_stop"}}}\n' "$id" ;;
    *'"method":"turn/start"'*) printf '{"id":%s,"result":{"turn":{"id":"turn_stop","status":"inProgress"}}}\n' "$id" ;;
    *'"method":"turn/interrupt"'*'"threadId":"thr_stop"'*'"turnId":"turn_stop"'*)
      printf '{"id":%s,"result":{}}\n' "$id"
      printf '{"method":"turn/completed","params":{"threadId":"thr_stop","turn":{"id":"turn_stop","status":"interrupted"}}}\n'
      ;;
  esac
done
`
	if err := os.WriteFile(script, []byte(fixture), 0o700); err != nil {
		t.Fatal(err)
	}
	codex, err := NewCodex(CodexConfig{Command: script, Cwd: root, SessionsFile: filepath.Join(root, "sessions.json")})
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
	if runtime.GOOS == "windows" {
		t.Skip("shell app-server fixture")
	}
	root := t.TempDir()
	script := filepath.Join(root, "fake-codex")
	fixture := `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*) printf '{"id":%s,"result":{}}\n' "$id" ;;
    *'"method":"initialized"'*) ;;
    *'"method":"model/list"'*)
      printf '{"id":%s,"result":{"data":[{"id":"model-a","model":"model-a","displayName":"Model A","defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"low"},{"reasoningEffort":"medium"}],"isDefault":true}],"nextCursor":null}}\n' "$id"
      ;;
  esac
done
`
	if err := os.WriteFile(script, []byte(fixture), 0o700); err != nil {
		t.Fatal(err)
	}
	codex, err := NewCodex(CodexConfig{Command: script, Cwd: root})
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
	if len(models) != 1 || models[0].ID != "model-a" || models[0].DisplayName != "Model A" || !models[0].Default || strings.Join(models[0].Efforts, ",") != "low,medium" {
		t.Fatalf("Models() = %#v", models)
	}
}
