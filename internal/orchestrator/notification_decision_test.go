package orchestrator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/extensions"
	"github.com/agent0ai/spynel/internal/instructions"
	"github.com/agent0ai/spynel/internal/workspace"
)

type notificationActionHarness struct {
	action func(context.Context, string) error
	calls  atomic.Int32
	mu     sync.Mutex
	prompt string
}

type delayedOrdinaryAgentHarness struct {
	active     atomic.Bool
	calls      atomic.Int32
	interrupts atomic.Int32
	steered    bool
	admitted   chan struct{}
	release    chan struct{}
	action     func(string) error
	mu         sync.Mutex
	prompt     string
}

func newDelayedOrdinaryAgentHarness() *delayedOrdinaryAgentHarness {
	return &delayedOrdinaryAgentHarness{admitted: make(chan struct{}), release: make(chan struct{})}
}

func (*delayedOrdinaryAgentHarness) Start(context.Context) error { return nil }
func (*delayedOrdinaryAgentHarness) Close() error                { return nil }
func (h *delayedOrdinaryAgentHarness) Interrupt(context.Context, string) (bool, error) {
	h.interrupts.Add(1)
	h.active.Store(false)
	return true, nil
}
func (*delayedOrdinaryAgentHarness) ResetSession(string) error { return nil }
func (*delayedOrdinaryAgentHarness) ThreadID(string) string    { return "thread-delayed" }
func (h *delayedOrdinaryAgentHarness) IsActive(string) bool    { return h.active.Load() }
func (h *delayedOrdinaryAgentHarness) Send(_ context.Context, _ string, prompt string, emit core.Emit) (string, bool, error) {
	h.calls.Add(1)
	h.active.Store(true)
	h.mu.Lock()
	h.prompt = prompt
	h.mu.Unlock()
	emit(core.Event{Kind: core.EventStatus, Text: "Codex turn started", ThreadID: "thread-delayed", TurnID: "turn-delayed",
		Execution: &core.ExecutionStatus{State: "running"}})
	close(h.admitted)
	go func() {
		<-h.release
		if h.action != nil {
			_ = h.action(prompt)
		}
		emit(core.Event{Kind: core.EventDelta, Text: "agent acted", ThreadID: "thread-delayed", TurnID: "turn-delayed"})
		emit(core.Event{Kind: core.EventFinal, Text: "agent finished", ThreadID: "thread-delayed", TurnID: "turn-delayed", Done: true})
		h.active.Store(false)
		emit(core.Event{Kind: core.EventFinal, Text: "duplicate", Done: true})
	}()
	return "thread-delayed", h.steered, nil
}

type ordinaryJobRecorder struct {
	mu       sync.Mutex
	events   []core.Event
	started  int
	finished int
	done     chan struct{}
}

func newOrdinaryJobRecorder(manager *Manager) *ordinaryJobRecorder {
	recorder := &ordinaryJobRecorder{done: make(chan struct{}, 1)}
	manager.JobStarted = func(Lease, string, time.Time, int, int) (int, error) {
		recorder.mu.Lock()
		recorder.started++
		recorder.mu.Unlock()
		return 41, nil
	}
	manager.JobEvent = func(_ int, event core.Event) {
		recorder.mu.Lock()
		recorder.events = append(recorder.events, event)
		recorder.mu.Unlock()
	}
	manager.JobFinished = func(int) {
		recorder.mu.Lock()
		recorder.finished++
		recorder.mu.Unlock()
		select {
		case recorder.done <- struct{}{}:
		default:
		}
	}
	return recorder
}

func (r *ordinaryJobRecorder) snapshot() (int, int, []core.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started, r.finished, append([]core.Event(nil), r.events...)
}

func (*notificationActionHarness) Start(context.Context) error { return nil }
func (*notificationActionHarness) Close() error                { return nil }
func (*notificationActionHarness) Interrupt(context.Context, string) (bool, error) {
	return false, nil
}
func (*notificationActionHarness) ResetSession(string) error { return nil }
func (*notificationActionHarness) ThreadID(string) string    { return "" }
func (*notificationActionHarness) IsActive(string) bool      { return false }
func (h *notificationActionHarness) Send(ctx context.Context, _ string, prompt string, _ core.Emit) (string, bool, error) {
	h.calls.Add(1)
	h.mu.Lock()
	h.prompt = prompt
	h.mu.Unlock()
	if h.action != nil {
		return "ignored provider prose", false, h.action(ctx, prompt)
	}
	return "ignored provider prose", false, nil
}

func notificationTestManager(t *testing.T, mode string, target *notificationActionHarness) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Orchestrator.TaskNotifications = mode
	manager := New(cfg, target, extensions.Runner{})
	path := filepath.Join(cfg.StatePath("tasks", "done"), "task.md")
	doc := Document{FrontMatter: map[string]any{
		"id": "task-1", "title": "Publish report", "status": "done", "attempt": 1,
		"notify": map[string]any{"enabled": true, "origin": "tui/local", "on": []any{"done"}},
	}, Body: "# Publish report\n\n## Progress\n\n- 2026-08-09T11:00:00Z — Report published.\n"}
	if err := WriteDocument(path, doc); err != nil {
		t.Fatal(err)
	}
	return manager, path
}

func TestNotificationAgentPromptSuppliesTaskAndOrdinaryCLI(t *testing.T) {
	manager, path := notificationTestManager(t, config.TaskNotificationsDecide, &notificationActionHarness{})
	if err := os.WriteFile(manager.Config.StatePath("instructions", "agent-notification.md"), []byte("notification-only-rule"), 0o600); err != nil {
		t.Fatal(err)
	}
	prompt, err := manager.notificationAgentPrompt(path, "done")
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wantCommand := notificationCommand(executable, manager.Config.Root, "tui/local")
	for _, want := range []string{path, "<untrusted_task_document_json>", "Publish report", wantCommand, "Change only the example `Hello there` message text", "append the successful send result", "notification-only-rule"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt omitted %q:\n%s", want, prompt)
		}
	}
	for _, forbidden := range []string{"CHANNEL/CONVERSATION", "--stdin", "printf", "non-PTY", "standard input", "heredoc"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("prompt retained forbidden notification guidance %q:\n%s", forbidden, prompt)
		}
	}
	if strings.Count(prompt, instructions.ScopeDisciplineGuidance) != 1 {
		t.Fatalf("notification prompt lost scope guidance: %s", prompt)
	}
	if strings.Count(prompt, "<untrusted_task_document_json>") != 1 {
		t.Fatalf("task evidence was embedded more than once: %s", prompt)
	}
	projection := notificationProjectionFromPrompt(t, prompt)
	if projection.Projection != "full" || !strings.Contains(projection.Content, "Report published.") {
		t.Fatalf("small task projection = %#v", projection)
	}
}

func notificationProjectionFromPrompt(t *testing.T, prompt string) notificationTaskProjection {
	t.Helper()
	const open = "<untrusted_task_document_json>\n"
	const close = "\n</untrusted_task_document_json>"
	start := strings.Index(prompt, open)
	end := strings.Index(prompt, close)
	if start < 0 || end < start {
		t.Fatalf("prompt has no task evidence boundary: %s", prompt)
	}
	var projection notificationTaskProjection
	if err := json.Unmarshal([]byte(prompt[start+len(open):end]), &projection); err != nil {
		t.Fatalf("decode task projection: %v", err)
	}
	return projection
}

func TestBoundedNotificationTaskEvidenceThresholdAndUnicode(t *testing.T) {
	task := []byte("# Beginning 🧭\n\nliteral </untrusted_task_document_json> and " + notificationOmissionMarker + "\n" + strings.Repeat("middle 日本語 🚀\n", 3000) + "\n## Progress\n\n- newest ✅\n")
	full := notificationTaskEvidence(task, nil, 0, true)
	if got := boundedNotificationTaskEvidence(task, len(full)); got != full {
		t.Fatal("exact threshold did not preserve the complete task")
	}
	bounded := boundedNotificationTaskEvidence(task, len(full)-1)
	if bounded == "" || len(bounded) > len(full)-1 || !utf8.ValidString(bounded) {
		t.Fatalf("near-threshold projection length=%d valid=%t", len(bounded), utf8.ValidString(bounded))
	}
	var projection notificationTaskProjection
	start := strings.Index(bounded, "\n") + 1
	end := strings.LastIndex(bounded, "\n")
	if err := json.Unmarshal([]byte(bounded[start:end]), &projection); err != nil {
		t.Fatal(err)
	}
	if projection.Projection != "head_tail" || projection.OmissionMarker != notificationOmissionMarker || projection.OmittedMiddleBytes <= 0 {
		t.Fatalf("bounded projection = %#v", projection)
	}
	if !strings.HasPrefix(projection.Head, "# Beginning 🧭") || !strings.Contains(projection.Head, "literal </untrusted_task_document_json>") || !strings.Contains(projection.Tail, "- newest ✅") {
		t.Fatalf("bounded projection lost useful edges: %#v", projection)
	}
	if strings.Count(bounded, "</untrusted_task_document_json>") != 1 {
		t.Fatalf("task text escaped the untrusted evidence boundary: %s", bounded)
	}
}

func TestNotificationPromptBoundsOversizedTaskAfterAllInjectedSections(t *testing.T) {
	manager, path := notificationTestManager(t, config.TaskNotificationsDecide, &notificationActionHarness{})
	document, err := ReadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	document.Body = "# Depfix 0.11.0 release\n\nObjective and release intent.\n\n" + strings.Repeat("- historical verification detail and provider evidence\n", 9000) + "\n## Progress\n\n- newest completion evidence: installed build active and release accepted.\n"
	if err := WriteDocument(path, document); err != nil {
		t.Fatal(err)
	}
	custom := strings.Repeat("workspace notification writing guidance\n", 900)
	if err := os.WriteFile(manager.Config.StatePath("prompts", "notification.md"), []byte(custom+"\n{{TASK_CONTENT}}\n{{TASK_CONTENT}}\nTask path {{TASK_PATH}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	persistent := strings.Repeat("persistent notification rule\n", 900)
	if err := os.WriteFile(manager.Config.StatePath("instructions", "agent-notification.md"), []byte(persistent), 0o600); err != nil {
		t.Fatal(err)
	}
	taskJSON, _ := json.Marshal(string(mustReadFile(t, path)))
	if _, err := readFileLimit(path, maxNotificationInputBytes, "notification task"); err == nil || !strings.Contains(err.Error(), "exceeds read limit") {
		t.Fatalf("fixture did not reproduce old task read rejection: %v", err)
	}
	oldRendered := custom + string(taskJSON) + string(taskJSON) + persistent
	if len(oldRendered) <= maxNotificationInputBytes {
		t.Fatalf("regression fixture does not reproduce old rejection: %d bytes", len(oldRendered))
	}
	prompt, err := manager.notificationAgentPrompt(path, "done")
	if err != nil {
		t.Fatal(err)
	}
	if len(prompt) > maxNotificationInputBytes-notificationPromptSafetyBytes {
		t.Fatalf("rendered prompt = %d bytes, budget %d", len(prompt), maxNotificationInputBytes-notificationPromptSafetyBytes)
	}
	if strings.Count(prompt, "<untrusted_task_document_json>") != 1 || !strings.Contains(prompt, string(mustJSON(t, path))) {
		t.Fatalf("prompt did not contain one evidence block and exact JSON path")
	}
	projection := notificationProjectionFromPrompt(t, prompt)
	if projection.Projection != "head_tail" || !strings.Contains(projection.Head, "Depfix 0.11.0") || !strings.Contains(projection.Tail, "newest completion evidence") || projection.OmissionMarker != notificationOmissionMarker {
		t.Fatalf("oversized projection = %#v", projection)
	}
}

func TestOversizedTaskTransitionAdmitsOneNotificationJobAndAgentJournals(t *testing.T) {
	target := &notificationActionHarness{}
	manager, path := notificationTestManager(t, config.TaskNotificationsAlways, target)
	document, err := ReadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	document.Body = "# Controlled oversized notification canary\n\n" + strings.Repeat("- accumulated progress evidence\n", 9000) + "\n## Progress\n\n- terminal result ready for notification.\n"
	if err := WriteDocument(path, document); err != nil {
		t.Fatal(err)
	}
	target.action = func(_ context.Context, prompt string) error {
		const open = "<untrusted_task_document_json>\n"
		const close = "\n</untrusted_task_document_json>"
		start, end := strings.Index(prompt, open), strings.Index(prompt, close)
		if start < 0 || end < start {
			t.Errorf("transition prompt omitted task evidence boundary")
			return nil
		}
		var projection notificationTaskProjection
		if err := json.Unmarshal([]byte(prompt[start+len(open):end]), &projection); err != nil {
			t.Errorf("decode transition projection: %v", err)
			return nil
		}
		if projection.Projection != "head_tail" || !strings.Contains(projection.Head, "Controlled oversized notification canary") || !strings.Contains(projection.Tail, "terminal result ready") {
			t.Errorf("transition projection = %#v", projection)
		}
		return updateDocumentProgress(path, time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC), "Notification agent sent the controlled test notification through the authorized origin.")
	}
	lease := Lease{ID: "oversized-canary", Route: "tasks", Phase: phaseTaskImplementation, ClaimAttempt: 1}
	if err := manager.completeTransition(context.Background(), manager.Config.Orchestrator.Routes[0], lease, "done", path); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	if target.calls.Load() != 1 {
		t.Fatalf("notification jobs admitted = %d, want 1", target.calls.Load())
	}
	journaled, err := ReadDocument(path)
	if err != nil || !strings.Contains(journaled.Body, "sent the controlled test notification") {
		t.Fatalf("agent-owned notification journal = %q, %v", journaled.Body, err)
	}
}

func TestNotificationPromptRejectsOversizedNonTaskSectionsPrecisely(t *testing.T) {
	manager, path := notificationTestManager(t, config.TaskNotificationsDecide, &notificationActionHarness{})
	if err := os.WriteFile(manager.Config.StatePath("prompts", "notification.md"), []byte(strings.Repeat("custom template guidance\n", 3000)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.Config.StatePath("instructions", "agent-notification.md"), []byte(strings.Repeat("persistent rule\n", 3900)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.notificationAgentPrompt(path, "done"); err == nil || !strings.Contains(err.Error(), "non-task sections exceed bounded input budget") {
		t.Fatalf("non-task overflow error = %v", err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustJSON(t *testing.T, value string) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestNotificationAgentPromptSanitizesStaleDeliveryGuidanceAndPreservesCustomization(t *testing.T) {
	manager, path := notificationTestManager(t, config.TaskNotificationsDecide, &notificationActionHarness{})
	stale := `# Custom notification policy

Keep this workspace-specific decision rule.

Inspect the task and use the exact authorized notify.origin from its metadata.

Supply the message on standard input using a non-PTY facility; do not change the config path or use a heredoc.

Record the result in the task progress.`
	if err := os.WriteFile(manager.Config.StatePath("prompts", "notification.md"), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	prompt, err := manager.notificationAgentPrompt(path, "done")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Keep this workspace-specific decision rule.",
		"Record the result in the task progress.",
		notificationCommand(mustExecutable(t), manager.Config.Root, "tui/local"),
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt omitted preserved guidance %q:\n%s", want, prompt)
		}
	}
	for _, forbidden := range []string{"notify.origin", "standard input", "non-PTY", "config path", "heredoc", "--stdin", "CHANNEL/CONVERSATION"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("prompt retained stale delivery guidance %q:\n%s", forbidden, prompt)
		}
	}
}

func mustExecutable(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return executable
}

func TestQualifyingTransitionDirectlyStartsOneOrdinaryJobWithoutRuntimeEvent(t *testing.T) {
	target := &notificationActionHarness{}
	manager, path := notificationTestManager(t, config.TaskNotificationsDecide, target)
	document, err := ReadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	before := document.Body
	lease := Lease{ID: "lease-1", Route: "tasks", Phase: phaseTaskImplementation, ClaimAttempt: 1}
	if err := manager.completeTransition(context.Background(), manager.Config.Orchestrator.Routes[0], lease, "done", path); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	if target.calls.Load() != 1 {
		t.Fatalf("notification harness calls = %d, want 1", target.calls.Load())
	}
	for _, obsolete := range []string{"notification-agents", "notification-agent-locks"} {
		if _, err := os.Stat(manager.Config.StatePath("runtime", obsolete)); !os.IsNotExist(err) {
			t.Fatalf("direct dispatch created obsolete runtime state %q: %v", obsolete, err)
		}
	}
	after, err := ReadDocument(path)
	if err != nil || after.Body != before {
		t.Fatalf("provider prose became a framework task result: %v\n%s", err, after.Body)
	}
}

func TestNotificationJobWaitsForDelayedProviderTerminalAndCapturesCompleteStream(t *testing.T) {
	target := newDelayedOrdinaryAgentHarness()
	manager, path := notificationTestManager(t, config.TaskNotificationsDecide, nil)
	manager.Harness = target
	recorder := newOrdinaryJobRecorder(manager)
	target.action = func(prompt string) error {
		if !strings.Contains(prompt, "<untrusted_task_document_json>") || !strings.Contains(prompt, "Publish report") || !strings.Contains(prompt, "--message \"Hello there\"") || strings.Contains(prompt, "--stdin") {
			t.Errorf("notification agent did not receive its complete prompt: %q", prompt)
		}
		return updateDocumentProgress(path, time.Date(2026, 8, 13, 15, 45, 0, 0, time.UTC), "Notification agent skipped sending after inspecting current policy.")
	}
	manager.startTaskNotificationAgent(context.Background(), Lease{ID: "lease-1", Route: "tasks", Phase: phaseTaskImplementation, ClaimAttempt: 1}, "done", path)
	select {
	case <-target.admitted:
	case <-time.After(time.Second):
		t.Fatal("notification provider was not admitted")
	}
	_, finished, events := recorder.snapshot()
	if finished != 0 || len(events) != 1 || events[0].Text != "Codex turn started" {
		t.Fatalf("job ended at asynchronous admission: finished=%d events=%#v", finished, events)
	}
	close(target.release)
	select {
	case <-recorder.done:
	case <-time.After(time.Second):
		t.Fatal("notification job did not finish after provider terminal")
	}
	manager.Wait()
	started, finished, events := recorder.snapshot()
	if started != 1 || finished != 1 || len(events) != 3 || events[0].Text != "Codex turn started" || events[1].Text != "agent acted" || events[2].Text != "agent finished" {
		t.Fatalf("archived lifecycle = started=%d finished=%d events=%#v", started, finished, events)
	}
	document, err := ReadDocument(path)
	if err != nil || !strings.Contains(document.Body, "Notification agent skipped sending after inspecting current policy.") {
		t.Fatalf("agent-owned notification journal = %q, %v", document.Body, err)
	}
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	restarted := New(manager.Config, target, extensions.Runner{})
	if err := restarted.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted.Wait()
	if target.calls.Load() != 1 {
		t.Fatalf("repeated or restarted scan launched %d notification turns, want 1", target.calls.Load())
	}
}

func TestNotificationAsyncTimeoutInterruptsAndFinishesExactlyOnce(t *testing.T) {
	target := newDelayedOrdinaryAgentHarness()
	manager, path := notificationTestManager(t, config.TaskNotificationsDecide, nil)
	manager.Harness = target
	manager.notificationAgentTimeout = 20 * time.Millisecond
	recorder := newOrdinaryJobRecorder(manager)
	manager.startTaskNotificationAgent(context.Background(), Lease{ID: "lease-timeout", Route: "tasks", Phase: phaseTaskImplementation, ClaimAttempt: 1}, "done", path)
	select {
	case <-target.admitted:
	case <-time.After(time.Second):
		t.Fatal("notification provider was not admitted")
	}
	select {
	case <-recorder.done:
	case <-time.After(time.Second):
		t.Fatal("timed-out notification job did not finish")
	}
	manager.Wait()
	close(target.release)
	time.Sleep(10 * time.Millisecond)
	started, finished, events := recorder.snapshot()
	if started != 1 || finished != 1 || target.interrupts.Load() != 1 || len(events) != 2 || events[0].Text != "Codex turn started" || events[1].Kind != core.EventError {
		t.Fatalf("timeout lifecycle = started=%d finished=%d interrupts=%d events=%#v", started, finished, target.interrupts.Load(), events)
	}
}

func TestNotificationParentCancellationSettlesAdmittedJobOnce(t *testing.T) {
	target := newDelayedOrdinaryAgentHarness()
	manager, path := notificationTestManager(t, config.TaskNotificationsDecide, nil)
	manager.Harness = target
	recorder := newOrdinaryJobRecorder(manager)
	ctx, cancel := context.WithCancel(context.Background())
	manager.startTaskNotificationAgent(ctx, Lease{ID: "lease-handoff", Route: "tasks", Phase: phaseTaskImplementation, ClaimAttempt: 1}, "done", path)
	select {
	case <-target.admitted:
	case <-time.After(time.Second):
		t.Fatal("notification provider was not admitted")
	}
	cancel()
	select {
	case <-recorder.done:
	case <-time.After(time.Second):
		t.Fatal("cancelled notification job did not finish")
	}
	manager.Wait()
	close(target.release)
	time.Sleep(10 * time.Millisecond)
	started, finished, events := recorder.snapshot()
	if started != 1 || finished != 1 || target.interrupts.Load() != 1 || len(events) != 2 || events[1].Kind != core.EventError {
		t.Fatalf("cancellation lifecycle = started=%d finished=%d interrupts=%d events=%#v", started, finished, target.interrupts.Load(), events)
	}
}

func TestNotificationSteeringStillWaitsForTerminalAndSettlesOnce(t *testing.T) {
	target := newDelayedOrdinaryAgentHarness()
	target.steered = true
	manager, path := notificationTestManager(t, config.TaskNotificationsDecide, nil)
	manager.Harness = target
	recorder := newOrdinaryJobRecorder(manager)
	manager.startTaskNotificationAgent(context.Background(), Lease{ID: "lease-steered", Route: "tasks", Phase: phaseTaskImplementation, ClaimAttempt: 1}, "done", path)
	select {
	case <-target.admitted:
	case <-time.After(time.Second):
		t.Fatal("steered notification provider was not admitted")
	}
	if _, finished, _ := recorder.snapshot(); finished != 0 {
		t.Fatal("steering was treated as terminal completion")
	}
	close(target.release)
	select {
	case <-recorder.done:
	case <-time.After(time.Second):
		t.Fatal("steered notification did not finish after terminal")
	}
	manager.Wait()
	started, finished, events := recorder.snapshot()
	if started != 1 || finished != 1 || len(events) != 3 || events[2].Kind != core.EventFinal {
		t.Fatalf("steered lifecycle = started=%d finished=%d events=%#v", started, finished, events)
	}
}

func TestNotificationAgentOwnsDirectTaskProgressWrite(t *testing.T) {
	target := &notificationActionHarness{}
	manager, path := notificationTestManager(t, config.TaskNotificationsOff, target)
	target.action = func(_ context.Context, _ string) error {
		return updateDocumentProgress(path, time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), "Notification agent skipped sending because notifications are off.")
	}
	manager.startTaskNotificationAgent(context.Background(), Lease{ID: "lease-1", Route: "tasks", Phase: phaseTaskImplementation, ClaimAttempt: 1}, "done", path)
	manager.Wait()
	document, err := ReadDocument(path)
	if err != nil || !strings.Contains(document.Body, "Notification agent skipped sending because notifications are off.") {
		t.Fatalf("agent-owned progress write = %q, %v", document.Body, err)
	}
}

func TestTaskNotificationQualificationExcludesFutureScheduledWait(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		status string
		wakeAt string
		want   bool
	}{
		{status: "done", want: true},
		{status: "failed", want: true},
		{status: "cancelled", want: true},
		{status: "waiting", want: true},
		{status: "waiting", wakeAt: "invalid", want: true},
		{status: "waiting", wakeAt: now.Add(time.Hour).Format(time.RFC3339), want: false},
		{status: "waiting", wakeAt: now.Format(time.RFC3339), want: true},
	}
	for _, test := range tests {
		document := Document{FrontMatter: map[string]any{"wake_at": test.wakeAt}}
		if got := requiresTaskNotificationDecision(document, test.status, now); got != test.want {
			t.Fatalf("status=%s wake_at=%q qualification = %t, want %t", test.status, test.wakeAt, got, test.want)
		}
	}
}

func TestNotificationTimeoutBoundsOnlyTheOrdinaryAgentJob(t *testing.T) {
	target := &notificationActionHarness{}
	manager, path := notificationTestManager(t, config.TaskNotificationsDecide, target)
	manager.notificationAgentTimeout = time.Millisecond
	target.action = func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	manager.startTaskNotificationAgent(context.Background(), Lease{ID: "lease-1", Route: "tasks", Phase: phaseTaskImplementation, ClaimAttempt: 1}, "done", path)
	manager.Wait()
	if target.calls.Load() != 1 {
		t.Fatalf("bounded job calls = %d, want 1", target.calls.Load())
	}
}

func TestNotificationCommandGuidanceShellQuotesFrameworkPaths(t *testing.T) {
	for input, want := range map[string]string{"plain": "'plain'", "$(touch /tmp/unsafe)": "'$(touch /tmp/unsafe)'", "a'b": `'a'"'"'b'`} {
		if got := shellQuote(input); got != want {
			t.Fatalf("shellQuote(%q) = %q, want %q", input, got, want)
		}
	}
	command := notificationCommand("/opt/Spynel Current/spynel", "/tmp/work's space", "telegram/TG-$(unsafe)")
	want := `'/opt/Spynel Current/spynel' notify --workdir '/tmp/work'"'"'s space' --origin 'telegram/TG-$(unsafe)' --message "Hello there"`
	if command != want {
		t.Fatalf("shell-safe notification command = %q, want %q", command, want)
	}
}

func TestNotificationCommandsBindEveryTaskOriginAndWorkspace(t *testing.T) {
	for _, origin := range []string{"tui/new-6rrwdamb", "cli/local", "telegram/TG-518743883", "whatsapp/WA-15551234567"} {
		command := notificationCommand("/root/.local/bin/spynel", "/workspace", origin)
		want := `/root/.local/bin/spynel notify --workdir /workspace --origin '` + origin + `' --message "Hello there"`
		if command != want {
			t.Fatalf("command for %s = %q, want %q", origin, command, want)
		}
		for _, forbidden := range []string{"CHANNEL/CONVERSATION", "--stdin", "printf", "|", "<<"} {
			if strings.Contains(command, forbidden) {
				t.Fatalf("command for %s contains %q: %s", origin, forbidden, command)
			}
		}
	}
}

func TestNotificationPromptRejectsUnauthorizedMetadataBeforeCommandConstruction(t *testing.T) {
	for _, test := range []struct {
		name   string
		notify map[string]any
		want   string
	}{
		{name: "disabled", notify: map[string]any{"enabled": false}, want: "not enabled"},
		{name: "wrong outcome", notify: map[string]any{"enabled": true, "origin": "tui/local", "on": []any{"failed"}}, want: "not authorized"},
		{name: "invalid origin", notify: map[string]any{"enabled": true, "origin": "email/elsewhere", "on": []any{"done"}}, want: "unsupported origin"},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, path := notificationTestManager(t, config.TaskNotificationsDecide, &notificationActionHarness{})
			document, err := ReadDocument(path)
			if err != nil {
				t.Fatal(err)
			}
			document.FrontMatter["notify"] = test.notify
			if err := WriteDocument(path, document); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.notificationAgentPrompt(path, "done"); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("prompt error = %v, want %q", err, test.want)
			}
		})
	}
}
