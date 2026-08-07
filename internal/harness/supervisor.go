package harness

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/agent0ai/spynel/internal/core"
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
	pending     map[string][]pendingSend
	ready       chan struct{}
	closed      bool
}

type pendingSend struct {
	prompt string
	emit   core.Emit
}

func NewSupervisor(registry *Registry, cfg HarnessConfig) *Supervisor {
	return &Supervisor{
		registry: registry, config: cfg, active: map[string]int{}, pending: map[string][]pendingSend{},
		ready: make(chan struct{}, 1),
	}
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
	wasActive := target.IsActive(key)
	if wasActive && followUpMode(target) == FollowUpQueue {
		threadID := target.ThreadID(key)
		s.pending[key] = append(s.pending[key], pendingSend{prompt: prompt, emit: emit})
		s.mu.Unlock()
		if emit != nil {
			emit(core.Event{Kind: core.EventStatus, Text: "Follow-up queued behind the active harness turn", ThreadID: threadID})
		}
		return threadID, true, nil
	}
	if !wasActive {
		s.active[key] = 1
	}
	s.mu.Unlock()
	threadID, steered, err := target.Send(ctx, key, prompt, s.executionEmit(key, target, emit))
	if err != nil {
		if wasActive && steered {
			// The active turn finished during the failed steering attempt. Retry
			// as the next ordinary turn instead of losing the follow-up.
			if !target.IsActive(key) {
				return s.Send(ctx, key, prompt, emit)
			}
			return threadID, steered, err
		}
		if !wasActive {
			s.mu.Lock()
			delete(s.active, key)
			s.mu.Unlock()
		}
	}
	return threadID, steered, err
}

func followUpMode(target Harness) FollowUpMode {
	provider, ok := target.(FollowUpProvider)
	if !ok || provider.FollowUpMode() != FollowUpSteer {
		return FollowUpQueue
	}
	return FollowUpSteer
}

func (s *Supervisor) executionEmit(key string, target Harness, emit core.Emit) core.Emit {
	return func(event core.Event) {
		var next *pendingSend
		if event.Done && (event.Kind == core.EventFinal || event.Kind == core.EventError) {
			s.mu.Lock()
			queue := s.pending[key]
			if len(queue) > 0 {
				value := queue[0]
				next = &value
				if len(queue) == 1 {
					delete(s.pending, key)
				} else {
					s.pending[key] = queue[1:]
				}
				event.Continues = true
			} else {
				delete(s.active, key)
			}
			s.mu.Unlock()
		}
		if emit != nil {
			emit(event)
		}
		if next != nil {
			s.startQueued(key, target, *next)
		}
	}
}

func (s *Supervisor) startQueued(key string, target Harness, next pendingSend) {
	wrapper := s.executionEmit(key, target, next.emit)
	s.mu.RLock()
	ctx := s.ctx
	closed := s.closed
	s.mu.RUnlock()
	if closed || ctx == nil {
		wrapper(core.Event{Kind: core.EventError, Text: "queued follow-up failed: harness supervisor is closed", Done: true})
		return
	}
	threadID, _, err := target.Send(ctx, key, next.prompt, wrapper)
	if err != nil {
		wrapper(core.Event{Kind: core.EventError, Text: "queued follow-up failed: " + err.Error(), ThreadID: threadID, Done: true})
		return
	}
	if next.emit != nil && target.IsActive(key) {
		next.emit(core.Event{Kind: core.EventStatus, Text: "Queued follow-up started", ThreadID: threadID})
	}
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
