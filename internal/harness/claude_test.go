package harness

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
)

func TestClaudeRecordsNonzeroProcessExitAfterResult(t *testing.T) {
	command, root, _ := portableHarnessFixture(t, "claude-result-nonzero")
	var diagnostics bytes.Buffer
	claude, err := NewClaude(HarnessConfig{Command: command, Cwd: root, ApprovalPolicy: "plan", Stderr: &diagnostics})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := claude.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer claude.Close()
	done := make(chan core.Event, 1)
	if _, _, err := claude.Send(ctx, "chat", "test", func(event core.Event) {
		if event.Done {
			done <- event
		}
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-done:
		if event.Kind != core.EventFinal {
			t.Fatalf("terminal event = %#v", event)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for Claude result")
	}
	got := diagnostics.String()
	if !strings.Contains(got, "post-result process failure") || !strings.Contains(got, "returned a result but its process failed: exit status 7") {
		t.Fatalf("post-result exit evidence = %q", got)
	}
}

func TestClaudeLifecycleDiagnosticMappingIsNarrowAndSupportsUnknownTotals(t *testing.T) {
	tests := []struct {
		line    string
		state   string
		attempt int
		total   int
	}{
		{"Attempting to reconnect... (attempt 2/5)", "reconnecting", 2, 5},
		{"Attempting to reconnect… (attempt 3)", "reconnecting", 3, 0},
		{"Attempting to reconnect", "reconnecting", 0, 0},
		{"Connection restored", "running", 0, 0},
		{"error: reconnecting to unrelated tool", "", 0, 0},
		{"Attempting to reconnect... attempt 2/5", "", 0, 0},
	}
	for _, test := range tests {
		got := parseClaudeLifecycleDiagnostic(test.line)
		if test.state == "" {
			if got != nil {
				t.Fatalf("%q unexpectedly mapped to %#v", test.line, got)
			}
			continue
		}
		if got == nil || got.State != test.state || got.ReconnectAttempt != test.attempt || got.ReconnectTotal != test.total {
			t.Fatalf("%q = %#v", test.line, got)
		}
	}
}

func TestClaudeLifecycleWriterMapsOnlyCompleteReconnectDiagnostics(t *testing.T) {
	events := make(chan core.Event, 2)
	turn := &claudeTurn{emit: func(event core.Event) { events <- event }}
	var output bytes.Buffer
	writer := &claudeLifecycleWriter{dst: &output, turn: turn}
	for _, chunk := range []string{"Attempting to re", "connect... (attempt 2/5)\nordinary reconnect prose\n"} {
		if _, err := writer.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case event := <-events:
		if event.Execution == nil || event.Execution.State != "reconnecting" || event.Execution.ReconnectAttempt != 2 || event.Execution.ReconnectTotal != 5 {
			t.Fatalf("mapped event = %#v", event)
		}
	default:
		t.Fatal("complete reconnect diagnostic was not mapped")
	}
	select {
	case event := <-events:
		t.Fatalf("fallback prose unexpectedly mapped: %#v", event)
	default:
	}
	if output.String() != "Attempting to reconnect... (attempt 2/5)\nordinary reconnect prose\n" {
		t.Fatalf("diagnostic output changed: %q", output.String())
	}
}

func TestClaudeStreamsAndResumesSessions(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "claude-stream")
	sessionsPath := filepath.Join(root, "sessions.json")
	config := HarnessConfig{Command: command, Cwd: root, Model: "sonnet", Effort: "high", ApprovalPolicy: "never", SessionsFile: sessionsPath}
	claude, err := NewClaude(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := claude.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer claude.Close()

	for attempt := 0; attempt < 1; attempt++ {
		var mu sync.Mutex
		var streamed strings.Builder
		var final core.Event
		foundRunning := false
		done := make(chan struct{})
		threadID, steered, err := claude.Send(ctx, "chat:tui:local", "test prompt", func(event core.Event) {
			mu.Lock()
			if event.Kind == core.EventDelta {
				streamed.WriteString(event.Text)
			}
			if event.Kind == core.EventFinal {
				final = event
			}
			foundRunning = foundRunning || event.Execution != nil && event.Execution.State == "running"
			mu.Unlock()
			if event.Done {
				close(done)
			}
		})
		if err != nil || steered || threadID != "claude-session" {
			t.Fatalf("Send() = %q, %t, %v", threadID, steered, err)
		}
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("timed out waiting for Claude result")
		}
		mu.Lock()
		if streamed.String() != "progress\nhello" || final.Text != "progress\nhello" {
			t.Fatalf("streamed/final = %q/%q", streamed.String(), final.Text)
		}
		if final.FinalText == nil || *final.FinalText != "hello" {
			t.Fatalf("last assistant item = %#v, want hello", final.FinalText)
		}
		if !foundRunning || final.Execution == nil || final.Execution.State != "finishing" {
			t.Fatalf("structured lifecycle missing: running=%t final=%#v", foundRunning, final.Execution)
		}
		mu.Unlock()
	}
	if err := claude.Close(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewClaude(config)
	if err != nil || reloaded.ThreadID("chat:tui:local") != "claude-session" {
		t.Fatalf("persisted session = %q, %v", reloaded.ThreadID("chat:tui:local"), err)
	}
	if err := reloaded.Start(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	if thread, steered, err := reloaded.Send(ctx, "chat:tui:local", "test prompt", func(event core.Event) {
		if event.Done {
			close(done)
		}
	}); err != nil || steered || thread != "claude-session" {
		t.Fatalf("resumed Send() = %q, %t, %v", thread, steered, err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("timed out waiting for resumed Claude result")
	}
	if err := reloaded.Close(); err != nil {
		t.Fatal(err)
	}
	invocations, inputs, resumed := 0, 0, false
	for _, record := range readFixtureRecords(t, logPath) {
		if record.Kind == "invocation" && !containsArgument(record.Args, "--help") {
			invocations++
			args := strings.Join(record.Args, " ")
			if !strings.Contains(args, "--input-format stream-json") || !strings.Contains(args, "--model sonnet") || !strings.Contains(args, "--effort high") || !strings.Contains(args, "--permission-mode dontAsk") || record.Cwd != root || record.Executable != command {
				t.Fatalf("portable Claude invocation = %#v", record)
			}
			resumed = resumed || strings.Contains(args, "--resume claude-session")
		}
		if record.Kind == "input" && strings.Contains(record.Text, `"text":"test prompt"`) {
			inputs++
		}
	}
	if invocations != 2 || inputs != 2 || !resumed {
		t.Fatalf("portable Claude fixture counts = invocations %d, inputs %d, resumed %t", invocations, inputs, resumed)
	}
}

func TestClaudeSteersFollowUpThroughActiveStreamingInput(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "claude-steer")
	claude, err := NewClaude(HarnessConfig{Command: command, Cwd: root, ApprovalPolicy: "never"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := claude.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer claude.Close()

	firstEvents := make(chan core.Event, 16)
	threadID, steered, err := claude.Send(ctx, "chat", "first prompt", func(event core.Event) { firstEvents <- event })
	if err != nil || steered || threadID != "steered-session" {
		t.Fatalf("first Send() = %q, %t, %v", threadID, steered, err)
	}
	for {
		select {
		case event := <-firstEvents:
			if event.Kind == core.EventDelta && event.Text == "first" {
				goto firstDeltaSeen
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for first Claude delta")
		}
	}

firstDeltaSeen:
	secondEvents := make(chan core.Event, 16)
	threadID, steered, err = claude.Send(ctx, "chat", "follow-up prompt", func(event core.Event) { secondEvents <- event })
	if err != nil || !steered || threadID != "steered-session" {
		t.Fatalf("follow-up Send() = %q, %t, %v", threadID, steered, err)
	}
	var final core.Event
	for !final.Done {
		select {
		case event := <-secondEvents:
			if event.Done {
				final = event
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for steered Claude result")
		}
	}
	if final.Kind != core.EventFinal || final.Text != "first second" {
		t.Fatalf("steered final = %#v", final)
	}
	foundReleased := false
	for len(firstEvents) > 0 {
		event := <-firstEvents
		if event.Kind == core.EventStatus && event.Done {
			foundReleased = true
		}
	}
	if !foundReleased {
		t.Fatal("previous emitter was not released after Claude steering")
	}
	invocations, first, followUp := 0, false, false
	for _, record := range readFixtureRecords(t, logPath) {
		if record.Kind == "invocation" && !containsArgument(record.Args, "--help") {
			invocations++
		}
		first = first || strings.Contains(record.Text, `"text":"first prompt"`)
		followUp = followUp || strings.Contains(record.Text, `"text":"follow-up prompt"`)
	}
	if invocations != 1 || !first || !followUp {
		t.Fatalf("steered fixture evidence = invocations %d, first %t, follow-up %t", invocations, first, followUp)
	}
}

func containsArgument(arguments []string, wanted string) bool {
	for _, argument := range arguments {
		if argument == wanted {
			return true
		}
	}
	return false
}

func TestClaudePermissionModesRespectSandboxAndPrivilegedFallback(t *testing.T) {
	tests := []struct {
		name       string
		config     HarnessConfig
		privileged bool
		want       string
	}{
		{name: "read only", config: HarnessConfig{ApprovalPolicy: "never", Sandbox: "read-only"}, want: "--permission-mode plan"},
		{name: "workspace write", config: HarnessConfig{ApprovalPolicy: "never", Sandbox: "workspace-write"}, want: "--permission-mode acceptEdits --allowedTools Bash(*)"},
		{name: "unrestricted", config: HarnessConfig{ApprovalPolicy: "never", Sandbox: "danger-full-access"}, want: "--dangerously-skip-permissions"},
		{name: "privileged unrestricted fallback", config: HarnessConfig{ApprovalPolicy: "never", Sandbox: "danger-full-access"}, privileged: true, want: "--permission-mode acceptEdits --allowedTools * Bash(*)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := strings.Join(claudePermissionArgsFor(test.config, test.privileged), " ")
			if got != test.want {
				t.Fatalf("permission args = %q, want %q", got, test.want)
			}
		})
	}
}

func TestClaudeInputModeMatchesPermissionCapabilities(t *testing.T) {
	tests := []struct {
		name       string
		config     HarnessConfig
		privileged bool
		streaming  bool
	}{
		{name: "read only streams", config: HarnessConfig{ApprovalPolicy: "never", Sandbox: "read-only"}, streaming: true},
		{name: "workspace tools use text", config: HarnessConfig{ApprovalPolicy: "never", Sandbox: "workspace-write"}},
		{name: "unrestricted streams", config: HarnessConfig{ApprovalPolicy: "never", Sandbox: "danger-full-access"}, streaming: true},
		{name: "privileged unrestricted tools use text", config: HarnessConfig{ApprovalPolicy: "never", Sandbox: "danger-full-access"}, privileged: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := claudeUsesStreamingInputFor(test.config, test.privileged); got != test.streaming {
				t.Fatalf("streaming input = %t, want %t", got, test.streaming)
			}
		})
	}
}

func TestClaudeTextInputSupportsToolCapablePermissionModes(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "claude-text")
	claude, err := NewClaude(HarnessConfig{
		Command: command, Cwd: root, ApprovalPolicy: "never", Sandbox: "workspace-write",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := claude.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer claude.Close()
	done := make(chan core.Event, 1)
	if _, steered, err := claude.Send(ctx, "task", "run the tools", func(event core.Event) {
		if event.Done {
			done <- event
		}
	}); err != nil || steered {
		t.Fatalf("Send() = steered %t, error %v", steered, err)
	}
	select {
	case event := <-done:
		if event.Kind != core.EventFinal || event.Text != "tool done" {
			t.Fatalf("text-mode final = %#v", event)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for text-mode result")
	}
	var args, input string
	for _, record := range readFixtureRecords(t, logPath) {
		if record.Kind == "invocation" {
			args = strings.Join(record.Args, " ")
		}
		if record.Kind == "input" {
			input = record.Text
		}
	}
	if strings.Contains(args, "--input-format") || !strings.Contains(args, "--allowedTools Bash(*)") || input != "run the tools" {
		t.Fatalf("text-mode args/input = %q/%q", args, input)
	}
}

func TestClaudeInterruptsActiveTurn(t *testing.T) {
	command, root, _ := portableHarnessFixture(t, "claude-interrupt")
	claude, err := NewClaude(HarnessConfig{Command: command, Cwd: root, ApprovalPolicy: "never"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := claude.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer claude.Close()
	done := make(chan core.Event, 1)
	if thread, steered, err := claude.Send(ctx, "chat", "long task", func(event core.Event) {
		if event.Done {
			done <- event
		}
	}); err != nil || steered || thread != "interrupt-session" {
		t.Fatalf("Send() = %q, %t, %v", thread, steered, err)
	}
	interrupted, err := claude.Interrupt(ctx, "chat")
	if err != nil || !interrupted {
		t.Fatalf("Interrupt() = %t, %v", interrupted, err)
	}
	select {
	case event := <-done:
		if event.Kind != core.EventError {
			t.Fatalf("interrupted terminal event = %#v", event)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for interrupted Claude process")
	}
	if claude.IsActive("chat") {
		t.Fatal("Claude turn remained active after interruption")
	}
}

func TestClaudeRotatesSessionsWhenRuntimePolicyChanges(t *testing.T) {
	root := t.TempDir()
	sessionsPath := filepath.Join(root, "sessions.json")
	if err := os.WriteFile(sessionsPath, []byte("{\"chat\":\"legacy-session\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspaceConfig := HarnessConfig{
		Command: "claude", Cwd: root, ApprovalPolicy: "never", Sandbox: "workspace-write", SessionsFile: sessionsPath,
	}
	claude, err := NewClaude(workspaceConfig)
	if err != nil {
		t.Fatal(err)
	}
	if claude.ThreadID("chat") != "legacy-session" {
		t.Fatalf("legacy session was not readable: %q", claude.ThreadID("chat"))
	}
	claude.mu.Lock()
	if resumed := claude.resumeSessionLocked("chat", workspaceConfig); resumed != "" {
		claude.mu.Unlock()
		t.Fatalf("legacy session with unknown permissions was resumed: %q", resumed)
	}
	claude.sessions["chat"] = claudeSession{ID: "configured-session", Policy: claudeSessionPolicy(workspaceConfig)}
	if resumed := claude.resumeSessionLocked("chat", workspaceConfig); resumed != "configured-session" {
		claude.mu.Unlock()
		t.Fatalf("matching configured session = %q", resumed)
	}
	dangerConfig := workspaceConfig
	dangerConfig.Sandbox = "danger-full-access"
	if resumed := claude.resumeSessionLocked("chat", dangerConfig); resumed != "" {
		claude.mu.Unlock()
		t.Fatalf("session survived a permission-policy change: %q", resumed)
	}
	claude.mu.Unlock()
}

func TestClaudeModelsUseStableAliasesAndCustomSelection(t *testing.T) {
	claude, err := NewClaude(HarnessConfig{Command: "claude", Model: "gateway/custom"})
	if err != nil {
		t.Fatal(err)
	}
	models, err := claude.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, model := range models {
		seen[model.ID] = true
	}
	for _, expected := range []string{"default", "best", "fable", "sonnet", "opus", "haiku", "sonnet[1m]", "opus[1m]", "opusplan", "gateway/custom"} {
		if !seen[expected] {
			t.Fatalf("missing model alias %q from %#v", expected, models)
		}
	}
}
