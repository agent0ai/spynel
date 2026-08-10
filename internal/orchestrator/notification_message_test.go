package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/extensions"
	"github.com/agent0ai/spynel/internal/workspace"
)

func TestReviewSummaryReworkCountIsCodeManaged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.md")
	document := Document{FrontMatter: map[string]any{
		"review_attempt": 3,
		"completion_summary": map[string]any{
			"verdict": "accepted", "outcome": "All checks passed.", "reviewed_at": "2026-08-08T00:00:00Z",
		},
	}, Body: "# Task\n"}
	if err := WriteDocument(path, document); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{}
	manager.finalizeTaskCompletionSummary(path, "done")
	updated, err := ReadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	summary, ok := parseCompletionSummary(updated)
	if !ok || !summary.HasRework || summary.ReworkCount != 2 {
		t.Fatalf("code-managed summary = %#v, %v", summary, ok)
	}
}

func TestMalformedSummaryDoesNotBlockNotificationAgentScheduling(t *testing.T) {
	directory := t.TempDir()
	if err := workspace.Init(directory, false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "done.md")
	document := Document{FrontMatter: map[string]any{
		"id": "terminal-id", "title": "Terminal transition", "created_at": "bad", "updated_at": time.Now().UTC().Format(time.RFC3339),
		"notify":             map[string]any{"enabled": true, "origin": "cli/local", "on": []any{"done"}},
		"completion_summary": map[string]any{"verdict": "accepted", "outcome": strings.Repeat("x", notificationOutcomeRunes+1)},
	}, Body: "# Task\n\n## Progress\n\n- The practical work completed.\n"}
	if err := WriteDocument(path, document); err != nil {
		t.Fatal(err)
	}
	harness := newFakeRecipient()
	cfg := config.Default()
	cfg.Root = directory
	cfg.Path = filepath.Join(directory, ".spynel", "config.yaml")
	manager := New(cfg, harness, extensions.Runner{})
	if err := manager.completeTransition(context.Background(), config.Route{Name: "tasks"}, Lease{ID: "lease", Phase: phaseTaskImplementation, ClaimAttempt: 1}, "done", path); err != nil {
		t.Fatal(err)
	}
	if harness.calls != 0 {
		t.Fatalf("terminal transition synchronously called notification harness %d times", harness.calls)
	}
	entries, err := os.ReadDir(manager.notificationAgentDirectory())
	if err != nil || len(entries) != 1 {
		t.Fatalf("terminal notification agent was not scheduled: %v, %d", err, len(entries))
	}
}
