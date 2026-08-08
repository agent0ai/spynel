package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/workspace"
)

func TestNotificationFrontMatterPreservesUnknownFieldsAndValidatesOrigin(t *testing.T) {
	doc, err := ParseDocument([]byte("---\nid: x\ncustom: keep\nnotify:\n  enabled: true\n  origin: telegram/TG-7\n  on: [done, waiting]\n---\nbody\n"))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := NotificationFromDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !policy.Enabled || policy.Origin.Conversation != "TG-7" || !policy.Outcomes["done"] {
		t.Fatalf("policy = %#v", policy)
	}
	encoded, err := doc.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := ParseDocument(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip.FrontMatter["custom"] != "keep" {
		t.Fatal("unknown front matter was lost")
	}
	doc.FrontMatter["notify"] = map[string]any{"enabled": true, "origin": "email/me", "on": []any{"done"}}
	if _, err := NotificationFromDocument(doc); err == nil {
		t.Fatal("unsupported origin accepted")
	}
}

func TestOutboxDeduplicatesAndRecoversRetryState(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "outbox")
	now := time.Date(2026, 8, 7, 20, 0, 0, 0, time.UTC)
	attempts := 0
	outbox := &Outbox{Directory: directory, Now: func() time.Time { return now }, Deliver: func(context.Context, Origin, string, string) ([]string, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("offline")
		}
		return []string{"native-1"}, nil
	}}
	first, err := outbox.Enqueue("task-1", "done", "cli/local", "complete")
	if err != nil {
		t.Fatal(err)
	}
	second, err := outbox.Enqueue("task-1", "done", "cli/local", "complete")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatal("stable event was not deduplicated")
	}
	if err := outbox.Process(context.Background()); err == nil {
		t.Fatal("offline attempt was not reported")
	}
	data, err := os.ReadFile(outbox.path(first.ID))
	if err != nil {
		t.Fatal(err)
	}
	entry, err := decodeOutboxForTest(data)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Attempts != 1 || entry.LastError == "" || entry.State != "pending" {
		t.Fatalf("retry state = %#v", entry)
	}
	now = now.Add(2 * time.Second)
	restarted := &Outbox{Directory: directory, Now: func() time.Time { return now }, Deliver: outbox.Deliver}
	if err := restarted.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(restarted.path(first.ID))
	entry, _ = decodeOutboxForTest(data)
	if entry.State != "delivered" || entry.Attempts != 2 || entry.DeliveredAt.IsZero() {
		t.Fatalf("delivered state = %#v", entry)
	}
}

func TestTaskCreationPolicyAllowsExplicitChildOverrideAndValidatesOutcomes(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	path, err := CreateWithOptions(cfg, "tasks", "child", "", CreateOptions{Notify: true, Origin: "cli/local", ParentTaskID: "parent", Outcomes: []string{"done"}})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ReadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := NotificationFromDocument(doc)
	if err != nil || !policy.Enabled {
		t.Fatalf("explicit child policy = %#v, %v", policy, err)
	}
	if _, err := CreateWithOptions(cfg, "tasks", "bad", "", CreateOptions{Notify: true, Origin: "cli/local", Outcomes: []string{"review"}}); err == nil {
		t.Fatal("invalid creation outcome accepted")
	}
}

func TestGoalLinkedTaskCreationRequiresRoundAndPersistsStableReference(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	if _, err := CreateWithOptions(cfg, "tasks", "bad link", "", CreateOptions{GoalID: "goal-1"}); err == nil {
		t.Fatal("goal-linked task without a round was accepted")
	}
	path, err := CreateWithOptions(cfg, "tasks", "linked", "", CreateOptions{GoalID: "goal-1", GoalRound: 2, Notify: true, Origin: "cli/local", Outcomes: []string{"cancelled"}})
	if err != nil {
		t.Fatal(err)
	}
	document, err := ReadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if document.FrontMatter["goal_id"] != "goal-1" || numberValue(document.FrontMatter["goal_round"]) != 2 {
		t.Fatalf("goal link = %#v", document.FrontMatter)
	}
	policy, err := NotificationFromDocument(document)
	if err != nil || !policy.Outcomes["cancelled"] {
		t.Fatalf("cancelled notification = %#v, %v", policy, err)
	}
}

func decodeOutboxForTest(data []byte) (OutboxEntry, error) {
	var entry OutboxEntry
	err := json.Unmarshal(data, &entry)
	return entry, err
}
