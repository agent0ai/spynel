package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/agent0ai/spynel/internal/agentdocs"
	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/channel/telegram"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
	"github.com/agent0ai/spynel/internal/orchestrator"
	"github.com/agent0ai/spynel/internal/updater"
	"github.com/agent0ai/spynel/internal/workspace"
)

func TestFormatStatusShowsScheduledGoalCheckpoint(t *testing.T) {
	text := FormatStatus(StatusSnapshot{ScheduledGoals: []orchestrator.ScheduledCheckpoint{{
		ID: "goals-12345678", Title: "release", At: time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC), Reason: "bounded rollout check",
	}}})
	for _, want := range []string{"Scheduled goal checkpoint", "release", "2026-08-08T00:00:00Z", "bounded rollout check"} {
		if !strings.Contains(text, want) {
			t.Fatalf("status missing %q:\n%s", want, text)
		}
	}
}

type serviceHarness struct {
	mu       sync.Mutex
	prompts  map[string][]string
	threads  map[string]string
	active   map[string]bool
	resetErr error
}

type heldServiceHarness struct {
	*serviceHarness
	emits map[string]core.Emit
}

type notificationRouter struct{ calls []string }

func (r *notificationRouter) Deliver(_ context.Context, channelName, conversation, eventID, text string) error {
	r.calls = append(r.calls, channelName+"/"+conversation+"/"+eventID+"/"+text)
	return nil
}

func TestNotifyDeliversAllOriginsWithStableEventIdentity(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	cfg.Channels.Telegram.AllowedUsers = []string{"7"}
	cfg.Channels.WhatsApp.AllowedNumbers = []string{"15557654321"}
	service := New(cfg, newServiceHarness())
	router := &notificationRouter{}
	service.DeliveryControl = router
	for _, origin := range []string{"tui/local", "cli/local", "telegram/TG-7", "whatsapp/WA-15557654321"} {
		channelName, conversation, _ := strings.Cut(origin, "/")
		if _, err := service.History.Append(channelName, conversation, history.Entry{Role: "user", Content: "known"}); err != nil {
			t.Fatal(err)
		}
		id, err := service.Notify(context.Background(), origin, "\x1b]11;rgb:0000/0000/0000\x07\x1b[1;1Rcomplete")
		if err != nil {
			t.Fatalf("%s: %v", origin, err)
		}
		if id == "" {
			t.Fatalf("%s returned no event identity", origin)
		}
		entries, _, err := service.History.RecentEntries(channelName, conversation, 10, 10000)
		if err != nil {
			t.Fatal(err)
		}
		if entries[len(entries)-1].Content != "complete" || entries[len(entries)-1].Sender != "Spy" {
			t.Fatalf("%s history = %#v", origin, entries)
		}
	}
	if len(router.calls) != 2 || !strings.Contains(router.calls[0], "telegram/TG-7/") || !strings.Contains(router.calls[1], "whatsapp/WA-15557654321/") {
		t.Fatalf("remote calls = %#v", router.calls)
	}
	if strings.Contains(strings.Join(router.calls, "\n"), "rgb:0000") {
		t.Fatalf("terminal replies reached remote delivery: %#v", router.calls)
	}
}

func TestAutomaticNotifyCommandUsesAuthorizedTransitionAndDeduplicates(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	cfg.Orchestrator.TaskNotifications = config.TaskNotificationsAlways
	service := New(cfg, newServiceHarness())
	if _, err := service.History.Append("cli", "local", history.Entry{Role: "user", Content: "known"}); err != nil {
		t.Fatal(err)
	}
	taskFile := filepath.Join(cfg.StatePath("tasks", "done"), "task.md")
	document := orchestrator.Document{FrontMatter: map[string]any{
		"id": "task-1", "title": "Report", "status": "done", "attempt": 1,
		"notify": map[string]any{"enabled": true, "origin": "cli/local", "on": []any{"done"}},
	}, Body: "# Report\n\n## Progress\n\n- Complete.\n"}
	if err := orchestrator.WriteDocument(taskFile, document); err != nil {
		t.Fatal(err)
	}
	eventDirectory := cfg.StatePath("runtime", "notification-agents")
	if err := os.MkdirAll(eventDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	event := map[string]any{
		"id": "event-1", "task_id": "task-1", "outcome": "done", "task_file": taskFile,
		"transition": "task_implementation:1", "origin": "cli/local", "mode": "always", "state": "pending",
	}
	data, _ := json.Marshal(event)
	if err := os.WriteFile(filepath.Join(eventDirectory, "event-1.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := service.NotifyWithIdentity(context.Background(), "cli/local", "\x1b]11;rgb:0000/0000/0000\x07\x1b[1;1RThe report is ready.", "task-notification:event-1", "done")
	if err != nil {
		t.Fatal(err)
	}
	again, err := service.NotifyWithIdentity(context.Background(), "cli/local", "Changed retry wording.", "task-notification:event-1", "done")
	if err != nil || again != id {
		t.Fatalf("retry identity = %q, %v; want %q", again, err, id)
	}
	if _, err := service.NotifyWithIdentity(context.Background(), "cli/other", "redirect", "task-notification:event-1", "done"); err == nil {
		t.Fatal("automatic command redirected its authorized origin")
	}
	journaled, err := orchestrator.ReadDocument(taskFile)
	if err != nil || strings.Count(journaled.Body, "Sent the user a notification: The report is ready.") != 1 {
		t.Fatalf("transition journal = %#v, %v", journaled.Body, err)
	}
	eventData, err := os.ReadFile(filepath.Join(eventDirectory, "event-1.json"))
	if err != nil || !strings.Contains(string(eventData), `"journaled": true`) {
		t.Fatalf("transition receipt = %s, %v", eventData, err)
	}
	entries, _, err := service.History.RecentEntries("cli", "local", 10, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[1].Content != "The report is ready." {
		t.Fatalf("deduplicated history = %#v", entries)
	}
}

func TestNotificationDeclineRequiresAuthorizedDecideEvent(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	service := New(cfg, newServiceHarness())
	if _, err := service.History.Append("cli", "local", history.Entry{Role: "user", Content: "known"}); err != nil {
		t.Fatal(err)
	}
	taskFile := filepath.Join(cfg.StatePath("tasks", "done"), "task.md")
	document := orchestrator.Document{FrontMatter: map[string]any{
		"id": "task-1", "title": "Report", "status": "done", "attempt": 1,
		"notify": map[string]any{"enabled": true, "origin": "cli/local", "on": []any{"done"}},
	}, Body: "# Report\n\n## Progress\n\n- Complete.\n"}
	if err := orchestrator.WriteDocument(taskFile, document); err != nil {
		t.Fatal(err)
	}
	eventDirectory := cfg.StatePath("runtime", "notification-agents")
	if err := os.MkdirAll(eventDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	event := map[string]any{
		"id": "event-1", "task_id": "task-1", "outcome": "done", "task_file": taskFile,
		"transition": "task_implementation:1", "origin": "cli/local", "mode": "decide", "state": "pending",
	}
	data, _ := json.Marshal(event)
	if err := os.WriteFile(filepath.Join(eventDirectory, "event-1.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := service.DeclineNotification("cli/other", "task-notification:event-1", "done"); err == nil {
		t.Fatal("decline redirected its authorized origin")
	}
	if err := service.DeclineNotification("cli/local", "task-notification:event-1", "done"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(eventDirectory, "event-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if json.Unmarshal(data, &saved) != nil || saved["state"] != "declined" {
		t.Fatalf("declined event = %#v", saved)
	}
}

func TestNotifyUsesVerifiedTelegramUsernameMappingAndRechecksRevocation(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	cfg.Channels.Telegram.AllowedUsers = []string{" @FrD3L "}
	service := New(cfg, newServiceHarness())
	service.DeliveryControl = &notificationRouter{}
	if _, err := service.History.Append("telegram", "TG-518743883", history.Entry{Role: "user", Content: "known"}); err != nil {
		t.Fatal(err)
	}
	identities := telegram.NewIdentityStore(cfg.StatePath("runtime", "telegram-identities.json"))
	if err := identities.RecordVerifiedPrivate(518743883, 518743883, "frd3l"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Notify(context.Background(), "telegram/TG-518743883", "complete"); err != nil {
		t.Fatalf("verified username notification: %v", err)
	}
	if _, err := service.Settings.Update(func(next *config.Config) error {
		next.Channels.Telegram.AllowedUsers = []string{"someone_else"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.validateOrigin(orchestrator.Origin{Channel: "telegram", Conversation: "TG-518743883"}); err == nil {
		t.Fatal("revoked mapped username remained authorized")
	}
}

func TestTaskCommandDelegatesCreationPolicyToCommunicationAgent(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	harness := newServiceHarness()
	service := New(cfg, harness)
	if err := service.Handle(context.Background(), core.Message{Channel: "cli", Conversation: "deploy", Sender: "cli", Text: "/task ship it"}, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	prompts := harness.prompts["chat:cli:deploy"]
	if len(prompts) != 1 || !strings.Contains(prompts[0], "explicitly invoked `/task`") || !strings.Contains(prompts[0], "ship it") || !strings.Contains(prompts[0], "origin `cli/deploy`") || !strings.Contains(prompts[0], "review_required") || !strings.Contains(prompts[0], "cancelled") {
		t.Fatalf("task creation prompt = %#v", prompts)
	}
}

func TestChatAgentPrefixAppliesToOrdinaryAndExplicitSteerMessages(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	cfg.Harness.ChatAgentPrefix = "/ultrathink"
	instructionPath := cfg.StatePath("instructions", "agent-chat.md")
	if err := os.WriteFile(instructionPath, []byte("ordinary chat rule"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := newServiceHarness()
	service := New(cfg, target)
	message := core.Message{Channel: "cli", Conversation: "prefix", Text: "inspect this"}
	if err := service.Handle(context.Background(), message, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	key := "chat:cli:prefix"
	if got := target.prompts[key][0]; !strings.HasPrefix(got, "/ultrathink ") || !strings.Contains(got, "Configured task review mode: skip-trivial") || !strings.Contains(got, "ordinary chat rule") {
		t.Fatalf("ordinary chat prompt = %q", got)
	}
	if err := os.WriteFile(instructionPath, []byte("fresh steer chat rule"), 0o600); err != nil {
		t.Fatal(err)
	}
	target.mu.Lock()
	target.active[key] = true
	target.mu.Unlock()
	message.Text = "/steer follow up"
	if err := service.Handle(context.Background(), message, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if got := target.prompts[key][1]; !strings.HasPrefix(got, "/ultrathink follow up\n\n") || !strings.Contains(got, "Configured task review mode: skip-trivial") || !strings.Contains(got, "the chat agent from .spynel/instructions/agent-chat.md") || !strings.Contains(got, "fresh steer chat rule") || strings.Contains(got, "ordinary chat rule") || !strings.HasSuffix(got, "The precedence stated above still applies to every imported rule.") {
		t.Fatalf("steer prompt = %q", got)
	}
}

func TestRemoteNotificationEventIsNotRedeliveredAfterRetryOrRestart(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	cfg.Channels.Telegram.AllowedUsers = []string{"7"}
	service := New(cfg, newServiceHarness())
	router := &notificationRouter{}
	service.DeliveryControl = router
	_, _ = service.History.Append("telegram", "TG-7", history.Entry{Role: "user", Content: "known"})
	origin := orchestrator.Origin{Channel: "telegram", Conversation: "TG-7"}
	if err := service.deliverNotification(context.Background(), origin, "stable-id", "complete"); err != nil {
		t.Fatal(err)
	}
	if err := service.deliverNotification(context.Background(), origin, "stable-id", "complete"); err != nil {
		t.Fatal(err)
	}
	restarted := New(cfg, newServiceHarness())
	restarted.DeliveryControl = router
	if err := restarted.deliverNotification(context.Background(), origin, "stable-id", "complete"); err != nil {
		t.Fatal(err)
	}
	if len(router.calls) != 1 {
		t.Fatalf("duplicate provider deliveries = %#v", router.calls)
	}
}

func TestRemoteNotificationRetriesPreSendCrashMarker(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	cfg.Channels.Telegram.AllowedUsers = []string{"7"}
	service := New(cfg, newServiceHarness())
	router := &notificationRouter{}
	service.DeliveryControl = router
	_, _ = service.History.Append("telegram", "TG-7", history.Entry{Role: "user", Content: "known"})
	_, _ = service.History.Append("telegram", "TG-7", history.Entry{Role: "notification_sending", EventID: "interrupted"})
	if err := service.deliverNotification(context.Background(), orchestrator.Origin{Channel: "telegram", Conversation: "TG-7"}, "interrupted", "complete"); err != nil {
		t.Fatal(err)
	}
	if len(router.calls) != 1 {
		t.Fatalf("pre-send marker suppressed delivery: %#v", router.calls)
	}
}

type reconfigurableServiceHarness struct {
	*serviceHarness
	config harness.HarnessConfig
	calls  int
}

func (r *reconfigurableServiceHarness) HarnessConfig() harness.HarnessConfig {
	return r.config
}

func (r *reconfigurableServiceHarness) Reconfigure(next harness.HarnessConfig) error {
	r.config = next
	r.calls++
	return nil
}

type fakeStartupManager struct {
	calls []bool
	err   error
}

type fakePairingManager struct {
	retries []string
	phones  []string
	code    string
	err     error
}

func (m *fakePairingManager) RetryPairing(name string) error {
	m.retries = append(m.retries, name)
	return m.err
}

func (m *fakePairingManager) PairPhone(_ context.Context, name, phone string) (string, error) {
	m.phones = append(m.phones, name+":"+phone)
	return m.code, m.err
}

func (m *fakeStartupManager) Sync(_ config.Config, enabled bool) error {
	m.calls = append(m.calls, enabled)
	return m.err
}

func newHeldServiceHarness() *heldServiceHarness {
	return &heldServiceHarness{serviceHarness: newServiceHarness(), emits: map[string]core.Emit{}}
}

func (r *heldServiceHarness) Send(_ context.Context, key, prompt string, emit core.Emit) (string, bool, error) {
	r.mu.Lock()
	r.prompts[key] = append(r.prompts[key], prompt)
	thread := r.threads[key]
	if thread == "" {
		thread = "thread-" + key
		r.threads[key] = thread
	}
	steered := r.active[key]
	previousEmit := r.emits[key]
	r.active[key] = true
	r.emits[key] = emit
	r.mu.Unlock()
	if steered && previousEmit != nil {
		previousEmit(core.Event{Kind: core.EventStatus, Done: true, ThreadID: thread})
	}
	return thread, steered, nil
}

func (r *heldServiceHarness) finish(key string) {
	r.mu.Lock()
	emit := r.emits[key]
	delete(r.emits, key)
	r.active[key] = false
	r.mu.Unlock()
	if emit != nil {
		emit(core.Event{Kind: core.EventFinal, Text: "finished", Done: true})
	}
}

func newServiceHarness() *serviceHarness {
	return &serviceHarness{prompts: map[string][]string{}, threads: map[string]string{}, active: map[string]bool{}}
}

func (r *serviceHarness) Start(context.Context) error { return nil }
func (r *serviceHarness) Close() error                { return nil }
func (r *serviceHarness) Models(context.Context) ([]harness.Model, error) {
	return []harness.Model{{ID: "sonnet", DisplayName: "Sonnet"}, {ID: "opus", DisplayName: "Opus"}}, nil
}
func (r *serviceHarness) ResetSession(key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.resetErr != nil {
		return r.resetErr
	}
	delete(r.threads, key)
	return nil
}
func (r *serviceHarness) ThreadID(key string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.threads[key]
}
func (r *serviceHarness) IsActive(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active[key]
}

func (r *serviceHarness) Interrupt(_ context.Context, key string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active[key] {
		return false, nil
	}
	r.active[key] = false
	return true, nil
}
func (r *serviceHarness) Send(_ context.Context, key, prompt string, emit core.Emit) (string, bool, error) {
	r.mu.Lock()
	r.prompts[key] = append(r.prompts[key], prompt)
	thread := r.threads[key]
	if thread == "" {
		thread = "thread-" + key
		r.threads[key] = thread
	}
	r.mu.Unlock()
	emit(core.Event{Kind: core.EventFinal, Text: "reply for " + key, ThreadID: thread, Done: true})
	return thread, false, nil
}

func TestServiceKeepsChannelContextsSeparateAndLinksFullHistory(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	harness := newServiceHarness()
	service := New(cfg, harness)
	for _, message := range []core.Message{
		{Channel: "tui", Conversation: "local", Text: "TUI question"},
		{Channel: "telegram", Conversation: "42", Text: "Telegram question"},
	} {
		if err := service.Handle(context.Background(), message, func(core.Event) {}); err != nil {
			t.Fatal(err)
		}
	}
	tuiPrompt := harness.prompts["chat:tui:local"][0]
	telegramPrompt := harness.prompts["chat:telegram:42"][0]
	if strings.Contains(tuiPrompt, "Telegram question") || strings.Contains(telegramPrompt, "TUI question") {
		t.Fatal("channel histories leaked into each other")
	}
	if !strings.Contains(tuiPrompt, service.History.Path("tui", "local")) {
		t.Fatal("prompt does not link the complete history file")
	}
	recent, _, err := service.History.Recent("tui", "local", 10000)
	if err != nil || !strings.Contains(recent, "reply for chat:tui:local") {
		t.Fatalf("assistant response not persisted: %q, %v", recent, err)
	}
}

func TestEveryChatTransportFreshLoadsTheSameChatInstructionsAtPromptEnd(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	path := cfg.StatePath("instructions", "agent-chat.md")
	harness := newServiceHarness()
	service := New(cfg, harness)
	channels := []string{"tui", "telegram", "whatsapp", "cli"}
	for index, channelName := range channels {
		instruction := fmt.Sprintf("chat rule %d", index)
		if err := os.WriteFile(path, []byte(instruction), 0o600); err != nil {
			t.Fatal(err)
		}
		message := core.Message{Channel: channelName, Conversation: "memory", Text: "question"}
		if err := service.Handle(context.Background(), message, func(core.Event) {}); err != nil {
			t.Fatal(err)
		}
		prompt := harness.prompts["chat:"+channelName+":memory"][0]
		if !strings.Contains(prompt, "\n"+instruction+"\n</workspace_owner_persistent_instructions>") || !strings.Contains(prompt, "the chat agent from .spynel/instructions/agent-chat.md") || !strings.HasSuffix(prompt, "The precedence stated above still applies to every imported rule.") {
			t.Fatalf("%s prompt omitted final fresh chat instructions:\n%s", channelName, prompt)
		}
	}
}

func TestServicePersistsReplyContextAndProjectsItIntoChatPrompt(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	harness := newServiceHarness()
	service := New(cfg, harness)
	message := core.Message{Channel: "telegram", Conversation: "TG-7", Sender: "@user", ReplyTo: "91 referenced caption", Text: "/tasks active"}
	if err := service.Handle(context.Background(), message, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	entries, _, err := service.History.Entries("telegram", "TG-7")
	if err != nil || len(entries) == 0 || entries[0].ReplyTo != message.ReplyTo {
		t.Fatalf("persisted entries = %#v, %v", entries, err)
	}
	prompt, err := service.chatPrompt(message)
	if err != nil || !strings.Contains(prompt, "[reply_to: 91 referenced caption]") {
		t.Fatalf("chat prompt missing reply context: %v\n%s", err, prompt)
	}
}

func TestMessageReceivedHookPayloadIncludesReplyContext(t *testing.T) {
	payload := messageHookPayload(core.Message{Channel: "whatsapp", Conversation: "WA-1", Sender: "1", Text: "answer", ReplyTo: "quoted-id quoted text"})
	if payload["reply_to"] != "quoted-id quoted text" || payload["conversation"] != "WA-1" {
		t.Fatalf("message.received payload = %#v", payload)
	}
}

func TestRemoteFinalResponseExtractsOutboundAttachment(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	outsideRoot := t.TempDir()
	path := filepath.Join(outsideRoot, "report.txt")
	if err := os.WriteFile(path, []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}
	progressPath := filepath.Join(root, "progress.txt")
	if err := os.WriteFile(progressPath, []byte("progress"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(cfg, newServiceHarness())
	message := core.Message{Channel: "telegram", Conversation: "TG-7"}
	var got core.Event
	finalText := "Ready.\n\n[Send attachment](<" + path + ">)"
	service.wrapEmit(message, 0, func(event core.Event) { got = event })(core.Event{
		Kind: core.EventFinal, Text: "Progress update.\n[Send attachment](<" + progressPath + ">)\n" + finalText, FinalText: &finalText, Done: true,
	})
	if got.Text != "Progress update.\nReady." || len(got.Attachments) != 1 || got.Attachments[0].Path != path {
		t.Fatalf("event = %#v", got)
	}
	if got.FinalText == nil || *got.FinalText != "Ready." {
		value := "<nil>"
		if got.FinalText != nil {
			value = *got.FinalText
		}
		t.Fatalf("remote final text = %q, want Ready.", value)
	}
	recent, _, err := service.History.Recent("telegram", "TG-7", 10000)
	if err != nil || !strings.Contains(recent, "Progress update.") || !strings.Contains(recent, "[Sent attachment report.txt]") || strings.Contains(recent, "progress.txt") || strings.Contains(recent, "[Send attachment]") {
		t.Fatalf("history = %q, %v", recent, err)
	}
	prompt, err := service.chatPrompt(core.Message{Channel: "telegram", Conversation: "TG-7"})
	if err != nil || !strings.Contains(prompt, "[Send photo](</absolute/path/to/image.png>)") || !strings.Contains(prompt, "outside the active workspace") {
		t.Fatalf("remote prompt missing attachment instructions: %v", err)
	}
}

func TestNonterminalDoneEventDoesNotEndActiveJob(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	service := New(cfg, newServiceHarness())
	jobID := service.Runtime.BeginJob("chat:telegram:TG-7", "telegram", "TG-7", "first message")
	emit := service.wrapEmit(core.Message{Channel: "telegram", Conversation: "TG-7"}, jobID, nil)
	emit(core.Event{Kind: core.EventStatus, Done: true})
	if service.Runtime.Status().Jobs != 1 {
		t.Fatal("transport handoff ended the active harness job")
	}
	emit(core.Event{Kind: core.EventFinal, Text: "answered follow-up", Done: true})
	if service.Runtime.Status().Jobs != 0 {
		t.Fatal("terminal harness event did not end the active job")
	}
}

func TestChatActivityEmitterStopsBeforeTerminalDeliveryAndOnHandoff(t *testing.T) {
	var events []core.Event
	activity := newChatActivityEmitter(func(event core.Event) { events = append(events, event) })
	activity.start()
	activity.emit(core.Event{Kind: core.EventFinal, Text: "partial", Done: true, Continues: true})
	activity.emit(core.Event{Kind: core.EventStatus, Done: true})
	activity.emit(core.Event{Kind: core.EventFinal, Text: "late", Done: true})

	if len(events) != 5 {
		t.Fatalf("activity event sequence = %#v", events)
	}
	if events[0].Kind != core.EventActivity || !events[0].Active {
		t.Fatalf("first activity event = %#v", events[0])
	}
	if events[2].Kind != core.EventActivity || events[2].Active {
		t.Fatalf("handoff did not stop activity before release: %#v", events)
	}
	if events[3].Kind != core.EventStatus || events[4].Kind != core.EventFinal {
		t.Fatalf("downstream ordering = %#v", events)
	}
}

func TestChatActivityEmitterReplacementOrdersNewStartBeforeOldRelease(t *testing.T) {
	var events []core.Event
	downstream := func(event core.Event) { events = append(events, event) }
	old := newChatActivityEmitter(downstream)
	replacement := newChatActivityEmitter(downstream)

	old.start()
	replacement.start()
	old.emit(core.Event{Kind: core.EventStatus, Done: true})
	old.emit(core.Event{Kind: core.EventFinal, Text: "replaced response", Done: true})
	replacement.emit(core.Event{Kind: core.EventFinal, Text: "current response", Done: true})

	wantActivity := []bool{true, true, false, false}
	var gotActivity []bool
	for _, event := range events {
		if event.Kind == core.EventActivity {
			gotActivity = append(gotActivity, event.Active)
		}
	}
	if !reflect.DeepEqual(gotActivity, wantActivity) {
		t.Fatalf("replacement activity sequence = %v, want %v; events = %#v", gotActivity, wantActivity, events)
	}
	if len(events) != 7 || events[2].Kind != core.EventActivity || events[3].Kind != core.EventStatus || events[5].Kind != core.EventActivity || events[6].Kind != core.EventFinal {
		t.Fatalf("replacement handoff ordering = %#v", events)
	}
}

func TestContinuingFinalDoesNotEndActiveJob(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	service := New(cfg, newServiceHarness())
	jobID := service.Runtime.BeginJob("chat:tui:local", "tui", "local", "first message")
	emit := service.wrapEmit(core.Message{Channel: "tui", Conversation: "local"}, jobID, nil)
	emit(core.Event{Kind: core.EventFinal, Text: "first answer", Done: true, Continues: true})
	if service.Runtime.Status().Jobs != 1 {
		t.Fatal("continuing final ended the logical conversation job")
	}
	emit(core.Event{Kind: core.EventFinal, Text: "follow-up answer", Done: true})
	if service.Runtime.Status().Jobs != 0 {
		t.Fatal("last queued final did not end the logical conversation job")
	}
}

func TestFollowUpTakesOverActiveResponseWithoutAbandoningTurn(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	target := newHeldServiceHarness()
	service := New(cfg, target)
	key := "chat:telegram:TG-7"
	var firstEvents []core.Event
	var secondEvents []core.Event
	first := core.Message{Channel: "telegram", Conversation: "TG-7", Text: "dispatch the finite work"}
	followUp := core.Message{Channel: "telegram", Conversation: "TG-7", Text: "what is its state?", FollowupOnly: true}
	if err := service.Handle(context.Background(), followUp, nil); err == nil || !strings.Contains(err.Error(), "no active execution") {
		t.Fatalf("inactive explicit follow-up error = %v", err)
	}
	if err := service.Handle(context.Background(), first, func(event core.Event) { firstEvents = append(firstEvents, event) }); err != nil {
		t.Fatal(err)
	}
	if err := service.Handle(context.Background(), followUp, func(event core.Event) { secondEvents = append(secondEvents, event) }); err != nil {
		t.Fatal(err)
	}
	if service.Runtime.Status().Jobs != 1 || !target.IsActive(key) {
		t.Fatalf("steered turn state = jobs %#v, active %t", service.Runtime.Jobs(), target.IsActive(key))
	}
	if len(firstEvents) == 0 || !firstEvents[len(firstEvents)-1].Done || firstEvents[len(firstEvents)-1].Kind != core.EventStatus {
		t.Fatalf("previous emitter was not released with a nonterminal done status: %#v", firstEvents)
	}
	if len(target.prompts[key]) != 2 || !strings.Contains(target.prompts[key][1], "what is its state?") || !strings.Contains(target.prompts[key][1], "dispatch the finite work") {
		t.Fatalf("follow-up prompt did not retain live conversation context: %#v", target.prompts[key])
	}
	target.finish(key)
	if service.Runtime.Status().Jobs != 0 || target.IsActive(key) {
		t.Fatalf("completed steered turn state = jobs %#v, active %t", service.Runtime.Jobs(), target.IsActive(key))
	}
	foundFinal := false
	for _, event := range secondEvents {
		if event.Kind == core.EventFinal && event.Done && event.Text == "finished" {
			foundFinal = true
		}
	}
	if !foundFinal {
		t.Fatalf("latest emitter did not receive the final response: %#v", secondEvents)
	}
}

func TestSlashCommandsSendCreationPromptsToCommunicationAgent(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	harness := newServiceHarness()
	service := New(cfg, harness)
	if err := service.Handle(context.Background(), core.Message{Channel: "telegram", Conversation: "7", Text: "/task inspect the queue"}, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if err := service.Handle(context.Background(), core.Message{Channel: "telegram", Conversation: "7", Text: "/goal keep the queue healthy"}, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	prompts := harness.prompts["chat:telegram:7"]
	if len(prompts) != 2 || !strings.Contains(prompts[0], "<user_task_request>\ninspect the queue") || !strings.Contains(prompts[1], "<user_goal_request>\nkeep the queue healthy") || !strings.Contains(prompts[1], "success_criteria") {
		t.Fatalf("creation prompts = %#v", prompts)
	}
	for _, route := range cfg.Orchestrator.Routes {
		entries, err := filepath.Glob(filepath.Join(cfg.Resolve(route.Source), "*.md"))
		if err != nil || len(entries) != 0 {
			t.Fatalf("framework command bypassed communication agent for %s: %#v, %v", route.Name, entries, err)
		}
	}
}

func TestCreationPromptsUseLiveRouteSettings(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	service := New(cfg, newServiceHarness())
	routes := append([]config.Route(nil), cfg.Orchestrator.Routes...)
	routes[0].Source = ".spynel/live-task-prompts/todo"
	routes[1].Source = ".spynel/live-goal-prompts/proposed"
	routeValue, err := json.Marshal(routes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ApplySettings(map[string]string{"orchestrator.routes": string(routeValue)}); err != nil {
		t.Fatal(err)
	}
	next := service.Settings.Snapshot()
	reloaded, err := config.Load(cfg.Path)
	if err != nil || !reflect.DeepEqual(reloaded.Orchestrator.Routes, next.Orchestrator.Routes) {
		t.Fatalf("saved routes were not reloaded into shared memory: disk=%#v snapshot=%#v err=%v", reloaded.Orchestrator.Routes, next.Orchestrator.Routes, err)
	}
	prompt, err := service.creationCommandPrompt(core.Message{Channel: "cli", Conversation: "live-routes"}, "task", "inspect live routes")
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{next.Resolve(next.Orchestrator.Routes[0].Source), next.Resolve(next.Orchestrator.Routes[1].Source)} {
		if !strings.Contains(prompt, source) {
			t.Fatalf("creation prompt does not contain live route %q:\n%s", source, prompt)
		}
	}
	for _, route := range cfg.Orchestrator.Routes[:2] {
		if strings.Contains(prompt, cfg.Resolve(route.Source)) {
			t.Fatalf("creation prompt retained process-start route %q", cfg.Resolve(route.Source))
		}
	}
}

func TestSlashCommandCatalogBuildsHelpAndReturnsACopy(t *testing.T) {
	commands := SlashCommands()
	if len(commands) == 0 {
		t.Fatal("slash command catalog is empty")
	}
	const statusDescription = "Show work, runtime, channel, and orchestrator state"
	statusFound := false
	tasksFound := false
	goalsFound := false
	for _, command := range commands {
		if !strings.Contains(commandHelp, command.Usage) || !strings.Contains(commandHelp, command.Description) {
			t.Fatalf("command help does not contain %#v", command)
		}
		if command.Value == "/status" {
			statusFound = true
			if command.Description != statusDescription {
				t.Fatalf("status command description = %q, want %q", command.Description, statusDescription)
			}
		}
		if command.Value == "/tasks" {
			tasksFound = true
		}
		if command.Value == "/goals" {
			goalsFound = true
		}
		if command.Value == "/tasks open" || command.Value == "/goals open" {
			t.Fatalf("default-open listing has a redundant catalog entry: %#v", command)
		}
	}
	if !statusFound {
		t.Fatal("slash command catalog does not contain /status")
	}
	if !tasksFound || !goalsFound {
		t.Fatalf("slash command catalog is missing base durable-work commands: tasks=%v goals=%v", tasksFound, goalsFound)
	}

	original := commands[0].Value
	commands[0].Value = "/mutated"
	if SlashCommands()[0].Value != original {
		t.Fatal("SlashCommands returned mutable catalog storage")
	}
}

func TestThemeCommandOpensPickerListsAndPersistsThemes(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	service := New(cfg, newServiceHarness())
	run := func(channelName, command string) core.Event {
		var response core.Event
		if err := service.Handle(context.Background(), core.Message{Channel: channelName, Conversation: "theme-test", Text: command}, func(event core.Event) { response = event }); err != nil {
			t.Fatal(err)
		}
		return response
	}
	if response := run("tui", "/theme"); response.Kind != core.EventThemePicker {
		t.Fatalf("TUI theme response = %#v", response)
	}
	if response := run("telegram", "/theme"); response.Kind != core.EventFinal || !strings.Contains(response.Text, "hack-the-box") || !strings.Contains(response.Text, "catppuccin-latte") {
		t.Fatalf("remote theme list = %#v", response)
	}
	if response := run("tui", "/theme catppuccin-latte"); response.Kind != core.EventFinal || !strings.Contains(response.Text, "catppuccin-latte") {
		t.Fatalf("theme selection = %#v", response)
	}
	select {
	case selected := <-service.ThemeChanges():
		if selected.Name != "catppuccin-latte" {
			t.Fatalf("theme event = %#v", selected)
		}
	default:
		t.Fatal("theme selection was not published")
	}
	reloaded, err := config.Load(config.PathForRoot(root))
	if err != nil || reloaded.Channels.TUI.Theme != "catppuccin-latte" {
		t.Fatalf("persisted theme = %q, %v", reloaded.Channels.TUI.Theme, err)
	}
	_ = run("telegram", "/theme catppuccin-latte")
	select {
	case selected := <-service.ThemeChanges():
		if selected.Name != "catppuccin-latte" {
			t.Fatalf("reloaded theme event = %#v", selected)
		}
	default:
		t.Fatal("reselecting the active theme did not publish its reloaded file")
	}
	if response := run("tui", "/theme missing"); response.Kind != core.EventFinal || !strings.Contains(response.Text, "Unknown theme") {
		t.Fatalf("unknown theme response = %#v", response)
	}
	changed, err := service.ApplySettings(map[string]string{"channels.tui.theme": "CATPPUCCIN-LATTE"})
	if err != nil || len(changed) != 1 || changed[0].Value != "catppuccin-latte" {
		t.Fatalf("canonical theme setting = %#v, %v", changed, err)
	}
}

func TestApplySettingsPublishesLiveHeartbeatConfiguration(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	service := New(cfg, newServiceHarness())
	service.SetPrimaryInstanceID("primary-instance")

	changed, err := service.ApplySettings(map[string]string{
		"orchestrator.enabled":                    "off",
		"orchestrator.interval_seconds":           "2",
		"orchestrator.max_parallel":               "2",
		"orchestrator.task_notifications":         "always",
		"orchestrator.semantic_heartbeat_minutes": "30",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range changed {
		if setting.Restart {
			t.Fatalf("live heartbeat setting remained restart-bound: %#v", setting)
		}
	}
	snapshot, err := service.Status(core.Message{Channel: "cli", Conversation: "local"})
	if err != nil || snapshot.HeartbeatState != "disabled" {
		t.Fatalf("live disabled heartbeat status = %#v, %v", snapshot, err)
	}
	if _, err := service.ApplySettings(map[string]string{"orchestrator.enabled": "on"}); err != nil {
		t.Fatal(err)
	}
	if snapshot, err = service.Status(core.Message{Channel: "cli", Conversation: "local"}); err != nil || snapshot.HeartbeatState != "unavailable" {
		t.Fatalf("live enabled heartbeat status = %#v, %v", snapshot, err)
	}
}

func TestHelpRoutesToBriefIndexAndFocusedTopics(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	target := newServiceHarness()
	service := New(cfg, target)

	help := func(command string) string {
		t.Helper()
		var response core.Event
		if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: command}, func(event core.Event) { response = event }); err != nil {
			t.Fatal(err)
		}
		if response.Kind != core.EventFinal {
			t.Fatalf("%s response = %#v", command, response)
		}
		return response.Text
	}

	overview := help("/help")
	for _, topic := range []string{"about", "commands", "extensions", "config", "channels", "workflows"} {
		if !strings.Contains(overview, "/help "+topic) {
			t.Fatalf("help overview does not link topic %q: %s", topic, overview)
		}
	}
	if strings.Contains(overview, "/extension install") {
		t.Fatalf("bare help should stay brief, got %s", overview)
	}
	for _, want := range []string{"classic, non-AI program", "external coding agents", "one assistant relationship"} {
		if !strings.Contains(overview, want) {
			t.Errorf("help overview missing %q: %s", want, overview)
		}
	}

	tests := map[string]string{
		"/help about":      "# About Spynel",
		"/help commands":   "/stop",
		"/help extensions": "/extension install",
		"/help config":     ".spynel/config.yaml",
		"/help channels":   "Telegram",
		"/help workflows":  "/task <request>",
	}
	for command, want := range tests {
		if response := help(command); !strings.Contains(response, want) {
			t.Errorf("%s response does not contain %q: %s", command, want, response)
		}
	}
	about := help("/help about")
	for _, want := range []string{"Simplicity at scale", "classic, non-AI program", "One human → one agent → infinite agents", "communication interface", "Markdown task management", "harness supplies intelligence", "Simplicity. Leverage. Quality."} {
		if !strings.Contains(about, want) {
			t.Errorf("about help missing %q: %s", want, about)
		}
	}

	unknown := help("/help nowhere")
	if !strings.Contains(unknown, "Unknown help topic `nowhere`") || !strings.Contains(unknown, "/help commands") {
		t.Fatalf("unknown help response = %s", unknown)
	}
	if len(target.prompts) != 0 {
		t.Fatal("help commands should not call harness")
	}
}

func TestCommunicationPromptGetsOneCallableDocsGuidance(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	service := New(cfg, newServiceHarness())
	prompt, err := service.chatPrompt(core.Message{Channel: "cli", Conversation: "docs-guidance"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(prompt, " docs <topic>") != 1 || strings.Contains(prompt, "{{SPYNEL_DOCS_GUIDANCE}}") || !strings.Contains(prompt, "AGENTS.md") {
		t.Fatalf("communication guidance is missing, duplicated, or unresolved:\n%s", prompt)
	}
}

func TestAgentDocsCommandsExistInCanonicalSlashCatalog(t *testing.T) {
	available := map[string]bool{}
	for _, command := range SlashCommands() {
		base := strings.Fields(command.Value)[0]
		available[base] = true
	}
	for _, command := range agentdocs.DocumentedSlashCommands() {
		if !available[command] {
			t.Errorf("agent docs name missing slash command %q", command)
		}
	}
}

func TestSharedHelpMetadataHasExactlyOneRoutedBody(t *testing.T) {
	bodies := map[string]int{}
	for _, topic := range helpTopics {
		bodies[topic.name]++
	}
	shared := map[string]int{}
	for _, topic := range agentdocs.HelpTopics() {
		shared[topic.ID]++
		if bodies[topic.ID] != 1 {
			t.Errorf("shared help topic %q has %d routed bodies", topic.ID, bodies[topic.ID])
		}
	}
	for name, count := range bodies {
		if count != 1 || shared[name] != 1 {
			t.Errorf("routed help topic %q has %d bodies and %d shared metadata records", name, count, shared[name])
		}
	}
}

func TestConfigurationHelpDocumentsOptionalEmptyAgentPrefixes(t *testing.T) {
	body := helpFor("config")
	for _, want := range []string{"Chat, developer, reviewer, and heartbeat prefixes default empty", "optional harness-native commands such as `/goal`", "separated from the original prompt by one ASCII space"} {
		if !strings.Contains(body, want) {
			t.Errorf("configuration help missing %q:\n%s", want, body)
		}
	}
}

func TestStatusShowsSharedIndicatorsAndShortThreadID(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	target := newServiceHarness()
	service := New(cfg, target)
	service.SetPrimaryInstanceID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	key := "chat:telegram:42"
	target.threads[key] = "019c9f42-a3b1-7ced-9e10-123456789abc"
	target.active[key] = true
	service.Runtime.Log("connected")
	service.Runtime.BeginJob(key, "telegram", "42", "inspect production")
	service.SetConnectionStatus(channel.ConnectionStatus{Name: "telegram", State: channel.ConnectionConnected})
	service.SetConnectionStatus(channel.ConnectionStatus{Name: "whatsapp", State: channel.ConnectionError, Detail: "offline"})
	if err := os.WriteFile(cfg.StatePath("tui-title"), []byte("Production\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var response core.Event
	if err := service.Handle(context.Background(), core.Message{Channel: "telegram", Conversation: "42", InstanceID: "11111111-2222-3333-4444-555555555555", Text: "/status"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Status", "Title: Production", "Instance ID: `11111111`", "Primary instance ID: `aaaaaaaa`", "Jobs: 1", "Tasks: 0 active (0 waiting)", "Goals: 0 active", "Orchestrator:", "Next heartbeat: unavailable", "Telegram: ● connected", "WhatsApp: ▲ error — offline", "Logs: 2", "Agent filesystem access: danger-full-access", "Turn: active"} {
		if !strings.Contains(response.Text, want) {
			t.Fatalf("status response does not contain %q:\n%s", want, response.Text)
		}
	}
	if strings.Contains(response.Text, "a3b1-7ced") {
		t.Fatalf("status exposed full thread ID: %s", response.Text)
	}
	if strings.Contains(response.Text, "Theme:") || strings.Contains(response.Text, "Thread:") || strings.Contains(response.Text, "019c9f42") {
		t.Fatalf("status retained low-value theme/thread output: %s", response.Text)
	}
	if strings.Contains(response.Text, "bbbb-cccc") || strings.Contains(response.Text, "2222-3333") {
		t.Fatalf("status exposed full instance ID: %s", response.Text)
	}
	message := core.Message{Channel: "telegram", Conversation: "42", InstanceID: "11111111-2222-3333-4444-555555555555"}
	snapshot, err := service.Status(message)
	if err != nil || snapshot.HeartbeatState != "unavailable" || snapshot.NextHeartbeatAt != nil {
		t.Fatalf("primary startup heartbeat snapshot = %#v, %v", snapshot, err)
	}
	cfg.Orchestrator.Enabled = false
	disabled := New(cfg, newServiceHarness())
	disabled.SetPrimaryInstanceID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	disabledSnapshot, err := disabled.Status(message)
	if err != nil || disabledSnapshot.HeartbeatState != "disabled" || disabledSnapshot.NextHeartbeatAt != nil || !strings.Contains(FormatStatus(disabledSnapshot), "Next heartbeat: disabled") {
		t.Fatalf("disabled primary heartbeat snapshot = %#v, %v", disabledSnapshot, err)
	}
}

func TestFormatStatusGroupsWorkAndRoundsHeartbeatUp(t *testing.T) {
	now := time.Date(2026, 8, 8, 14, 0, 0, 0, time.UTC)
	status := StatusSnapshot{
		Title: "Production", Instance: "instance", PrimaryInstance: "primary",
		Runtime: core.RuntimeStatus{Jobs: 3, Logs: 4}, TasksActive: 5, TasksWaiting: 2, GoalsActive: 2,
		OrchestratorLease: 6, OrchestratorRuns: 1, HeartbeatState: "scheduled", NextHeartbeatAt: timePointer(now.Add(time.Second)),
	}
	if got := formatNextHeartbeat(status, now); got != "in 1m" {
		t.Fatalf("one-second heartbeat = %q", got)
	}
	status.NextHeartbeatAt = timePointer(now.Add(61 * time.Second))
	if got := formatNextHeartbeat(status, now); got != "in 2m" {
		t.Fatalf("61-second heartbeat = %q", got)
	}
	status.NextHeartbeatAt = timePointer(now)
	if got := formatNextHeartbeat(status, now); got != "now" {
		t.Fatalf("due heartbeat = %q", got)
	}
	status.HeartbeatState = "running"
	if got := formatNextHeartbeat(status, now); got != "now" {
		t.Fatalf("running heartbeat = %q", got)
	}
	status.HeartbeatState = "disabled"
	if got := formatNextHeartbeat(status, now); got != "disabled" {
		t.Fatalf("disabled heartbeat = %q", got)
	}
	status.HeartbeatState = "not_primary"
	if got := formatNextHeartbeat(status, now); got != "not primary" {
		t.Fatalf("secondary heartbeat = %q", got)
	}

	status.HeartbeatState = "disabled"
	status.NextHeartbeatAt = nil
	status.Harness = "codex"
	status.HarnessState = "connected"
	status.Sandbox = "danger-full-access"
	text := FormatStatus(status)
	want := "# Status\n\n" + strings.Join([]string{
		"- Title: Production",
		"- Instance ID: `instance`",
		"- Primary instance ID: `primary`",
		"- Jobs: 3 — `/jobs`",
		"- Tasks: 5 active (2 waiting)",
		"- Goals: 2 active",
		"- Orchestrator: 6 leases, 1 dispatch goroutines",
		"- Next heartbeat: disabled",
		"- Telegram: ○ not configured",
		"- WhatsApp: ○ not configured",
		"- Coding harness: codex (connected)",
		"- Model: harness default",
		"- Agent filesystem access: danger-full-access",
		"- Run at startup: disabled",
		"- Logs: 4 — `/log`",
		"- Turn: idle",
	}, "\n")
	if text != want {
		t.Fatalf("exact status changed:\ngot:\n%s\nwant:\n%s", text, want)
	}
	ordered := []string{"Title:", "Instance ID:", "Primary instance ID:", "Jobs:", "Tasks:", "Goals:", "Orchestrator:", "Next heartbeat:", "Telegram:", "Coding harness:", "Agent filesystem access:", "Run at startup:", "Logs:", "Turn:"}
	previous := -1
	for _, row := range ordered {
		index := strings.Index(text, row)
		if index <= previous {
			t.Fatalf("status row %q is out of order:\n%s", row, text)
		}
		previous = index
	}
	if strings.Contains(text, "Theme:") || strings.Contains(text, "Thread:") {
		t.Fatalf("compact status contains omitted rows:\n%s", text)
	}
}

func timePointer(value time.Time) *time.Time { return &value }

func TestStatusSurvivesUnreadableGoalCheckpointPath(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(root, ".spynel", "goals", "active")
	if err := os.Remove(active); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(active, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := New(cfg, newServiceHarness()).Status(core.Message{Channel: "cli", Conversation: "local"})
	if err != nil {
		t.Fatalf("status failed on unreadable checkpoint path: %v", err)
	}
	if status.GoalsActive != 0 || len(status.WorkDiagnostics) == 0 || !strings.Contains(strings.Join(status.WorkDiagnostics, "\n"), "lower bound") {
		t.Fatalf("degraded status = %#v", status)
	}
	for _, diagnostic := range status.WorkDiagnostics {
		if len([]rune(diagnostic)) > 240 || strings.ContainsAny(diagnostic, "\r\n\t") {
			t.Fatalf("unbounded work diagnostic = %q", diagnostic)
		}
	}
}

func TestStatusBoundsGoalCheckpointFilesystemDiagnostic(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	for index := range cfg.Orchestrator.Routes {
		if cfg.Orchestrator.Routes[index].Name == "goals" {
			cfg.Orchestrator.Routes[index].Source = filepath.Join(".spynel", strings.Repeat("x", 300)+"\u0085\nraw-path", "todo")
		}
	}
	status, err := New(cfg, newServiceHarness()).Status(core.Message{Channel: "cli", Conversation: "local"})
	if err != nil {
		t.Fatalf("status failed on overlong checkpoint path: %v", err)
	}
	if len(status.WorkDiagnostics) == 0 || len(status.WorkDiagnostics) > 8 {
		t.Fatalf("work diagnostics count = %d, want 1..8: %#v", len(status.WorkDiagnostics), status.WorkDiagnostics)
	}
	foundCheckpoint := false
	for _, diagnostic := range status.WorkDiagnostics {
		if strings.Contains(diagnostic, "goal checkpoint display is incomplete") {
			foundCheckpoint = true
		}
		if len([]rune(diagnostic)) > 240 || strings.IndexFunc(diagnostic, unicode.IsControl) >= 0 {
			t.Fatalf("unbounded work diagnostic = %q", diagnostic)
		}
	}
	if !foundCheckpoint {
		t.Fatalf("missing checkpoint diagnostic: %#v", status.WorkDiagnostics)
	}
}

func TestTelegramCommandMentionRoutesToSharedCommand(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	target := newServiceHarness()
	service := New(cfg, target)
	var response core.Event

	if err := service.Handle(context.Background(), core.Message{Channel: "telegram", Conversation: "group", Text: "/help@spynel_bot commands"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if response.Kind != core.EventFinal || !strings.Contains(response.Text, "# Commands") {
		t.Fatalf("Telegram command mention response = %#v", response)
	}
	if len(target.prompts) != 0 {
		t.Fatal("Telegram command mention should not call harness")
	}
}

func TestTitleCommandPersistsFromRemoteChannelsAndNotifiesTUI(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	target := newServiceHarness()
	service := New(cfg, target)

	for _, test := range []struct {
		channel string
		title   string
	}{
		{channel: "telegram", title: "Production API"},
		{channel: "whatsapp", title: "Release Monitor"},
	} {
		var response core.Event
		err := service.Handle(context.Background(), core.Message{Channel: test.channel, Conversation: "remote", Text: "/title " + test.title}, func(event core.Event) { response = event })
		if err != nil {
			t.Fatal(err)
		}
		if response.Kind != core.EventFinal || !strings.Contains(response.Text, test.title) {
			t.Fatalf("%s title response = %#v", test.channel, response)
		}
		select {
		case title := <-service.TitleChanges():
			if title != test.title {
				t.Fatalf("%s title event = %q, want %q", test.channel, title, test.title)
			}
		default:
			t.Fatalf("%s title command did not notify the TUI", test.channel)
		}
		data, err := os.ReadFile(cfg.StatePath("tui-title"))
		if err != nil || strings.TrimSpace(string(data)) != test.title {
			t.Fatalf("%s persisted title = %q, %v", test.channel, data, err)
		}
	}
	if len(target.prompts) != 0 {
		t.Fatal("title commands should not call harness")
	}
}

func TestTitleNotificationsKeepOnlyLatestChange(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	service := New(cfg, newServiceHarness())

	service.publishTitle("Older")
	service.publishTitle("Newest")
	if title := <-service.TitleChanges(); title != "Newest" {
		t.Fatalf("queued title = %q, want Newest", title)
	}
}

func TestQuitIsRecognizedButDoesNotStopRemoteServer(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	service := New(cfg, newServiceHarness())

	for _, channel := range []string{"telegram", "whatsapp"} {
		var response core.Event
		if err := service.Handle(context.Background(), core.Message{Channel: channel, Conversation: "remote", Text: "/quit"}, func(event core.Event) { response = event }); err != nil {
			t.Fatal(err)
		}
		if response.Kind != core.EventFinal || !strings.Contains(response.Text, "interactive TUI only") {
			t.Fatalf("%s quit response = %#v", channel, response)
		}
	}
}

func TestStopCommandInterruptsOnlyAnActiveConversation(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	target := newServiceHarness()
	service := New(cfg, target)
	key := "chat:tui:local"
	target.active[key] = true

	var response core.Event
	err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: "/stop"}, func(event core.Event) { response = event })
	if err != nil {
		t.Fatal(err)
	}
	if target.IsActive(key) || response.Kind != core.EventFinal || !strings.Contains(response.Text, "Stop requested") {
		t.Fatalf("active=%t response=%#v", target.IsActive(key), response)
	}

	response = core.Event{}
	err = service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: "/stop"}, func(event core.Event) { response = event })
	if err != nil || !strings.Contains(response.Text, "no active execution") {
		t.Fatalf("inactive stop response=%#v err=%v", response, err)
	}
}

func TestRestartCommandAcknowledgesAndRequestsProcessRestartAcrossChannels(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	target := newServiceHarness()
	service := New(cfg, target)

	const acknowledgment = "Restarting Spynel..."
	for _, channelName := range []string{"tui", "telegram", "whatsapp", "cli"} {
		var response core.Event
		message := core.Message{Channel: channelName, Conversation: "restart-" + channelName, Text: "/restart"}
		if err := service.Handle(context.Background(), message, func(event core.Event) { response = event }); err != nil {
			t.Fatalf("%s restart: %v", channelName, err)
		}
		if response.Kind != core.EventFinal || !response.Done || !response.Local || response.Text != acknowledgment {
			t.Fatalf("%s restart response = %#v", channelName, response)
		}
		select {
		case <-service.RestartRequests():
		default:
			t.Fatalf("%s restart did not publish a process request", channelName)
		}
		recent, _, err := service.History.Recent(channelName, message.Conversation, 1000)
		if err != nil || !strings.Contains(recent, acknowledgment) || strings.Contains(recent, "Saved configuration and conversation history") {
			t.Fatalf("%s restart acknowledgment was not persisted: %q, %v", channelName, recent, err)
		}
	}
	if len(target.prompts) != 0 {
		t.Fatal("restart command should not call the coding harness")
	}
	found := false
	for _, command := range SlashCommands() {
		if command.Value == "/restart" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("restart command is missing from the canonical picker")
	}
}

func TestUpdateCommandChecksNPMAndRequestsLauncherManagedInstall(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	registry := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"name":"spynel","version":"1.3.0"}`))
	}))
	defer registry.Close()
	service := New(cfg, newServiceHarness())
	service.Updates = &updater.Manager{
		CurrentVersion: "1.2.0", PackageRoot: root, LauncherManaged: true,
		RegistryURL: registry.URL,
	}
	run := func(command string) core.Event {
		var response core.Event
		if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "updates", Text: command}, func(event core.Event) { response = event }); err != nil {
			t.Fatal(err)
		}
		return response
	}
	if response := run("/update"); !strings.Contains(response.Text, "1.3.0") || !strings.Contains(response.Text, "/update install") {
		t.Fatalf("update response = %#v", response)
	}
	if response := run("/update install"); !strings.Contains(response.Text, "Updating Spynel") {
		t.Fatalf("install response = %#v", response)
	}
	select {
	case <-service.UpdateRequests():
	default:
		t.Fatal("update install did not publish a process request")
	}
	found := false
	for _, command := range SlashCommands() {
		if command.Value == "/update" {
			found = true
		}
	}
	if !found {
		t.Fatal("update command is missing from the canonical picker")
	}
}

func TestPrimaryCommandRequestsSafeLocalHandoffOnlyWhileIdle(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	target := newServiceHarness()
	service := New(cfg, target)
	service.SetPrimaryInstanceID("primary-instance")
	run := func(message core.Message) core.Event {
		var response core.Event
		if err := service.Handle(context.Background(), message, func(event core.Event) { response = event }); err != nil {
			t.Fatal(err)
		}
		return response
	}
	if response := run(core.Message{Channel: "telegram", Conversation: "TG-7", Text: "/primary"}); !strings.Contains(response.Text, "only from a local TUI") {
		t.Fatalf("remote primary response = %#v", response)
	}
	if response := run(core.Message{Channel: "tui", Conversation: "local-primary", InstanceID: "primary-instance", Text: "/primary"}); !strings.Contains(response.Text, "already the primary") {
		t.Fatalf("owner primary response = %#v", response)
	}
	jobID := service.Runtime.BeginJob("chat:tui:other", "tui", "other", "busy")
	if response := run(core.Message{Channel: "tui", Conversation: "local-secondary", InstanceID: "secondary-instance", Text: "/primary"}); !strings.Contains(response.Text, "1 agent job is running") {
		t.Fatalf("busy primary response = %#v", response)
	}
	service.Runtime.EndJob(jobID)
	response := run(core.Message{Channel: "tui", Conversation: "local-secondary", InstanceID: "secondary-instance", Text: "/primary"})
	if !strings.Contains(response.Text, "Primary handoff requested") {
		t.Fatalf("handoff response = %#v", response)
	}
	select {
	case instanceID := <-service.PrimaryRequests():
		if instanceID != "secondary-instance" {
			t.Fatalf("handoff target = %q", instanceID)
		}
	default:
		t.Fatal("primary command did not publish a handoff request")
	}
	found := false
	for _, command := range SlashCommands() {
		if command.Value == "/primary" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("primary command is missing from the canonical picker")
	}
	if len(target.prompts) != 0 {
		t.Fatal("primary command should not call the coding harness")
	}
}

func TestJobsListAndKillActiveExecutionsByNumericID(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	target := newHeldServiceHarness()
	service := New(cfg, target)

	for _, message := range []core.Message{
		{Channel: "telegram", Conversation: "42", Text: "investigate production"},
		{Channel: "whatsapp", Conversation: "1555", Text: "review the release"},
	} {
		if err := service.Handle(context.Background(), message, func(core.Event) {}); err != nil {
			t.Fatal(err)
		}
	}
	if status := service.Runtime.Status(); status.Jobs != 2 {
		t.Fatalf("runtime status = %#v, want 2 jobs", status)
	}

	var response core.Event
	if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: "/jobs"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Job 1", "telegram/42", "Job 2", "whatsapp/1555", "/job kill <number>"} {
		if !strings.Contains(response.Text, want) {
			t.Fatalf("jobs response does not contain %q: %s", want, response.Text)
		}
	}

	response = core.Event{}
	if err := service.Handle(context.Background(), core.Message{Channel: "telegram", Conversation: "42", Text: "/job kill 1"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Text, "Kill requested for job 1") || service.Runtime.Status().Jobs != 1 {
		t.Fatalf("kill response=%#v status=%#v", response, service.Runtime.Status())
	}
	if target.IsActive("chat:telegram:42") {
		t.Fatal("killed harness session is still active")
	}

	target.finish("chat:whatsapp:1555")
	if service.Runtime.Status().Jobs != 0 {
		t.Fatalf("completed job remains registered: %#v", service.Runtime.Jobs())
	}
}

func TestJobKillKeepsHeartbeatInspectableUntilProviderRelease(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	target := newServiceHarness()
	service := New(cfg, target)
	sessionKey := "orchestrator:semantic-heartbeat"
	target.active[sessionKey] = true
	jobID := service.Runtime.BeginJobWithDetails(sessionKey, "orchestrator", "semantic-heartbeat", "semantic workflow heartbeat", JobDetails{
		Kind:  "heartbeat",
		Route: "semantic-heartbeat",
	})

	var response core.Event
	if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: "/job kill 1"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	job, ok := service.Runtime.Job(jobID)
	if !strings.Contains(response.Text, "Kill requested for job 1") || !ok {
		t.Fatalf("kill response=%#v job=%#v exists=%t", response, job, ok)
	}
	if job.Execution != JobCancelling {
		t.Fatalf("heartbeat execution = %q, want cancelling", job.Execution)
	}

	// The semantic-heartbeat completion callback owns this transition after
	// the provider's Send call releases.
	service.Runtime.EndJob(jobID)
	if _, ok := service.Runtime.Job(jobID); ok {
		t.Fatal("released heartbeat remains registered")
	}
}

func TestLogCommandShowsCapturedRuntimeOutput(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	service := New(cfg, newServiceHarness())
	service.Runtime.Log("Spynel started")
	service.Runtime.Log("orchestrator scan complete")
	var response core.Event

	if err := service.Handle(context.Background(), core.Message{Channel: "whatsapp", Conversation: "1555", Text: "/log"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if response.Kind != core.EventFinal || !response.Local || !strings.Contains(response.Text, "2 entries") || !strings.Contains(response.Text, "Spynel started") {
		t.Fatalf("log response = %#v", response)
	}
}

func TestLogCommandSupportsPaginationSearchAndClear(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	service := New(cfg, newServiceHarness())
	for index := 1; index <= 45; index++ {
		service.Runtime.Log(fmt.Sprintf("entry-%02d", index))
	}
	service.Runtime.Log("Needle in the newest haystack")

	run := func(channel, command string) core.Event {
		t.Helper()
		var response core.Event
		if err := service.Handle(context.Background(), core.Message{Channel: channel, Conversation: "test", Text: command}, func(event core.Event) { response = event }); err != nil {
			t.Fatal(err)
		}
		return response
	}

	pageOne := run("tui", "/log page 1").Text
	if !strings.Contains(pageOne, "Page 1 of 3") || !strings.Contains(pageOne, "entry-27") || !strings.Contains(pageOne, "Needle") || strings.Contains(pageOne, "entry-26") {
		t.Fatalf("unexpected newest log page: %s", pageOne)
	}
	pageTwo := run("telegram", "/log page 2").Text
	if !strings.Contains(pageTwo, "Page 2 of 3") || !strings.Contains(pageTwo, "entry-07") || !strings.Contains(pageTwo, "entry-26") || strings.Contains(pageTwo, "entry-06") || strings.Contains(pageTwo, "entry-27") {
		t.Fatalf("unexpected second log page: %s", pageTwo)
	}
	pageThree := run("whatsapp", "/logs page 3").Text
	if !strings.Contains(pageThree, "Page 3 of 3") || !strings.Contains(pageThree, "entry-01") || !strings.Contains(pageThree, "entry-06") || strings.Contains(pageThree, "entry-07") {
		t.Fatalf("unexpected oldest log page: %s", pageThree)
	}
	if output := run("tui", "/log page 100").Text; !strings.Contains(output, "Page 100 does not exist") || !strings.Contains(output, "3 pages") {
		t.Fatalf("unexpected out-of-range response: %s", output)
	}
	if output := run("telegram", "/log search needle").Text; !strings.Contains(output, "1 matches") || !strings.Contains(output, "Needle in the newest haystack") || strings.Contains(output, "entry-45") {
		t.Fatalf("unexpected search response: %s", output)
	}
	if output := run("whatsapp", "/log clear").Text; output != "Cleared 46 runtime log entries." {
		t.Fatalf("unexpected clear response: %s", output)
	}
	if status := service.Runtime.Status(); status.Logs != 0 {
		t.Fatalf("runtime status after clear = %#v", status)
	}
	if output := run("tui", "/log").Text; !strings.Contains(output, "No runtime log entries") {
		t.Fatalf("unexpected empty log response: %s", output)
	}
}

func TestLogCommandSanitizesPagesRangesAndSearchAcrossChannels(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	service := New(cfg, newServiceHarness())
	service.Runtime.Log("\x1b[31mold failure\x1b[0m")
	for index := 1; index < logPageEntries; index++ {
		service.Runtime.Log(fmt.Sprintf("filler-%02d", index))
	}
	service.Runtime.Log("\x1b[2mnew failure\x1b[0m\n原因 🧪\x00")

	run := func(channel, command string) string {
		t.Helper()
		var response core.Event
		if err := service.Handle(context.Background(), core.Message{Channel: channel, Conversation: "clean-log", Text: command}, func(event core.Event) { response = event }); err != nil {
			t.Fatal(err)
		}
		return response.Text
	}

	outputs := map[string]string{
		"ordinary": run("tui", "/log"),
		"page":     run("telegram", "/log page 2"),
		"range":    run("whatsapp", "/log page 1-2"),
		"search":   run("tui", "/log search failure"),
	}
	for name, output := range outputs {
		if strings.ContainsRune(output, '\x1b') || strings.Contains(output, "[31m") || strings.Contains(output, "[2m") || strings.Contains(output, "[0m") || strings.ContainsRune(output, '\x00') {
			t.Errorf("%s command leaked terminal formatting: %q", name, output)
		}
	}
	if !strings.Contains(outputs["range"], "old failure") || !strings.Contains(outputs["range"], "new failure\n  原因 🧪") {
		t.Fatalf("range did not preserve clean multiline content: %q", outputs["range"])
	}
	if !strings.Contains(outputs["search"], "2 matches") {
		t.Fatalf("search did not find sanitized content: %q", outputs["search"])
	}
	if logs := service.Runtime.Logs(); logs[0].Text != "old failure" {
		t.Fatalf("capture boundary did not sanitize source log: %#v", logs[0])
	}
}

func TestLogCommandRejectsInvalidOptions(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	service := New(cfg, newServiceHarness())
	overflowingPage := strconv.FormatUint(uint64(^uint(0)>>1), 10) + "0"
	for _, command := range []string{"/log page 0", "/log page nope", "/log page 1-", "/log page -1-2", "/log page 3-2", "/log page 1-" + overflowingPage, "/log page", "/log search", "/log clear extra", "/log unknown"} {
		var response core.Event
		if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: command}, func(event core.Event) { response = event }); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(response.Text, "Usage: /log") {
			t.Fatalf("%s response = %q", command, response.Text)
		}
	}
}

func TestParseLogPageSpec(t *testing.T) {
	tests := []struct {
		spec      string
		wantFirst int
		wantLast  int
		wantErr   string
	}{
		{spec: "2", wantFirst: 2, wantLast: 2},
		{spec: "1-5", wantFirst: 1, wantLast: 5},
		{spec: "1-two", wantErr: "positive numbers"},
		{spec: "0-2", wantErr: "positive numbers"},
		{spec: "-1-2", wantErr: "positive number or inclusive range"},
		{spec: "4-2", wantErr: "lower page"},
		{spec: "1-6", wantFirst: 1, wantLast: 6},
		{spec: "1-" + strconv.FormatUint(uint64(^uint(0)>>1), 10), wantFirst: 1, wantLast: int(^uint(0) >> 1)},
		{spec: "1-" + strconv.FormatUint(uint64(^uint(0)>>1), 10) + "0", wantErr: "positive numbers"},
	}
	for _, test := range tests {
		t.Run(test.spec, func(t *testing.T) {
			first, last, err := parseLogPageSpec(test.spec)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("parseLogPageSpec(%q) error = %v, want containing %q", test.spec, err, test.wantErr)
				}
				return
			}
			if err != nil || first != test.wantFirst || last != test.wantLast {
				t.Fatalf("parseLogPageSpec(%q) = %d, %d, %v", test.spec, first, last, err)
			}
		})
	}
}

func TestLogCommandLargeRangeStopsAtAvailableRetainedPages(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	service := New(cfg, newServiceHarness())
	for index := 1; index <= 121; index++ {
		service.Runtime.Log(fmt.Sprintf("bounded-range-%03d", index))
	}

	maxPage := strconv.FormatUint(uint64(^uint(0)>>1), 10)
	var response core.Event
	if err := service.Handle(context.Background(), core.Message{Channel: "telegram", Conversation: "large-range", Text: "/log page 1-" + maxPage}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Text, "121 entries. Pages 1-7 of 7, showing 121.") || !strings.Contains(response.Text, "bounded-range-001") || !strings.Contains(response.Text, "bounded-range-121") {
		t.Fatalf("large range was not clamped to available pages: %s", response.Text)
	}
	if strings.Contains(response.Text, "Usage: /log") {
		t.Fatalf("range beyond five pages was rejected: %s", response.Text)
	}
}

func TestConfigurationCommandsPersistAcrossChannelsAndProtectOwnChannel(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	path := config.PathForRoot(root)
	cfg, _ := config.Load(path)
	service := New(cfg, newServiceHarness())
	run := func(channel, command string) core.Event {
		t.Helper()
		var response core.Event
		if err := service.Handle(context.Background(), core.Message{Channel: channel, Conversation: "settings", Text: command}, func(event core.Event) { response = event }); err != nil {
			t.Fatal(err)
		}
		return response
	}

	if response := run("telegram", "/telegram off"); !strings.Contains(response.Text, "cannot be configured from Telegram itself") {
		t.Fatalf("Telegram configured itself: %#v", response)
	}
	if response := run("telegram", "/config set channels.telegram.enabled off"); !strings.Contains(response.Text, "cannot be configured from Telegram itself") {
		t.Fatalf("Telegram bypassed own-channel protection through /config: %#v", response)
	}
	if response := run("whatsapp", "/telegram set token 123:super-secret"); !strings.Contains(response.Text, "`set`") || strings.Contains(response.Text, "super-secret") {
		t.Fatalf("secret setting response was exposed: %#v", response)
	}
	if response := run("whatsapp", "/telegram set allowed_users 42"); !strings.Contains(response.Text, "channels.telegram.allowed_users") {
		t.Fatalf("Telegram whitelist response = %#v", response)
	}
	if response := run("whatsapp", "/telegram on"); !strings.Contains(response.Text, "channels.telegram.enabled") {
		t.Fatalf("Telegram toggle response = %#v", response)
	}
	if response := run("whatsapp", "/whatsapp on"); !strings.Contains(response.Text, "cannot be configured from WhatsApp itself") {
		t.Fatalf("WhatsApp configured itself: %#v", response)
	}
	if response := run("whatsapp", "/config set channels.whatsapp.enabled off"); !strings.Contains(response.Text, "cannot be configured from WhatsApp itself") {
		t.Fatalf("WhatsApp bypassed own-channel protection through /config: %#v", response)
	}
	if response := run("telegram", "/whatsapp on"); !strings.Contains(response.Text, "allowed_numbers requires at least one number") {
		t.Fatalf("WhatsApp enabled without a whitelist: %#v", response)
	}
	if response := run("telegram", "/whatsapp set allowed_numbers 15551234567"); !strings.Contains(response.Text, "channels.whatsapp.allowed_numbers") {
		t.Fatalf("WhatsApp whitelist response = %#v", response)
	}
	if response := run("telegram", "/whatsapp on"); !strings.Contains(response.Text, "channels.whatsapp.enabled") {
		t.Fatalf("WhatsApp cross-channel toggle response = %#v", response)
	}
	if response := run("tui", "/config set workspace.history_max_messages 7"); !strings.Contains(response.Text, "history_max_messages") {
		t.Fatalf("config setting response = %#v", response)
	}
	if response := run("telegram", `/config set harness.acp_args -a --param2 "value with spaces"`); !strings.Contains(response.Text, `-a --param2 "value with spaces"`) {
		t.Fatalf("command-line ACP argument response = %#v", response)
	}
	if response := run("whatsapp", "/config set harness.acp_args \"unterminated"); !strings.Contains(response.Text, "harness.acp_args has an unmatched") {
		t.Fatalf("malformed ACP argument response = %#v", response)
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Channels.Telegram.Enabled || reloaded.Channels.Telegram.Token != "123:super-secret" || len(reloaded.Channels.Telegram.AllowedUsers) != 1 || reloaded.Channels.Telegram.AllowedUsers[0] != "42" || !reloaded.Channels.WhatsApp.Enabled || len(reloaded.Channels.WhatsApp.AllowedNumbers) != 1 || reloaded.Workspace.HistoryMaxMessages != 7 || !reflect.DeepEqual(reloaded.Harness.ACPArgs, []string{"-a", "--param2", "value with spaces"}) {
		t.Fatalf("configuration commands were not persisted: %#v", reloaded)
	}
	historyEntries, _, err := service.History.Entries("whatsapp", "settings")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range historyEntries {
		if strings.Contains(entry.Content, "super-secret") {
			t.Fatalf("secret leaked into conversation history: %#v", historyEntries)
		}
	}
	persisted, rejected := false, false
	for _, entry := range service.Runtime.Logs() {
		persisted = persisted || entry.Component == "config" && entry.Event == "persisted"
		rejected = rejected || entry.Component == "config" && entry.Event == "validation_failed"
		if strings.Contains(entry.Text, "super-secret") {
			t.Fatalf("secret leaked into configuration lifecycle log: %#v", entry)
		}
	}
	if !persisted || !rejected {
		t.Fatalf("configuration lifecycle evidence = persisted %t rejected %t; logs=%#v", persisted, rejected, service.Runtime.Logs())
	}
}

func TestHarnessAndModelCommandsUseSharedSettings(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	service := New(cfg, newServiceHarness())
	for _, test := range []struct {
		command string
		want    string
	}{
		{"/harness claude-code", "harness.name"},
		{"/model opus", "harness.model"},
	} {
		var response core.Event
		if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: test.command}, func(event core.Event) { response = event }); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(response.Text, test.want) {
			t.Fatalf("%s response = %q, want %q", test.command, response.Text, test.want)
		}
	}
	var response core.Event
	if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: "/harness"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if response.Kind != core.EventScreen || response.Screen == nil || response.Screen.ID != "harness" || len(response.Screen.Controls) != len(harness.Catalog()) || !response.Screen.SaveDisabled || response.Screen.InitialControl != "select:claude-code" || response.Screen.Controls[0].Kind != "action" {
		t.Fatalf("/harness response = %#v", response)
	}
	if next, err := service.ScreenAction(context.Background(), "harness", "select:codex", nil); err != nil || next == nil || next.ID != "welcome" || next.ActionMessage != "Saved `harness.name` = `codex` and connected the coding harness." || service.Settings.Snapshot().Harness.Name != "codex" {
		t.Fatalf("harness screen selection = %#v, %v, config %#v", next, err, service.Settings.Snapshot().Harness)
	}
	if next, err := service.ScreenAction(context.Background(), "harness", "select:codex", nil); err != nil || next == nil || next.ID != "" || next.ActionMessage != "Saved `harness.name` = `codex` and connected the coding harness." {
		t.Fatalf("harness screen result-only confirmation = %#v, %v", next, err)
	}
	response = core.Event{}
	if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: "/model"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if response.Kind != core.EventScreen || response.Screen == nil || response.Screen.ID != "model" || len(response.Screen.Controls) != 3 || !response.Screen.SaveDisabled || response.Screen.InitialControl != "select:opus" || response.Screen.Controls[0].Key != "select:" || response.Screen.Controls[1].Kind != "action" {
		t.Fatalf("/model response = %#v", response)
	}
	if next, err := service.ScreenAction(context.Background(), "model", "select:sonnet", nil); err != nil || next != nil || service.Settings.Snapshot().Harness.Model != "sonnet" {
		t.Fatalf("model screen selection = %#v, %v, config %#v", next, err, service.Settings.Snapshot().Harness)
	}
	response = core.Event{}
	if err := service.Handle(context.Background(), core.Message{Channel: "telegram", Conversation: "TG-7", Text: "/harness"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Text, "claude-code") || !strings.Contains(response.Text, "codex") || !strings.Contains(response.Text, "/harness <name>") {
		t.Fatalf("remote /harness response = %#v", response)
	}
}

func TestInactiveCustomACPFieldsDoNotReconfigureAnotherHarness(t *testing.T) {
	previous := config.Harness{Name: "codex", Model: "model-a", Sandbox: "read-only"}
	next := previous
	next.ACPCommand = "future-agent"
	next.ACPArgs = []string{"--stdio"}
	if harnessRuntimeChanged(previous, next) {
		t.Fatal("inactive custom ACP fields reconfigured Codex")
	}
	previous.Name = "acp"
	next.Name = "acp"
	if !harnessRuntimeChanged(previous, next) {
		t.Fatal("active custom ACP command change did not reconfigure ACP")
	}
}

func TestSandboxSettingReconfiguresHarnessWithoutWorkspaceConfinement(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Harness.Name = "codex"
	target := &reconfigurableServiceHarness{
		serviceHarness: newServiceHarness(),
		config:         harness.HarnessConfig{Name: "codex", Sandbox: "danger-full-access"},
	}
	service := New(cfg, target)
	if _, err := service.ApplySettings(map[string]string{
		"harness.sandbox":              "danger-full-access",
		"workspace.history_char_limit": "13000",
	}); err != nil {
		t.Fatal(err)
	}
	if target.calls != 0 {
		t.Fatalf("unchanged sandbox reconfigured the harness %d time(s)", target.calls)
	}
	var response core.Event
	err = service.Handle(context.Background(), core.Message{Channel: "telegram", Conversation: "TG-7", Text: "/config set harness.sandbox workspace-write"}, func(event core.Event) { response = event })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Text, "Saved `harness.sandbox` = `workspace-write`") || service.Settings.Snapshot().Harness.Sandbox != "workspace-write" || target.config.Sandbox != "workspace-write" || target.calls != 1 {
		t.Fatalf("sandbox update = response %#v, persisted %q, runtime %q", response, service.Settings.Snapshot().Harness.Sandbox, target.config.Sandbox)
	}
	if target.config.Cwd != root || target.config.ApprovalPolicy != "never" {
		t.Fatalf("runtime harness config = %#v", target.config)
	}
}

func TestStartupSettingReloadsBeforeRegistrationErrorReturns(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	service := New(cfg, newServiceHarness())
	manager := &fakeStartupManager{}
	service.Startup = manager
	if _, err := service.ApplySettings(map[string]string{"startup.enabled": "on"}); err != nil {
		t.Fatal(err)
	}
	if len(manager.calls) != 1 || !manager.calls[0] || !service.Settings.Snapshot().Startup.Enabled {
		t.Fatalf("startup enable = calls %#v config %#v", manager.calls, service.Settings.Snapshot().Startup)
	}
	manager.err = fmt.Errorf("registration denied")
	if _, err := service.ApplySettings(map[string]string{"startup.enabled": "off"}); err == nil {
		t.Fatal("failed startup removal did not report its OS error")
	}
	if service.Settings.Snapshot().Startup.Enabled {
		t.Fatal("saved startup setting was not reloaded before the OS error returned")
	}
	reloaded, err := config.Load(config.PathForRoot(root))
	if err != nil || reloaded.Startup.Enabled {
		t.Fatalf("saved startup setting = %#v, %v", reloaded.Startup, err)
	}
}

func TestUnchangedStartupSettingDoesNotTouchOperatingSystem(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	service := New(cfg, newServiceHarness())
	manager := &fakeStartupManager{err: fmt.Errorf("must not be called")}
	service.Startup = manager
	changed, err := service.ApplySettings(map[string]string{"startup.enabled": "off"})
	if err != nil || len(changed) != 1 || len(manager.calls) != 0 {
		t.Fatalf("unchanged startup = changed %#v, calls %#v, error %v", changed, manager.calls, err)
	}
}

func TestBareUnconfiguredTUIChannelCommandsOpenSetupWizards(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	if err := os.WriteFile(cfg.Resolve(cfg.Channels.WhatsApp.Database), []byte("existing session store"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(cfg, newServiceHarness())
	for _, test := range []struct {
		command string
		wantID  string
	}{
		{command: "/telegram", wantID: "wizard:telegram:intro"},
		{command: "/whatsapp", wantID: "wizard:whatsapp:mode"},
	} {
		var response core.Event
		if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: test.command}, func(event core.Event) { response = event }); err != nil {
			t.Fatal(err)
		}
		if response.Kind != core.EventScreen || response.Screen == nil || response.Screen.ID != test.wantID || response.Screen.ParentID != "" || !response.Screen.StartAtTop || !response.Screen.SaveDisabled {
			t.Fatalf("%s initial setup screen = %#v", test.command, response)
		}
	}
}

func TestBareTUIChannelCommandsEvaluateSetupIndependently(t *testing.T) {
	tests := []struct {
		name         string
		configure    func(*config.Config)
		wantTelegram string
		wantWhatsApp string
	}{
		{
			name: "only Telegram configured",
			configure: func(cfg *config.Config) {
				cfg.Channels.Telegram.Token = "123:secret"
				cfg.Channels.Telegram.AllowedUsers = []string{"123456789"}
			},
			wantTelegram: "telegram",
			wantWhatsApp: "wizard:whatsapp:mode",
		},
		{
			name: "only WhatsApp configured",
			configure: func(cfg *config.Config) {
				cfg.Channels.WhatsApp.AllowedNumbers = []string{"15551234567"}
			},
			wantTelegram: "wizard:telegram:intro",
			wantWhatsApp: "whatsapp",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := workspace.Init(root, false); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(config.PathForRoot(root))
			if err != nil {
				t.Fatal(err)
			}
			test.configure(&cfg)
			service := New(cfg, newServiceHarness())

			for _, command := range []struct {
				text   string
				wantID string
			}{
				{text: "/telegram", wantID: test.wantTelegram},
				{text: "/whatsapp", wantID: test.wantWhatsApp},
			} {
				var response core.Event
				if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: command.text}, func(event core.Event) { response = event }); err != nil {
					t.Fatal(err)
				}
				if response.Kind != core.EventScreen || response.Screen == nil || response.Screen.ID != command.wantID {
					t.Fatalf("%s screen = %#v, want %q", command.text, response, command.wantID)
				}
				if strings.HasPrefix(command.wantID, "wizard:") && response.Screen.ParentID != "" {
					t.Fatalf("direct %s wizard unexpectedly has parent %q", command.text, response.Screen.ParentID)
				}
			}
		})
	}
}

func TestBareConfiguredTUIChannelCommandOpensTypedScreen(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	cfg.Channels.Telegram.Token = "123:secret"
	cfg.Channels.Telegram.AllowedUsers = []string{"123456789"}
	cfg.Channels.WhatsApp.AllowedNumbers = []string{"15551234567"}
	if err := os.WriteFile(cfg.Resolve(cfg.Channels.WhatsApp.Database), []byte("existing session store"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(cfg, newServiceHarness())
	var response core.Event
	if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: "/telegram"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if response.Kind != core.EventScreen || response.Screen == nil || response.Screen.ID != "telegram" || !response.Screen.StartAtTop || len(response.Screen.Controls) == 0 {
		t.Fatalf("configuration screen response = %#v", response)
	}
	if response.Screen.Title != "" || response.Screen.Subtitle != "" {
		t.Fatalf("Telegram configuration has redundant title copy: title %q subtitle %q", response.Screen.Title, response.Screen.Subtitle)
	}
	wantFirst := []string{"channels.telegram.enabled", "wizard", "channels.telegram.token", "channels.telegram.allowed_users", "advanced"}
	if len(response.Screen.Controls) < len(wantFirst) {
		t.Fatalf("Telegram controls = %#v", response.Screen.Controls)
	}
	for index, key := range wantFirst {
		if response.Screen.Controls[index].Key != key {
			t.Fatalf("Telegram control %d = %q, want %q", index, response.Screen.Controls[index].Key, key)
		}
	}
	if response.Screen.Controls[0].Section != "" || response.Screen.Controls[1].Section != "Setup" || response.Screen.Controls[2].Section != "Basic settings" || response.Screen.Controls[4].Section != "Advanced settings" {
		t.Fatalf("Telegram control sections = %#v", response.Screen.Controls[:5])
	}
	if response.Screen.Controls[4].Kind != "disclosure" || !response.Screen.Controls[5].Advanced {
		t.Fatalf("Telegram advanced disclosure = %#v", response.Screen.Controls)
	}
	foundSecret := false
	foundWhitelistHelp := false
	for _, control := range response.Screen.Controls {
		if control.Key == "channels.telegram.token" {
			foundSecret = control.Secret && control.Value == ""
		}
		if control.Key == telegramAllowedKey {
			foundWhitelistHelp = control.DescriptionMarkdown && strings.Contains(control.Description, "[@userinfobot](https://t.me/userinfobot)")
		}
	}
	if !foundSecret {
		t.Fatalf("Telegram token control is missing or exposed: %#v", response.Screen.Controls)
	}
	if !foundWhitelistHelp {
		t.Fatalf("Telegram whitelist help link is missing: %#v", response.Screen.Controls)
	}
	whatsapp, err := service.Screen("whatsapp")
	if err != nil {
		t.Fatal(err)
	}
	if !whatsapp.StartAtTop {
		t.Fatal("WhatsApp configuration screen does not start at its status section")
	}
	if whatsapp.Title != "" || whatsapp.Subtitle != "" {
		t.Fatalf("WhatsApp configuration has redundant title copy: title %q subtitle %q", whatsapp.Title, whatsapp.Subtitle)
	}
	wantFirst = []string{"channels.whatsapp.enabled", "wizard", "channels.whatsapp.mode", "channels.whatsapp.allowed_numbers", "advanced"}
	for index, key := range wantFirst {
		if whatsapp.Controls[index].Key != key {
			t.Fatalf("WhatsApp control %d = %q, want %q", index, whatsapp.Controls[index].Key, key)
		}
	}
	if whatsapp.Controls[0].Section != "" || whatsapp.Controls[1].Section != "Setup" || whatsapp.Controls[2].Section != "Basic settings" || whatsapp.Controls[4].Section != "Advanced settings" {
		t.Fatalf("WhatsApp control sections = %#v", whatsapp.Controls[:5])
	}
}

func TestMainConfigurationStartsWithHarnessModelAndEssentials(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	service := New(cfg, newServiceHarness())
	screen, err := service.Screen("config")
	if err != nil {
		t.Fatal(err)
	}
	if screen.Title != "" || screen.Subtitle != "" {
		t.Fatalf("main configuration has redundant heading copy: title %q subtitle %q", screen.Title, screen.Subtitle)
	}
	want := []string{"harness", "model", "harness.sandbox", "harness.reviews", "workspace.history_max_messages", "workspace.history_char_limit", "startup.enabled", "advanced"}
	if len(screen.Controls) < len(want)+1 {
		t.Fatalf("main configuration controls = %#v", screen.Controls)
	}
	for index, key := range want {
		if screen.Controls[index].Key != key {
			t.Fatalf("main control %d = %q, want %q", index, screen.Controls[index].Key, key)
		}
	}
	if screen.Controls[0].Kind != "action" || screen.Controls[1].Kind != "action" || screen.Controls[2].Kind != "select" || screen.Controls[3].Kind != "select" || screen.Controls[7].Kind != "disclosure" || !screen.Controls[8].Advanced {
		t.Fatalf("main control kinds/order = %#v", screen.Controls)
	}
	if screen.Controls[0].Section != "Core settings" || screen.Controls[7].Section != "Advanced settings" {
		t.Fatalf("main control sections = %#v", screen.Controls[:8])
	}
	harnessScreen, err := service.ScreenAction(context.Background(), "config", "harness", nil)
	if err != nil || harnessScreen == nil || harnessScreen.ID != "harness" || harnessScreen.ParentID != "config" || len(harnessScreen.Controls) != len(harness.Catalog()) {
		t.Fatalf("config harness action = %#v, %v", harnessScreen, err)
	}
	modelScreen, err := service.ScreenAction(context.Background(), "config", "model", nil)
	if err != nil || modelScreen == nil || modelScreen.ID != "model" || modelScreen.ParentID != "config" {
		t.Fatalf("config model action = %#v, %v", modelScreen, err)
	}
}

func TestTelegramSetupWizardCarriesAndAtomicallySavesEssentials(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	service := New(cfg, newServiceHarness())
	screen, err := service.ScreenAction(context.Background(), "telegram", "wizard", nil)
	if err != nil || screen == nil || screen.ID != "wizard:telegram:intro" || screen.ParentID != "telegram" || !screen.Markdown || !screen.SaveDisabled || !screen.StartAtTop || !strings.Contains(screen.Subtitle, "desktop.\n\nUse Telegram's verified bot-management account: [@BotFather](https://t.me/BotFather)") {
		t.Fatalf("Telegram wizard intro = %#v, %v", screen, err)
	}
	screen, err = service.ScreenAction(context.Background(), screen.ID, "next", map[string]string{})
	if err != nil || screen.ID != "wizard:telegram:create" {
		t.Fatalf("Telegram create step = %#v, %v", screen, err)
	}
	screen, err = service.ScreenAction(context.Background(), screen.ID, "next", map[string]string{})
	if err != nil || screen.ID != "wizard:telegram:token" || screen.Controls[0].Kind != "password" {
		t.Fatalf("Telegram token step = %#v, %v", screen, err)
	}
	if _, err = service.ScreenAction(context.Background(), screen.ID, "next", map[string]string{}); err == nil {
		t.Fatal("Telegram wizard accepted a missing token")
	}
	values := map[string]string{
		telegramTokenKey:   "123456:secret-token",
		telegramAllowedKey: "alice,42",
		telegramEnabledKey: "on",
	}
	screen, err = service.ScreenAction(context.Background(), screen.ID, "next", values)
	if err != nil || screen.ID != "wizard:telegram:access" {
		t.Fatalf("Telegram access step = %#v, %v", screen, err)
	}
	if !strings.Contains(screen.Subtitle, "[@userinfobot](https://t.me/userinfobot)") || !screen.Controls[0].DescriptionMarkdown {
		t.Fatalf("Telegram access help = %#v", screen)
	}
	emptyValues := map[string]string{
		telegramTokenKey:   values[telegramTokenKey],
		telegramAllowedKey: " , ",
		telegramEnabledKey: "on",
	}
	if _, err = service.ScreenAction(context.Background(), screen.ID, "next", emptyValues); err == nil || !strings.Contains(err.Error(), "at least one allowed Telegram user") {
		t.Fatalf("Telegram wizard accepted an empty whitelist: %v", err)
	}
	screen, err = service.ScreenAction(context.Background(), screen.ID, "next", values)
	if err != nil || screen.ID != "wizard:telegram:enable" {
		t.Fatalf("Telegram enable step = %#v, %v", screen, err)
	}
	screen, err = service.ScreenAction(context.Background(), screen.ID, "finish", values)
	if err != nil || screen.ID != "telegram" || !strings.Contains(screen.Status, "setup saved") {
		t.Fatalf("Telegram wizard finish = %#v, %v", screen, err)
	}
	saved := service.Settings.Snapshot().Channels.Telegram
	if !saved.Enabled || saved.Token != values[telegramTokenKey] || len(saved.AllowedUsers) != 2 || saved.AllowedUsers[0] != "alice" {
		t.Fatalf("Telegram wizard saved %#v", saved)
	}
}

func TestWhatsAppSetupWizardEnablesAfterAccessThenShowsPairingStep(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	service := New(cfg, newServiceHarness())
	screen, err := service.ScreenAction(context.Background(), "whatsapp", "wizard", nil)
	if err != nil || screen == nil || screen.ID != "wizard:whatsapp:mode" || screen.ParentID != "whatsapp" || screen.Controls[0].Key != whatsappModeKey {
		t.Fatalf("WhatsApp mode step = %#v, %v", screen, err)
	}
	values := map[string]string{
		whatsappModeKey:    "dedicated",
		whatsappAllowedKey: "15551234567",
	}
	screen, err = service.ScreenAction(context.Background(), screen.ID, "next", values)
	if err != nil || screen.ID != "wizard:whatsapp:access" {
		t.Fatalf("WhatsApp access step = %#v, %v", screen, err)
	}
	emptyValues := map[string]string{
		whatsappModeKey:    values[whatsappModeKey],
		whatsappAllowedKey: " + ",
	}
	if _, err = service.ScreenAction(context.Background(), screen.ID, "next", emptyValues); err == nil || !strings.Contains(err.Error(), "at least one allowed WhatsApp number") {
		t.Fatalf("WhatsApp wizard accepted an empty whitelist: %v", err)
	}
	screen, err = service.ScreenAction(context.Background(), screen.ID, "next", values)
	if err != nil || screen.ID != "wizard:whatsapp:pair" || !screen.StartAtTop || screen.Banner != "" || !strings.Contains(screen.Subtitle, "terminal can use every") {
		t.Fatalf("WhatsApp pair step = %#v, %v", screen, err)
	}
	if got := []string{screen.Controls[0].Key, screen.Controls[1].Key, screen.Controls[2].Key}; !reflect.DeepEqual(got, []string{"show_qr", "phone", "retry"}) {
		t.Fatalf("WhatsApp pair actions = %#v", screen.Controls)
	}
	if got := strings.Join(screen.Tabs, ","); got != "Mode,Access,Pair" || screen.ActiveTab != 2 {
		t.Fatalf("WhatsApp wizard tabs = %q at %d", got, screen.ActiveTab)
	}
	back, err := service.ScreenAction(context.Background(), screen.ID, "back", nil)
	if err != nil || back == nil || back.ID != "wizard:whatsapp:access" || back.Controls[0].Value != values[whatsappAllowedKey] {
		t.Fatalf("WhatsApp pair Back step = %#v, %v", back, err)
	}
	saved := service.Settings.Snapshot().Channels.WhatsApp
	if !saved.Enabled || saved.Mode != "dedicated" || len(saved.AllowedNumbers) != 1 {
		t.Fatalf("WhatsApp wizard saved %#v", saved)
	}
}

func TestWhatsAppPairingActionsShowFullscreenRetryAndPhoneCode(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	service := New(cfg, newServiceHarness())
	manager := &fakePairingManager{code: "ABCD-EFGH"}
	service.PairingControl = manager
	service.SetPairing(channel.PairingEvent{Name: "whatsapp", State: "code", Rendered: "FULL-QR", Detail: "WhatsApp is ready to pair"})

	screen, err := service.ScreenAction(context.Background(), "wizard:whatsapp:pair", "show_qr", nil)
	if err != nil || screen == nil || screen.ID != core.ScreenWhatsAppQR || screen.ParentID != "wizard:whatsapp:pair" || screen.Banner != "FULL-QR" || screen.Title != "" || len(screen.Controls) != 0 {
		t.Fatalf("fullscreen QR screen = %#v, %v", screen, err)
	}
	service.SetPairing(channel.PairingEvent{Name: "whatsapp", State: "phone-code", Rendered: "FULL-QR", Detail: "Enter the code"})
	if screen, err = service.ScreenAction(context.Background(), "wizard:whatsapp:pair", "show_qr", nil); err != nil || screen == nil || screen.Banner != "FULL-QR" {
		t.Fatalf("QR after phone-code selection = %#v, %v", screen, err)
	}
	general, err := service.ScreenAction(context.Background(), "wizard:whatsapp:pair", "done", nil)
	if err != nil || general == nil || general.ID != "whatsapp" || general.Banner != "" || general.Status != "" {
		t.Fatalf("general WhatsApp settings exposed QR data = %#v, %v", general, err)
	}

	screen, err = service.ScreenAction(context.Background(), "wizard:whatsapp:pair", "phone", nil)
	if err != nil || screen == nil || screen.ID != "wizard:whatsapp:phone" || !strings.Contains(screen.Subtitle, "Link with phone number instead") {
		t.Fatalf("phone pairing screen = %#v, %v", screen, err)
	}
	values := map[string]string{whatsappPhoneKey: "+1 (555) 123-4567"}
	screen, err = service.ScreenAction(context.Background(), screen.ID, "generate", values)
	if err != nil || screen == nil || screen.Banner != "Pairing code: ABCD-EFGH" || len(manager.phones) != 1 || manager.phones[0] != "whatsapp:+1 (555) 123-4567" {
		t.Fatalf("generated phone code = %#v, calls %#v, err %v", screen, manager.phones, err)
	}

	service.SetPairing(channel.PairingEvent{Name: "whatsapp", State: "timeout", Detail: "WhatsApp pairing: timeout"})
	screen, err = service.ScreenAction(context.Background(), "wizard:whatsapp:pair", "retry", nil)
	if err != nil || screen == nil || screen.ID != "wizard:whatsapp:pair" || len(manager.retries) != 1 || screen.Status != "Starting a fresh WhatsApp pairing session…" {
		t.Fatalf("retry pairing = %#v, calls %#v, err %v", screen, manager.retries, err)
	}
	pairing, ok := service.Pairing("whatsapp")
	if !ok || pairing.State != "starting" || pairing.Rendered != "" {
		t.Fatalf("pairing state after retry = %#v, %t", pairing, ok)
	}
}

func TestWhatsAppTimeoutReopensPairingAndStartOverRetries(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	service := New(cfg, newServiceHarness())
	manager := &fakePairingManager{}
	service.PairingControl = manager
	if _, err := service.ApplySettings(map[string]string{
		whatsappModeKey: "self-chat", whatsappAllowedKey: "15551234567", whatsappEnabledKey: "on",
	}); err != nil {
		t.Fatal(err)
	}
	service.SetPairing(channel.PairingEvent{Name: "whatsapp", State: "timeout", Rendered: "STALE-QR", Detail: "WhatsApp pairing: timeout"})
	screen, err := service.Screen("whatsapp")
	if err != nil || screen.ID != "wizard:whatsapp:pair" || screen.Banner != "" || screen.Status != "WhatsApp pairing: timeout" {
		t.Fatalf("resumed timeout screen = %#v, %v", screen, err)
	}
	values := map[string]string{whatsappModeKey: "self-chat", whatsappAllowedKey: "15551234567"}
	screenPtr, err := service.ScreenAction(context.Background(), "wizard:whatsapp:access", "next", values)
	if err != nil || screenPtr == nil || screenPtr.Status != "Starting a fresh WhatsApp pairing session…" || len(manager.retries) != 1 {
		t.Fatalf("start-over retry = %#v, calls %#v, err %v", screenPtr, manager.retries, err)
	}
}

func TestRenderedRemotePromptKeepsRoutineConfirmationsHumanFacing(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	service := New(cfg, newServiceHarness())
	for _, channelName := range []string{"telegram", "whatsapp"} {
		prompt, err := service.creationCommandPrompt(core.Message{Channel: channelName, Conversation: "remote"}, "task", "Fix the report")
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"Understood—this is being worked on.",
			"never include a local-path Markdown link",
			"explicitly asks for technical details",
			"/status`, `/jobs`, `/tasks`, `/goals`, `/job info`, `/log`",
			"exact blocker",
		} {
			if !strings.Contains(prompt, want) {
				t.Fatalf("%s prompt is missing %q:\n%s", channelName, want, prompt)
			}
		}
		if strings.Contains(prompt, "provide the durable path") || strings.Contains(prompt, "its durable path") {
			t.Fatalf("%s routine task confirmation still requires a durable path", channelName)
		}
	}
}

func TestResumeBranchesExternalConversationIntoIndependentTUIChat(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	service := New(cfg, newServiceHarness())
	_, _ = service.History.Append("telegram", "TG-7", history.Entry{Role: "user", Content: "remote question"})
	_, _ = service.History.Append("telegram", "TG-7", history.Entry{Role: "assistant", Content: "remote answer"})
	var response core.Event
	if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: "/resume"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if response.Kind != core.EventScreen || response.Screen == nil || response.Screen.ID != "resume" || len(response.Screen.Controls) == 0 {
		t.Fatalf("resume screen = %#v", response)
	}
	if response.Screen.Subtitle != "" {
		t.Fatalf("resume screen has redundant prose: %q", response.Screen.Subtitle)
	}
	wantHints := []core.ScreenHint{
		{Key: "↑↓/⇥", Action: "nav"},
		{Key: "␠/↵", Action: "choose"},
		{Key: "⌦", Action: "delete"},
		{Key: "␛", Action: "exit"},
	}
	if !reflect.DeepEqual(response.Screen.Hints, wantHints) {
		t.Fatalf("resume hints = %#v, want %#v", response.Screen.Hints, wantHints)
	}
	var action string
	var resumeChoice core.ScreenControl
	for _, control := range response.Screen.Controls {
		if strings.Contains(control.Value, "TG-7") {
			action = control.Key
			resumeChoice = control
			break
		}
	}
	if action == "" {
		t.Fatalf("Telegram conversation missing from %#v", response.Screen.Controls)
	}
	if !strings.HasPrefix(resumeChoice.Value, "TG   ") || !strings.HasSuffix(resumeChoice.Value, "  TG-7.jsonl") {
		t.Fatalf("Telegram resume row is not platform/date/filename aligned: %q", resumeChoice.Value)
	}
	chat, err := service.ScreenAction(context.Background(), "resume", action, nil)
	if err != nil {
		t.Fatal(err)
	}
	if chat == nil || chat.ID != "chat" || !strings.HasPrefix(chat.Conversation, "resume-") || len(chat.Transcript) != 3 || chat.Transcript[2].Text != "remote answer" || !strings.Contains(chat.Transcript[0].Text, "complete copied transcript") {
		t.Fatalf("branched chat = %#v", chat)
	}
	_, _ = service.History.Append("tui", chat.Conversation, history.Entry{Role: "user", Content: "branch only"})
	source, _, _ := service.History.RecentEntries("telegram", "TG-7", 1, 1000)
	if len(source) != 1 || source[0].Content != "remote answer" {
		t.Fatalf("source history changed: %#v", source)
	}

	deleteScreen, err := service.ScreenAction(context.Background(), "resume", "delete:"+strings.TrimPrefix(action, "resume:"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if deleteScreen == nil || deleteScreen.ID != "resume" {
		t.Fatalf("delete did not return the refreshed resume screen: %#v", deleteScreen)
	}
	deleted, _, err := service.History.RecentEntries("telegram", "TG-7", 1, 1000)
	if err != nil || len(deleted) != 0 {
		t.Fatalf("deleted source remains: entries=%#v err=%v", deleted, err)
	}
	for _, control := range deleteScreen.Controls {
		if strings.Contains(control.Value, "TG-7") {
			t.Fatalf("deleted conversation remains in picker: %#v", deleteScreen.Controls)
		}
	}

	response = core.Event{}
	if err := service.Handle(context.Background(), core.Message{Channel: "telegram", Conversation: "TG-7", Text: "/resume"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Text, "TUI only") {
		t.Fatalf("remote resume response = %#v", response)
	}
}

func TestResumePlatformUsesFixedThreeCellCodes(t *testing.T) {
	for channelName, want := range map[string]string{
		"tui": "TUI", "cli": "CLI", "telegram": "TG", "whatsapp": "WA", "extension-channel": "EXT",
	} {
		if got := resumePlatform(channelName); got != want {
			t.Errorf("resumePlatform(%q) = %q, want %q", channelName, got, want)
		}
	}
}

func TestCustomScreensDeclareContextSpecificHints(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	service := New(cfg, newServiceHarness())

	tests := []struct {
		name   string
		screen core.Screen
		want   []core.ScreenHint
	}{
		{name: "configuration", screen: settingsScreen(cfg, "config"), want: formScreenHints()},
		{name: "telegram wizard", screen: service.telegramWizardScreen("token", nil), want: wizardScreenHints()},
		{name: "whatsapp wizard", screen: service.whatsappWizardScreen("mode", nil), want: wizardScreenHints()},
		{name: "harness selection", screen: service.HarnessScreen(false), want: selectionScreenHints()},
	}
	model, err := service.modelScreen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tests = append(tests, struct {
		name   string
		screen core.Screen
		want   []core.ScreenHint
	}{name: "model selection", screen: *model, want: selectionScreenHints()})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !reflect.DeepEqual(test.screen.Hints, test.want) {
				t.Fatalf("hints = %#v, want %#v", test.screen.Hints, test.want)
			}
		})
	}
}

func TestWelcomeScreenIsAutomaticOnceAndCommandPrintsAChannelAppropriateMessage(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	service := New(cfg, newServiceHarness())
	first, err := service.InitialWelcome()
	if err != nil || first == nil || first.ID != "welcome" || first.Banner != core.SpynelASCII || len(first.Controls) != 0 || !first.Markdown {
		t.Fatalf("first welcome = %#v, %v", first, err)
	}
	for _, want := range []string{"👋 Hey, I'm **Spynel**", "call me **Spy**", "classic, non-AI core", "external coding agents", "desk or phone", "ask when your input is needed", "👍", "- type `/help` if you ever feel lost", "- type `/telegram` to connect Telegram", "- type `/whatsapp` to connect WhatsApp"} {
		if !strings.Contains(first.Subtitle, want) {
			t.Fatalf("welcome message is missing %q: %q", want, first.Subtitle)
		}
	}
	if !strings.Contains(first.Subtitle, "one assistant relationship.\nShare an objective") || strings.Contains(first.Subtitle, "one assistant relationship.\n\nShare an objective") {
		t.Fatalf("welcome intro spacing = %q", first.Subtitle)
	}
	second, err := service.InitialWelcome()
	if err != nil || second != nil {
		t.Fatalf("welcome was automatic more than once: %#v, %v", second, err)
	}
	service.SetConnectionStatus(channel.ConnectionStatus{Name: "telegram", State: channel.ConnectionConnected})
	var response core.Event
	if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: "/welcome"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if response.Kind != core.EventFinal || !response.Done || !response.Local || response.Screen != nil || !strings.HasPrefix(response.Text, core.SpynelLogoMarkdown) || !strings.Contains(response.Text, "**Spynel**") || !strings.Contains(response.Text, "**Spy**") || !strings.Contains(response.Text, "`/help`") || strings.Contains(response.Text, "`/telegram`") || !strings.Contains(response.Text, "`/whatsapp`") {
		t.Fatalf("manual TUI welcome = %#v", response)
	}
	recent, _, err := service.History.Recent("tui", "local", 100000)
	if err != nil || !strings.Contains(recent, core.SpynelASCII) {
		t.Fatalf("manual TUI welcome was not persisted as chat: %q, %v", recent, err)
	}
	for _, channelName := range []string{"telegram", "whatsapp"} {
		response = core.Event{}
		if err := service.Handle(context.Background(), core.Message{Channel: channelName, Conversation: "remote", Text: "/welcome"}, func(event core.Event) { response = event }); err != nil {
			t.Fatal(err)
		}
		if response.Kind != core.EventFinal || !response.Done || !response.Local || !strings.Contains(response.Text, "**Spynel**") || !strings.Contains(response.Text, "**Spy**") || !strings.Contains(response.Text, "`/help`") || strings.Contains(response.Text, core.SpynelASCII) || strings.Contains(response.Text, "`/telegram`") || strings.Contains(response.Text, "`/whatsapp`") || strings.Contains(response.Text, "Heads up") {
			t.Fatalf("%s welcome = %#v", channelName, response)
		}
	}
}

func TestClearCommandRemovesDurableConversationHistory(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	harness := newServiceHarness()
	service := New(cfg, harness)
	_, _ = service.History.Append("tui", "local", history.Entry{Role: "assistant", Content: "old response"})
	harness.threads["chat:tui:local"] = "stale-codex-thread"

	var response core.Event
	err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: "/clear"}, func(event core.Event) { response = event })
	if err != nil {
		t.Fatal(err)
	}
	if response.Kind != core.EventFinal || !response.Clear || response.Text == "" {
		t.Fatalf("unexpected clear response %#v", response)
	}
	entries, _, err := service.History.Entries("tui", "local")
	if err != nil || len(entries) != 0 {
		t.Fatalf("history after clear = %#v, %v", entries, err)
	}
	if len(harness.prompts) != 0 {
		t.Fatal("clear command should not call harness")
	}
	if thread := harness.ThreadID("chat:tui:local"); thread != "" {
		t.Fatalf("harness thread after clear = %q", thread)
	}

	response = core.Event{}
	if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: "next message"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if response.ThreadID == "" || response.ThreadID == "stale-codex-thread" {
		t.Fatalf("next message did not start a fresh harness thread: %#v", response)
	}
}

func TestClearCommandPreservesHistoryWhenRecipientResetFails(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	harness := newServiceHarness()
	harness.resetErr = fmt.Errorf("cannot reset while active")
	service := New(cfg, harness)
	_, _ = service.History.Append("telegram", "42", history.Entry{Role: "assistant", Content: "keep me"})

	var response core.Event
	if err := service.Handle(context.Background(), core.Message{Channel: "telegram", Conversation: "42", Text: "/clear"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if response.Clear || !strings.Contains(response.Text, "Cannot clear this conversation") {
		t.Fatalf("unexpected failed-clear response: %#v", response)
	}
	entries, _, err := service.History.Entries("telegram", "42")
	if err != nil || len(entries) < 1 || entries[0].Content != "keep me" {
		t.Fatalf("history changed after failed harness reset: %#v, %v", entries, err)
	}
}
