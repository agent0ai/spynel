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

func TestFormatTaskNotificationGoldens(t *testing.T) {
	tests := []struct {
		name, status, summary, want string
	}{
		{
			name: "done", status: "done",
			summary: "  verdict: accepted\n  outcome: TUI integration checks passed.\n  evidence: Telegram and WhatsApp renderers preserved the compact message.\n  reviewed_at: 2026-08-08T00:30:00Z\n  rework_count: 1\n",
			want:    "Preserve symmetric inline-code padding is complete.\nTUI integration checks passed.\nVerified: Telegram and WhatsApp renderers preserved the compact message.",
		},
		{
			name: "waiting", status: "waiting",
			summary: "  verdict: waiting\n  outcome: Production credentials are required from the user.\n  evidence: Retry after the API token is added.\n",
			want:    "Preserve symmetric inline-code padding is waiting.\nProduction credentials are required from the user.\nVerified: Retry after the API token is added.",
		},
		{
			name: "failed", status: "failed",
			summary: "  verdict: failed\n  outcome: The migration cannot parse the corrupt durable record.\n  evidence: Restore the recorded backup, then retry.\n",
			want:    "Preserve symmetric inline-code padding failed.\nThe migration cannot parse the corrupt durable record.\nVerified: Restore the recorded backup, then retry.",
		},
		{
			name: "cancelled", status: "cancelled",
			summary: "  verdict: cancelled\n  outcome: Work stopped at the user's request.\n",
			want:    "Preserve symmetric inline-code padding was cancelled.\nWork stopped at the user's request.",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document, err := ParseDocument([]byte("---\ntitle: Preserve symmetric inline-code padding\ncreated_at: 2026-08-08T00:00:00Z\nupdated_at: 2026-08-08T00:30:00Z\nattempt: 3\nreview_attempt: 2\nnotification_summary:\n" + test.summary + "---\nbody\n"))
			if err != nil {
				t.Fatal(err)
			}
			if got := FormatTaskNotification(document, test.status, "task-7"); got != test.want {
				t.Fatalf("notification mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, test.want)
			}
		})
	}
}

func TestFormatTaskNotificationLegacyMalformedAndClockAnomalies(t *testing.T) {
	tests := []struct {
		name, front string
	}{
		{"legacy", ""},
		{"malformed", "notification_summary:\n  verdict: maybe\n  outcome: generic\n"},
		{"unknown summary field", "notification_summary:\n  verdict: accepted\n  outcome: should not be trusted\n  transcript: secret\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document, err := ParseDocument([]byte("---\ntitle: Legacy task\ncreated_at: 2026-08-08T01:00:00Z\nupdated_at: 2026-08-08T00:00:00Z\nattempt: 1\n" + test.front + "---\nbody\n"))
			if err != nil {
				t.Fatal(err)
			}
			got := FormatTaskNotification(document, "done", "legacy-id")
			if !strings.Contains(got, "The recorded result was independently verified.") || strings.Contains(got, "Task: legacy-id") || strings.Contains(got, "-1") || strings.Contains(got, "unknown") || strings.Contains(got, "Status:") {
				t.Fatalf("unsafe fallback notification: %q", got)
			}
			if strings.Contains(got, "1h") || strings.Contains(got, "implementation attempt") {
				t.Fatalf("internal metrics were not omitted: %q", got)
			}
		})
	}
}

func TestFormatTaskNotificationRejectsAbsolutePathsFromTitleAndSummary(t *testing.T) {
	for _, path := range []string{
		"/workspace/private/project/file.go", "path:/workspace/private/file.go", "`/workspace/private/file.go`",
		"file:///workspace/private/file.go", `C:\\private\\project\\file.go`, `\\\\server\\private\\file.go`,
	} {
		document := Document{FrontMatter: map[string]any{
			"title": path,
			"notification_summary": map[string]any{
				"verdict": "accepted", "outcome": "Delivered the change.", "evidence": path + " passed", "reviewed_at": "2026-08-08T00:00:00Z",
			},
		}}
		got := FormatTaskNotification(document, "done", "private-id")
		if strings.Contains(got, path) || !strings.Contains(got, "private-id is complete.") || strings.Contains(got, "Task: private-id") {
			t.Fatalf("absolute path was not replaced by safe fallback: %q", got)
		}
	}
}

func TestFormatTaskNotificationBoundsLongUnicodeAndMarkdown(t *testing.T) {
	document := Document{FrontMatter: map[string]any{
		"title":      strings.Repeat("界", 130) + " *unsafe* [link](x)\x1b[31m",
		"created_at": "2026-08-06T22:28:00Z", "updated_at": "2026-08-08T00:00:00Z",
		"attempt": 1, "review_attempt": 1,
		"notification_summary": map[string]any{
			"verdict": "accepted", "outcome": "Unicode ✓ and Markdown *characters* remain bounded.\x00",
			"evidence": "Tests passed for `code`, underscores_, and emoji 🚀.\r", "reviewed_at": "2026-08-08T00:00:00Z",
		},
	}}
	got := FormatTaskNotification(document, "done", "unicode-id")
	lines := strings.Split(got, "\n")
	if !strings.Contains(lines[0], "… is complete.") || strings.Contains(got, "1d 1h 32m") || strings.ContainsAny(got, "\x00\x1b\r") {
		t.Fatalf("long/unicode notification = %q", got)
	}
	if len(lines) != 3 {
		t.Fatalf("notification lines = %d, want 3: %q", len(lines), got)
	}
}

func TestConciseDurationAcrossRestartsAndAttemptShapes(t *testing.T) {
	document := Document{FrontMatter: map[string]any{
		"created_at": "2026-08-08T00:00:00Z", "updated_at": "2026-08-08T00:00:40Z",
		"attempt": float64(1), "review_attempt": int64(1),
	}}
	if got := taskNotificationMetrics(document, "done"); got != "<1m · 1 implementation attempt · 1 review" {
		t.Fatalf("metrics = %q", got)
	}
}

func TestReviewSummaryReworkCountIsCodeManaged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.md")
	document := Document{FrontMatter: map[string]any{
		"review_attempt": 3,
		"notification_summary": map[string]any{
			"verdict": "accepted", "outcome": "All checks passed.", "reviewed_at": "2026-08-08T00:00:00Z",
		},
	}, Body: "# Task\n"}
	if err := WriteDocument(path, document); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{}
	manager.finalizeTaskNotificationSummary(path, "done")
	updated, err := ReadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	summary, ok := parseNotificationSummary(updated)
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
		"notify":               map[string]any{"enabled": true, "origin": "cli/local", "on": []any{"done"}},
		"notification_summary": map[string]any{"verdict": "accepted", "outcome": strings.Repeat("x", notificationOutcomeRunes+1)},
	}, Body: "# Task\n\n## Progress\n\n- The practical work completed.\n"}
	if err := WriteDocument(path, document); err != nil {
		t.Fatal(err)
	}
	harness := newFakeRecipient()
	cfg := config.Default()
	cfg.Root = directory
	cfg.Path = filepath.Join(directory, ".spynel", "config.yaml")
	manager := New(cfg, harness, extensions.Runner{})
	if err := manager.completeTransition(context.Background(), config.Route{Name: "tasks"}, Lease{ID: "lease"}, "done", path); err != nil {
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
