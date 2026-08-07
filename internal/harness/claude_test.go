package harness

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
)

func TestClaudeStreamsAndResumesSessions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell Claude Code fixture")
	}
	root := t.TempDir()
	script := filepath.Join(root, "fake-claude")
	argsPath := filepath.Join(root, "args")
	inputPath := filepath.Join(root, "input")
	fixture := `#!/bin/sh
printf '%s\n' "$*" >> "` + argsPath + `"
IFS= read -r input
printf '%s\n' "$input" >> "` + inputPath + `"
printf '%s\n' '{"type":"system","subtype":"init","session_id":"claude-session"}'
printf '%s\n' '{"type":"stream_event","session_id":"claude-session","event":{"type":"message_start"}}'
printf '%s\n' '{"type":"stream_event","session_id":"claude-session","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"progress"}}}'
printf '%s\n' '{"type":"stream_event","session_id":"claude-session","event":{"type":"message_start"}}'
printf '%s\n' '{"type":"stream_event","session_id":"claude-session","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}}'
printf '%s\n' '{"type":"result","subtype":"success","session_id":"claude-session","is_error":false,"result":"progress\nhello"}'
cat >/dev/null
`
	if err := os.WriteFile(script, []byte(fixture), 0o700); err != nil {
		t.Fatal(err)
	}
	claude, err := NewClaude(HarnessConfig{Command: script, Cwd: root, Model: "sonnet", Effort: "high", ApprovalPolicy: "never", SessionsFile: filepath.Join(root, "sessions.json")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := claude.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer claude.Close()

	for attempt := 0; attempt < 2; attempt++ {
		var mu sync.Mutex
		var streamed strings.Builder
		var final core.Event
		done := make(chan struct{})
		threadID, steered, err := claude.Send(ctx, "chat:tui:local", "test prompt", func(event core.Event) {
			mu.Lock()
			if event.Kind == core.EventDelta {
				streamed.WriteString(event.Text)
			}
			if event.Kind == core.EventFinal {
				final = event
			}
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
		mu.Unlock()
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(args)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "--input-format stream-json") || !strings.Contains(lines[0], "--model sonnet") || !strings.Contains(lines[0], "--effort high") || !strings.Contains(lines[0], "--permission-mode dontAsk") || !strings.Contains(lines[1], "--resume claude-session") {
		t.Fatalf("Claude arguments = %q", string(args))
	}
	input, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(strings.TrimSpace(string(input)), "\n") != 1 || !strings.Contains(string(input), `"type":"user"`) || !strings.Contains(string(input), `"text":"test prompt"`) {
		t.Fatalf("Claude streaming input = %q", input)
	}
	reloaded, err := NewClaude(HarnessConfig{Command: script, Cwd: root, SessionsFile: filepath.Join(root, "sessions.json")})
	if err != nil || reloaded.ThreadID("chat:tui:local") != "claude-session" {
		t.Fatalf("persisted session = %q, %v", reloaded.ThreadID("chat:tui:local"), err)
	}
}

func TestClaudeSteersFollowUpThroughActiveStreamingInput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell Claude Code fixture")
	}
	root := t.TempDir()
	script := filepath.Join(root, "fake-claude")
	inputPath := filepath.Join(root, "input")
	invocationsPath := filepath.Join(root, "invocations")
	fixture := `#!/bin/sh
printf 'run\n' >> "` + invocationsPath + `"
IFS= read -r first
printf '%s\n' "$first" >> "` + inputPath + `"
printf '%s\n' '{"type":"system","subtype":"init","session_id":"steered-session"}'
printf '%s\n' '{"type":"stream_event","session_id":"steered-session","event":{"type":"message_start"}}'
printf '%s\n' '{"type":"stream_event","session_id":"steered-session","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"first"}}}'
IFS= read -r second
printf '%s\n' "$second" >> "` + inputPath + `"
printf '%s\n' '{"type":"stream_event","session_id":"steered-session","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":" second"}}}'
printf '%s\n' '{"type":"result","subtype":"success","session_id":"steered-session","is_error":false,"result":"first second"}'
cat >/dev/null
`
	if err := os.WriteFile(script, []byte(fixture), 0o700); err != nil {
		t.Fatal(err)
	}
	claude, err := NewClaude(HarnessConfig{Command: script, Cwd: root, ApprovalPolicy: "never"})
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
	invocations, err := os.ReadFile(invocationsPath)
	if err != nil || strings.Count(string(invocations), "run\n") != 1 {
		t.Fatalf("Claude process invocations = %q, %v", invocations, err)
	}
	input, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(input), `"text":"first prompt"`) || !strings.Contains(string(input), `"text":"follow-up prompt"`) {
		t.Fatalf("steered input = %q", input)
	}
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
	if runtime.GOOS == "windows" {
		t.Skip("shell Claude Code fixture")
	}
	root := t.TempDir()
	script := filepath.Join(root, "fake-claude")
	argsPath := filepath.Join(root, "args")
	inputPath := filepath.Join(root, "input")
	fixture := `#!/bin/sh
printf '%s\n' "$*" > "` + argsPath + `"
cat > "` + inputPath + `"
printf '%s\n' '{"type":"system","subtype":"init","session_id":"text-session"}'
printf '%s\n' '{"type":"result","subtype":"success","session_id":"text-session","is_error":false,"result":"tool done"}'
`
	if err := os.WriteFile(script, []byte(fixture), 0o700); err != nil {
		t.Fatal(err)
	}
	claude, err := NewClaude(HarnessConfig{
		Command: script, Cwd: root, ApprovalPolicy: "never", Sandbox: "workspace-write",
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
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), "--input-format") || !strings.Contains(string(args), "--allowedTools Bash(*)") {
		t.Fatalf("text-mode args = %q", args)
	}
	input, err := os.ReadFile(inputPath)
	if err != nil || string(input) != "run the tools" {
		t.Fatalf("text-mode input = %q, %v", input, err)
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
