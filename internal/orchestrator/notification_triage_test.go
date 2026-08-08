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

	"github.com/agent0ai/spynel/internal/core"
)

type blockingTriageHarness struct{}

func (*blockingTriageHarness) Start(context.Context) error                     { return nil }
func (*blockingTriageHarness) Close() error                                    { return nil }
func (*blockingTriageHarness) Interrupt(context.Context, string) (bool, error) { return false, nil }
func (*blockingTriageHarness) ResetSession(string) error                       { return nil }
func (*blockingTriageHarness) ThreadID(string) string                          { return "" }
func (*blockingTriageHarness) IsActive(string) bool                            { return false }
func (*blockingTriageHarness) Send(ctx context.Context, _ string, _ string, _ core.Emit) (string, bool, error) {
	<-ctx.Done()
	return "", false, ctx.Err()
}

type signalingBlockingTriageHarness struct {
	entered chan struct{}
}

func (*signalingBlockingTriageHarness) Start(context.Context) error { return nil }
func (*signalingBlockingTriageHarness) Close() error                { return nil }
func (*signalingBlockingTriageHarness) Interrupt(context.Context, string) (bool, error) {
	return false, nil
}
func (*signalingBlockingTriageHarness) ResetSession(string) error { return nil }
func (*signalingBlockingTriageHarness) ThreadID(string) string    { return "" }
func (*signalingBlockingTriageHarness) IsActive(string) bool      { return false }
func (h *signalingBlockingTriageHarness) Send(ctx context.Context, _ string, _ string, _ core.Emit) (string, bool, error) {
	select {
	case h.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return "", false, ctx.Err()
}

func TestParseTriageResultEveryOutcomeAndSkip(t *testing.T) {
	base := TriageResult{
		Schema: notificationTriageSchema, Decision: "notify", Message: "Practical outcome.",
		Urgency: "normal", FollowUp: FollowUpPolicy{},
	}
	for _, outcome := range []string{"done", "cancelled"} {
		data, _ := json.Marshal(base)
		if _, err := parseTriageResult(string(data), outcome); err != nil {
			t.Fatalf("%s notification: %v", outcome, err)
		}
	}
	for _, outcome := range []string{"waiting", "failed"} {
		result := base
		result.Question = "Retry or stop?"
		result.Choices = []string{"Retry", "Stop"}
		result.ResponseRequired = true
		result.FollowUp = FollowUpPolicy{Enabled: true, AfterMinutes: 60, MaxReminders: 2}
		data, _ := json.Marshal(result)
		if _, err := parseTriageResult(string(data), outcome); err != nil {
			t.Fatalf("%s notification: %v", outcome, err)
		}
	}
	skip := `{"schema":"spynel.notification-triage/v1","decision":"skip","message":"","urgency":"low","response_required":false,"follow_up":{"enabled":false}}`
	if _, err := parseTriageResult(skip, "done"); err != nil {
		t.Fatalf("skip: %v", err)
	}
}

func TestActionableNotificationIncludesExactQuestionChoicesAndNextAction(t *testing.T) {
	result := TriageResult{
		Message: "The deployment is waiting on a region.", Question: "Which region should I use?",
		Choices: []string{"EU", "US"}, NextAction: "Reply with one option.",
	}
	got := actionableNotificationMessage(result)
	for _, want := range []string{result.Message, result.Question, "- EU", "- US", result.NextAction} {
		if !strings.Contains(got, want) {
			t.Fatalf("actionable message omitted %q: %q", want, got)
		}
	}
}

func TestParseTriageRejectsMalformedAndInconsistentResults(t *testing.T) {
	cases := []string{
		`not json`,
		`{"schema":"other","decision":"notify","message":"done","urgency":"normal","response_required":false,"follow_up":{"enabled":false}}`,
		`{"schema":"spynel.notification-triage/v1","decision":"skip","message":"not empty","urgency":"low","response_required":false,"follow_up":{"enabled":false}}`,
		`{"schema":"spynel.notification-triage/v1","decision":"notify","message":"done","question":"unexpected?","urgency":"normal","response_required":false,"follow_up":{"enabled":false}}`,
		`{"schema":"spynel.notification-triage/v1","decision":"notify","message":"done","urgency":"normal","response_required":true,"follow_up":{"enabled":true,"after_minutes":60,"max_reminders":2}}`,
		`{"schema":"spynel.notification-triage/v1","decision":"notify","message":"done","urgency":"normal","response_required":false,"follow_up":{"enabled":false},"unknown":true}`,
	}
	for _, raw := range cases {
		if _, err := parseTriageResult(raw, "done"); err == nil {
			t.Fatalf("accepted malformed result: %s", raw)
		}
	}
}

func TestTriageRetryBackoffAndDeterministicWaitingFallback(t *testing.T) {
	manager, now := actionRequestManager(t)
	taskPath := filepath.Join(manager.Config.Root, ".spynel", "tasks", "waiting", "task.md")
	document := Document{FrontMatter: map[string]any{"id": "task-1", "title": "Choose region", "status": "waiting"}, Body: "\n## Progress\n\n- Waiting for a region.\n"}
	if err := WriteDocument(taskPath, document); err != nil {
		t.Fatal(err)
	}
	event := TriageEvent{ID: "triage-1", TaskID: "task-1", TransitionID: "lease:waiting", Outcome: "waiting", Origin: "telegram/TG-7", TaskFile: taskPath, State: "running", CreatedAt: *now}
	eventPath := manager.Config.StatePath("runtime", "notification-triage", event.ID+".json")
	if err := writePrivateJSON(eventPath, event); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= maxTriageAttempts; attempt++ {
		before := *now
		err := manager.deferTriage(eventPath, &event, errors.New("provider unavailable"))
		if err == nil {
			t.Fatal("defer omitted provider error")
		}
		if attempt < maxTriageAttempts {
			want := before.Add(time.Second * time.Duration(1<<(attempt-1)))
			if event.State != "pending" || !event.NextAttemptAt.Equal(want) {
				t.Fatalf("attempt %d state = %#v", attempt, event)
			}
			*now = event.NextAttemptAt
		}
	}
	if event.State != "completed" || event.Result == nil || !event.Result.ResponseRequired || event.Result.Question == "" {
		t.Fatalf("fallback event = %#v", event)
	}
	entries, err := os.ReadDir(manager.Outbox.Directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("fallback outbox = %d, %v", len(entries), err)
	}
	var delivery OutboxEntry
	if err := readPrivateJSON(filepath.Join(manager.Outbox.Directory, entries[0].Name()), &delivery, 128<<10); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(delivery.Message, event.Result.Question) {
		t.Fatalf("first fallback delivery omitted exact question: %q", delivery.Message)
	}
	status, ok := manager.ActionRequestStatusForTask("task-1")
	if !ok || status.State != "pending_delivery" {
		t.Fatalf("fallback action status = %#v, %t", status, ok)
	}
}

func TestTriageTimeoutRetainsEventForRetry(t *testing.T) {
	manager, _ := actionRequestManager(t)
	manager.Harness = &blockingTriageHarness{}
	manager.notificationTriageTimeout = time.Millisecond
	promptPath := manager.Config.StatePath("prompts", "notification.md")
	if err := os.MkdirAll(filepath.Dir(promptPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(promptPath, []byte("Outcome {{OUTCOME}} {{TITLE}} {{SUMMARY}} {{PROGRESS}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	taskPath := filepath.Join(manager.Config.Root, ".spynel", "tasks", "done", "task.md")
	if err := WriteDocument(taskPath, Document{FrontMatter: map[string]any{"id": "task-1", "title": "Task", "status": "done"}}); err != nil {
		t.Fatal(err)
	}
	event := TriageEvent{ID: "timeout", TaskID: "task-1", Outcome: "done", Origin: "tui/local", TaskFile: taskPath, State: "pending"}
	eventPath := manager.Config.StatePath("runtime", "notification-triage", event.ID+".json")
	if err := writePrivateJSON(eventPath, event); err != nil {
		t.Fatal(err)
	}
	if err := manager.processOneTriage(context.Background(), eventPath, &event); err == nil {
		t.Fatal("timeout was not reported")
	}
	if event.State != "pending" || event.Attempts != 1 || event.LastError == "" {
		t.Fatalf("timeout event = %#v", event)
	}
}

func TestNotificationProviderDoesNotBlockSubsequentScans(t *testing.T) {
	manager, _ := actionRequestManager(t)
	manager.orchestratorEnabled.Store(true)
	entered := make(chan struct{}, 1)
	manager.Harness = &signalingBlockingTriageHarness{entered: entered}
	manager.notificationTriageTimeout = time.Second
	promptPath := manager.Config.StatePath("prompts", "notification.md")
	if err := os.MkdirAll(filepath.Dir(promptPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(promptPath, []byte("Outcome {{OUTCOME}} {{TITLE}} {{SUMMARY}} {{PROGRESS}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	taskPath := filepath.Join(manager.Config.Root, ".spynel", "tasks", "done", "task.md")
	if err := WriteDocument(taskPath, Document{FrontMatter: map[string]any{"id": "task-1", "title": "Task", "status": "done"}}); err != nil {
		t.Fatal(err)
	}
	event := TriageEvent{ID: "blocked", TaskID: "task-1", Outcome: "done", Origin: "tui/local", TaskFile: taskPath, State: "pending"}
	if err := writePrivateJSON(manager.Config.StatePath("runtime", "notification-triage", event.ID+".json"), event); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := manager.ScanOnce(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("notification provider did not start")
	}
	started := time.Now()
	if err := manager.ScanOnce(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		cancel()
		t.Fatalf("scan waited for notification provider: %s", elapsed)
	}
	cancel()
	manager.Wait()
}
