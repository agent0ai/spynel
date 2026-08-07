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

	"github.com/frdel/spynel/internal/core"
)

func TestClaudeStreamsAndResumesSessions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell Claude Code fixture")
	}
	root := t.TempDir()
	script := filepath.Join(root, "fake-claude")
	argsPath := filepath.Join(root, "args")
	fixture := `#!/bin/sh
printf '%s\n' "$*" >> "` + argsPath + `"
cat >/dev/null
printf '%s\n' '{"type":"system","subtype":"init","session_id":"claude-session"}'
printf '%s\n' '{"type":"stream_event","session_id":"claude-session","event":{"type":"message_start"}}'
printf '%s\n' '{"type":"stream_event","session_id":"claude-session","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}}'
printf '%s\n' '{"type":"result","subtype":"success","session_id":"claude-session","is_error":false,"result":"hello"}'
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
		var final string
		done := make(chan struct{})
		threadID, steered, err := claude.Send(ctx, "chat:tui:local", "test prompt", func(event core.Event) {
			mu.Lock()
			if event.Kind == core.EventDelta {
				streamed.WriteString(event.Text)
			}
			if event.Kind == core.EventFinal {
				final = event.Text
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
		if streamed.String() != "hello" || final != "hello" {
			t.Fatalf("streamed/final = %q/%q", streamed.String(), final)
		}
		mu.Unlock()
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(args)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "--model sonnet") || !strings.Contains(lines[0], "--effort high") || !strings.Contains(lines[0], "--permission-mode dontAsk") || !strings.Contains(lines[1], "--resume claude-session") {
		t.Fatalf("Claude arguments = %q", string(args))
	}
	reloaded, err := NewClaude(HarnessConfig{Command: script, Cwd: root, SessionsFile: filepath.Join(root, "sessions.json")})
	if err != nil || reloaded.ThreadID("chat:tui:local") != "claude-session" {
		t.Fatalf("persisted session = %q, %v", reloaded.ThreadID("chat:tui:local"), err)
	}
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
