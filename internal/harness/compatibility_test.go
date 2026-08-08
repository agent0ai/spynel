package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
)

// These labels identify the public protocol snapshots represented by the
// synthetic process variants. Refresh them with docs/harness-compatibility.md
// whenever a consumed method, flag, field, event, or terminal shape changes.
const (
	codexFixtureProvenance  = "codex-app-server-public-schema-retrieved-2026-08-07"
	claudeFixtureProvenance = "claude-code-stream-json-docs-retrieved-2026-08-07"
)

func TestCompatibilityFixtureProvenanceIsVersionLabeled(t *testing.T) {
	for _, label := range []string{codexFixtureProvenance, claudeFixtureProvenance} {
		if !strings.Contains(label, "2026-08-07") {
			t.Fatalf("fixture provenance is not retrieval-version labeled: %q", label)
		}
	}
}

func TestCodexRejectsMissingInitializeMethod(t *testing.T) {
	command, root, _ := portableHarnessFixture(t, "codex-init-missing-method")
	codex, err := NewCodex(CodexConfig{Command: command, Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = codex.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), "initialize") || !strings.Contains(err.Error(), "Method not found") || !strings.Contains(err.Error(), command) {
		t.Fatalf("Codex compatibility error = %v", err)
	}
}

func TestCodexPreservesPersistedSessionWhenResumeCapabilityIsMissing(t *testing.T) {
	command, root, _ := portableHarnessFixture(t, "codex-resume-missing-method")
	sessions := filepath.Join(root, "sessions.json")
	original := []byte("{\n  \"chat\": \"persisted-thread\"\n}\n")
	if err := os.WriteFile(sessions, original, 0o600); err != nil {
		t.Fatal(err)
	}
	codex, err := NewCodex(CodexConfig{Command: command, Cwd: root, SessionsFile: sessions})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := codex.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer codex.Close()
	_, _, err = codex.Send(ctx, "chat", "do not replace my session", nil)
	if err == nil || !strings.Contains(err.Error(), "thread/resume") || !strings.Contains(err.Error(), "persisted-thread") {
		t.Fatalf("resume compatibility error = %v", err)
	}
	current, readErr := os.ReadFile(sessions)
	if readErr != nil || string(current) != string(original) || codex.ThreadID("chat") != "persisted-thread" {
		t.Fatalf("persisted session changed: file %q, id %q, error %v", current, codex.ThreadID("chat"), readErr)
	}
}

func TestCodexRejectsChangedThreadFieldWithoutPersistence(t *testing.T) {
	command, root, _ := portableHarnessFixture(t, "codex-thread-changed-field")
	sessions := filepath.Join(root, "sessions.json")
	codex, err := NewCodex(CodexConfig{Command: command, Cwd: root, SessionsFile: sessions})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := codex.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer codex.Close()
	_, _, err = codex.Send(ctx, "chat", "test", nil)
	if err == nil || !strings.Contains(err.Error(), "thread.id") {
		t.Fatalf("changed-field error = %v", err)
	}
	if _, statErr := os.Stat(sessions); !os.IsNotExist(statErr) || codex.ThreadID("chat") != "" {
		t.Fatalf("incompatible thread was persisted: id %q, stat %v", codex.ThreadID("chat"), statErr)
	}
}

func TestCodexRejectsChangedTerminalStatus(t *testing.T) {
	command, root, _ := portableHarnessFixture(t, "codex-terminal-changed-status")
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
	done := make(chan core.Event, 1)
	if _, _, err := codex.Send(ctx, "chat", "test", func(event core.Event) {
		if event.Done {
			done <- event
		}
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-done:
		if event.Kind != core.EventError || !strings.Contains(event.Text, "terminal status") || !strings.Contains(event.Text, "done") {
			t.Fatalf("changed terminal event = %#v", event)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for changed terminal status")
	}
}

func TestClaudeRejectsMissingCLIFlagBeforeStarting(t *testing.T) {
	command, root, _ := portableHarnessFixture(t, "claude-help-missing-flag")
	sessions := filepath.Join(root, "sessions.json")
	claude, err := NewClaude(HarnessConfig{Command: command, Cwd: root, ApprovalPolicy: "plan", SessionsFile: sessions})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = claude.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), "--include-partial-messages") || !strings.Contains(err.Error(), command) {
		t.Fatalf("Claude flag compatibility error = %v", err)
	}
	if _, statErr := os.Stat(sessions); !os.IsNotExist(statErr) {
		t.Fatalf("capability failure created session file: %v", statErr)
	}
}

func TestClaudeRejectsChangedInitEventWithoutPersistence(t *testing.T) {
	command, root, _ := portableHarnessFixture(t, "claude-init-changed-event")
	sessions := filepath.Join(root, "sessions.json")
	original := []byte("{\n  \"chat\": \"persisted-session\"\n}\n")
	if err := os.WriteFile(sessions, original, 0o600); err != nil {
		t.Fatal(err)
	}
	claude, err := NewClaude(HarnessConfig{Command: command, Cwd: root, ApprovalPolicy: "plan", SessionsFile: sessions})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := claude.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer claude.Close()
	_, _, err = claude.Send(ctx, "chat", "test", nil)
	if err == nil || !strings.Contains(err.Error(), "system/init") || !strings.Contains(err.Error(), "session_id") {
		t.Fatalf("Claude init compatibility error = %v", err)
	}
	current, readErr := os.ReadFile(sessions)
	if readErr != nil || string(current) != string(original) || claude.ThreadID("chat") != "persisted-session" {
		t.Fatalf("persisted Claude session changed: file %q, id %q, error %v", current, claude.ThreadID("chat"), readErr)
	}
}

func TestClaudeSurfacesDocumentedTerminalError(t *testing.T) {
	command, root, _ := portableHarnessFixture(t, "claude-terminal-error")
	claude, err := NewClaude(HarnessConfig{Command: command, Cwd: root, ApprovalPolicy: "plan"})
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
		if event.Kind != core.EventError || !strings.Contains(event.Text, "maximum turns exceeded") {
			t.Fatalf("Claude terminal error = %#v", event)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for Claude terminal error")
	}
}
