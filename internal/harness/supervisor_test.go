package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
)

type supervisorHarness struct {
	name             string
	startErr         error
	refuseSteer      bool
	interruptErr     error
	interruptEntered chan struct{}
	interruptRelease <-chan struct{}
	followUp         FollowUpMode
	mu               sync.Mutex
	active           map[string]bool
	emits            map[string]core.Emit
	prompts          map[string][]string
	closed           bool
	resetKeys        []string
	isActiveEntered  chan struct{}
	isActiveRelease  <-chan struct{}
	isActiveOnce     sync.Once
}

func waitSupervisorState(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for supervisor state")
}

func (r *supervisorHarness) FollowUpMode() FollowUpMode { return r.followUp }

func (r *supervisorHarness) Steer(_ context.Context, key, prompt string, emit core.Emit, beforeDelivery func() bool) (string, error) {
	if !r.IsActive(key) {
		return r.name + "-thread", errNativeTurnInactive
	}
	if beforeDelivery != nil && !beforeDelivery() {
		return r.name + "-thread", errNativeDeliveryUnreserved
	}
	r.mu.Lock()
	if r.refuseSteer {
		r.mu.Unlock()
		return r.name + "-thread", errors.New("active turns cannot be steered")
	}
	if r.prompts == nil {
		r.prompts = map[string][]string{}
	}
	r.prompts[key] = append(r.prompts[key], prompt)
	r.emits[key] = emit
	r.mu.Unlock()
	return r.name + "-thread", nil
}

func (r *supervisorHarness) Start(context.Context) error { return r.startErr }
func (r *supervisorHarness) Close() error {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	return nil
}
func (r *supervisorHarness) Send(_ context.Context, key, prompt string, emit core.Emit) (string, bool, error) {
	r.mu.Lock()
	if r.active[key] && r.refuseSteer {
		r.mu.Unlock()
		return r.name + "-thread", true, errors.New("active turns cannot be steered")
	}
	if r.prompts == nil {
		r.prompts = map[string][]string{}
	}
	r.prompts[key] = append(r.prompts[key], prompt)
	r.active[key] = true
	r.emits[key] = emit
	r.mu.Unlock()
	return r.name + "-thread", false, nil
}

func (r *supervisorHarness) finish(key string) {
	r.mu.Lock()
	emit := r.emits[key]
	delete(r.active, key)
	delete(r.emits, key)
	r.mu.Unlock()
	if emit != nil {
		emit(core.Event{Kind: core.EventFinal, Text: "done", Done: true})
	}
}
func (r *supervisorHarness) Interrupt(_ context.Context, key string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.interruptErr != nil {
		return false, r.interruptErr
	}
	if !r.active[key] {
		return false, nil
	}
	delete(r.active, key)
	if r.interruptEntered != nil {
		close(r.interruptEntered)
		<-r.interruptRelease
	}
	return true, nil
}

func TestSupervisorFailedInterruptRetainsActivityAndBlocksReconfigure(t *testing.T) {
	target := &supervisorHarness{name: "codex", followUp: FollowUpSteer, active: map[string]bool{}, emits: map[string]core.Emit{}, interruptErr: errors.New("interrupt transport failed")}
	replacement := &supervisorHarness{name: "claude-code", active: map[string]bool{}, emits: map[string]core.Emit{}}
	registry := NewRegistry()
	registry.Register("codex", func(HarnessConfig) (Harness, error) { return target, nil })
	registry.Register("claude-code", func(HarnessConfig) (Harness, error) { return replacement, nil })
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "codex"})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := supervisor.Send(context.Background(), "job", "original", nil); err != nil {
		t.Fatal(err)
	}
	if stopped, err := supervisor.Interrupt(context.Background(), "job"); err == nil || stopped {
		t.Fatalf("interrupt = stopped %t, err %v", stopped, err)
	}
	if !supervisor.IsActive("job") {
		t.Fatal("failed interrupt hid the still-active provider turn")
	}
	if err := supervisor.Reconfigure(HarnessConfig{Name: "claude-code"}); err == nil {
		t.Fatal("reconfigure replaced a harness after failed interrupt")
	}
}

func TestSupervisorSerializesSuccessorSendAfterInterruptCleanup(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	target := &supervisorHarness{name: "codex", followUp: FollowUpSteer, active: map[string]bool{}, emits: map[string]core.Emit{}, interruptEntered: entered, interruptRelease: release}
	registry := NewRegistry()
	registry.Register("codex", func(HarnessConfig) (Harness, error) { return target, nil })
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "codex"})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := supervisor.Send(context.Background(), "job", "old turn", nil); err != nil {
		t.Fatal(err)
	}
	interruptDone := make(chan error, 1)
	go func() { _, err := supervisor.Interrupt(context.Background(), "job"); interruptDone <- err }()
	<-entered
	sendDone := make(chan error, 1)
	go func() { _, _, err := supervisor.Send(context.Background(), "job", "successor", nil); sendDone <- err }()
	select {
	case err := <-sendDone:
		t.Fatalf("successor escaped interrupt fence: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-interruptDone; err != nil {
		t.Fatal(err)
	}
	if err := <-sendDone; err != nil {
		t.Fatal(err)
	}
	if !supervisor.IsActive("job") || !target.IsActive("job") {
		t.Fatalf("successor turn was hidden: supervisor=%t target=%t", supervisor.IsActive("job"), target.IsActive("job"))
	}
}
func (r *supervisorHarness) ResetSession(key string) error {
	r.mu.Lock()
	r.resetKeys = append(r.resetKeys, key)
	r.mu.Unlock()
	return nil
}
func (r *supervisorHarness) ThreadID(string) string { return r.name + "-thread" }
func (r *supervisorHarness) IsActive(key string) bool {
	if r.isActiveEntered != nil {
		r.isActiveOnce.Do(func() { close(r.isActiveEntered) })
		<-r.isActiveRelease
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active[key]
}

func TestSupervisorSelectionCannotRaceHarnessSwap(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	oldTarget := &supervisorHarness{name: "codex", active: map[string]bool{}, emits: map[string]core.Emit{}, isActiveEntered: entered, isActiveRelease: release}
	newTarget := &supervisorHarness{name: "claude-code", active: map[string]bool{}, emits: map[string]core.Emit{}}
	registry := NewRegistry()
	registry.Register("codex", func(HarnessConfig) (Harness, error) { return oldTarget, nil })
	registry.Register("claude-code", func(HarnessConfig) (Harness, error) { return newTarget, nil })
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "codex"})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	sent := make(chan error, 1)
	go func() {
		_, _, err := supervisor.Send(context.Background(), "chat", "work", nil)
		sent <- err
	}()
	<-entered
	swapped := make(chan error, 1)
	go func() { swapped <- supervisor.Reconfigure(HarnessConfig{Name: "claude-code"}) }()
	select {
	case err := <-swapped:
		close(release)
		<-sent
		t.Fatalf("harness swap escaped atomic target selection: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	if err := <-swapped; err == nil {
		t.Fatal("harness swap succeeded after the old target became active")
	}
	if oldTarget.closed {
		t.Fatal("active old target was closed")
	}
	oldTarget.finish("chat")
}

func TestSupervisorReconfiguresOnlyWhenIdle(t *testing.T) {
	registry := NewRegistry()
	created := map[string][]*supervisorHarness{}
	for _, name := range []string{"codex", "claude-code"} {
		name := name
		registry.Register(name, func(HarnessConfig) (Harness, error) {
			target := &supervisorHarness{name: name, active: map[string]bool{}, emits: map[string]core.Emit{}}
			created[name] = append(created[name], target)
			return target, nil
		})
	}
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "codex"})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if thread, _, err := supervisor.Send(context.Background(), "chat", "work", nil); err != nil || thread != "codex-thread" {
		t.Fatalf("Send() = %q, %v", thread, err)
	}
	if err := supervisor.Reconfigure(HarnessConfig{Name: "claude-code"}); err == nil {
		t.Fatal("active harness switch unexpectedly succeeded")
	}
	created["codex"][0].finish("chat")
	if err := supervisor.Reconfigure(HarnessConfig{Name: "claude-code"}); err != nil {
		t.Fatal(err)
	}
	if available, detail := supervisor.Available(); !available || detail != "" {
		t.Fatalf("Available() = %t, %q", available, detail)
	}
	if !created["codex"][0].closed {
		t.Fatal("previous harness was not closed after a successful swap")
	}
}

func TestSupervisorKeepsTurnActiveAcrossEmitterHandoff(t *testing.T) {
	target := &supervisorHarness{name: "codex", followUp: FollowUpSteer, active: map[string]bool{}, emits: map[string]core.Emit{}}
	registry := NewRegistry()
	registry.Register("codex", func(HarnessConfig) (Harness, error) { return target, nil })
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "codex"})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := supervisor.Send(context.Background(), "chat", "first", nil); err != nil {
		t.Fatal(err)
	}
	target.mu.Lock()
	firstEmit := target.emits["chat"]
	target.mu.Unlock()
	if _, _, err := supervisor.Send(context.Background(), "chat", "follow-up", nil); err != nil {
		t.Fatal(err)
	}
	firstEmit(core.Event{Kind: core.EventStatus, Done: true})
	if !supervisor.IsActive("chat") {
		t.Fatal("transport-only emitter completion ended the harness turn")
	}
	target.finish("chat")
	if supervisor.IsActive("chat") {
		t.Fatal("terminal event from the latest emitter left the harness turn active")
	}
}

func TestSupervisorQueuesFollowUpWhenHarnessCannotSteer(t *testing.T) {
	target := &supervisorHarness{
		name: "claude-code", refuseSteer: true, active: map[string]bool{}, emits: map[string]core.Emit{},
	}
	registry := NewRegistry()
	registry.Register("claude-code", func(HarnessConfig) (Harness, error) { return target, nil })
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "claude-code"})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	var eventsMu sync.Mutex
	var firstEvents []core.Event
	var secondEvents []core.Event
	if _, _, err := supervisor.Send(context.Background(), "chat", "first", func(event core.Event) {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		firstEvents = append(firstEvents, event)
	}); err != nil {
		t.Fatal(err)
	}
	followContext, cancelFollow := context.WithCancel(context.Background())
	if _, steered, err := supervisor.Send(followContext, "chat", "follow-up", func(event core.Event) {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		secondEvents = append(secondEvents, event)
	}); err != nil || !steered {
		t.Fatalf("queued follow-up = steered %t, error %v", steered, err)
	}
	// A queued message belongs to the durable conversation even if the
	// transport request that accepted it ends before the provider turn does.
	cancelFollow()
	target.finish("chat")
	waitSupervisorState(t, func() bool { return target.IsActive("chat") })
	if !supervisor.IsActive("chat") || !target.IsActive("chat") {
		t.Fatal("queued follow-up did not become the active turn")
	}
	eventsMu.Lock()
	firstContinues := len(firstEvents) > 0 && firstEvents[len(firstEvents)-1].Continues
	firstSnapshot := append([]core.Event(nil), firstEvents...)
	eventsMu.Unlock()
	if !firstContinues {
		t.Fatalf("first final did not preserve logical activity: %#v", firstSnapshot)
	}
	target.mu.Lock()
	prompts := append([]string(nil), target.prompts["chat"]...)
	target.mu.Unlock()
	if len(prompts) != 2 || prompts[0] != "first" || prompts[1] != "follow-up" {
		t.Fatalf("executed prompts = %#v", prompts)
	}
	target.finish("chat")
	if supervisor.IsActive("chat") || target.IsActive("chat") {
		t.Fatal("queued follow-up remained active after its final")
	}
	foundFinal := false
	eventsMu.Lock()
	for _, event := range secondEvents {
		if event.Kind == core.EventFinal && event.Done && !event.Continues {
			foundFinal = true
		}
	}
	secondSnapshot := append([]core.Event(nil), secondEvents...)
	eventsMu.Unlock()
	if !foundFinal {
		t.Fatalf("queued emitter did not receive terminal final: %#v", secondSnapshot)
	}
}

func TestSupervisorBatchesAccumulatedConversationFollowUps(t *testing.T) {
	target := &supervisorHarness{
		name: "acp", followUp: FollowUpQueue, active: map[string]bool{}, emits: map[string]core.Emit{},
	}
	registry := NewRegistry()
	registry.Register("acp", func(HarnessConfig) (Harness, error) { return target, nil })
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "acp"})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := supervisor.SendConversation(context.Background(), "chat", "initial prompt", "initial", nil); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var events [3][]core.Event
	for index, message := range []string{"first queued", "second queued", "third queued"} {
		index, message := index, message
		if _, queued, err := supervisor.SendConversation(context.Background(), "chat", "snapshot "+message, message, func(event core.Event) {
			mu.Lock()
			events[index] = append(events[index], event)
			mu.Unlock()
		}); err != nil || !queued {
			t.Fatalf("queue %d = steered %t, error %v", index, queued, err)
		}
	}
	target.finish("chat")
	waitSupervisorState(t, func() bool { return target.IsActive("chat") })
	target.mu.Lock()
	prompts := append([]string(nil), target.prompts["chat"]...)
	target.mu.Unlock()
	if len(prompts) != 2 {
		t.Fatalf("provider turns = %d, want initial plus one batch: %#v", len(prompts), prompts)
	}
	batch := prompts[1]
	if !strings.HasPrefix(batch, "snapshot third queued") {
		t.Fatalf("batch did not reuse newest conversation snapshot: %q", batch)
	}
	for index, message := range []string{"first queued", "second queued", "third queued"} {
		if !strings.Contains(batch, fmt.Sprintf("<queued_followup index=\"%d\">\n%s\n", index+1, message)) {
			t.Fatalf("batch lost ordered message %d: %q", index, batch)
		}
	}
	mu.Lock()
	for index := 0; index < 2; index++ {
		foundRelease := false
		for _, event := range events[index] {
			foundRelease = foundRelease || event.Kind == core.EventStatus && event.Done
		}
		if !foundRelease {
			mu.Unlock()
			t.Fatalf("queued emitter %d was not released: %#v", index, events[index])
		}
	}
	mu.Unlock()
	target.finish("chat")
	mu.Lock()
	latest := append([]core.Event(nil), events[2]...)
	mu.Unlock()
	foundFinal := false
	for _, event := range latest {
		foundFinal = foundFinal || event.Kind == core.EventFinal && event.Done && !event.Continues
	}
	if !foundFinal || supervisor.IsActive("chat") {
		t.Fatalf("batched final = %#v, active %t", latest, supervisor.IsActive("chat"))
	}
}

func TestQueuedConversationBatchCanAbsorbMessagesArrivingDuringHandoff(t *testing.T) {
	emit := func(core.Event) {}
	first, consumed := mergeQueuedConversationPrompts([]pendingSend{
		{prompt: "snapshot two", message: "one", emit: emit},
		{prompt: "snapshot two", message: "two", emit: emit},
	})
	if consumed != 2 || len(first.messages) != 2 || len(first.release) != 1 {
		t.Fatalf("initial queued batch = %#v, consumed %d", first, consumed)
	}
	combined, consumed := mergeQueuedConversationPrompts([]pendingSend{
		first,
		{prompt: "snapshot three", message: "three", emit: emit},
	})
	if consumed != 2 || len(combined.messages) != 3 || len(combined.release) != 2 {
		t.Fatalf("handoff batch = messages %#v releases %d consumed %d", combined.messages, len(combined.release), consumed)
	}
	for index, message := range []string{"one", "two", "three"} {
		if !strings.Contains(combined.prompt, fmt.Sprintf("<queued_followup index=\"%d\">\n%s\n", index+1, message)) {
			t.Fatalf("handoff batch lost message %d: %q", index, combined.prompt)
		}
	}
}

func TestSupervisorControlRetainsEmitterAndContinuesOnlyOnce(t *testing.T) {
	target := &supervisorHarness{name: "codex", followUp: FollowUpSteer, active: map[string]bool{}, emits: map[string]core.Emit{}}
	registry := NewRegistry()
	registry.Register("codex", func(HarnessConfig) (Harness, error) { return target, nil })
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "codex"})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	var stateMu sync.Mutex
	var events []core.Event
	if _, _, err := supervisor.Send(context.Background(), "job", "original", func(event core.Event) {
		stateMu.Lock()
		events = append(events, event)
		stateMu.Unlock()
	}); err != nil {
		t.Fatal(err)
	}
	prepared, reserved := 0, 0
	result, err := supervisor.SendControl(context.Background(), "job", ControlRequest{
		ID: "control-1", Prompt: "guidance", ContinuationPrompt: "continue original",
		PrepareContinuation: func() bool { stateMu.Lock(); prepared++; stateMu.Unlock(); return true },
		ReserveProviderTurn: func() bool { stateMu.Lock(); reserved++; stateMu.Unlock(); return true },
	})
	if err != nil || result.Queued || result.Duplicate {
		t.Fatalf("control result = %#v, %v", result, err)
	}
	target.finish("job")
	waitSupervisorState(t, func() bool { return target.IsActive("job") })
	stateMu.Lock()
	preparedSnapshot, reservedSnapshot := prepared, reserved
	stateMu.Unlock()
	if preparedSnapshot != 1 || reservedSnapshot != 2 || !supervisor.IsActive("job") || !target.IsActive("job") {
		t.Fatalf("continuation state: prepared=%d reserved=%d supervisor=%t target=%t", preparedSnapshot, reservedSnapshot, supervisor.IsActive("job"), target.IsActive("job"))
	}
	target.mu.Lock()
	prompts := append([]string(nil), target.prompts["job"]...)
	target.mu.Unlock()
	if strings.Join(prompts, ",") != "original,guidance,continue original" {
		t.Fatalf("prompts = %#v", prompts)
	}
	foundContinuingFinal := false
	stateMu.Lock()
	for _, event := range events {
		foundContinuingFinal = foundContinuingFinal || event.Kind == core.EventFinal && event.Continues
	}
	eventsSnapshot := append([]core.Event(nil), events...)
	stateMu.Unlock()
	if !foundContinuingFinal {
		t.Fatalf("control terminal did not preserve original execution: %#v", eventsSnapshot)
	}
	target.finish("job")
	stateMu.Lock()
	preparedSnapshot = prepared
	stateMu.Unlock()
	if preparedSnapshot != 1 || supervisor.IsActive("job") {
		t.Fatalf("continuation repeated: prepared=%d active=%t", preparedSnapshot, supervisor.IsActive("job"))
	}
}

func TestSupervisorQueuesControlsInOrderDeduplicatesAndBoundsBackpressure(t *testing.T) {
	target := &supervisorHarness{name: "claude-code", followUp: FollowUpQueue, active: map[string]bool{}, emits: map[string]core.Emit{}}
	registry := NewRegistry()
	registry.Register("claude-code", func(HarnessConfig) (Harness, error) { return target, nil })
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "claude-code"})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := supervisor.Send(context.Background(), "job", "original", nil); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < maxPendingControls; index++ {
		result, err := supervisor.SendControl(context.Background(), "job", ControlRequest{ID: fmt.Sprintf("id-%d", index), Prompt: fmt.Sprintf("control-%d", index)})
		if err != nil || !result.Queued {
			t.Fatalf("queue %d = %#v, %v", index, result, err)
		}
	}
	if _, err := supervisor.SendControl(context.Background(), "job", ControlRequest{ID: "overflow", Prompt: "overflow"}); err == nil || !strings.Contains(err.Error(), "queue is full") {
		t.Fatalf("overflow error = %v", err)
	}
	if result, err := supervisor.SendControl(context.Background(), "job", ControlRequest{ID: "id-0", Prompt: "control-0"}); err != nil || !result.Duplicate {
		t.Fatalf("duplicate = %#v, %v", result, err)
	}
	for index := 0; index <= maxPendingControls; index++ {
		target.finish("job")
		if index < maxPendingControls {
			waitSupervisorState(t, func() bool { return target.IsActive("job") })
		}
	}
	waitSupervisorState(t, func() bool { return !supervisor.IsActive("job") })
	target.mu.Lock()
	prompts := append([]string(nil), target.prompts["job"]...)
	target.mu.Unlock()
	if len(prompts) != maxPendingControls+1 {
		t.Fatalf("prompt count = %d: %#v", len(prompts), prompts)
	}
	for index := 0; index < maxPendingControls; index++ {
		if prompts[index+1] != fmt.Sprintf("control-%d", index) {
			t.Fatalf("ordered prompts = %#v", prompts)
		}
	}
}

func TestSupervisorDropsQueuedControlAfterDurableTransition(t *testing.T) {
	target := &supervisorHarness{name: "claude-code", followUp: FollowUpQueue, active: map[string]bool{}, emits: map[string]core.Emit{}}
	registry := NewRegistry()
	registry.Register("claude-code", func(HarnessConfig) (Harness, error) { return target, nil })
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "claude-code"})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := supervisor.Send(context.Background(), "job", "original", nil); err != nil {
		t.Fatal(err)
	}
	valid := true
	if result, err := supervisor.SendControl(context.Background(), "job", ControlRequest{ID: "transition", Prompt: "stale guidance", Validate: func() bool { return valid }}); err != nil || !result.Queued {
		t.Fatalf("queue result = %#v, %v", result, err)
	}
	valid = false
	target.finish("job")
	waitSupervisorState(t, func() bool { return !supervisor.IsActive("job") })
	target.mu.Lock()
	prompts := append([]string(nil), target.prompts["job"]...)
	target.mu.Unlock()
	if len(prompts) != 1 || supervisor.IsActive("job") {
		t.Fatalf("stale queued control executed: prompts=%#v active=%t", prompts, supervisor.IsActive("job"))
	}
}

func TestSupervisorNativeControlNeverStartsTurnAfterCompletionRace(t *testing.T) {
	target := &supervisorHarness{name: "codex", followUp: FollowUpSteer, active: map[string]bool{}, emits: map[string]core.Emit{}}
	registry := NewRegistry()
	registry.Register("codex", func(HarnessConfig) (Harness, error) { return target, nil })
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "codex"})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := supervisor.Send(context.Background(), "job", "original", nil); err != nil {
		t.Fatal(err)
	}
	_, err := supervisor.SendControl(context.Background(), "job", ControlRequest{
		ID: "completion-race", Prompt: "must not become a new turn",
		Validate: func() bool {
			target.finish("job")
			return true
		},
	})
	if err == nil || (!strings.Contains(err.Error(), "no longer active") && !strings.Contains(err.Error(), "completed before control delivery")) {
		t.Fatalf("completion race error = %v", err)
	}
	target.mu.Lock()
	prompts := append([]string(nil), target.prompts["job"]...)
	target.mu.Unlock()
	if len(prompts) != 1 || prompts[0] != "original" {
		t.Fatalf("control started a new turn after completion: %#v", prompts)
	}
}

func TestSupervisorDoesNotReserveNativeControlAfterCompletionWins(t *testing.T) {
	target := &supervisorHarness{name: "codex", followUp: FollowUpSteer, active: map[string]bool{}, emits: map[string]core.Emit{}}
	registry := NewRegistry()
	registry.Register("codex", func(HarnessConfig) (Harness, error) { return target, nil })
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "codex"})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := supervisor.Send(context.Background(), "job", "original", nil); err != nil {
		t.Fatal(err)
	}
	reserved := 0
	_, err := supervisor.SendControl(context.Background(), "job", ControlRequest{
		ID: "completion-before-reservation", Prompt: "must not be counted",
		Validate: func() bool {
			target.finish("job")
			return true
		},
		ReserveProviderTurn: func() bool { reserved++; return true },
	})
	if err == nil || reserved != 0 {
		t.Fatalf("completion race = err %v, reserved %d; want rejection without reservation", err, reserved)
	}
}

func TestSupervisorNativeReservationCommitsDeliveryWhenCompletionRaces(t *testing.T) {
	target := &supervisorHarness{name: "codex", followUp: FollowUpSteer, active: map[string]bool{}, emits: map[string]core.Emit{}}
	registry := NewRegistry()
	registry.Register("codex", func(HarnessConfig) (Harness, error) { return target, nil })
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "codex"})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := supervisor.Send(context.Background(), "job", "original", nil); err != nil {
		t.Fatal(err)
	}
	reserved := 0
	result, err := supervisor.SendControl(context.Background(), "job", ControlRequest{
		ID: "completion-during-reservation", Prompt: "counted control",
		ReserveProviderTurn: func() bool {
			reserved++
			target.finish("job")
			return true
		},
	})
	if err != nil || result.Queued || result.Duplicate {
		t.Fatalf("control = %#v, %v", result, err)
	}
	target.mu.Lock()
	prompts := append([]string(nil), target.prompts["job"]...)
	target.mu.Unlock()
	if reserved != 1 || strings.Join(prompts, ",") != "original,counted control" {
		t.Fatalf("reserved=%d prompts=%#v", reserved, prompts)
	}
}

func TestSupervisorDeduplicatesRetryAfterAmbiguousNativeSteerError(t *testing.T) {
	target := &supervisorHarness{name: "codex", followUp: FollowUpSteer, refuseSteer: true, active: map[string]bool{}, emits: map[string]core.Emit{}}
	registry := NewRegistry()
	registry.Register("codex", func(HarnessConfig) (Harness, error) { return target, nil })
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "codex"})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	target.refuseSteer = false
	if _, _, err := supervisor.Send(context.Background(), "job", "original", nil); err != nil {
		t.Fatal(err)
	}
	target.refuseSteer = true
	request := ControlRequest{ID: "ambiguous", Prompt: "possibly accepted"}
	if _, err := supervisor.SendControl(context.Background(), "job", request); err == nil {
		t.Fatal("ambiguous steer unexpectedly succeeded")
	}
	if result, err := supervisor.SendControl(context.Background(), "job", request); err != nil || !result.Duplicate {
		t.Fatalf("retry = %#v, %v", result, err)
	}
}

func TestSupervisorFencesQueuedStartWhenCancellationWinsRevalidation(t *testing.T) {
	target := &supervisorHarness{name: "claude-code", followUp: FollowUpQueue, active: map[string]bool{}, emits: map[string]core.Emit{}}
	registry := NewRegistry()
	registry.Register("claude-code", func(HarnessConfig) (Harness, error) { return target, nil })
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "claude-code"})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := supervisor.Send(context.Background(), "job", "original", nil); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	cancelled := false
	if result, err := supervisor.SendControl(context.Background(), "job", ControlRequest{
		ID: "cancel-race", Prompt: "must not start", Validate: func() bool { return true },
		PrepareContinuation: func() bool {
			close(entered)
			<-release
			return !cancelled
		},
	}); err != nil || !result.Queued {
		t.Fatalf("queue result = %#v, %v", result, err)
	}
	target.finish("job")
	<-entered
	cancelled = true
	interrupted := make(chan bool, 1)
	go func() {
		stopped, _ := supervisor.Interrupt(context.Background(), "job")
		interrupted <- stopped
	}()
	close(release)
	<-interrupted
	target.mu.Lock()
	prompts := append([]string(nil), target.prompts["job"]...)
	target.mu.Unlock()
	if len(prompts) != 1 {
		t.Fatalf("queued control started after cancellation: %#v", prompts)
	}
}

func TestSupervisorPreparesEachQueuedControlTurnAfterTerminal(t *testing.T) {
	target := &supervisorHarness{name: "claude-code", followUp: FollowUpQueue, active: map[string]bool{}, emits: map[string]core.Emit{}}
	registry := NewRegistry()
	registry.Register("claude-code", func(HarnessConfig) (Harness, error) { return target, nil })
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "claude-code"})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := supervisor.Send(context.Background(), "job", "original", nil); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	prepared, reserved := 0, 0
	prepare := func() bool { mu.Lock(); prepared++; mu.Unlock(); return true }
	reserve := func() bool { mu.Lock(); reserved++; mu.Unlock(); return true }
	for _, id := range []string{"one", "two"} {
		if result, err := supervisor.SendControl(context.Background(), "job", ControlRequest{ID: id, Prompt: id, Validate: func() bool { return true }, PrepareContinuation: prepare, ReserveProviderTurn: reserve}); err != nil || !result.Queued {
			t.Fatalf("queue %s = %#v, %v", id, result, err)
		}
	}
	for index := 0; index < 2; index++ {
		target.finish("job")
		waitSupervisorState(t, func() bool { return target.IsActive("job") })
	}
	mu.Lock()
	count, reservationCount := prepared, reserved
	mu.Unlock()
	if count != 2 || reservationCount != 2 {
		t.Fatalf("prepared/reserved queued turns = %d/%d, want 2/2", count, reservationCount)
	}
	target.finish("job")
}

func TestSupervisorKeepsPreviousHarnessWhenReplacementFails(t *testing.T) {
	registry := NewRegistry()
	registry.Register("codex", func(HarnessConfig) (Harness, error) {
		return &supervisorHarness{name: "codex", active: map[string]bool{}, emits: map[string]core.Emit{}}, nil
	})
	registry.Register("broken", func(HarnessConfig) (Harness, error) {
		return &supervisorHarness{name: "broken", startErr: errors.New("not installed"), active: map[string]bool{}, emits: map[string]core.Emit{}}, nil
	})
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "codex"})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Reconfigure(HarnessConfig{Name: "broken"}); err == nil {
		t.Fatal("broken replacement unexpectedly succeeded")
	}
	if got := supervisor.HarnessConfig().Name; got != "codex" {
		t.Fatalf("active harness = %q, want codex", got)
	}
}

func TestSupervisorRecordsUnavailableSelectionOnlyWithoutWorkingHarness(t *testing.T) {
	registry := NewRegistry()
	registry.Register("codex", func(HarnessConfig) (Harness, error) {
		return &supervisorHarness{name: "codex", active: map[string]bool{}, emits: map[string]core.Emit{}}, nil
	})
	missing := errors.New("Claude Code is not installed")
	empty := NewSupervisor(registry, HarnessConfig{})
	if err := empty.Start(context.Background()); err == nil {
		t.Fatal("empty harness unexpectedly started")
	}
	if err := empty.ConfigureUnavailable(HarnessConfig{Name: "claude-code", Command: "claude"}, missing); err != nil {
		t.Fatal(err)
	}
	if empty.HarnessConfig().Name != "claude-code" {
		t.Fatalf("unavailable selection was not recorded: %#v", empty.HarnessConfig())
	}
	if ready, detail := empty.Available(); ready || !strings.Contains(detail, "not installed") {
		t.Fatalf("unavailable state = %t, %q", ready, detail)
	}

	working := NewSupervisor(registry, HarnessConfig{Name: "codex"})
	if err := working.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := working.ConfigureUnavailable(HarnessConfig{Name: "claude-code"}, missing); !errors.Is(err, missing) {
		t.Fatalf("working harness accepted unavailable replacement: %v", err)
	}
	if working.HarnessConfig().Name != "codex" {
		t.Fatalf("working harness selection changed: %#v", working.HarnessConfig())
	}
}

func TestUnavailableSupervisorStillClearsDurableAdapterSession(t *testing.T) {
	registry := NewRegistry()
	failed := &supervisorHarness{name: "codex", startErr: errors.New("missing executable"), active: map[string]bool{}, emits: map[string]core.Emit{}}
	resetter := &supervisorHarness{name: "codex", active: map[string]bool{}, emits: map[string]core.Emit{}}
	created := 0
	registry.Register("codex", func(HarnessConfig) (Harness, error) {
		created++
		if created == 1 {
			return failed, nil
		}
		return resetter, nil
	})
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "codex"})
	if err := supervisor.Start(context.Background()); err == nil {
		t.Fatal("unavailable harness unexpectedly started")
	}
	if err := supervisor.ResetSession("chat:tui:local"); err != nil {
		t.Fatal(err)
	}
	resetter.mu.Lock()
	defer resetter.mu.Unlock()
	if len(resetter.resetKeys) != 1 || resetter.resetKeys[0] != "chat:tui:local" || !resetter.closed {
		t.Fatalf("durable reset adapter = keys %#v, closed %t", resetter.resetKeys, resetter.closed)
	}
}
