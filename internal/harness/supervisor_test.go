package harness

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frdel/spynel/internal/core"
)

type supervisorHarness struct {
	name            string
	startErr        error
	mu              sync.Mutex
	active          map[string]bool
	emits           map[string]core.Emit
	closed          bool
	resetKeys       []string
	isActiveEntered chan struct{}
	isActiveRelease <-chan struct{}
	isActiveOnce    sync.Once
}

func (r *supervisorHarness) Start(context.Context) error { return r.startErr }
func (r *supervisorHarness) Close() error {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	return nil
}
func (r *supervisorHarness) Send(_ context.Context, key, _ string, emit core.Emit) (string, bool, error) {
	r.mu.Lock()
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
	if !r.active[key] {
		return false, nil
	}
	delete(r.active, key)
	return true, nil
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
