package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
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

func TestRecoveryFreshPrimaryDeliversOnceAfterLongOldHistory(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Channels.Telegram.Enabled = true
	cfg.Channels.Telegram.AllowedUsers = []string{"7"}
	interrupted := newHeldServiceHarness()
	previous := New(cfg, interrupted)
	old := time.Now().UTC().Add(-48 * time.Hour)
	for i := 0; i < 2001; i++ {
		if _, err := previous.History.Append("telegram", "TG-7", history.Entry{At: old, Role: "assistant", Content: "old context"}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < recoveryTotalEntryMax+1; i++ {
		if _, err := previous.History.Append("telegram", "TG-7", history.Entry{Role: "assistant", Content: "recent context"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := previous.History.Append("telegram", "TG-7", history.Entry{At: old, Role: "assistant", Content: strings.Repeat("large output ", 500000)}); err != nil {
		t.Fatal(err)
	}
	message := core.Message{Channel: "telegram", Conversation: "TG-7", SourceMessageID: "telegram:7:interrupted", Text: "check whether this report is still needed"}
	if err := previous.Handle(context.Background(), message, nil); err != nil {
		t.Fatal(err)
	}
	key := sessionKey(message)
	if !interrupted.IsActive(key) {
		t.Fatal("fixture did not reach provider admission")
	}
	// Closing the fixture without a provider terminal models loss after admission.
	if err := previous.Close(); err != nil {
		t.Fatal(err)
	}
	target := newHeldServiceHarness()
	service := New(cfg, target)
	defer service.Close()
	service.SetPrimaryInstanceID("fresh-primary")
	if result := service.scanRecovery(context.Background(), "startup"); result.Dispatched != 0 {
		t.Fatalf("disconnected startup dispatched: %#v", result)
	}
	router := &conversationEventRouter{}
	service.SetConversationDelivery(router)
	service.SetConnectionStatus(channel.ConnectionStatus{Name: "telegram", State: channel.ConnectionConnected})
	result := service.scanRecovery(context.Background(), "reconnect")
	if result.Dispatched != 1 || result.Eligible != 1 || len(target.prompts[key]) != 1 {
		t.Fatalf("fresh recovery admission = %#v", result)
	}
	prompt := target.prompts[key][0]
	for _, want := range []string{"Reconcile first", "current evidence", "Always produce a visible response", message.Text} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("ordinary recovery prompt missing %q", want)
		}
	}
	if result := service.scanRecovery(context.Background(), "periodic"); result.Dispatched != 0 {
		t.Fatalf("active recovery duplicated: %#v", result)
	}
	target.finish(key)
	finals := 0
	for _, event := range router.snapshot() {
		if event.Kind == core.EventFinal && event.Done {
			finals++
			if event.Text != "finished" {
				t.Fatalf("wrong transport reply: %#v", event)
			}
		}
	}
	if finals != 1 {
		t.Fatalf("transport received %d terminal replies", finals)
	}
	before := len(router.snapshot())
	for _, trigger := range []string{"reconnect", "periodic"} {
		if result := service.scanRecovery(context.Background(), trigger); result.Dispatched != 0 || result.Reasons["terminal_coverage"] != 1 {
			t.Fatalf("terminal source not suppressed: %#v", result)
		}
	}
	if len(target.prompts[key]) != 1 || len(router.snapshot()) != before {
		t.Fatal("repeated scans duplicated work or replies")
	}
	snapshot, err := service.History.RecoveryWindow(message.Channel, message.Conversation, time.Now().UTC().Add(-recoveryWindow), recoveryTotalEntryMax)
	for err == nil && snapshot.More && snapshot.Covered[message.SourceMessageID] == "" {
		snapshot, err = service.History.RecoveryPage(message.Channel, message.Conversation, time.Now().UTC().Add(-recoveryWindow), snapshot.Next, recoveryTotalEntryMax)
	}
	if err != nil || snapshot.Covered[message.SourceMessageID] != "terminal_coverage" || !snapshot.Admitted[message.SourceMessageID] {
		t.Fatalf("terminal correlation missing: %#v, %v", snapshot, err)
	}
	for _, entry := range service.Runtime.Logs() {
		if entry.Component == "recovery" && (strings.Contains(entry.Text, message.SourceMessageID) || strings.Contains(entry.Text, message.Text) || strings.Contains(entry.Text, message.Conversation)) {
			t.Fatal("recovery diagnostics leaked private input")
		}
	}
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

func TestRecoveryProcessesConversationBeyondRecentRecordBudget(t *testing.T) {
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
	if result.Dispatched != 1 || result.FailedClosed != 0 || len(target.prompts) != 1 {
		t.Fatalf("recent history budget prevented recovery: result=%#v prompts=%#v", result, target.prompts)
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

func TestRecoveryStatusOwnsReasonSnapshot(t *testing.T) {
	service, _, _ := newRecoveryTestService(t)
	now := time.Now().UTC()
	_, err := service.History.Append("cli", "snapshot", history.Entry{At: now, Role: "user", Content: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	result := service.scanRecovery(context.Background(), "periodic")
	if result.Reasons["legacy_or_pre_activation"] != 1 {
		t.Fatalf("missing exclusion reason: %#v", result)
	}
	result.Reasons["legacy_or_pre_activation"] = 10
	published := service.RecoveryStatus()
	if published.Reasons["legacy_or_pre_activation"] != 1 {
		t.Fatal("scan caller can mutate published status")
	}
	published.Reasons["legacy_or_pre_activation"] = 20
	if service.RecoveryStatus().Reasons["legacy_or_pre_activation"] != 1 {
		t.Fatal("status reader can mutate published status")
	}
}

func TestRecoveryHistoryFailureDiagnostics(t *testing.T) {
	for _, kind := range []string{"corrupt_history"} {
		t.Run(kind, func(t *testing.T) {
			service, target, _ := newRecoveryTestService(t)
			now := time.Now().UTC()
			path, err := service.History.Append("cli", "private-conversation", history.Entry{At: now, AcceptedAt: now, Role: "user", SourceMessageID: "private-source", Content: "private-body"})
			if err != nil {
				t.Fatal(err)
			}
			if kind == "corrupt_history" {
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteString("{private-corrupt-payload}\n")
				closeErr := f.Close()
				if err != nil || closeErr != nil {
					t.Fatalf("corrupt fixture: %v, %v", err, closeErr)
				}
			} else {
				for i := 0; i < recoveryTotalEntryMax; i++ {
					if _, err := service.History.Append("cli", "private-conversation", history.Entry{At: now, Role: "assistant"}); err != nil {
						t.Fatal(err)
					}
				}
			}
			result := service.scanRecovery(context.Background(), "startup")
			if result.Dispatched != 0 || result.FailedClosed != 1 || result.Reasons[kind] != 1 || len(target.prompts) != 0 {
				t.Fatalf("unsafe or unexplained scan failure: %#v", result)
			}
			found := false
			for _, entry := range service.Runtime.Logs() {
				if entry.Component != "recovery" {
					continue
				}
				found = found || entry.Event == "decision" && strings.Contains(entry.Text, "conversation_slot=1 candidate_slot=0 reason="+kind)
				for _, private := range []string{"private-conversation", "private-source", "private-body", "private-corrupt-payload"} {
					if strings.Contains(entry.Text, private) {
						t.Fatal("recovery diagnostics retained private input")
					}
				}
			}
			if !found {
				t.Fatal("missing bounded scan-local failure reason")
			}
		})
	}
}

func TestStableSourceIdentityDeduplicatesDeliveryAndTerminalCoverage(t *testing.T) {
	service, target, _ := newRecoveryTestService(t)
	message := core.Message{Channel: "cli", Conversation: "duplicate", Sender: "cli", SourceMessageID: "local:same", Text: "run once", ReceivedAt: time.Now().UTC()}
	if err := service.Handle(context.Background(), message, nil); err != nil {
		t.Fatal(err)
	}
	if err := service.Handle(context.Background(), message, nil); !errors.Is(err, ErrDuplicateMessage) {
		t.Fatalf("retry = %v, want explicit duplicate", err)
	}
	if len(target.prompts["chat:cli:duplicate"]) != 1 {
		t.Fatalf("duplicate source reached provider %d times", len(target.prompts["chat:cli:duplicate"]))
	}
	entries, _, err := service.History.RecoveryEntries("cli", "duplicate", recoveryTotalEntryMax)
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
	if status := service.Runtime.Status(); status.Jobs != 0 || status.LiveJobs != 0 {
		t.Fatalf("harness send failure left runtime job active: %#v", service.Runtime.Status())
	}
	archived, output, err := service.Runtime.ArchivedJob(1)
	if err != nil || archived.State != "error" || !strings.Contains(output, "send failed") {
		t.Fatalf("failed new dispatch must retain a settled error archive: %#v, %v", archived, err)
	}
}

type rejectedSteerServiceHarness struct {
	*heldServiceHarness
	rejectNext bool
}

func (*rejectedSteerServiceHarness) FollowUpMode() harness.FollowUpMode { return harness.FollowUpSteer }

func (r *rejectedSteerServiceHarness) SendWithModel(ctx context.Context, key, prompt, model string, emit core.Emit) (string, bool, error) {
	r.mu.Lock()
	reject := r.rejectNext && r.active[key]
	if reject {
		r.rejectNext = false
	}
	r.mu.Unlock()
	if reject {
		// Native steering can reject a request while retaining the original emitter.
		return r.ThreadID(key), true, errors.New("provider rejected follow-up")
	}
	return r.heldServiceHarness.SendWithModel(ctx, key, prompt, model, emit)
}

func TestFailedFollowupKeepsGlobalJobLiveAndRollsBackOnlyItsSource(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	target := &rejectedSteerServiceHarness{heldServiceHarness: newHeldServiceHarness(), rejectNext: true}
	registry := harness.NewRegistry()
	registry.Register("fixture", func(harness.HarnessConfig) (harness.Harness, error) { return target, nil })
	supervisor := harness.NewSupervisor(registry, harness.HarnessConfig{Name: "fixture"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	service := New(cfg, supervisor)
	defer service.Close()
	if err := service.RegisterLiveTUI("idle-instance", "idle-displayed", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	check := func(want int) {
		t.Helper()
		state := service.SharedStateForInstance("idle-instance")
		status, err := service.Status(core.Message{Channel: "tui", Conversation: "idle-displayed"})
		if err != nil {
			t.Fatal(err)
		}
		if state.ConversationActivity != 0 || state.DurableWork != (core.DurableWorkCounts{}) || status.TurnActive || status.OrchestratorLease != 0 || status.OrchestratorRuns != 0 {
			t.Fatal("displayed conversation and durable work must stay idle")
		}
		if state.Runtime != status.Runtime || state.Runtime.Jobs != want || state.Runtime.LiveJobs != want {
			t.Fatalf("global jobs = %#v, status = %#v; want %d", state.Runtime, status.Runtime, want)
		}
	}
	check(0)
	first := core.Message{Channel: "cli", Conversation: "followup-failure", SourceMessageID: "local:first", Text: "first"}
	firstEvents := &conversationEventRouter{}
	if err := service.Handle(ctx, first, func(event core.Event) { _ = firstEvents.DeliverEvent(ctx, "", "", "", event) }); err != nil {
		t.Fatal(err)
	}
	key := sessionKey(first)
	target.mu.Lock()
	emit := target.emits[key]
	target.mu.Unlock()
	emit(core.Event{Kind: core.EventStatus, Execution: &core.ExecutionStatus{State: "reconnecting", Detail: "retrying", ReconnectAttempt: 1, ReconnectTotal: 2}})
	before, _ := service.Runtime.JobForSession(key)
	check(1)
	second := core.Message{Channel: first.Channel, Conversation: first.Conversation, SourceMessageID: "local:second", Text: "second"}
	if err := service.Handle(ctx, second, nil); err == nil {
		t.Fatal("follow-up send failure was not returned")
	}
	check(1)
	job, exists := service.Runtime.JobForSession(key)
	if !exists || job != before || !target.IsActive(key) || !supervisor.IsActive(key) {
		t.Fatalf("failed steering changed the original live execution: before=%#v after=%#v", before, job)
	}
	archived, output, err := service.Runtime.ArchivedJob(job.StableID)
	if err != nil || archived.State != "reconnecting" || !strings.Contains(output, "provider rejected follow-up") {
		t.Fatalf("request failure must remain inspectable without settling the archive: %#v, %v", archived, err)
	}
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
	emit(core.Event{Kind: core.EventStatus, Execution: &core.ExecutionStatus{State: "running"}})
	emit(core.Event{Kind: core.EventDelta, Text: "original work continues"})
	check(1)
	job, _ = service.Runtime.JobForSession(key)
	if job.Execution != JobRunning || job.StatusDetail != "" {
		t.Fatalf("fresh provider output did not restore running: %#v", job)
	}
	events := firstEvents.snapshot()
	if events[len(events)-1].Text != "original work continues" {
		t.Fatal("original emitter lost continuing output")
	}
	for _, event := range events {
		if event.Kind == core.EventActivity && !event.Active {
			t.Fatal("rejected follow-up stopped original activity")
		}
	}
	third := core.Message{Channel: first.Channel, Conversation: first.Conversation, SourceMessageID: "local:third", Text: "accepted follow-up"}
	thirdEvents := &conversationEventRouter{}
	if err := service.Handle(ctx, third, func(event core.Event) { _ = thirdEvents.DeliverEvent(ctx, "", "", "", event) }); err != nil {
		t.Fatal(err)
	}
	check(1)
	target.finish(key)
	check(0)
	if supervisor.IsActive(key) || service.conversationInFlight(key) {
		t.Fatal("actual provider completion left execution ownership active")
	}
	events = thirdEvents.snapshot()
	if events[len(events)-1].Kind != core.EventFinal || !events[len(events)-1].Done {
		t.Fatal("accepted follow-up did not receive provider completion")
	}
	archived, _, err = service.Runtime.ArchivedJob(job.StableID)
	if err != nil || archived.State != "completed" {
		t.Fatalf("provider completion did not settle archive: %#v, %v", archived, err)
	}
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
	entries, _, err := service.History.RecoveryEntries("cli", "rapid", recoveryTotalEntryMax)
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

func TestRecoveryEventuallyProcessesConversationAndCandidateBatches(t *testing.T) {
	service, target, cfg := newRecoveryTestService(t)
	now := time.Now().UTC()
	for i := 0; i < recoveryConversationMax+5; i++ {
		name := fmt.Sprintf("batch-%03d", i)
		if _, err := service.History.Append("cli", name, history.Entry{At: now, AcceptedAt: now, Role: "user", SourceMessageID: name, Content: "request"}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 41; i++ {
		if _, err := service.History.Append("cli", "many", history.Entry{At: now, AcceptedAt: now, Role: "user", SourceMessageID: fmt.Sprintf("source-%d", i), Content: strings.Repeat("large request ", 4000)}); err != nil {
			t.Fatal(err)
		}
	}
	for scan := 0; scan < 15; scan++ {
		result := service.scanRecovery(context.Background(), "periodic")
		if result.Dispatched > recoveryDispatchMax || result.FailedClosed != 0 {
			t.Fatalf("unsafe batch scan: %#v", result)
		}
		if result.Truncated {
			select {
			case <-service.recoveryTrigger:
			default:
				t.Fatal("deferred conversation batch did not schedule continuation")
			}
		}
	}
	if len(target.prompts) != recoveryConversationMax+6 {
		t.Fatalf("conversation starvation: dispatched %d", len(target.prompts))
	}
	for key, prompts := range target.prompts {
		want := 1
		if key == "chat:cli:many" {
			want = 3
		}
		if len(prompts) != want {
			t.Fatalf("duplicate or missing work in %s: %d", key, len(prompts))
		}
		for _, prompt := range prompts {
			if len(prompt) > 128*1024 || key == "chat:cli:many" && (!strings.Contains(prompt, `excerpt="true"`) || !strings.Contains(prompt, "Read each complete source message")) {
				t.Fatalf("unbounded or unexplained excerpt: %d bytes", len(prompt))
			}
		}
	}
	entries, _, err := service.History.RecoveryEntries("cli", "many", recoveryTotalEntryMax)
	if err != nil {
		t.Fatal(err)
	}
	admitted, covered := map[string]int{}, map[string]int{}
	terminals := 0
	for _, entry := range entries {
		if entry.Admission != "" {
			admitted[entry.SourceMessageID]++
		}
		if entry.Outcome == "terminal_assistant" {
			terminals++
			for _, id := range entry.Covers {
				covered[id]++
			}
		}
	}
	if terminals != 3 || len(admitted) != 41 || len(covered) != 41 {
		t.Fatalf("batch correlation: terminals=%d admitted=%d covered=%d", terminals, len(admitted), len(covered))
	}
	for i := 0; i < 41; i++ {
		id := fmt.Sprintf("source-%d", i)
		if admitted[id] != 1 || covered[id] != 1 {
			t.Fatalf("batch source %s: admissions=%d terminal coverage=%d", id, admitted[id], covered[id])
		}
	}
	fresh := New(cfg, newServiceHarness())
	defer fresh.Close()
	fresh.SetPrimaryInstanceID("replacement")
	for scan := 0; scan < 3; scan++ {
		if result := fresh.scanRecovery(context.Background(), "reconnect"); result.Dispatched != 0 || result.FailedClosed != 0 {
			t.Fatalf("completed batches not durably suppressed: %#v", result)
		}
	}
}

func TestRecoveryBatchTerminalAndCancellationCoverEverySource(t *testing.T) {
	for _, command := range []string{"error", "/stop", "/job kill 1"} {
		t.Run(command, func(t *testing.T) {
			service, _, cfg := newRecoveryTestService(t)
			target := &synchronousInterruptHarness{heldServiceHarness: newHeldServiceHarness()}
			service.Harness = target
			now := time.Now().UTC()
			for _, id := range []string{"first", "second", "third"} {
				if _, err := service.History.Append("cli", "batch-cancel", history.Entry{At: now, AcceptedAt: now, Role: "user", SourceMessageID: id, Content: "request"}); err != nil {
					t.Fatal(err)
				}
			}
			if result := service.scanRecovery(context.Background(), "startup"); result.Dispatched != 1 || result.Eligible != 3 {
				t.Fatalf("batch not admitted: %#v", result)
			}
			if result := service.scanRecovery(context.Background(), "reconnect"); result.Dispatched != 0 {
				t.Fatalf("active batch duplicated: %#v", result)
			}
			want := "intentional_cancellation"
			if command == "error" {
				want = "terminal_coverage"
				target.mu.Lock()
				emit := target.emits["chat:cli:batch-cancel"]
				target.active["chat:cli:batch-cancel"] = false
				target.mu.Unlock()
				emit(core.Event{Kind: core.EventError, Text: "provider failed", Done: true})
			} else if err := service.Handle(context.Background(), core.Message{Channel: "cli", Conversation: "batch-cancel", Text: command}, nil); err != nil {
				t.Fatal(err)
			}
			snapshot, err := service.History.RecoveryWindow("cli", "batch-cancel", now.Add(-recoveryWindow), recoveryTotalEntryMax)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"first", "second", "third"} {
				if !snapshot.Admitted[id] || snapshot.Covered[id] != want {
					t.Fatalf("batch source %s: admitted=%t, coverage=%q", id, snapshot.Admitted[id], snapshot.Covered[id])
				}
			}
			fresh := New(cfg, newServiceHarness())
			defer fresh.Close()
			fresh.SetPrimaryInstanceID("replacement")
			if result := fresh.scanRecovery(context.Background(), "startup"); result.Dispatched != 0 || result.Reasons[want] != 3 {
				t.Fatalf("batch not durably suppressed: %#v", result)
			}
		})
	}
}

func TestRecoveryBatchSendFailureRollsBackOnlyAddedSources(t *testing.T) {
	for _, followup := range []bool{false, true} {
		t.Run(fmt.Sprint(followup), func(t *testing.T) {
			service, target, _ := newRecoveryTestService(t)
			message := core.Message{Channel: "cli", Conversation: "batch-failure", SourceMessageID: "older"}
			if followup {
				if _, _, err := service.reserveRecoveryExecution(message); err != nil {
					t.Fatal(err)
				}
			}
			service.Harness = &failingServiceHarness{serviceHarness: target, err: errors.New("send failed")}
			message.SourceMessageID = "attempted-first"
			if err := service.dispatchHarnessPrompt(context.Background(), message, "prompt", nil, "attempted-second", "older"); err == nil {
				t.Fatal("batch send failure was not returned")
			}
			snapshot := service.recoveryCancellationSnapshot(sessionKey(message))
			if !followup {
				if snapshot != nil {
					t.Fatal("rejected new batch left execution correlation")
				}
			} else if snapshot == nil || len(snapshot.sources) != 1 {
				t.Fatalf("rejected follow-up changed older correlation: %#v", snapshot)
			} else if _, ok := snapshot.sources["older"]; !ok {
				t.Fatal("rejected follow-up removed the older source")
			}
		})
	}
}

func TestRecoverySourceIndexBeyondDocumentBudgetAndLargeProgress(t *testing.T) {
	service, target, cfg := newRecoveryTestService(t)
	for i := 0; i < 2001; i++ {
		text := fmt.Sprintf("---\nid: doc-%d\nsource_message_ids: [source-%d]\n---\n", i, i)
		if i == 2000 {
			text += strings.Repeat("progress\n", 1<<18)
		}
		if err := os.WriteFile(filepath.Join(cfg.StatePath("tasks", "done"), fmt.Sprintf("%04d.md", i)), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	for _, id := range []string{"source-2000", "unlinked"} {
		if _, err := service.History.Append("cli", "linked", history.Entry{At: now, AcceptedAt: now, Role: "user", SourceMessageID: id, Content: "request"}); err != nil {
			t.Fatal(err)
		}
	}
	result := service.scanRecovery(context.Background(), "periodic")
	if result.Dispatched != 1 || result.Reasons["linked_work"] != 1 || result.FailedClosed != 0 {
		t.Fatalf("source index discarded workflow: %#v", result)
	}
	prompt := target.prompts["chat:cli:linked"][0]
	if strings.Contains(prompt, `source_message_id="source-2000"`) || !strings.Contains(prompt, `source_message_id="unlinked"`) {
		t.Fatal("linked-work suppression changed past document budget")
	}
}
