package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/extensions"
	"github.com/agent0ai/spynel/internal/workspace"
)

func TestWorkflowItemsUsesFolderStateAndDoesNotFollowDocumentSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	validPath := cfg.StatePath("tasks", "working", "valid.md")
	if err := WriteDocument(validPath, Document{FrontMatter: map[string]any{
		"id": "task-valid", "title": "Visible task", "status": "todo", "review_required": false,
		"created_at": now.Add(-time.Hour).Format(time.RFC3339), "updated_at": now.Format(time.RFC3339),
		"attempt": 2, "review_attempt": 1, "provider_iterations": 3,
	}, Body: "## Progress\n\n- First.\n- Latest step\n  with continuation.\n\n## Notes\n\n- Not progress.\n"}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "outside.md")
	if err := WriteDocument(target, Document{FrontMatter: map[string]any{
		"id": "secret", "title": "Must not be read", "status": "working", "updated_at": now.Format(time.RFC3339),
	}, Body: "## Progress\n- secret progress\n"}); err != nil {
		t.Fatal(err)
	}
	link := cfg.StatePath("tasks", "working", "linked.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	inventory := New(cfg, &heartbeatHarness{}, extensions.Runner{}).WorkflowItems("tasks")
	if len(inventory.Items) != 2 {
		t.Fatalf("workflow items = %#v", inventory.Items)
	}
	var valid, linked WorkflowItem
	for _, item := range inventory.Items {
		switch item.FileName {
		case "valid.md":
			valid = item
		case "linked.md":
			linked = item
		}
	}
	if valid.Title != "Visible task" || valid.Status != "working" || valid.Step != "Latest step with continuation." || valid.Attempt != 2 || valid.ReviewAttempt != 1 || valid.ProviderIterations != 3 || !valid.HasReviewPolicy || valid.ReviewRequired {
		t.Fatalf("valid workflow summary = %#v", valid)
	}
	if linked.DetailsAvailable || linked.Title != "linked" {
		t.Fatalf("symlink workflow summary = %#v", linked)
	}
	warnings := strings.Join(inventory.Diagnostics, "\n")
	if !strings.Contains(warnings, "front matter says") || !strings.Contains(warnings, "not a readable regular file") || strings.Contains(warnings, root) || strings.Contains(warnings, "Must not be read") {
		t.Fatalf("workflow diagnostics = %#v", inventory.Diagnostics)
	}
	if !containsWorkflowStatus(inventory.Statuses, "working") || !containsWorkflowStatus(inventory.Statuses, "done") {
		t.Fatalf("workflow statuses = %#v", inventory.Statuses)
	}
}

func containsWorkflowStatus(statuses []string, wanted string) bool {
	for _, status := range statuses {
		if status == wanted {
			return true
		}
	}
	return false
}
