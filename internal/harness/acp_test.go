package harness

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
)

func TestACPStreamsPersistsAndResumesSessions(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "acp-lifecycle")
	sessionsPath := filepath.Join(root, "sessions.json")
	config := HarnessConfig{
		Command: command, Args: []string{"--stdio", "value with spaces"}, Cwd: root,
		Model: "model-a", Effort: "high", Sandbox: "read-only", SessionsFile: sessionsPath, Version: "test",
	}
	adapter, err := NewACP(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := adapter.Start(ctx); err != nil {
		t.Fatal(err)
	}
	events := make(chan core.Event, 32)
	threadID, steered, err := adapter.Send(ctx, "chat", "test prompt", func(event core.Event) { events <- event })
	if err != nil || steered || threadID != "acp-session" {
		t.Fatalf("ACP Send() = %q, %t, %v", threadID, steered, err)
	}
	var streamed strings.Builder
	var final core.Event
	foundTool := false
	for !final.Done {
		select {
		case event := <-events:
			if event.Kind == core.EventDelta {
				streamed.WriteString(event.Text)
			}
			foundTool = foundTool || event.Kind == core.EventStatus && strings.Contains(event.Text, "Edit file")
			if event.Done {
				final = event
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for ACP result")
		}
	}
	if streamed.String() != "hello world" || final.Kind != core.EventFinal || final.Text != "hello world" || final.FinalText == nil || *final.FinalText != "hello world" || !foundTool {
		t.Fatalf("ACP events streamed=%q final=%#v tool=%t", streamed.String(), final, foundTool)
	}
	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewACP(config)
	if err != nil || restarted.ThreadID("chat") != "acp-session" {
		t.Fatalf("persisted ACP session = %q, %v", restarted.ThreadID("chat"), err)
	}
	if err := restarted.Start(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{}, 1)
	if threadID, steered, err := restarted.Send(ctx, "chat", "continued", func(event core.Event) {
		if event.Done {
			done <- struct{}{}
		}
	}); err != nil || steered || threadID != "acp-session" {
		t.Fatalf("resumed ACP Send() = %q, %t, %v", threadID, steered, err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("timed out waiting for resumed ACP turn")
	}
	_ = restarted.Close()

	invocations, resumed, configured, rejected := 0, 0, 0, false
	for _, record := range readFixtureRecords(t, logPath) {
		if record.Kind == "invocation" {
			invocations++
			if record.Cwd != root || record.Executable != command || len(record.Args) != 2 || record.Args[0] != "--stdio" || record.Args[1] != "value with spaces" {
				t.Fatalf("portable ACP invocation = %#v", record)
			}
		}
		resumed = resumed + boolInt(record.Method == "session/resume")
		if record.Method == "session/set_config_option" {
			configured++
			params := string(record.Params)
			model := strings.Contains(params, `"configId":"model"`) && strings.Contains(params, `"value":"model-a"`)
			thought := strings.Contains(params, `"configId":"thought"`) && strings.Contains(params, `"value":"high"`)
			if !strings.Contains(params, `"sessionId":"acp-session"`) || (!model && !thought) {
				t.Fatalf("ACP config option request = %s", record.Params)
			}
		}
		if record.Method == "permission-response" && strings.Contains(string(record.Params), `"optionId":"reject"`) {
			rejected = true
		}
	}
	if invocations != 2 || resumed != 1 || configured != 2 || !rejected {
		t.Fatalf("ACP evidence = invocations %d, resumes %d, config options %d, read-only rejected edit %t", invocations, resumed, configured, rejected)
	}
}

func TestACPCancelNotificationTerminatesPrompt(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "acp-interrupt")
	adapter, err := NewACP(HarnessConfig{Command: command, Cwd: root, SessionsFile: filepath.Join(root, "sessions.json")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := adapter.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	done := make(chan core.Event, 1)
	if _, _, err := adapter.Send(ctx, "chat", "work", func(event core.Event) {
		if event.Done {
			done <- event
		}
	}); err != nil {
		t.Fatal(err)
	}
	if stopped, err := adapter.Interrupt(ctx, "chat"); err != nil || !stopped {
		t.Fatalf("ACP interrupt = %t, %v", stopped, err)
	}
	select {
	case event := <-done:
		if event.Kind != core.EventError || !strings.Contains(event.Text, "cancelled") {
			t.Fatalf("ACP cancelled terminal = %#v", event)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for ACP cancellation")
	}
	foundCancel, cancelledPermission := false, false
	for _, record := range readFixtureRecords(t, logPath) {
		foundCancel = foundCancel || record.Method == "session/cancel"
		cancelledPermission = cancelledPermission || record.Method == "permission-response" &&
			strings.Contains(string(record.Params), `"outcome":{"outcome":"cancelled"}`)
	}
	if !foundCancel || !cancelledPermission {
		t.Fatalf("ACP cancellation evidence = cancel notification %t, late permission cancelled %t", foundCancel, cancelledPermission)
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
