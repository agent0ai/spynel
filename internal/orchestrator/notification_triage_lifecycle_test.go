package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
)

func actionRequestManager(t *testing.T) (*Manager, *time.Time) {
	t.Helper()
	cfg := config.Default()
	cfg.Root = t.TempDir()
	cfg.Workspace.StateDir = ".spynel"
	now := time.Date(2026, 8, 8, 19, 0, 0, 0, time.UTC)
	manager := &Manager{
		Config: cfg,
		Outbox: &Outbox{Directory: cfg.StatePath("runtime", "outbox"), Now: func() time.Time { return now }},
	}
	manager.Outbox.OnDelivered = manager.markActionDelivered
	manager.primaryOwned.Store(true)
	manager.AuthorizeNotificationOrigin = func(origin Origin) error { return nil }
	return manager, &now
}

func TestPendingActionSummaryIsExactOriginAndDoesNotAcknowledge(t *testing.T) {
	manager, now := actionRequestManager(t)
	request := ActionRequest{ID: "request-1", TaskID: "task-1", Origin: "telegram/TG-7", Question: "Which region?", Choices: []string{"EU", "US"}, State: "awaiting_response", CreatedAt: *now}
	path := manager.Config.StatePath("runtime", "action-requests", request.ID+".json")
	if err := writePrivateJSON(path, request); err != nil {
		t.Fatal(err)
	}
	if got := manager.PendingActionSummary("telegram/TG-8"); got != "" {
		t.Fatalf("cross-origin summary leaked: %q", got)
	}
	got := manager.PendingActionSummary("telegram/TG-7")
	if !strings.Contains(got, "Which region?") || !strings.Contains(got, "do not acknowledge unrelated messages") {
		t.Fatalf("summary = %q", got)
	}
	var stored ActionRequest
	if err := readPrivateJSON(path, &stored, 64<<10); err != nil || stored.State != "awaiting_response" || !stored.AcknowledgedAt.IsZero() {
		t.Fatalf("summary changed request: %#v, %v", stored, err)
	}
}

func TestNativeReplyCorrelatesExactDeliveryWithoutAcknowledging(t *testing.T) {
	manager, now := actionRequestManager(t)
	for _, request := range []ActionRequest{
		{ID: "request-1", TaskID: "task-1", Origin: "tui/local", Question: "First question?", State: "awaiting_response", CreatedAt: now.Add(-time.Minute), Deliveries: []ActionDelivery{{EventID: "event-1", Origin: "telegram/TG-7", NativeMessageIDs: []string{"101"}}}},
		{ID: "request-2", TaskID: "task-2", Origin: "telegram/TG-7", Question: "Second question?", State: "awaiting_response", CreatedAt: *now, Deliveries: []ActionDelivery{{EventID: "event-2", Origin: "telegram/TG-7", NativeMessageIDs: []string{"202"}}}},
	} {
		if err := writePrivateJSON(manager.Config.StatePath("runtime", "action-requests", request.ID+".json"), request); err != nil {
			t.Fatal(err)
		}
	}
	got := manager.PendingActionSummary("telegram/TG-7", "101")
	if !strings.Contains(got, "explicit native reply") || !strings.Contains(got, "First question?") || strings.Contains(got, "Second question?") {
		t.Fatalf("correlated summary = %q", got)
	}
	if got := manager.PendingActionSummary("telegram/TG-7", "unrelated"); got != "" {
		t.Fatalf("unrelated native reply correlated: %q", got)
	}
	// Repeated inspection is intentionally side-effect free; only a validated
	// durable task transition may acknowledge and resume the request once.
	if got := manager.PendingActionSummary("telegram/TG-7", "101"); !strings.Contains(got, "First question?") {
		t.Fatalf("repeated reply inspection changed state: %q", got)
	}
	var stored ActionRequest
	if err := readPrivateJSON(manager.Config.StatePath("runtime", "action-requests", "request-1.json"), &stored, 64<<10); err != nil || stored.State != "awaiting_response" || !stored.AcknowledgedAt.IsZero() {
		t.Fatalf("reply inspection acknowledged request: %#v, %v", stored, err)
	}
}

func TestDeliveredActionPersistsBoundedNativeReceipt(t *testing.T) {
	manager, now := actionRequestManager(t)
	request := ActionRequest{ID: "request-1", TaskID: "task-1", Origin: "telegram/TG-7", State: "pending_delivery", CreatedAt: *now}
	if err := writePrivateJSON(manager.Config.StatePath("runtime", "action-requests", request.ID+".json"), request); err != nil {
		t.Fatal(err)
	}
	entry := OutboxEntry{ID: "event-1", Origin: "telegram/TG-7", ActionRequestID: request.ID, Kind: "action_request", NativeMessageIDs: []string{"91", "92"}}
	if err := manager.markActionDelivered(entry); err != nil {
		t.Fatal(err)
	}
	var stored ActionRequest
	if err := readPrivateJSON(manager.Config.StatePath("runtime", "action-requests", request.ID+".json"), &stored, 64<<10); err != nil {
		t.Fatal(err)
	}
	if stored.State != "awaiting_response" || len(stored.Deliveries) != 1 || len(stored.Deliveries[0].NativeMessageIDs) != 2 || stored.Deliveries[0].NativeMessageIDs[1] != "92" {
		t.Fatalf("stored delivery = %#v", stored)
	}
}

func TestHeartbeatReminderDeduplicatesAndDeliveryAdvancesBackoff(t *testing.T) {
	manager, now := actionRequestManager(t)
	request := ActionRequest{ID: "request-1", TaskID: "task-1", Origin: "telegram/TG-7", Question: "Retry or stop?", State: "awaiting_response", ReminderDueAt: now.Add(-time.Minute), MaxReminders: 2}
	path := manager.Config.StatePath("runtime", "action-requests", request.ID+".json")
	if err := writePrivateJSON(path, request); err != nil {
		t.Fatal(err)
	}
	if err := manager.processActionReminders(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.processActionReminders(context.Background()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(manager.Outbox.Directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("reminder entries = %d, %v", len(entries), err)
	}
	manager.Outbox.Deliver = func(context.Context, Origin, string, string) ([]string, error) { return []string{"native-1"}, nil }
	if err := manager.Outbox.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	var stored ActionRequest
	if err := readPrivateJSON(path, &stored, 64<<10); err != nil {
		t.Fatal(err)
	}
	if stored.ReminderCount != 1 || !stored.ReminderDueAt.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("delivered reminder state = %#v", stored)
	}
}

func TestQuietHoursDefersNonUrgentReminderAndUrgentBypasses(t *testing.T) {
	manager, now := actionRequestManager(t)
	*now = time.Date(2026, 8, 8, 23, 30, 0, 0, time.UTC)
	manager.Config.Notifications.QuietHours = config.QuietHours{Enabled: true, Start: "22:00", End: "07:00"}
	write := func(request ActionRequest) string {
		path := manager.Config.StatePath("runtime", "action-requests", request.ID+".json")
		if err := writePrivateJSON(path, request); err != nil {
			t.Fatal(err)
		}
		return path
	}
	deferredPath := write(ActionRequest{ID: "deferred", TaskID: "task-1", Origin: "telegram/TG-7", Question: "Proceed?", Urgency: "normal", State: "awaiting_response", ReminderDueAt: now.Add(-time.Minute), MaxReminders: 1})
	write(ActionRequest{ID: "urgent", TaskID: "task-2", Origin: "telegram/TG-7", Question: "Proceed now?", Urgency: "urgent", State: "awaiting_response", ReminderDueAt: now.Add(-time.Minute), MaxReminders: 1})
	if err := manager.processActionReminders(context.Background()); err != nil {
		t.Fatal(err)
	}
	var deferred ActionRequest
	if err := readPrivateJSON(deferredPath, &deferred, 64<<10); err != nil {
		t.Fatal(err)
	}
	wantEnd := time.Date(2026, 8, 9, 7, 0, 0, 0, time.UTC)
	if !deferred.ReminderDueAt.Equal(wantEnd) {
		t.Fatalf("deferred until %s, want %s", deferred.ReminderDueAt, wantEnd)
	}
	entries, err := os.ReadDir(manager.Outbox.Directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("urgent outbox entries = %d, %v", len(entries), err)
	}
}

func TestActionRequestStatusForTaskIsBoundedAndNewest(t *testing.T) {
	manager, now := actionRequestManager(t)
	for _, request := range []ActionRequest{
		{ID: "old", TaskID: "task-1", Origin: "telegram/TG-secret", Question: "secret old", State: "awaiting_response", CreatedAt: now.Add(-time.Hour)},
		{ID: "new", TaskID: "task-1", Origin: "whatsapp/WA-secret", Question: "secret new", SentChannel: "whatsapp", State: "answered", CreatedAt: *now, ReminderCount: 2, MaxReminders: 3, AcknowledgedAt: *now},
	} {
		if err := writePrivateJSON(manager.Config.StatePath("runtime", "action-requests", request.ID+".json"), request); err != nil {
			t.Fatal(err)
		}
	}
	status, ok := manager.ActionRequestStatusForTask("task-1")
	if !ok || status.State != "answered" || status.SentChannel != "whatsapp" || status.ReminderCount != 2 || !status.Acknowledged {
		t.Fatalf("status = %#v, %t", status, ok)
	}
	if _, ok := manager.ActionRequestStatusForTask("missing"); ok {
		t.Fatal("missing task returned action state")
	}
}

func TestReminderUsesMostRecentAuthorizedBoundRemoteContact(t *testing.T) {
	manager, now := actionRequestManager(t)
	manager.Config.Notifications.ContactBindings = []config.ContactBinding{{
		Principal: "owner", Contacts: []string{"tui/local", "telegram/TG-7", "whatsapp/WA-1555"},
	}}
	manager.AuthorizeNotificationOrigin = func(origin Origin) error {
		if origin.Channel == "whatsapp" {
			return os.ErrPermission
		}
		return nil
	}
	if err := manager.RecordContactActivity("telegram/TG-7", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := manager.RecordContactActivity("whatsapp/WA-1555", *now); err != nil {
		t.Fatal(err)
	}
	request := ActionRequest{ID: "request-bound", TaskID: "task-1", Origin: "tui/local", Question: "Proceed?", State: "awaiting_response", ReminderDueAt: now.Add(-time.Minute), MaxReminders: 1}
	if err := writePrivateJSON(manager.Config.StatePath("runtime", "action-requests", request.ID+".json"), request); err != nil {
		t.Fatal(err)
	}
	if err := manager.processActionReminders(context.Background()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(manager.Outbox.Directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("outbox = %d, %v", len(entries), err)
	}
	var reminder OutboxEntry
	if err := readPrivateJSON(filepath.Join(manager.Outbox.Directory, entries[0].Name()), &reminder, 64<<10); err != nil {
		t.Fatal(err)
	}
	if reminder.Origin != "telegram/TG-7" {
		t.Fatalf("reminder origin = %q, want most recent authorized remote", reminder.Origin)
	}
}

func TestReminderOwnerHandoffReconstructsWithoutDuplicate(t *testing.T) {
	manager, now := actionRequestManager(t)
	manager.primaryOwned.Store(false)
	request := ActionRequest{ID: "handoff", TaskID: "task-1", Origin: "telegram/TG-7", Question: "Proceed?", Urgency: "normal", State: "awaiting_response", ReminderDueAt: now.Add(-time.Minute), MaxReminders: 1}
	if err := writePrivateJSON(manager.Config.StatePath("runtime", "action-requests", request.ID+".json"), request); err != nil {
		t.Fatal(err)
	}
	if err := manager.processActionReminders(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(manager.Outbox.Directory); !os.IsNotExist(err) {
		t.Fatalf("non-primary created reminder state: %v", err)
	}
	restarted := &Manager{Config: manager.Config, Outbox: &Outbox{Directory: manager.Outbox.Directory, Now: manager.Outbox.Now}, AuthorizeNotificationOrigin: manager.AuthorizeNotificationOrigin}
	restarted.primaryOwned.Store(true)
	if err := restarted.processActionReminders(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := restarted.processActionReminders(context.Background()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(restarted.Outbox.Directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("handoff reminders = %d, %v", len(entries), err)
	}
}

func TestActionDeliveryDisconnectReconnectRetainsNativeCorrelation(t *testing.T) {
	manager, now := actionRequestManager(t)
	request := ActionRequest{ID: "reconnect", TaskID: "task-1", Origin: "telegram/TG-7", State: "pending_delivery", CreatedAt: *now, MaxReminders: 1}
	requestPath := manager.Config.StatePath("runtime", "action-requests", request.ID+".json")
	if err := writePrivateJSON(requestPath, request); err != nil {
		t.Fatal(err)
	}
	entry, err := manager.Outbox.Enqueue("reconnect-event", "waiting", request.Origin, "waiting")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Outbox.linkAction(entry.ID, request.ID, "action_request"); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	manager.Outbox.Deliver = func(context.Context, Origin, string, string) ([]string, error) {
		attempts++
		if attempts == 1 {
			return nil, os.ErrNotExist
		}
		return []string{"native-reconnected"}, nil
	}
	if err := manager.Outbox.Process(context.Background()); err == nil {
		t.Fatal("disconnect was not retained for retry")
	}
	*now = now.Add(2 * time.Second)
	if err := manager.Outbox.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	var stored ActionRequest
	if err := readPrivateJSON(requestPath, &stored, 64<<10); err != nil {
		t.Fatal(err)
	}
	if stored.State != "awaiting_response" || len(stored.Deliveries) != 1 || stored.Deliveries[0].NativeMessageIDs[0] != "native-reconnected" {
		t.Fatalf("reconnected delivery = %#v", stored)
	}
}

func TestUnboundReminderStaysOnOrigin(t *testing.T) {
	manager, _ := actionRequestManager(t)
	origin, reason, err := manager.reminderOrigin("tui/local")
	if err != nil {
		t.Fatal(err)
	}
	if origin != (Origin{Channel: "tui", Conversation: "local"}) || !strings.Contains(reason, "explicit trusted identity binding") {
		t.Fatalf("selection = %#v, %q", origin, reason)
	}
}

func TestDurableTaskResumeAnswersAndCancelsPendingReminder(t *testing.T) {
	manager, now := actionRequestManager(t)
	request := ActionRequest{ID: "request-1", TaskID: "task-1", Outcome: "waiting", Origin: "tui/local", Question: "Use A or B?", State: "awaiting_response", ReminderDueAt: now.Add(-time.Minute), MaxReminders: 2}
	requestPath := manager.Config.StatePath("runtime", "action-requests", request.ID+".json")
	if err := writePrivateJSON(requestPath, request); err != nil {
		t.Fatal(err)
	}
	if err := manager.processActionReminders(context.Background()); err != nil {
		t.Fatal(err)
	}
	taskPath := filepath.Join(manager.Config.Root, ".spynel", "tasks", "todo", "task.md")
	document := Document{FrontMatter: map[string]any{"id": "task-1", "title": "Task", "status": "todo", "updated_at": now.Format(time.RFC3339)}, Body: "\n## Progress\n\n- User chose A.\n"}
	if err := WriteDocument(taskPath, document); err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileActionRequests(); err != nil {
		t.Fatal(err)
	}
	var stored ActionRequest
	if err := readPrivateJSON(requestPath, &stored, 64<<10); err != nil {
		t.Fatal(err)
	}
	if stored.State != "answered" || stored.AcknowledgedAt.IsZero() || stored.ReminderDueAt != (time.Time{}) {
		t.Fatalf("resolved request = %#v", stored)
	}
	entries, err := os.ReadDir(manager.Outbox.Directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("outbox = %d, %v", len(entries), err)
	}
	var reminder OutboxEntry
	if err := readPrivateJSON(filepath.Join(manager.Outbox.Directory, entries[0].Name()), &reminder, 128<<10); err != nil || reminder.State != "cancelled" {
		t.Fatalf("reminder = %#v, %v", reminder, err)
	}
}

func TestFailedActionRequestRequiresTransitionAwayFromFailed(t *testing.T) {
	manager, now := actionRequestManager(t)
	// Omit Outcome to exercise migration of a record created before that field
	// existed; the stable transition identity still carries the failed outcome.
	request := ActionRequest{ID: "failed-request", TaskID: "task-failed", TransitionID: "lease:failed", Origin: "tui/local", Question: "Retry?", State: "awaiting_response", ReminderDueAt: now.Add(time.Hour)}
	requestPath := manager.Config.StatePath("runtime", "action-requests", request.ID+".json")
	if err := writePrivateJSON(requestPath, request); err != nil {
		t.Fatal(err)
	}
	taskPath := filepath.Join(manager.Config.Root, ".spynel", "tasks", "failed", "failed.md")
	if err := WriteDocument(taskPath, Document{FrontMatter: map[string]any{"id": request.TaskID, "title": "Failed task", "status": "failed"}}); err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileActionRequests(); err != nil {
		t.Fatal(err)
	}
	var stored ActionRequest
	if err := readPrivateJSON(requestPath, &stored, 64<<10); err != nil {
		t.Fatal(err)
	}
	if stored.State != "awaiting_response" || !stored.AcknowledgedAt.IsZero() {
		t.Fatalf("failed outcome self-acknowledged: %#v", stored)
	}
	resumedPath := filepath.Join(manager.Config.Root, ".spynel", "tasks", "todo", "failed.md")
	if err := os.MkdirAll(filepath.Dir(resumedPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(taskPath, resumedPath); err != nil {
		t.Fatal(err)
	}
	document, err := ReadDocument(resumedPath)
	if err != nil {
		t.Fatal(err)
	}
	document.FrontMatter["status"] = "todo"
	if err := WriteDocument(resumedPath, document); err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileActionRequests(); err != nil {
		t.Fatal(err)
	}
	if err := readPrivateJSON(requestPath, &stored, 64<<10); err != nil {
		t.Fatal(err)
	}
	if stored.State != "answered" || stored.AcknowledgedAt.IsZero() {
		t.Fatalf("validated failed-task transition was not acknowledged: %#v", stored)
	}
}

func TestConcurrentResolutionAndReminderLeaveNoOutstandingDelivery(t *testing.T) {
	manager, now := actionRequestManager(t)
	request := ActionRequest{ID: "race", TaskID: "task-race", Outcome: "waiting", Origin: "tui/local", Question: "Proceed?", State: "awaiting_response", ReminderDueAt: now.Add(-time.Minute), MaxReminders: 2}
	requestPath := manager.Config.StatePath("runtime", "action-requests", request.ID+".json")
	if err := writePrivateJSON(requestPath, request); err != nil {
		t.Fatal(err)
	}
	taskPath := filepath.Join(manager.Config.Root, ".spynel", "tasks", "todo", "race.md")
	if err := WriteDocument(taskPath, Document{FrontMatter: map[string]any{"id": "task-race", "title": "Race", "status": "todo"}}); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	errorsSeen := make(chan error, 2)
	group.Add(2)
	go func() { defer group.Done(); errorsSeen <- manager.processActionReminders(context.Background()) }()
	go func() { defer group.Done(); errorsSeen <- manager.reconcileActionRequests() }()
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	var stored ActionRequest
	if err := readPrivateJSON(requestPath, &stored, 64<<10); err != nil || stored.State != "answered" {
		t.Fatalf("request = %#v, %v", stored, err)
	}
	entries, err := os.ReadDir(manager.Outbox.Directory)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, entry := range entries {
		var outbox OutboxEntry
		if err := readPrivateJSON(filepath.Join(manager.Outbox.Directory, entry.Name()), &outbox, 64<<10); err != nil {
			t.Fatal(err)
		}
		if outbox.ActionRequestID == request.ID && outbox.Kind == "reminder" && outbox.State == "pending" {
			t.Fatalf("answered request retained pending reminder: %#v", outbox)
		}
	}
}
