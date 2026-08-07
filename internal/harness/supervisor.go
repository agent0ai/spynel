package harness

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/frdel/spynel/internal/core"
)

// Supervisor keeps the rest of Spynel provider-neutral while allowing the
// selected harness to be replaced at runtime. It only swaps an idle harness;
// active turns retain their original process and session semantics.
type Supervisor struct {
	registry *Registry

	operationMu sync.Mutex
	mu          sync.RWMutex
	ctx         context.Context
	config      HarnessConfig
	current     Harness
	startErr    error
	active      map[string]int
	ready       chan struct{}
	closed      bool
}

func NewSupervisor(registry *Registry, cfg HarnessConfig) *Supervisor {
	return &Supervisor{registry: registry, config: cfg, active: map[string]int{}, ready: make(chan struct{}, 1)}
}

func (s *Supervisor) HarnessConfig() HarnessConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config
}

func (s *Supervisor) Start(ctx context.Context) error {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("harness supervisor is closed")
	}
	if s.current != nil {
		s.mu.Unlock()
		return nil
	}
	s.ctx = ctx
	cfg := s.config
	s.mu.Unlock()

	target, err := s.startTarget(ctx, cfg)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.startErr = err
		return err
	}
	s.current = target
	s.startErr = nil
	s.signalReady()
	return nil
}

func (s *Supervisor) startTarget(ctx context.Context, cfg HarnessConfig) (Harness, error) {
	target, err := s.registry.Create(cfg)
	if err != nil {
		return nil, err
	}
	if err := target.Start(ctx); err != nil {
		_ = target.Close()
		return nil, err
	}
	return target, nil
}

// Reconfigure validates a new harness by starting it before replacing the old
// one. This makes configuration transactional from the user's perspective.
func (s *Supervisor) Reconfigure(cfg HarnessConfig) error {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return errors.New("harness supervisor is closed")
	}
	if len(s.active) > 0 {
		s.mu.RUnlock()
		return errors.New("cannot change the harness while a harness turn is active; use /stop or wait for completion")
	}
	ctx := s.ctx
	s.mu.RUnlock()
	if ctx == nil {
		// The service has not started yet; construction will validate it later.
		s.mu.Lock()
		s.config = cfg
		s.mu.Unlock()
		return nil
	}

	target, err := s.startTarget(ctx, cfg)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed || len(s.active) > 0 {
		s.mu.Unlock()
		_ = target.Close()
		return errors.New("harness became busy while applying configuration")
	}
	previous := s.current
	s.current = target
	s.config = cfg
	s.startErr = nil
	s.mu.Unlock()
	s.signalReady()
	if previous != nil {
		_ = previous.Close()
	}
	return nil
}

// ConfigureUnavailable records a user's selection when Spynel has no running
// harness yet and the executable is not installed. This lets onboarding save
// intent and remain usable; it never replaces a working harness with a broken
// selection.
func (s *Supervisor) ConfigureUnavailable(cfg HarnessConfig, cause error) error {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("harness supervisor is closed")
	}
	if len(s.active) > 0 {
		return errors.New("cannot change the harness while a harness turn is active; use /stop or wait for completion")
	}
	if s.current != nil {
		return cause
	}
	s.config = cfg
	s.startErr = cause
	return nil
}

func (s *Supervisor) Available() (bool, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.current != nil {
		return true, ""
	}
	if s.startErr != nil {
		return false, s.startErr.Error()
	}
	return false, "harness is not started"
}

func (s *Supervisor) ReadyEvents() <-chan struct{} { return s.ready }

func (s *Supervisor) signalReady() {
	select {
	case s.ready <- struct{}{}:
	default:
	}
}

func (s *Supervisor) Models(ctx context.Context) ([]Model, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	target, err := s.targetLocked()
	if err != nil {
		return nil, err
	}
	provider, ok := target.(ModelProvider)
	if !ok {
		return nil, errors.New("the active harness does not provide a model catalog")
	}
	return provider.Models(ctx)
}

func (s *Supervisor) target() (Harness, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.targetLocked()
}

func (s *Supervisor) targetLocked() (Harness, error) {
	if s.current != nil {
		return s.current, nil
	}
	if s.startErr != nil {
		return nil, fmt.Errorf("harness unavailable: %w", s.startErr)
	}
	return nil, errors.New("harness is not started")
}

func (s *Supervisor) Send(ctx context.Context, key, prompt string, emit core.Emit) (string, bool, error) {
	// Selecting the target and marking a new turn active must be atomic with
	// respect to Reconfigure. Otherwise a swap could close the old harness in
	// the narrow window after target selection but before active bookkeeping.
	s.mu.Lock()
	target, err := s.targetLocked()
	if err != nil {
		s.mu.Unlock()
		return "", false, err
	}
	if target.IsActive(key) {
		s.mu.Unlock()
		return target.Send(ctx, key, prompt, emit)
	}
	s.active[key] = 1
	s.mu.Unlock()
	var once sync.Once
	finish := func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.active, key)
			s.mu.Unlock()
		})
	}
	wrapper := func(event core.Event) {
		if emit != nil {
			emit(event)
		}
		if event.Done {
			finish()
		}
	}
	threadID, steered, err := target.Send(ctx, key, prompt, wrapper)
	if err != nil {
		finish()
	}
	return threadID, steered, err
}

func (s *Supervisor) Interrupt(ctx context.Context, key string) (bool, error) {
	target, err := s.target()
	if err != nil {
		return false, err
	}
	return target.Interrupt(ctx, key)
}

func (s *Supervisor) ResetSession(key string) error {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	s.mu.RLock()
	target := s.current
	cfg := s.config
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return errors.New("harness supervisor is closed")
	}
	if target != nil {
		return target.ResetSession(key)
	}
	// A missing executable can leave the harness unavailable while the user is
	// still able to run /clear in the TUI. Constructing the adapter does not
	// start its subprocess, but does load its durable session map, so resetting
	// through it prevents an old thread from reappearing after configuration is
	// repaired or the process restarts.
	target, err := s.registry.Create(cfg)
	if err != nil {
		return err
	}
	defer target.Close()
	return target.ResetSession(key)
}

func (s *Supervisor) ThreadID(key string) string {
	target, err := s.target()
	if err != nil {
		return ""
	}
	return target.ThreadID(key)
}

func (s *Supervisor) IsActive(key string) bool {
	s.mu.RLock()
	active := s.active[key] > 0
	s.mu.RUnlock()
	return active
}

func (s *Supervisor) Close() error {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	target := s.current
	s.current = nil
	s.mu.Unlock()
	if target != nil {
		return target.Close()
	}
	return nil
}
