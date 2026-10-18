package orchestrator

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/extensions"
	"github.com/agent0ai/spynel/internal/workspace"
)

func TestStatusReadersKeepLargeProgressDocuments(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	checkpoint := now.Add(time.Hour)
	body := "# Evidence\n" + strings.Repeat("historical evidence\n", 100000) + "\n## Progress\n- " + strings.Repeat("界", 500000) + "\n- Latest 確認\n  continued\n\n## Notes\n- Not progress\n"
	for _, item := range []struct{ kind, status string }{{"tasks", "working"}, {"goals", "active"}} {
		path := cfg.StatePath(item.kind, item.status, "large.md")
		document := Document{FrontMatter: map[string]any{
			"id": item.kind, "title": "Large evidence", "status": item.status, "review_required": true,
			"updated_at": now.Format(time.RFC3339), "review_trigger": "scheduled", "next_review_at": checkpoint.Format(time.RFC3339),
		}, Body: body}
		if err := WriteDocument(path, document); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(path)
		manager := New(cfg, &heartbeatHarness{}, extensions.Runner{})
		inventory := manager.WorkflowItems(item.kind)
		if len(inventory.Diagnostics) != 0 || len(inventory.Items) != 1 || !inventory.Items[0].DetailsAvailable || inventory.Items[0].ID != item.kind || inventory.Items[0].Step != "Latest 確認 continued" || !inventory.Items[0].UpdatedAt.Equal(now) || !inventory.Items[0].ReviewRequired {
			t.Errorf("large %s listing lost metadata/progress: %#v", item.kind, inventory)
		}
		if status := manager.WorkStatus(); len(status.CountDiagnostics) != 0 {
			t.Errorf("large %s status falsely unreadable: %#v", item.kind, status)
		}
		if item.kind == "goals" {
			checkpoints, err := manager.ScheduledCheckpoints(now)
			if err != nil || len(checkpoints) != 1 || !checkpoints[0].At.Equal(checkpoint) {
				t.Errorf("large goal checkpoint lost: %#v %v", checkpoints, err)
			}
		}
		after, _ := os.ReadFile(path)
		if string(before) != string(after) {
			t.Fatal("inspection changed durable evidence")
		}
	}
}

func TestLatestWorkflowProgressStreamsOversizedLines(t *testing.T) {
	for _, body := range []string{
		"## Progress\n- " + strings.Repeat("界", 500000),
		"## Progress\r\n- First\r\n  " + strings.Repeat("界", 500000),
	} {
		step := latestWorkflowProgress(body)
		if !utf8.ValidString(step) || len([]rune(step)) != maxWorkflowStepRunes || !strings.HasSuffix(step, "…") {
			t.Fatalf("invalid oversized progress projection: %q", step)
		}
	}
	ignored := "## Progress\n- Latest\n" + strings.Repeat("irrelevant", 200000)
	if got := latestWorkflowProgress(ignored); got != "Latest" {
		t.Fatalf("ignored oversized paragraph changed the step: %q", got)
	}
	if got := latestWorkflowProgress("## Progress\n" + strings.Repeat(" ", 10000) + "- Latest\n"); got != "Latest" {
		t.Fatalf("indented entry lost beyond the line prefix: %q", got)
	}
}

func TestStatusMetadataStillRejectsInvalidAndOversizedHeaders(t *testing.T) {
	for _, header := range []string{"---\nid: [broken\n---\n", "---\n" + strings.Repeat("x", 1<<20) + "\n---\n"} {
		path := filepath.Join(t.TempDir(), "invalid.md")
		if err := os.WriteFile(path, []byte(header+"## Progress\n- Must not hide the invalid header\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readStatusDocument(path, true); !errors.Is(err, errStatusMetadata) {
			t.Fatalf("invalid metadata accepted or misclassified: %v", err)
		}
	}
}

func TestWorkStatusCountsDurableActiveFoldersConservatively(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	write := func(relative, body string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	valid := func(status string) string { return "---\nid: fixture\nstatus: " + status + "\n---\n# Fixture\n" }
	write(".spynel/tasks/todo/valid.md", valid("todo"))
	write(".spynel/tasks/working/corrupt.md", "missing front matter")
	write(".spynel/tasks/waiting/waiting.md", valid("waiting"))
	write(".spynel/tasks/waiting/corrupt-waiting.md", "missing front matter")
	write(".spynel/tasks/triage/custom.md", valid("triage"))
	write(".spynel/tasks/review/mismatch.md", valid(strings.Repeat("x", 1000)))
	write(".spynel/tasks/done/terminal.md", valid("done"))
	write(".spynel/tasks/failed/failed.md", valid("failed"))
	write(".spynel/tasks/cancelled/cancelled.md", valid("cancelled"))
	write(".spynel/goals/proposed/active.md", valid("proposed"))
	write(".spynel/goals/done/done.md", valid("done"))
	write(".spynel/goals/abandoned/abandoned.md", valid("abandoned"))

	manager := New(cfg, &heartbeatHarness{}, extensions.Runner{})
	status := manager.WorkStatus()
	if status.TasksActive != 5 || status.TasksWaiting != 2 || status.GoalsActive != 1 {
		t.Fatalf("durable counts = tasks %d waiting %d goals %d", status.TasksActive, status.TasksWaiting, status.GoalsActive)
	}
	if len(status.CountDiagnostics) != 3 || !strings.Contains(strings.Join(status.CountDiagnostics, "\n"), "corrupt-waiting.md counts active") {
		t.Fatalf("count diagnostics = %#v", status.CountDiagnostics)
	}
	for _, diagnostic := range status.CountDiagnostics {
		if len([]rune(diagnostic)) > 240 || strings.IndexFunc(diagnostic, unicode.IsControl) >= 0 {
			t.Fatalf("unbounded count diagnostic = %q", diagnostic)
		}
	}
	if status.HeartbeatState != "not_primary" || !status.NextHeartbeatAt.IsZero() {
		t.Fatalf("offline heartbeat status = %#v", status)
	}
	manager.SetPrimaryOwned(true)
	if primary := manager.WorkStatus(); primary.HeartbeatState != "unavailable" || !primary.NextHeartbeatAt.IsZero() {
		t.Fatalf("primary startup heartbeat status = %#v", primary)
	}

	cfg.Orchestrator.SemanticHeartbeatMinutes = 0
	if disabled := New(cfg, &heartbeatHarness{}, extensions.Runner{}).WorkStatus(); disabled.HeartbeatState != "disabled" {
		t.Fatalf("disabled heartbeat status = %#v", disabled)
	}
	cfg.Orchestrator.SemanticHeartbeatMinutes = 15
	cfg.Orchestrator.Enabled = false
	disabled := New(cfg, &heartbeatHarness{}, extensions.Runner{})
	disabled.SetPrimaryOwned(true)
	if status := disabled.WorkStatus(); status.HeartbeatState != "disabled" || !status.NextHeartbeatAt.IsZero() {
		t.Fatalf("orchestrator-disabled heartbeat status = %#v", status)
	}
}

func TestStatusDiagnosticsStripUnicodeAndC1Controls(t *testing.T) {
	status := WorkStatus{}
	status.AddCountDiagnostic("waiting/bad\u0085name\nend.md")
	if len(status.CountDiagnostics) != 1 || strings.IndexFunc(status.CountDiagnostics[0], unicode.IsControl) >= 0 {
		t.Fatalf("diagnostic retained a Unicode control: %#v", status.CountDiagnostics)
	}
}

func TestWorkStatusCountsButDoesNotOpenSymlinkDocuments(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("not markdown"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, ".spynel", "tasks", "waiting", "linked.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	status := New(cfg, &heartbeatHarness{}, extensions.Runner{}).WorkStatus()
	if status.TasksActive != 1 || status.TasksWaiting != 1 || len(status.CountDiagnostics) != 1 || !strings.Contains(status.CountDiagnostics[0], "not a readable regular file") {
		t.Fatalf("symlink census = %#v", status)
	}
}

func TestReadDirectoryEntriesIsBoundedAndDeterministic(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"z.md", "a.txt", "m.md"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entries, truncated, err := readDirectoryEntries(directory, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(entries) != 2 || entries[0].Name() > entries[1].Name() {
		t.Fatalf("bounded entries = %#v truncated=%t", entries, truncated)
	}
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readDirectoryEntries(file, 2); err == nil {
		t.Fatal("regular file was accepted as a status directory")
	}
}
