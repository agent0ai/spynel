package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/extensions"
	"github.com/agent0ai/spynel/internal/workspace"
)

type heartbeatHarness struct {
	mu                 sync.Mutex
	calls              int
	entered            chan struct{}
	release            chan struct{}
	ignoreCancellation bool
	err                error
	prompt             string
}

func (h *heartbeatHarness) Start(context.Context) error                     { return nil }
func (h *heartbeatHarness) Close() error                                    { return nil }
func (h *heartbeatHarness) Interrupt(context.Context, string) (bool, error) { return false, nil }
func (h *heartbeatHarness) ResetSession(string) error                       { return nil }
func (h *heartbeatHarness) ThreadID(string) string                          { return "heartbeat-thread" }
func (h *heartbeatHarness) IsActive(string) bool                            { return false }
func (h *heartbeatHarness) Send(ctx context.Context, key, prompt string, emit core.Emit) (string, bool, error) {
	h.mu.Lock()
	h.calls++
	h.prompt = prompt
	h.mu.Unlock()
	if h.entered != nil {
		select {
		case h.entered <- struct{}{}:
		default:
		}
	}
	if h.release != nil {
		if h.ignoreCancellation {
			<-h.release
		} else {
			select {
			case <-ctx.Done():
				return "", false, ctx.Err()
			case <-h.release:
			}
		}
	}
	if h.err != nil {
		return "", false, h.err
	}
	id := regexp.MustCompile(`heartbeat-[0-9]+-[a-z0-9]+`).FindString(prompt)
	now := regexp.MustCompile(`[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:]+Z`).FindString(prompt)
	result := fmt.Sprintf(`{"schema":"%s","execution_id":"%s","observed_at":"%s","status":"healthy","findings":[]}`, semanticHeartbeatSchema, id, now)
	emit(core.Event{Kind: core.EventFinal, Text: result, Done: true})
	return "heartbeat-thread", false, nil
}

func newHeartbeatManager(t *testing.T, target *heartbeatHarness) *Manager {
	t.Helper()
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	manager := New(cfg, target, extensions.Runner{Directory: filepath.Join(root, "missing")})
	manager.SetPrimaryOwned(true)
	return manager
}

func recordAndDeliverHeartbeat(t *testing.T, manager *Manager, result semanticHeartbeatResult) {
	t.Helper()
	if err := manager.recordSemanticHeartbeatResult(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	if err := manager.Outbox.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSemanticHeartbeatRejectsMalformedAndStaleResults(t *testing.T) {
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	if _, err := parseSemanticHeartbeatResult("not json", "run-1", now); err == nil {
		t.Fatal("malformed result was accepted")
	}
	raw := fmt.Sprintf(`{"schema":"%s","execution_id":"old","observed_at":"%s","status":"healthy","findings":[]}`, semanticHeartbeatSchema, now.Format(time.RFC3339))
	if _, err := parseSemanticHeartbeatResult(raw, "run-1", now); err == nil {
		t.Fatal("stale execution result was accepted")
	}
	valid := fmt.Sprintf(`{"schema":"%s","execution_id":"run-1","observed_at":"%s","status":"healthy","findings":[]}`, semanticHeartbeatSchema, now.Format(time.RFC3339))
	if _, err := parseSemanticHeartbeatResult(valid+" trailing", "run-1", now); err == nil {
		t.Fatal("malformed trailing bytes were accepted")
	}
}

func TestSemanticHeartbeatRejectsInconsistentActionFields(t *testing.T) {
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		finding string
	}{
		{"empty workflow", `{"category":"external_input_required","workflow_id":"","evidence":"evidence","action":"none"}`},
		{"empty evidence", `{"category":"external_input_required","workflow_id":"task-1","evidence":"","action":"none"}`},
		{"notify without message", `{"category":"external_input_required","workflow_id":"task-1","evidence":"evidence","action":"notify","notification_origin":"tui/local"}`},
		{"notification on none", `{"category":"external_input_required","workflow_id":"task-1","evidence":"evidence","action":"none","notification_origin":"tui/local","notification":"message"}`},
		{"incompatible recovery", `{"category":"due_waiting_condition","workflow_id":"task-1","evidence":"evidence","action":"request_recover"}`},
		{"notification on repair", `{"category":"stale_or_orphaned_claim","workflow_id":"task-1","evidence":"evidence","action":"request_recover","notification_origin":"tui/local","notification":"message"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := fmt.Sprintf(`{"schema":%q,"execution_id":"run-1","observed_at":%q,"status":"findings","findings":[%s]}`, semanticHeartbeatSchema, now.Format(time.RFC3339), test.finding)
			if _, err := parseSemanticHeartbeatResult(raw, "run-1", now); err == nil {
				t.Fatal("inconsistent result was accepted")
			}
		})
	}
}

func TestSemanticHeartbeatRejectsInconsistentStatusAndHealthyActions(t *testing.T) {
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	tests := []string{
		fmt.Sprintf(`{"schema":%q,"execution_id":"run-1","observed_at":%q,"status":"findings","findings":[]}`, semanticHeartbeatSchema, now.Format(time.RFC3339)),
		fmt.Sprintf(`{"schema":%q,"execution_id":"run-1","observed_at":%q,"status":"failed","findings":[{"category":"external_input_required","workflow_id":"task-1","evidence":"evidence","action":"notify","notification_origin":"tui/local","notification":"message"}]}`, semanticHeartbeatSchema, now.Format(time.RFC3339)),
		fmt.Sprintf(`{"schema":%q,"execution_id":"run-1","observed_at":%q,"status":"findings","findings":[{"category":"healthy_or_progressing","workflow_id":"task-1","evidence":"fresh lease","action":"notify","notification_origin":"tui/local","notification":"message"}]}`, semanticHeartbeatSchema, now.Format(time.RFC3339)),
	}
	for index, raw := range tests {
		if _, err := parseSemanticHeartbeatResult(raw, "run-1", now); err == nil {
			t.Fatalf("inconsistent status case %d was accepted", index)
		}
	}
}

func TestSemanticHeartbeatDoesNotOverlapSlowAudit(t *testing.T) {
	target := &heartbeatHarness{entered: make(chan struct{}, 2), release: make(chan struct{})}
	manager := newHeartbeatManager(t, target)
	ticks := make(chan time.Time, 2)
	manager.heartbeatTicks = ticks
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { manager.runSemanticHeartbeat(ctx); close(done) }()
	ticks <- time.Now()
	select {
	case <-target.entered:
	case <-time.After(time.Second):
		t.Fatal("audit did not start")
	}
	ticks <- time.Now()
	time.Sleep(20 * time.Millisecond)
	target.mu.Lock()
	calls := target.calls
	target.mu.Unlock()
	if calls != 1 {
		t.Fatalf("overlapping audit calls = %d", calls)
	}
	close(target.release)
	manager.Wait()
	cancel()
	<-done
}

func TestSemanticHeartbeatPublishesOwnedDeadlineAndClearsItAtHandoff(t *testing.T) {
	target := &heartbeatHarness{entered: make(chan struct{}, 1), release: make(chan struct{})}
	manager := newHeartbeatManager(t, target)
	base := time.Date(2026, 8, 8, 14, 0, 0, 0, time.UTC)
	manager.heartbeatNow = func() time.Time { return base }
	ticks := make(chan time.Time, 1)
	manager.heartbeatTicks = ticks
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { manager.runSemanticHeartbeat(ctx); close(done) }()

	deadline := base.Add(time.Duration(manager.Config.Orchestrator.SemanticHeartbeatMinutes) * time.Minute)
	until := time.Now().Add(time.Second)
	for {
		status := manager.WorkStatus()
		if status.HeartbeatState == "scheduled" && status.NextHeartbeatAt.Equal(deadline) {
			break
		}
		if time.Now().After(until) {
			t.Fatalf("scheduler did not publish deadline: %#v", status)
		}
		time.Sleep(time.Millisecond)
	}
	tick := base.Add(2 * time.Minute)
	base = tick.Add(4 * time.Minute)
	ticks <- tick
	select {
	case <-target.entered:
	case <-time.After(time.Second):
		t.Fatal("audit did not start")
	}
	status := manager.WorkStatus()
	if status.HeartbeatState != "running" || !status.NextHeartbeatAt.IsZero() {
		t.Fatalf("running heartbeat status = %#v", status)
	}
	base = base.Add(20 * time.Minute)
	close(target.release)
	until = time.Now().Add(time.Second)
	for {
		status = manager.WorkStatus()
		if status.HeartbeatState == "scheduled" && status.NextHeartbeatAt.Equal(base.Add(15*time.Minute)) {
			break
		}
		if time.Now().After(until) {
			t.Fatalf("scheduler did not publish fixed-delay successor: %#v", status)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	manager.SetPrimaryOwned(false)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop at handoff")
	}
	status = manager.WorkStatus()
	if status.HeartbeatState != "not_primary" || !status.NextHeartbeatAt.IsZero() {
		t.Fatalf("handoff heartbeat status = %#v", status)
	}
	manager.Wait()
}

func TestSemanticHeartbeatSuccessorUsesTerminalReleaseNotDelayedHandling(t *testing.T) {
	target := &heartbeatHarness{entered: make(chan struct{}, 1), release: make(chan struct{})}
	manager := newHeartbeatManager(t, target)
	base := time.Date(2026, 8, 8, 14, 0, 0, 0, time.UTC)
	manager.heartbeatNow = func() time.Time { return base }
	ticks := make(chan time.Time, 1)
	manager.heartbeatTicks = ticks
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { manager.runSemanticHeartbeat(ctx); close(done) }()
	ticks <- base
	select {
	case <-target.entered:
	case <-time.After(time.Second):
		t.Fatal("audit did not start")
	}
	manager.heartbeatCommit.Lock()
	completedAt := base.Add(20 * time.Minute)
	base = completedAt
	close(target.release)
	until := time.Now().Add(time.Second)
	for manager.semanticHeartbeatProviderInFlight() {
		if time.Now().After(until) {
			manager.heartbeatCommit.Unlock()
			t.Fatal("provider did not release while result commit was blocked")
		}
		time.Sleep(time.Millisecond)
	}
	// Keep scheduler handling blocked until well after the provider's terminal
	// release, including valid-result parsing and commit. The successor must
	// retain the provider's carried release timestamp.
	base = base.Add(4 * time.Minute)
	manager.heartbeatCommit.Unlock()
	manager.Wait()
	until = time.Now().Add(time.Second)
	want := completedAt.Add(15 * time.Minute)
	for {
		status := manager.WorkStatus()
		if status.HeartbeatState == "scheduled" && status.NextHeartbeatAt.Equal(want) {
			break
		}
		if time.Now().After(until) {
			t.Fatalf("delayed completion handling published %#v, want %s", status, want)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

func TestSemanticHeartbeatRestartWaitsForPriorProviderRelease(t *testing.T) {
	target := &heartbeatHarness{entered: make(chan struct{}, 1), release: make(chan struct{}), ignoreCancellation: true}
	manager := newHeartbeatManager(t, target)
	base := time.Date(2026, 8, 8, 14, 0, 0, 0, time.UTC)
	manager.heartbeatNow = func() time.Time { return base }
	ticks := make(chan time.Time, 1)
	manager.heartbeatTicks = ticks
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstDone := make(chan struct{})
	go func() { manager.runSemanticHeartbeat(firstCtx); close(firstDone) }()
	ticks <- base
	select {
	case <-target.entered:
	case <-time.After(time.Second):
		t.Fatal("audit did not start before scheduler restart")
	}
	cancelFirst()
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first scheduler did not stop")
	}
	secondCtx, cancelSecond := context.WithCancel(context.Background())
	secondDone := make(chan struct{})
	go func() { manager.runSemanticHeartbeat(secondCtx); close(secondDone) }()
	until := time.Now().Add(time.Second)
	for {
		status := manager.WorkStatus()
		if status.HeartbeatState == "unavailable" && status.NextHeartbeatAt.IsZero() {
			break
		}
		if time.Now().After(until) {
			t.Fatalf("restarted scheduler published before prior provider release: %#v", status)
		}
		time.Sleep(time.Millisecond)
	}
	base = base.Add(20 * time.Minute)
	releasedAt := base
	close(target.release)
	until = time.Now().Add(time.Second)
	want := releasedAt.Add(15 * time.Minute)
	for {
		status := manager.WorkStatus()
		if status.HeartbeatState == "scheduled" && status.NextHeartbeatAt.Equal(want) {
			break
		}
		if time.Now().After(until) {
			t.Fatalf("restarted scheduler did not arm from prior provider release: %#v", status)
		}
		time.Sleep(time.Millisecond)
	}
	cancelSecond()
	<-secondDone
	manager.Wait()
}

func TestLiveOrchestratorEnableRequestsImmediateScan(t *testing.T) {
	manager := newHeartbeatManager(t, &heartbeatHarness{})
	cfg := manager.Config
	cfg.Orchestrator.Enabled = false
	manager.ApplyRuntimeConfig(cfg)
	select {
	case <-manager.scanNow:
		t.Fatal("disabling orchestration requested a scan")
	default:
	}
	cfg.Orchestrator.Enabled = true
	manager.ApplyRuntimeConfig(cfg)
	select {
	case <-manager.scanNow:
	case <-time.After(time.Second):
		t.Fatal("enabling orchestration did not request an immediate scan")
	}
}

func TestSemanticHeartbeatLiveConfigReschedulesAndDisablesOwnedDeadline(t *testing.T) {
	manager := newHeartbeatManager(t, &heartbeatHarness{})
	base := time.Date(2026, 8, 8, 14, 0, 0, 0, time.UTC)
	manager.heartbeatNow = func() time.Time { return base }
	manager.heartbeatTicks = make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { manager.runSemanticHeartbeat(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor := func(wantState string, wantDeadline time.Time) {
		t.Helper()
		until := time.Now().Add(time.Second)
		for {
			status := manager.WorkStatus()
			if status.HeartbeatState == wantState && status.NextHeartbeatAt.Equal(wantDeadline) {
				return
			}
			if time.Now().After(until) {
				t.Fatalf("heartbeat did not reach %s at %s: %#v", wantState, wantDeadline, status)
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitFor("scheduled", base.Add(15*time.Minute))
	cfg := manager.Config
	cfg.Orchestrator.SemanticHeartbeatMinutes = 30
	acceptedAt := base
	manager.ApplyRuntimeConfig(cfg)
	base = base.Add(4 * time.Minute)
	waitFor("scheduled", acceptedAt.Add(30*time.Minute))
	cfg.Orchestrator.SemanticHeartbeatMinutes = 0
	manager.ApplyRuntimeConfig(cfg)
	waitFor("disabled", time.Time{})
	cfg.Orchestrator.SemanticHeartbeatMinutes = 5
	manager.ApplyRuntimeConfig(cfg)
	waitFor("scheduled", base.Add(5*time.Minute))
}

func TestSemanticHeartbeatLiveDisableSynchronouslyFencesUnacknowledgedTick(t *testing.T) {
	target := &heartbeatHarness{entered: make(chan struct{}, 1)}
	manager := newHeartbeatManager(t, target)
	ticks := make(chan time.Time, 1)
	manager.heartbeatTicks = ticks
	// Make the scheduler notification deliberately unavailable. ApplyRuntimeConfig
	// must still fence the old term synchronously before it returns.
	manager.heartbeatConfigChanged = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { manager.runSemanticHeartbeat(ctx); close(done) }()
	until := time.Now().Add(time.Second)
	for manager.WorkStatus().HeartbeatState != "scheduled" {
		if time.Now().After(until) {
			t.Fatalf("heartbeat scheduler did not start: %#v", manager.WorkStatus())
		}
		time.Sleep(time.Millisecond)
	}
	cfg := manager.Config
	cfg.Orchestrator.Enabled = false
	manager.ApplyRuntimeConfig(cfg)
	ticks <- time.Now()
	select {
	case <-target.entered:
		t.Fatal("superseded tick dispatched after live disable was accepted")
	case <-time.After(20 * time.Millisecond):
	}
	if status := manager.WorkStatus(); status.HeartbeatState != "disabled" || !status.NextHeartbeatAt.IsZero() {
		t.Fatalf("disabled status retained superseded schedule: %#v", status)
	}
	cancel()
	<-done
}

func TestSemanticHeartbeatLiveDisableSynchronouslyStopsProductionTimer(t *testing.T) {
	manager := newHeartbeatManager(t, &heartbeatHarness{})
	manager.heartbeatConfigChanged = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { manager.runSemanticHeartbeat(ctx); close(done) }()
	until := time.Now().Add(time.Second)
	for manager.WorkStatus().HeartbeatState != "scheduled" {
		if time.Now().After(until) {
			t.Fatalf("heartbeat scheduler did not start: %#v", manager.WorkStatus())
		}
		time.Sleep(time.Millisecond)
	}
	manager.heartbeatTimerMu.Lock()
	armed := manager.heartbeatTimer != nil
	manager.heartbeatTimerMu.Unlock()
	if !armed {
		t.Fatal("scheduled heartbeat did not register its production timer")
	}
	cfg := manager.Config
	cfg.Orchestrator.Enabled = false
	manager.ApplyRuntimeConfig(cfg)
	manager.heartbeatTimerMu.Lock()
	stillArmed := manager.heartbeatTimer != nil
	manager.heartbeatTimerMu.Unlock()
	if stillArmed {
		t.Fatal("live disable returned with the superseded production timer armed")
	}
	cancel()
	<-done
}

func TestSemanticHeartbeatRestartSchedulesFromNewOwnershipTime(t *testing.T) {
	manager := newHeartbeatManager(t, &heartbeatHarness{})
	base := time.Date(2026, 8, 8, 14, 0, 0, 0, time.UTC)
	manager.heartbeatNow = func() time.Time { return base }
	manager.heartbeatTicks = make(chan time.Time)
	cfg := manager.Config
	cfg.Orchestrator.SemanticHeartbeatMinutes = 30
	manager.ApplyRuntimeConfig(cfg)
	run := func(want time.Time) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { manager.runSemanticHeartbeat(ctx); close(done) }()
		until := time.Now().Add(time.Second)
		for {
			status := manager.WorkStatus()
			if status.HeartbeatState == "scheduled" && status.NextHeartbeatAt.Equal(want) {
				break
			}
			if time.Now().After(until) {
				t.Fatalf("heartbeat deadline = %#v, want %s", status, want)
			}
			time.Sleep(time.Millisecond)
		}
		cancel()
		<-done
	}
	run(base.Add(30 * time.Minute))
	base = base.Add(time.Hour)
	run(base.Add(30 * time.Minute))
}

func TestDisabledLiveHeartbeatIgnoresInjectedSchedulerTicks(t *testing.T) {
	target := &heartbeatHarness{entered: make(chan struct{}, 1)}
	manager := newHeartbeatManager(t, target)
	ticks := make(chan time.Time, 1)
	manager.heartbeatTicks = ticks
	cfg := manager.Config
	cfg.Orchestrator.Enabled = false
	manager.ApplyRuntimeConfig(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { manager.runSemanticHeartbeat(ctx); close(done) }()
	ticks <- time.Now()
	select {
	case <-target.entered:
		t.Fatal("disabled semantic scheduler accepted a tick")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	<-done
}

func TestSemanticHeartbeatLiveRescheduleAndReenableHideSupersededInFlightAudit(t *testing.T) {
	target := &heartbeatHarness{entered: make(chan struct{}, 1), release: make(chan struct{}), ignoreCancellation: true}
	manager := newHeartbeatManager(t, target)
	base := time.Date(2026, 8, 8, 14, 0, 0, 0, time.UTC)
	manager.heartbeatNow = func() time.Time { return base }
	ticks := make(chan time.Time, 1)
	manager.heartbeatTicks = ticks
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { manager.runSemanticHeartbeat(ctx); close(done) }()
	ticks <- time.Now()
	select {
	case <-target.entered:
	case <-time.After(time.Second):
		t.Fatal("audit did not start before live disable")
	}
	cfg := manager.Config
	cfg.Orchestrator.SemanticHeartbeatMinutes = 30
	manager.ApplyRuntimeConfig(cfg)
	until := time.Now().Add(time.Second)
	for manager.WorkStatus().HeartbeatState != "unavailable" {
		if time.Now().After(until) {
			t.Fatalf("live reschedule did not suppress successor deadline: %#v", manager.WorkStatus())
		}
		time.Sleep(time.Millisecond)
	}
	if status := manager.WorkStatus(); !status.NextHeartbeatAt.IsZero() {
		t.Fatalf("live reschedule retained a successor deadline: %#v", status)
	}
	cfg.Orchestrator.Enabled = false
	manager.ApplyRuntimeConfig(cfg)
	until = time.Now().Add(time.Second)
	for manager.WorkStatus().HeartbeatState != "disabled" {
		if time.Now().After(until) {
			t.Fatalf("live disable did not clear scheduler state: %#v", manager.WorkStatus())
		}
		time.Sleep(time.Millisecond)
	}
	cfg.Orchestrator.Enabled = true
	manager.ApplyRuntimeConfig(cfg)
	until = time.Now().Add(time.Second)
	for manager.WorkStatus().HeartbeatState != "unavailable" {
		if time.Now().After(until) {
			t.Fatalf("live re-enable did not wait for superseded audit: %#v", manager.WorkStatus())
		}
		time.Sleep(time.Millisecond)
	}
	close(target.release)
	manager.Wait()
	until = time.Now().Add(time.Second)
	wantDeadline := base.Add(30 * time.Minute)
	for {
		status := manager.WorkStatus()
		if status.HeartbeatState == "scheduled" && status.NextHeartbeatAt.Equal(wantDeadline) {
			break
		}
		if time.Now().After(until) {
			t.Fatalf("live re-enable did not arm after terminal provider completion: %#v", status)
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := os.Stat(manager.Config.StatePath("runtime", "semantic-heartbeat.json")); !os.IsNotExist(err) {
		t.Fatalf("audit superseded by live disable mutated heartbeat state: %v", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop after live-disable test")
	}
}

func TestSemanticHeartbeatCancelsBlockedProviderAtTimeout(t *testing.T) {
	target := &heartbeatHarness{release: make(chan struct{})}
	manager := newHeartbeatManager(t, target)
	manager.heartbeatTimeout = 10 * time.Millisecond
	started := time.Now()
	manager.runSemanticHeartbeatOnce(context.Background())
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("blocked provider cancellation took %s", elapsed)
	}
	target.mu.Lock()
	calls := target.calls
	target.mu.Unlock()
	if calls != 1 {
		t.Fatalf("provider calls = %d, want 1", calls)
	}
}

func TestSemanticHeartbeatBoundsCancellationIgnoringProviderWithoutOverlap(t *testing.T) {
	target := &heartbeatHarness{release: make(chan struct{}), ignoreCancellation: true}
	manager := newHeartbeatManager(t, target)
	manager.heartbeatTimeout = 10 * time.Millisecond
	started := time.Now()
	manager.runSemanticHeartbeatOnce(context.Background())
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancellation-ignoring provider blocked audit for %s", elapsed)
	}
	manager.runSemanticHeartbeatOnce(context.Background())
	target.mu.Lock()
	calls := target.calls
	target.mu.Unlock()
	if calls != 1 {
		t.Fatalf("stuck provider overlapped with %d calls", calls)
	}
	close(target.release)
}

func TestSemanticHeartbeatJobRemainsInspectableUntilProviderRelease(t *testing.T) {
	target := &heartbeatHarness{entered: make(chan struct{}, 1), release: make(chan struct{}), ignoreCancellation: true}
	manager := newHeartbeatManager(t, target)
	manager.heartbeatTimeout = 10 * time.Millisecond
	started := make(chan Lease, 1)
	finished := make(chan int, 1)
	manager.JobStarted = func(lease Lease, description string, _ time.Time, _, _ int) int {
		if description != "semantic workflow heartbeat" {
			t.Errorf("heartbeat job description = %q", description)
		}
		started <- lease
		return 17
	}
	manager.JobFinished = func(id int) { finished <- id }

	manager.runSemanticHeartbeatOnce(context.Background())
	select {
	case lease := <-started:
		if lease.DocumentType != "heartbeat" || lease.Route != "semantic-heartbeat" || lease.SessionKey != semanticHeartbeatSession || lease.State != "audit" || lease.Phase != "semantic_audit" {
			t.Fatalf("heartbeat job lease = %#v", lease)
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat job was not registered")
	}
	select {
	case id := <-finished:
		t.Fatalf("cancellation-ignoring heartbeat job %d disappeared before provider release", id)
	default:
	}
	close(target.release)
	select {
	case id := <-finished:
		if id != 17 {
			t.Fatalf("finished heartbeat job = %d", id)
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat job remained registered after provider release")
	}
}

func TestSemanticHeartbeatRejectsLateResultAfterPrimaryCancellation(t *testing.T) {
	target := &heartbeatHarness{entered: make(chan struct{}, 1), release: make(chan struct{}), ignoreCancellation: true}
	manager := newHeartbeatManager(t, target)
	parent, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		manager.runSemanticHeartbeatOnce(parent)
		close(done)
	}()
	select {
	case <-target.entered:
	case <-time.After(time.Second):
		t.Fatal("audit did not reach provider")
	}
	cancel()
	close(target.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled audit did not return after provider released")
	}
	if _, err := os.Stat(manager.Config.StatePath("runtime", "semantic-heartbeat.json")); !os.IsNotExist(err) {
		t.Fatalf("late result mutated heartbeat state: %v", err)
	}
	select {
	case <-manager.scanNow:
		t.Fatal("late result requested a recovery scan")
	default:
	}
}

func TestSemanticHeartbeatPrimaryHandoffFencesInFlightAudit(t *testing.T) {
	target := &heartbeatHarness{entered: make(chan struct{}, 1), release: make(chan struct{}), ignoreCancellation: true}
	manager := newHeartbeatManager(t, target)
	ticks := make(chan time.Time, 1)
	manager.heartbeatTicks = ticks
	ctx, cancel := context.WithCancel(context.Background())
	schedulerDone := make(chan struct{})
	go func() {
		manager.runSemanticHeartbeat(ctx)
		close(schedulerDone)
	}()
	ticks <- time.Now()
	select {
	case <-target.entered:
	case <-time.After(time.Second):
		t.Fatal("audit did not start under primary term")
	}
	cancel()
	select {
	case <-schedulerDone:
	case <-time.After(time.Second):
		t.Fatal("heartbeat scheduler did not stop at handoff")
	}
	close(target.release)
	manager.Wait()
	if _, err := os.Stat(manager.Config.StatePath("runtime", "semantic-heartbeat.json")); !os.IsNotExist(err) {
		t.Fatalf("superseded primary persisted late audit: %v", err)
	}
}

func TestSemanticHeartbeatProviderFailureLeavesOrchestrationUsable(t *testing.T) {
	manager := newHeartbeatManager(t, &heartbeatHarness{err: fmt.Errorf("provider unavailable")})
	manager.runSemanticHeartbeatOnce(context.Background())
	if _, err := os.Stat(manager.Config.StatePath("runtime", "semantic-heartbeat.json")); !os.IsNotExist(err) {
		t.Fatalf("provider failure persisted an audit result: %v", err)
	}
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatalf("ordinary scan failed after provider failure: %v", err)
	}
}

func TestSemanticHeartbeatRepairRequiresInScopeWorkflow(t *testing.T) {
	manager := newHeartbeatManager(t, &heartbeatHarness{})
	now := time.Now().UTC()
	result := semanticHeartbeatResult{Schema: semanticHeartbeatSchema, ExecutionID: "run", ObservedAt: now, Status: "findings", Findings: []semanticHeartbeatFinding{{
		Category: "stale_or_orphaned_claim", WorkflowID: "missing-workflow", Evidence: "stale lease", Action: "request_recover",
	}}}
	if err := manager.recordSemanticHeartbeatResult(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	select {
	case <-manager.scanNow:
		t.Fatal("out-of-scope repair requested a scan")
	default:
	}
	path, err := Create(manager.Config, "tasks", "in-scope repair", "")
	if err != nil {
		t.Fatal(err)
	}
	document, err := ReadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	result.ObservedAt = now.Add(time.Minute)
	result.Findings[0].WorkflowID = documentID(document)
	if err := manager.recordSemanticHeartbeatResult(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	select {
	case <-manager.scanNow:
	case <-time.After(time.Second):
		t.Fatal("in-scope repair did not request serialized scan")
	}
	journaled, err := ReadDocument(path)
	if err != nil || !strings.Contains(journaled.Body, "Semantic heartbeat recorded stale or orphaned claim and requested recover") {
		t.Fatalf("heartbeat repair request was not journaled: body=%q err=%v", journaled.Body, err)
	}
}

func TestSemanticHeartbeatRequeuesOnlyValidatedDueWaitingCondition(t *testing.T) {
	manager := newHeartbeatManager(t, &heartbeatHarness{})
	now := time.Date(2026, 8, 8, 20, 0, 0, 0, time.UTC)
	path, err := Create(manager.Config, "tasks", "waiting fallback", "")
	if err != nil {
		t.Fatal(err)
	}
	document, err := ReadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	document.FrontMatter["status"] = "waiting"
	document.FrontMatter["wake_at"] = now.Add(-time.Minute).Format(time.RFC3339)
	if err := WriteDocument(path, document); err != nil {
		t.Fatal(err)
	}
	waitingPath := filepath.Join(filepath.Dir(filepath.Dir(path)), "waiting", filepath.Base(path))
	if err := os.Rename(path, waitingPath); err != nil {
		t.Fatal(err)
	}
	result := semanticHeartbeatResult{Schema: semanticHeartbeatSchema, ExecutionID: "run", ObservedAt: now, Status: "findings", Findings: []semanticHeartbeatFinding{{
		Category: "due_waiting_condition", WorkflowID: documentID(document), Evidence: "documented fallback is due", Action: "request_requeue",
	}}}
	if err := manager.recordSemanticHeartbeatResult(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	select {
	case <-manager.scanNow:
	case <-time.After(time.Second):
		t.Fatal("validated due wait did not request a serialized scan")
	}
	journaled, err := ReadDocument(waitingPath)
	if err != nil || !strings.Contains(journaled.Body, "requested requeue") {
		t.Fatalf("due waiting requeue was not journaled: body=%q err=%v", journaled.Body, err)
	}

	delete(journaled.FrontMatter, "wake_at")
	if err := WriteDocument(waitingPath, journaled); err != nil {
		t.Fatal(err)
	}
	result.ObservedAt = now.Add(time.Minute)
	result.Findings[0].Evidence = "changed but unsupported waiting evidence"
	if err := manager.recordSemanticHeartbeatResult(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	select {
	case <-manager.scanNow:
		t.Fatal("waiting task without a due condition requested requeue")
	default:
	}
}

func TestSemanticHeartbeatAcceptsRealClockPrecisionAndPersistsDiagnostic(t *testing.T) {
	manager := newHeartbeatManager(t, &heartbeatHarness{})
	manager.heartbeatNow = func() time.Time {
		return time.Date(2026, 8, 8, 9, 0, 0, 987654321, time.UTC)
	}
	manager.runSemanticHeartbeatOnce(context.Background())
	data, err := os.ReadFile(manager.Config.StatePath("runtime", "semantic-heartbeat.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state semanticHeartbeatState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if state.LastAudit.Status != "healthy" || state.LastAudit.ObservedAt.Nanosecond() != 0 {
		t.Fatalf("last audit diagnostic = %#v", state.LastAudit)
	}
}

func TestSemanticHeartbeatNotificationDecisionUsesProgressInsteadOfResponseState(t *testing.T) {
	manager := newHeartbeatManager(t, &heartbeatHarness{})
	manager.AuthorizeNotificationOrigin = func(Origin) error { return nil }
	path, err := Create(manager.Config, "tasks", "needs a user decision", "")
	if err != nil {
		t.Fatal(err)
	}
	document, err := ReadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	workflowID := documentID(document)
	document.FrontMatter["status"] = "waiting"
	document.FrontMatter["notify"] = map[string]any{"enabled": true, "origin": "tui/local", "on": []any{"waiting"}}
	if err := WriteDocument(path, document); err != nil {
		t.Fatal(err)
	}
	waitingPath := filepath.Join(filepath.Dir(filepath.Dir(path)), "waiting", filepath.Base(path))
	if err := os.Rename(path, waitingPath); err != nil {
		t.Fatal(err)
	}
	deliveries := 0
	manager.Outbox.Deliver = func(context.Context, Origin, string, string) error {
		deliveries++
		return nil
	}
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	result := semanticHeartbeatResult{Schema: semanticHeartbeatSchema, ExecutionID: "run", ObservedAt: now, Status: "findings", Findings: []semanticHeartbeatFinding{{
		Category: "external_input_required", WorkflowID: workflowID, Evidence: "waiting condition needs a decision", Action: "notify", NotificationOrigin: "tui/local", Notification: "A task needs your decision.",
	}}}
	recordAndDeliverHeartbeat(t, manager, result)
	recordAndDeliverHeartbeat(t, manager, result)
	if deliveries != 1 {
		t.Fatalf("notification deliveries = %d, want 1", deliveries)
	}
	restarted := New(manager.Config, &heartbeatHarness{}, extensions.Runner{Directory: filepath.Join(manager.Config.Root, "missing")})
	restarted.AuthorizeNotificationOrigin = manager.AuthorizeNotificationOrigin
	restarted.Outbox.Deliver = manager.Outbox.Deliver
	recordAndDeliverHeartbeat(t, restarted, result)
	if deliveries != 1 {
		t.Fatalf("restart redelivered identical finding: %d", deliveries)
	}
	result.ExecutionID = "run-2"
	result.ObservedAt = now.Add(30 * time.Minute)
	recordAndDeliverHeartbeat(t, manager, result)
	if deliveries != 2 {
		t.Fatalf("new agent decision was suppressed by old response state: %d", deliveries)
	}
	journaled, err := ReadDocument(waitingPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(journaled.Body, "Semantic heartbeat queued this proactive notification: A task needs your decision.") != 2 {
		t.Fatalf("notification messages were not recorded in Progress:\n%s", journaled.Body)
	}
}

func TestSemanticHeartbeatRejectsOriginNotBoundToWorkflow(t *testing.T) {
	manager := newHeartbeatManager(t, &heartbeatHarness{})
	manager.AuthorizeNotificationOrigin = func(Origin) error { return nil }
	path, err := Create(manager.Config, "tasks", "private workflow", "")
	if err != nil {
		t.Fatal(err)
	}
	document, err := ReadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	workflowID := documentID(document)
	document.FrontMatter["notify"] = map[string]any{"enabled": true, "origin": "tui/authorized", "on": []any{"waiting"}}
	if err := WriteDocument(path, document); err != nil {
		t.Fatal(err)
	}
	deliveries := 0
	manager.Outbox.Deliver = func(context.Context, Origin, string, string) error {
		deliveries++
		return nil
	}
	result := semanticHeartbeatResult{Schema: semanticHeartbeatSchema, ExecutionID: "run", ObservedAt: time.Now().UTC(), Status: "findings", Findings: []semanticHeartbeatFinding{{
		Category: "external_input_required", WorkflowID: workflowID, Evidence: "needs a private decision", Action: "notify", NotificationOrigin: "tui/unrelated", Notification: "Private workflow details.",
	}}}
	if err := manager.recordSemanticHeartbeatResult(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 {
		t.Fatalf("unauthorized cross-conversation deliveries = %d", deliveries)
	}
}

func TestSemanticHeartbeatRequiresWaitingNotificationOutcome(t *testing.T) {
	manager := newHeartbeatManager(t, &heartbeatHarness{})
	manager.AuthorizeNotificationOrigin = func(Origin) error { return nil }
	path, err := Create(manager.Config, "tasks", "done-only notification", "")
	if err != nil {
		t.Fatal(err)
	}
	document, err := ReadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	document.FrontMatter["notify"] = map[string]any{"enabled": true, "origin": "tui/local", "on": []any{"done"}}
	if err := WriteDocument(path, document); err != nil {
		t.Fatal(err)
	}
	deliveries := 0
	manager.Outbox.Deliver = func(context.Context, Origin, string, string) error { deliveries++; return nil }
	result := semanticHeartbeatResult{Schema: semanticHeartbeatSchema, ExecutionID: "run", ObservedAt: time.Now().UTC(), Status: "findings", Findings: []semanticHeartbeatFinding{{
		Category: "external_input_required", WorkflowID: documentID(document), Evidence: "decision required", Action: "notify", NotificationOrigin: "tui/local", Notification: "A decision is required.",
	}}}
	if err := manager.recordSemanticHeartbeatResult(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 {
		t.Fatalf("done-only policy received waiting notification: %d", deliveries)
	}
}

func TestSemanticHeartbeatCancellationDuringAuthorizationCannotCommit(t *testing.T) {
	manager := newHeartbeatManager(t, &heartbeatHarness{})
	path, err := Create(manager.Config, "tasks", "cancel during authorization", "")
	if err != nil {
		t.Fatal(err)
	}
	document, err := ReadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	document.FrontMatter["status"] = "waiting"
	document.FrontMatter["notify"] = map[string]any{"enabled": true, "origin": "tui/local", "on": []any{"waiting"}}
	if err := WriteDocument(path, document); err != nil {
		t.Fatal(err)
	}
	waitingPath := filepath.Join(filepath.Dir(filepath.Dir(path)), "waiting", filepath.Base(path))
	if err := os.Rename(path, waitingPath); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	manager.AuthorizeNotificationOrigin = func(Origin) error {
		cancel()
		return nil
	}
	manager.Outbox.Deliver = func(context.Context, Origin, string, string) error {
		t.Fatal("cancelled result reached delivery")
		return nil
	}
	result := semanticHeartbeatResult{Schema: semanticHeartbeatSchema, ExecutionID: "run", ObservedAt: time.Now().UTC(), Status: "findings", Findings: []semanticHeartbeatFinding{{
		Category: "external_input_required", WorkflowID: documentID(document), Evidence: "decision required", Action: "notify", NotificationOrigin: "tui/local", Notification: "A decision is required.",
	}}}
	if err := manager.recordSemanticHeartbeatResult(ctx, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("commit error = %v, want context cancellation", err)
	}
	entries, err := os.ReadDir(manager.Config.StatePath("runtime", "outbox"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("cancelled commit created %d outbox entries", len(entries))
	}
	if _, err := os.Stat(manager.Config.StatePath("runtime", "semantic-heartbeat.json")); !os.IsNotExist(err) {
		t.Fatalf("cancelled commit persisted heartbeat state: %v", err)
	}
}

func TestSemanticHeartbeatBoundsRenderedPromptAndPersistedState(t *testing.T) {
	manager := newHeartbeatManager(t, &heartbeatHarness{})
	manager.Config.Orchestrator.Routes[0].Source = strings.Repeat("x", 9<<10)
	if _, err := manager.semanticHeartbeatPrompt("run", time.Now().UTC()); err == nil {
		t.Fatal("unbounded configured route was accepted")
	}

	statePath := manager.Config.StatePath("runtime", "semantic-heartbeat.json")
	if err := os.WriteFile(statePath, make([]byte, maxHeartbeatStateBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	result := semanticHeartbeatResult{Schema: semanticHeartbeatSchema, ExecutionID: "run", ObservedAt: time.Now().UTC(), Status: "healthy"}
	if err := manager.recordSemanticHeartbeatResult(context.Background(), result); err == nil {
		t.Fatal("oversized persisted state was accepted")
	}

	findings := make(map[string]semanticFindingState, maxHeartbeatStateEntries+10)
	for index := 0; index < maxHeartbeatStateEntries+10; index++ {
		findings[fmt.Sprintf("%04d", index)] = semanticFindingState{LastSeen: time.Unix(int64(index), 0)}
	}
	pruneSemanticFindingState(findings, maxHeartbeatStateEntries)
	if len(findings) != maxHeartbeatStateEntries {
		t.Fatalf("pruned finding count = %d", len(findings))
	}
	if _, exists := findings["0000"]; exists {
		t.Fatal("oldest finding was not pruned")
	}
}

func TestSemanticHeartbeatUsesWorkspacePromptOverride(t *testing.T) {
	manager := newHeartbeatManager(t, &heartbeatHarness{})
	path := manager.Config.StatePath("prompts", "heartbeat.md")
	override := "WORKSPACE HEARTBEAT {{EXECUTION_ID}} {{NOW_UTC}} {{ROUTES}}"
	if err := os.WriteFile(path, []byte(override), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.Config.StatePath("instructions", "agent-heartbeat.md"), []byte("heartbeat-only-rule"), 0o600); err != nil {
		t.Fatal(err)
	}
	prompt, err := manager.semanticHeartbeatPrompt("audit-7", time.Date(2026, 8, 8, 20, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"WORKSPACE HEARTBEAT", "audit-7", "2026-08-08T20:00:00Z", "tasks:", "Configured task review mode: skip-trivial"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("workspace heartbeat prompt omitted %q: %q", want, prompt)
		}
	}
	if !strings.Contains(prompt, "\nheartbeat-only-rule\n</workspace_owner_persistent_instructions>") || !strings.HasSuffix(prompt, "The precedence stated above still applies to every imported rule.") {
		t.Fatalf("heartbeat instructions were not the final prompt section: %q", prompt)
	}
}

func TestSemanticHeartbeatMessageUsesHeartbeatAgentPrefix(t *testing.T) {
	target := &heartbeatHarness{}
	manager := newHeartbeatManager(t, target)
	settings := manager.harnessSettings()
	settings.HeartbeatAgentPrefix = "/audit"
	manager.ApplyHarnessConfig(settings)
	manager.runSemanticHeartbeatOnce(context.Background())
	target.mu.Lock()
	prompt := target.prompt
	target.mu.Unlock()
	if !strings.HasPrefix(prompt, "/audit ") {
		t.Fatalf("heartbeat prompt = %q", prompt)
	}
}
