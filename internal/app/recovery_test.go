package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/history"
	"github.com/agent0ai/spynel/internal/orchestrator"
	"github.com/agent0ai/spynel/internal/workspace"
)

func newRecoveryTestService(t *testing.T) (*Service, *serviceHarness, config.Config) {
	t.Helper()
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	target := newServiceHarness()
	service := New(cfg, target)
	t.Cleanup(func() { _ = service.Close() })
	service.SetPrimaryInstanceID("primary")
	return service, target, cfg
}

func TestRecoveryUpgradeBaselineCategoricallyExcludesLegacyHistory(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	store := history.New(cfg.StatePath("history"))
	if _, err := store.Append("cli", "upgrade", history.Entry{At: time.Now().UTC().Add(-time.Minute), Role: "user", Content: "legacy request without correlation"}); err != nil {
		t.Fatal(err)
	}
	target := newServiceHarness()
	service := New(cfg, target)
	defer service.Close()
	service.SetPrimaryInstanceID("primary")
	result := service.scanRecovery(context.Background(), "startup")
	if result.Dispatched != 0 || len(target.prompts) != 0 {
		t.Fatalf("first upgraded scan retriggered legacy history: result=%#v prompts=%#v", result, target.prompts)
	}
	acceptedAt := time.Now().UTC()
	if _, err := service.History.Append("cli", "upgrade", history.Entry{At: acceptedAt, AcceptedAt: acceptedAt, Role: "user", Content: "new correlated request", SourceMessageID: "local:new"}); err != nil {
		t.Fatal(err)
	}
	result = service.scanRecovery(context.Background(), "periodic")
	if result.Dispatched != 1 || len(target.prompts["chat:cli:upgrade"]) != 1 {
		t.Fatalf("post-activation request was not recovered: result=%#v prompts=%#v", result, target.prompts)
	}
}

func TestRecoverySettingUsesApprovedLiveUserFacingMeaning(t *testing.T) {
	service, _, _ := newRecoveryTestService(t)
	screen, err := service.Screen("config")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, control := range screen.Controls {
		if control.Key == "orchestrator.retrigger_unresponded_messages" {
			found = control.Label == "re-trigger unresponded messages" && control.Description == "Automatically processes stalled messages after restarts and disconnects." && control.Value == "on"
		}
	}
	if !found {
		t.Fatalf("approved recovery setting not rendered: %#v", screen.Controls)
	}
}

func TestRecoveryLeavesOlderUncoveredRequestEligibleAfterExcludedCommand(t *testing.T) {
	service, target, _ := newRecoveryTestService(t)
	now := time.Now().UTC()
	for _, entry := range []history.Entry{
		{At: now, AcceptedAt: now, Role: "user", Content: "please finish the report", SourceMessageID: "local:request"},
		{At: now.Add(time.Millisecond), AcceptedAt: now.Add(time.Millisecond), Role: "user", Content: "/status", SourceMessageID: "local:status"},
		{At: now.Add(2 * time.Millisecond), Role: "assistant", Content: "status reply"},
	} {
		if _, err := service.History.Append("cli", "older", entry); err != nil {
			t.Fatal(err)
		}
	}
	result := service.scanRecovery(context.Background(), "periodic")
	if result.Eligible != 1 || result.Dispatched != 1 {
		t.Fatalf("recovery classification = %#v", result)
	}
	prompt := target.prompts["chat:cli:older"][0]
	if !strings.Contains(prompt, `source_message_id="local:request"`) || !strings.Contains(prompt, "Always produce a visible response") {
		t.Fatalf("recovery prompt lacks exact candidate or response contract: %s", prompt)
	}
}

func TestRecoveryFailsClosedWhenConversationExceedsBound(t *testing.T) {
	service, target, _ := newRecoveryTestService(t)
	now := time.Now().UTC()
	if _, err := service.History.Append("cli", "overflow", history.Entry{At: now, AcceptedAt: now, Role: "user", Content: "must not recover from a partial history", SourceMessageID: "local:overflow"}); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < recoveryTotalEntryMax; index++ {
		if _, err := service.History.Append("cli", "overflow", history.Entry{At: now.Add(time.Duration(index+1) * time.Microsecond), Role: "assistant", Content: "bounded filler"}); err != nil {
			t.Fatal(err)
		}
	}
	result := service.scanRecovery(context.Background(), "periodic")
	if result.Dispatched != 0 || result.FailedClosed == 0 || !result.Truncated || len(target.prompts) != 0 {
		t.Fatalf("overflow recovery did not fail closed: result=%#v prompts=%#v", result, target.prompts)
	}
}

func TestRecoverySuppressesCoveredRetriedAndDurablyLinkedSources(t *testing.T) {
	service, target, cfg := newRecoveryTestService(t)
	now := time.Now().UTC()
	entries := []history.Entry{
		{At: now, AcceptedAt: now, Role: "user", Content: "covered", SourceMessageID: "local:covered"},
		{At: now, Role: "correlation", Covers: []string{"local:covered"}, Outcome: "terminal_assistant"},
		{At: now, AcceptedAt: now, Role: "user", Content: "retried", SourceMessageID: "local:retried"},
		{At: now, Role: "correlation", RetriggerOf: []string{"local:retried"}},
		{At: now, AcceptedAt: now, Role: "user", Content: "/task durable", SourceMessageID: "local:task"},
	}
	for _, entry := range entries {
		if _, err := service.History.Append("cli", "handled", entry); err != nil {
			t.Fatal(err)
		}
	}
	task := "---\nid: durable\ntitle: durable\nstatus: todo\ncreated_at: \"2026-08-16T00:00:00Z\"\nupdated_at: \"2026-08-16T00:00:00Z\"\nreview_required: false\nsource_message_ids: [\"local:task\"]\n---\n\n# Durable\n"
	if err := os.WriteFile(filepath.Join(cfg.StatePath("tasks", "todo"), "durable.md"), []byte(task), 0o600); err != nil {
		t.Fatal(err)
	}
	result := service.scanRecovery(context.Background(), "periodic")
	if result.Dispatched != 0 || len(target.prompts) != 0 {
		t.Fatalf("handled sources retriggered: result=%#v prompts=%#v", result, target.prompts)
	}
}

func TestRecoveryDisableAndActiveConversationFailClosed(t *testing.T) {
	service, target, _ := newRecoveryTestService(t)
	now := time.Now().UTC()
	_, _ = service.History.Append("cli", "disabled", history.Entry{At: now, AcceptedAt: now, Role: "user", Content: "request", SourceMessageID: "local:disabled"})
	_, err := service.Settings.Update(func(next *config.Config) error {
		next.Orchestrator.RetriggerUnrespondedMessages = false
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result := service.scanRecovery(context.Background(), "periodic"); result.Dispatched != 0 {
		t.Fatalf("disabled recovery dispatched: %#v", result)
	}
	_, _ = service.Settings.Update(func(next *config.Config) error {
		next.Orchestrator.RetriggerUnrespondedMessages = true
		return nil
	})
	target.active["chat:cli:disabled"] = true
	if result := service.scanRecovery(context.Background(), "periodic"); result.Dispatched != 0 || result.FailedClosed == 0 {
		t.Fatalf("active conversation was not fenced: %#v", result)
	}
}

func TestRecoveryStatusContainsOnlyAggregateEvidence(t *testing.T) {
	service, _, _ := newRecoveryTestService(t)
	secretID := "local:private-source-identity"
	now := time.Now().UTC()
	_, _ = service.History.Append("cli", "private-conversation", history.Entry{At: now, AcceptedAt: now, Role: "user", Content: "private request body", SourceMessageID: secretID})
	service.scanRecovery(context.Background(), "periodic")
	encoded, err := json.Marshal(service.RecoveryStatus())
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{secretID, "private-conversation", "private request body"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("aggregate status leaked %q: %s", forbidden, text)
		}
	}
}

func TestStableSourceIdentityDeduplicatesDeliveryAndTerminalCoverage(t *testing.T) {
	service, target, _ := newRecoveryTestService(t)
	message := core.Message{Channel: "cli", Conversation: "duplicate", Sender: "cli", SourceMessageID: "local:same", Text: "run once", ReceivedAt: time.Now().UTC()}
	if err := service.Handle(context.Background(), message, nil); err != nil {
		t.Fatal(err)
	}
	if err := service.Handle(context.Background(), message, nil); err != nil {
		t.Fatal(err)
	}
	if len(target.prompts["chat:cli:duplicate"]) != 1 {
		t.Fatalf("duplicate source reached provider %d times", len(target.prompts["chat:cli:duplicate"]))
	}
	entries, _, err := service.History.RecoveryEntries("cli", "duplicate", recoveryEntryMax)
	if err != nil {
		t.Fatal(err)
	}
	covered := false
	for _, entry := range entries {
		for _, source := range entry.Covers {
			covered = covered || source == message.SourceMessageID
		}
	}
	if !covered {
		t.Fatalf("terminal correlation did not cover source: %#v", entries)
	}
}

func TestRecoveryAdmissionAtomicallyRechecksLateTerminalCoverage(t *testing.T) {
	service, target, _ := newRecoveryTestService(t)
	now := time.Now().UTC()
	user := history.Entry{At: now, AcceptedAt: now, Role: "user", Content: "late terminal", SourceMessageID: "local:late"}
	if _, err := service.History.Append("cli", "late", user); err != nil {
		t.Fatal(err)
	}
	candidates := []recoveryCandidate{{entry: user, admitted: true}}
	if _, err := service.History.Append("cli", "late", history.Entry{At: now.Add(time.Millisecond), Role: "correlation", Covers: []string{"local:late"}, Outcome: "terminal_assistant"}); err != nil {
		t.Fatal(err)
	}
	if service.dispatchRecovery(context.Background(), orchestrator.Origin{Channel: "cli", Conversation: "late"}, candidates) {
		t.Fatal("stale candidate was reserved after exact terminal coverage landed")
	}
	if len(target.prompts) != 0 {
		t.Fatalf("late-covered candidate reached provider: %#v", target.prompts)
	}
}

func TestRecoveryKeepsExactOwnershipFenceThroughProviderAdmission(t *testing.T) {
	service, target, _ := newRecoveryTestService(t)
	insideFence := false
	admittedInsideFence := false
	service.Harness = &observingServiceHarness{
		serviceHarness: target,
		beforeSend: func() {
			admittedInsideFence = insideFence
		},
	}
	service.SetRecoveryOwnershipFence(func(action func() error) (bool, error) {
		insideFence = true
		defer func() { insideFence = false }()
		return true, action()
	})
	now := time.Now().UTC()
	entry := history.Entry{At: now, AcceptedAt: now, Role: "user", Content: "fenced recovery", SourceMessageID: "local:fenced"}
	if _, err := service.History.Append("cli", "fenced", entry); err != nil {
		t.Fatal(err)
	}
	if !service.dispatchRecovery(context.Background(), orchestrator.Origin{Channel: "cli", Conversation: "fenced"}, []recoveryCandidate{{entry: entry}}) {
		t.Fatal("recovery was not dispatched")
	}
	if !admittedInsideFence {
		t.Fatal("recovery reached the harness after releasing its exact ownership fence")
	}
}

func TestRecoveredTurnRoutesCanonicalActivityBeforeTerminalDelivery(t *testing.T) {
	service, _, _ := newRecoveryTestService(t)
	held := newHeldServiceHarness()
	service.Harness = held
	router := &conversationEventRouter{}
	service.SetConversationDelivery(router)
	if _, err := service.Settings.Update(func(next *config.Config) error {
		next.Channels.Telegram.Enabled = true
		next.Channels.Telegram.AllowedUsers = []string{"7"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	service.SetConnectionStatus(channel.ConnectionStatus{Name: "telegram", State: channel.ConnectionConnected})
	now := time.Now().UTC()
	entry := history.Entry{At: now, AcceptedAt: now, Role: "user", Content: "recover with activity", SourceMessageID: "telegram:7:activity"}
	origin := orchestrator.Origin{Channel: "telegram", Conversation: "TG-7"}
	if _, err := service.History.Append(origin.Channel, origin.Conversation, entry); err != nil {
		t.Fatal(err)
	}
	if !service.dispatchRecovery(context.Background(), origin, []recoveryCandidate{{entry: entry}}) {
		t.Fatal("recovery was not dispatched")
	}
	events := router.snapshot()
	if len(events) < 1 || events[0].Kind != core.EventActivity || !events[0].Active {
		t.Fatalf("recovery admission events = %#v", events)
	}
	held.finish("chat:telegram:TG-7")
	events = router.snapshot()
	if len(events) < 3 || events[len(events)-2].Kind != core.EventActivity || events[len(events)-2].Active || events[len(events)-1].Kind != core.EventFinal {
		t.Fatalf("recovery terminal ordering = %#v", events)
	}
}

func TestRecoveredTUIActivityIsConversationScopedAndOverlapSafe(t *testing.T) {
	service, _, _ := newRecoveryTestService(t)
	emit := service.recoveryEmitter(orchestrator.Origin{Channel: "tui", Conversation: "selected"})
	emit(core.Event{Kind: core.EventActivity, Active: true})
	emit(core.Event{Kind: core.EventActivity, Active: true})
	emit(core.Event{Kind: core.EventActivity})
	if err := service.RegisterLiveTUI("test-instance", "selected", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterLiveTUI("other-instance", "other", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	activity := service.SharedStateForInstance("test-instance").ConversationActivity
	if activity != 1 || service.SharedStateForInstance("other-instance").ConversationActivity != 0 {
		t.Fatalf("conversation activity = %#v", activity)
	}
	emit(core.Event{Kind: core.EventActivity})
	if activity = service.SharedStateForInstance("test-instance").ConversationActivity; activity != 0 {
		t.Fatalf("completed conversation retained activity = %#v", activity)
	}
}

func TestHarnessSendFailureRollsBackNewConversationCorrelation(t *testing.T) {
	service, target, _ := newRecoveryTestService(t)
	failing := &failingServiceHarness{serviceHarness: target, err: errors.New("send failed")}
	service.Harness = failing
	message := core.Message{Channel: "cli", Conversation: "send-failure", SourceMessageID: "local:send-failure", Text: "request"}
	if err := service.dispatchHarnessPrompt(context.Background(), message, "prompt", nil); err == nil {
		t.Fatal("harness send failure was not returned")
	}
	if service.conversationInFlight(sessionKey(message)) {
		t.Fatal("harness send failure left conversation correlation in flight")
	}
	if service.Runtime.Status().Jobs != 0 {
		t.Fatalf("harness send failure left runtime job active: %#v", service.Runtime.Status())
	}
}

func TestFailedFollowupRollsBackOnlyItsSourceCorrelation(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	held := newHeldServiceHarness()
	service := New(cfg, held)
	defer service.Close()
	first := core.Message{Channel: "cli", Conversation: "followup-failure", SourceMessageID: "local:first", Text: "first"}
	if err := service.dispatchHarnessPrompt(context.Background(), first, "first", nil); err != nil {
		t.Fatal(err)
	}
	service.Harness = &failingServiceHarness{serviceHarness: held.serviceHarness, err: errors.New("followup failed")}
	second := core.Message{Channel: first.Channel, Conversation: first.Conversation, SourceMessageID: "local:second", Text: "second"}
	if err := service.dispatchHarnessPrompt(context.Background(), second, "second", nil); err == nil {
		t.Fatal("follow-up send failure was not returned")
	}
	key := sessionKey(first)
	if !service.conversationInFlight(key) {
		t.Fatal("failed follow-up removed the older active execution correlation")
	}
	snapshot := service.recoveryCancellationSnapshot(key)
	if snapshot == nil {
		t.Fatal("older execution correlation is missing")
	}
	if _, ok := snapshot.sources[first.SourceMessageID]; !ok {
		t.Fatalf("older source was removed: %#v", snapshot.sources)
	}
	if _, ok := snapshot.sources[second.SourceMessageID]; ok {
		t.Fatalf("failed follow-up source remained reserved: %#v", snapshot.sources)
	}
	held.finish(key)
}

func TestRecoveryPromptAndDispatchFailuresLeaveNoExecutionCorrelation(t *testing.T) {
	t.Run("prompt", func(t *testing.T) {
		service, _, cfg := newRecoveryTestService(t)
		if err := os.WriteFile(cfg.StatePath("instructions", "agent-chat.md"), []byte{0xff}, 0o600); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		entry := history.Entry{At: now, AcceptedAt: now, Role: "user", Content: "recover", SourceMessageID: "local:prompt-failure"}
		if _, err := service.History.Append("cli", "prompt-failure", entry); err != nil {
			t.Fatal(err)
		}
		service.dispatchRecovery(context.Background(), orchestrator.Origin{Channel: "cli", Conversation: "prompt-failure"}, []recoveryCandidate{{entry: entry}})
		if service.conversationInFlight("chat:cli:prompt-failure") {
			t.Fatal("recovery prompt failure left conversation correlation in flight")
		}
	})
	t.Run("dispatch", func(t *testing.T) {
		service, target, _ := newRecoveryTestService(t)
		service.Harness = &failingServiceHarness{serviceHarness: target, err: errors.New("recovery send failed")}
		now := time.Now().UTC()
		entry := history.Entry{At: now, AcceptedAt: now, Role: "user", Content: "recover", SourceMessageID: "local:dispatch-failure"}
		if _, err := service.History.Append("cli", "dispatch-failure", entry); err != nil {
			t.Fatal(err)
		}
		service.dispatchRecovery(context.Background(), orchestrator.Origin{Channel: "cli", Conversation: "dispatch-failure"}, []recoveryCandidate{{entry: entry}})
		if service.conversationInFlight("chat:cli:dispatch-failure") {
			t.Fatal("recovery dispatch failure left conversation correlation in flight")
		}
	})
}

func TestRecoveredAdmissionFailureStopsActivityBeforeFailureDelivery(t *testing.T) {
	service, target, _ := newRecoveryTestService(t)
	service.Harness = &failingServiceHarness{serviceHarness: target, err: errors.New("recovery send failed")}
	router := &conversationEventRouter{}
	service.SetConversationDelivery(router)
	if _, err := service.Settings.Update(func(next *config.Config) error {
		next.Channels.WhatsApp.Enabled = true
		next.Channels.WhatsApp.AllowedNumbers = []string{"15551234567"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	service.SetConnectionStatus(channel.ConnectionStatus{Name: "whatsapp", State: channel.ConnectionConnected})
	now := time.Now().UTC()
	entry := history.Entry{At: now, AcceptedAt: now, Role: "user", Content: "recover", SourceMessageID: "whatsapp:15551234567:failure"}
	origin := orchestrator.Origin{Channel: "whatsapp", Conversation: "WA-15551234567"}
	if _, err := service.History.Append(origin.Channel, origin.Conversation, entry); err != nil {
		t.Fatal(err)
	}
	if !service.dispatchRecovery(context.Background(), origin, []recoveryCandidate{{entry: entry}}) {
		t.Fatal("recovery failure was not handled")
	}
	events := router.snapshot()
	if len(events) != 3 || events[0].Kind != core.EventActivity || !events[0].Active || events[1].Kind != core.EventActivity || events[1].Active || events[2].Kind != core.EventFinal || !events[2].Done {
		t.Fatalf("recovery failure event ordering = %#v", events)
	}
}

func TestRecoveredTurnIgnoresDuplicateLateTerminalBeforeDurableAppend(t *testing.T) {
	service, _, _ := newRecoveryTestService(t)
	message := core.Message{Channel: "tui", Conversation: "duplicate-terminal", Sender: "recovery", SourceMessageID: "local:duplicate-terminal"}
	events := make([]core.Event, 0, 2)
	emit := service.wrapEmit(message, 1, func(event core.Event) { events = append(events, event) })
	emit(core.Event{Kind: core.EventFinal, Text: "one answer", Done: true})
	emit(core.Event{Kind: core.EventFinal, Text: "one answer", Done: true})

	entries, _, err := service.History.RecentEntries("tui", "duplicate-terminal", 10, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || len(entries) != 1 || entries[0].Content != "one answer" || !entries[0].Recovery {
		t.Fatalf("duplicate terminal result: events=%#v entries=%#v", events, entries)
	}
}

func TestRecoveryTreatsBranchedSourceMessagesAsContextOnly(t *testing.T) {
	service, target, _ := newRecoveryTestService(t)
	now := time.Now().UTC()
	if _, err := service.History.Append("cli", "source", history.Entry{At: now, AcceptedAt: now, Role: "user", Content: "source request", SourceMessageID: "local:source"}); err != nil {
		t.Fatal(err)
	}
	branch, _, err := service.History.BranchTo("cli", "source", "cli")
	if err != nil {
		t.Fatal(err)
	}
	result := service.scanRecovery(context.Background(), "periodic")
	if len(target.prompts["chat:cli:"+branch]) != 0 {
		t.Fatalf("branched context was recovered: result=%#v prompts=%#v", result, target.prompts)
	}
}

func TestRecoveryUsesLocalAcceptanceBoundaryForDelayedTransportMessage(t *testing.T) {
	service, target, _ := newRecoveryTestService(t)
	acceptedAt := time.Now().UTC()
	entry := history.Entry{
		At:              service.recoveryActivation.Add(-time.Minute),
		AcceptedAt:      acceptedAt,
		Role:            "user",
		Content:         "delayed but newly accepted request",
		SourceMessageID: "telegram:delayed",
	}
	if _, err := service.History.Append("cli", "delayed", entry); err != nil {
		t.Fatal(err)
	}
	result := service.scanRecovery(context.Background(), "periodic")
	if result.Dispatched != 1 || len(target.prompts["chat:cli:delayed"]) != 1 {
		t.Fatalf("delayed post-activation admission was excluded: result=%#v prompts=%#v", result, target.prompts)
	}
}

func TestRemoteRecoveryWaitsForConnectedOrdinaryDelivery(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	cfg.Channels.Telegram.Enabled = true
	cfg.Channels.Telegram.AllowedUsers = []string{"7"}
	target := newServiceHarness()
	service := New(cfg, target)
	defer service.Close()
	service.SetPrimaryInstanceID("primary")
	now := time.Now().UTC()
	if _, err := service.History.Append("telegram", "TG-7", history.Entry{At: now, AcceptedAt: now, Role: "user", Content: "recover remotely", SourceMessageID: "telegram:7:1"}); err != nil {
		t.Fatal(err)
	}
	service.SetConnectionStatus(channel.ConnectionStatus{Name: "telegram", State: channel.ConnectionConnected})
	if result := service.scanRecovery(context.Background(), "startup"); result.Dispatched != 0 {
		t.Fatalf("remote recovery dispatched without response router: %#v", result)
	}
	router := &notificationRouter{}
	service.SetConversationDelivery(router)
	service.SetConnectionStatus(channel.ConnectionStatus{Name: "telegram", State: channel.ConnectionConnecting})
	if result := service.scanRecovery(context.Background(), "periodic"); result.Dispatched != 0 {
		t.Fatalf("remote recovery dispatched while disconnected: %#v", result)
	}
	service.SetConnectionStatus(channel.ConnectionStatus{Name: "telegram", State: channel.ConnectionConnected})
	if result := service.scanRecovery(context.Background(), "reconnect"); result.Dispatched != 1 {
		t.Fatalf("connected remote recovery was not dispatched: %#v", result)
	}
	if len(target.prompts["chat:telegram:TG-7"]) != 1 {
		t.Fatalf("remote recovery prompts = %#v", target.prompts)
	}
}

func TestConsolidatedTerminalCoversRapidFollowupSources(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	target := &heldServiceHarness{serviceHarness: newServiceHarness(), emits: map[string]core.Emit{}, models: map[string][]string{}}
	service := New(cfg, target)
	defer service.Close()
	for _, message := range []core.Message{
		{Channel: "cli", Conversation: "rapid", SourceMessageID: "local:first", Text: "first", ReceivedAt: time.Now().UTC()},
		{Channel: "cli", Conversation: "rapid", SourceMessageID: "local:second", Text: "second", ReceivedAt: time.Now().UTC()},
	} {
		if err := service.Handle(context.Background(), message, nil); err != nil {
			t.Fatal(err)
		}
	}
	target.finish("chat:cli:rapid")
	entries, _, err := service.History.RecoveryEntries("cli", "rapid", recoveryEntryMax)
	if err != nil {
		t.Fatal(err)
	}
	covered := map[string]bool{}
	for _, entry := range entries {
		for _, source := range entry.Covers {
			covered[source] = true
		}
	}
	if !covered["local:first"] || !covered["local:second"] {
		t.Fatalf("consolidated terminal coverage = %#v", covered)
	}
}
