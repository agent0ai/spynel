package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	outbox := &Outbox{Directory: directory, Now: func() time.Time { return now }, Deliver: func(context.Context, Origin, string, string) error {
		attempts++
		if attempts == 1 {
			return errors.New("offline")
		}
		return nil
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

func TestNormalizeNotificationTextRemovesTerminalControls(t *testing.T) {
	exactReply := "\x1b]11;rgb:0000/0000/0000\x07\x1b[1;1R"
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "observed OSC 11 and cursor reply", input: exactReply + "Report ready.", want: "Report ready."},
		{name: "CSI styles and queries", input: "A\x1b[31mred\x1b[0m B\u009b6nC", want: "Ared BC"},
		{name: "OSC ST", input: "before\x1b]0;unsafe title\x1b\\after", want: "beforeafter"},
		{name: "DCS APC PM and SOS", input: "a\x1bPpayload\x1b\\b\x1b_hidden\x1b\\c\x1b^private\u009cd\u0098secret\u009ce", want: "abcde"},
		{name: "C0 and C1 preserve newline tab", input: "one\x00\x08\ttwo\r\nthree\u0085four\x7f", want: "one\ttwo\nthreefour"},
		{name: "truncated CSI", input: "safe\x1b[12;", want: "safe"},
		{name: "truncated OSC", input: "safe\x1b]11;rgb:ffff/ffff/ffff", want: "safe"},
		{name: "malformed CSI keeps following Unicode", input: "safe\x1b[12;🛰️ prose", want: "safe🛰️ prose"},
		{name: "Unicode Markdown multiline", input: "  **Done** — café 🚀\n\n- 第一\n\tindented  ", want: "**Done** — café 🚀\n\n- 第一\n\tindented"},
		{name: "8-bit C1 CSI", input: "before\x9b1;1Rafter", want: "beforeafter"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NormalizeNotificationText(test.input)
			if err != nil || got != test.want {
				t.Fatalf("NormalizeNotificationText(%q) = %q, %v; want %q", test.input, got, err, test.want)
			}
		})
	}
}

func TestNormalizeNotificationTextRejectsControlOnlyAndInvalidUTF8(t *testing.T) {
	for _, input := range []string{"\x1b]11;rgb:0000/0000/0000\x07\x1b[1;1R", "\x00\r\x7f", string([]byte{'o', 'k', 0xff})} {
		if got, err := NormalizeNotificationText(input); err == nil || got != "" {
			t.Fatalf("NormalizeNotificationText(%q) = %q, %v; want rejection", input, got, err)
		}
	}
}

func TestOutboxNormalizesBeforePersistenceAndDelivery(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "outbox")
	var delivered string
	outbox := &Outbox{Directory: directory, Deliver: func(_ context.Context, _ Origin, _ string, message string) error {
		delivered = message
		return nil
	}}
	unsafe := "\x1b]11;rgb:0000/0000/0000\x07\x1b[1;1R**Ready** 🚀\nnext"
	entry, err := outbox.Enqueue("event", "done", "cli/local", unsafe)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Message != "**Ready** 🚀\nnext" {
		t.Fatalf("normalized entry = %q", entry.Message)
	}
	data, err := os.ReadFile(outbox.path(entry.ID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "rgb:0000") || strings.ContainsRune(string(data), '\x1b') {
		t.Fatalf("unsafe bytes reached durable outbox: %q", data)
	}
	if err := outbox.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	if delivered != entry.Message {
		t.Fatalf("delivered = %q; want %q", delivered, entry.Message)
	}
	if _, err := outbox.Enqueue("empty", "done", "cli/local", "\x1b[6n"); err == nil {
		t.Fatal("control-only notification was enqueued")
	}
}

func TestOutboxNormalizesLegacyPendingEntryBeforeDelivery(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "outbox")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 10, 15, 0, 0, 0, time.UTC)
	unsafeEntry := OutboxEntry{
		ID: "legacy", Origin: "cli/local", Message: "\x1b]11;rgb:0000/0000/0000\x07\x1b[1;1RReady.",
		State: "pending", CreatedAt: now, UpdatedAt: now, NextAttemptAt: now,
	}
	data, _ := json.Marshal(unsafeEntry)
	if err := os.WriteFile(filepath.Join(directory, "legacy.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	var delivered string
	outbox := &Outbox{Directory: directory, Now: func() time.Time { return now }, Deliver: func(_ context.Context, _ Origin, _ string, message string) error {
		delivered = message
		return nil
	}}
	if err := outbox.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	if delivered != "Ready." {
		t.Fatalf("legacy delivery = %q", delivered)
	}
	stored, err := os.ReadFile(filepath.Join(directory, "legacy.json"))
	if err != nil || strings.Contains(string(stored), "rgb:0000") {
		t.Fatalf("legacy outbox was not normalized: %q, %v", stored, err)
	}
}

func TestTaskCreationPolicyAllowsExplicitChildOverrideAndValidatesOutcomes(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
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
	cfg, _ := config.Load(config.PathForRoot(root))
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
