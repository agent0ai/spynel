package harness

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agent0ai/spynel/internal/core"
)

const (
	maxPendingControls  = 8
	controlDedupeWindow = time.Minute
)

// Supervisor keeps the rest of Spynel provider-neutral while allowing the
// selected harness to be replaced at runtime. It only swaps an idle harness;
// active turns retain their original process and session semantics.
type Supervisor struct {
	registry *Registry

	operationMu       sync.Mutex
	mu                sync.RWMutex
	ctx               context.Context
	config            HarnessConfig
	current           Harness
	startErr          error
	active            map[string]int
	pending           map[string][]pendingSend
	controlEmit       map[string]core.Emit
	controls          map[string]*controlState
	seenControl       map[string]map[string]time.Time
	controlGeneration map[string]uint64
	controlOpsMu      sync.Mutex
	controlOps        map[string]*sync.Mutex
	ready             chan struct{}
	closed            bool
}

type pendingSend struct {
	prompt          string
	message         string
	messages        []string
	emit            core.Emit
	control         *controlState
	preserveEmitter bool
	generation      uint64
	release         []core.Emit
}

type controlState struct {
	id                  string
	continuationPrompt  string
	validate            func() bool
	prepareContinuation func() bool
	reserveProviderTurn func() bool
	continued           bool
}

func NewSupervisor(registry *Registry, cfg HarnessConfig) *Supervisor {
	return &Supervisor{
		registry: registry, config: cfg, active: map[string]int{}, pending: map[string][]pendingSend{},
		controlEmit: map[string]core.Emit{}, controls: map[string]*controlState{}, seenControl: map[string]map[string]time.Time{},
		controlGeneration: map[string]uint64{}, controlOps: map[string]*sync.Mutex{},
		ready: make(chan struct{}, 1),
	}
}

func (s *Supervisor) HarnessConfig() HarnessConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config
}

// CommitModel atomically orders persistence and future dispatch snapshots.
// It deliberately does not replace the running harness: already-admitted work
// retains the model captured by its dispatch, while snapshots taken after the
// commit returns observe model.
func (s *Supervisor) CommitModel(model string, commit func() error) error {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("harness supervisor is closed")
	}
	if s.current != nil {
		if _, ok := s.current.(ModelDispatcher); !ok {
			return errors.New("the active harness does not support forward-looking model changes")
		}
	}
	if commit == nil {
		return errors.New("model configuration commit is unavailable")
	}
	if err := commit(); err != nil {
		return err
	}
	s.config.Model = model
	if target, ok := s.current.(ModelDispatcher); ok {
		target.SetModel(model)
	}
	return nil
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
	return s.send(ctx, key, prompt, "", emit)
}

// SendConversation gives the supervisor the raw user message needed for
// lossless batching. When no queue forms it is identical to Send.
func (s *Supervisor) SendConversation(ctx context.Context, key, prompt, message string, emit core.Emit) (string, bool, error) {
	return s.send(ctx, key, prompt, message, emit)
}

// ConversationAdmission reports the provider-neutral follow-up mode visible
// at the admission fence. The send path still rechecks under the same session
// operation lock before delivery.
func (s *Supervisor) ConversationAdmission(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	target, err := s.targetLocked()
	if err != nil || s.active[key] == 0 && !target.IsActive(key) {
		return "new"
	}
	if followUpMode(target) == FollowUpQueue {
		return "queued"
	}
	return "steered"
}

func (s *Supervisor) send(ctx context.Context, key, prompt, message string, emit core.Emit) (string, bool, error) {
	operation := s.controlOperation(key)
	operation.Lock()
	// Selecting the target and marking a new turn active must be atomic with
	// respect to Reconfigure. Otherwise a swap could close the old harness in
	// the narrow window after target selection but before active bookkeeping.
	s.mu.Lock()
	target, err := s.targetLocked()
	if err != nil {
		s.mu.Unlock()
		operation.Unlock()
		return "", false, err
	}
	wasActive := target.IsActive(key)
	model := s.config.Model
	logicalActive := s.active[key] > 0
	if (!wasActive && logicalActive) || (wasActive && followUpMode(target) == FollowUpQueue) {
		threadID := target.ThreadID(key)
		s.pending[key] = append(s.pending[key], pendingSend{prompt: prompt, message: message, emit: emit})
		s.mu.Unlock()
		operation.Unlock()
		if emit != nil {
			emit(core.Event{Kind: core.EventStatus, Text: "Follow-up queued behind the active harness turn", ThreadID: threadID})
		}
		return threadID, true, nil
	}
	if !wasActive {
		s.active[key] = 1
	}
	wrapper := s.executionEmit(key, target, emit)
	s.controlEmit[key] = wrapper
	s.mu.Unlock()
	threadID, steered, err := sendWithModel(target, ctx, key, prompt, model, wrapper)
	if err != nil {
		if wasActive && steered {
			// The active turn finished during the failed steering attempt. Retry
			// as the next ordinary turn instead of losing the follow-up.
			if !target.IsActive(key) {
				operation.Unlock()
				return s.send(ctx, key, prompt, message, emit)
			}
			operation.Unlock()
			return threadID, steered, err
		}
		if !wasActive {
			s.mu.Lock()
			delete(s.active, key)
			delete(s.controlEmit, key)
			s.mu.Unlock()
		}
	}
	operation.Unlock()
	return threadID, steered, err
}

// SendControl delivers guidance to an existing execution without changing its
// emitter. Queueing and retry deduplication are bounded per session.
func (s *Supervisor) SendControl(ctx context.Context, key string, request ControlRequest) (ControlResult, error) {
	if request.ID == "" || request.Prompt == "" {
		return ControlResult{}, errors.New("invalid empty control request")
	}
	s.mu.Lock()
	target, err := s.targetLocked()
	if err != nil {
		s.mu.Unlock()
		return ControlResult{}, err
	}
	if s.active[key] == 0 || !target.IsActive(key) {
		s.mu.Unlock()
		return ControlResult{}, errors.New("job provider turn is no longer active or steerable")
	}
	now := time.Now()
	seen := s.seenControl[key]
	if seen == nil {
		seen = map[string]time.Time{}
		s.seenControl[key] = seen
	}
	for id, at := range seen {
		if now.Sub(at) > controlDedupeWindow {
			delete(seen, id)
		}
	}
	if _, exists := seen[request.ID]; exists {
		s.mu.Unlock()
		return ControlResult{Duplicate: true}, nil
	}
	state := &controlState{id: request.ID, continuationPrompt: request.ContinuationPrompt, validate: request.Validate, prepareContinuation: request.PrepareContinuation, reserveProviderTurn: request.ReserveProviderTurn}
	owner := s.controlEmit[key]
	if owner == nil {
		s.mu.Unlock()
		return ControlResult{}, errors.New("job execution emitter is unavailable")
	}
	seen[request.ID] = now
	if followUpMode(target) == FollowUpQueue {
		if len(s.pending[key]) >= maxPendingControls {
			delete(seen, request.ID)
			s.mu.Unlock()
			return ControlResult{}, fmt.Errorf("job control queue is full (maximum %d messages)", maxPendingControls)
		}
		s.pending[key] = append(s.pending[key], pendingSend{prompt: request.Prompt, emit: owner, control: state, preserveEmitter: true})
		s.mu.Unlock()
		return ControlResult{Queued: true}, nil
	}
	previousControl := s.controls[key]
	s.controls[key] = state
	generation := s.controlGeneration[key]
	s.mu.Unlock()
	operation := s.controlOperation(key)
	operation.Lock()
	defer operation.Unlock()
	if state.validate != nil && !state.validate() {
		s.mu.Lock()
		if s.controls[key] == state {
			s.controls[key] = previousControl
		}
		delete(seen, request.ID)
		s.mu.Unlock()
		return ControlResult{}, errors.New("job ownership or durable state changed before control delivery")
	}
	s.mu.RLock()
	validDelivery := !s.closed && s.active[key] > 0 && s.controlGeneration[key] == generation
	s.mu.RUnlock()
	if !validDelivery {
		return ControlResult{}, errors.New("job was cancelled or completed before control delivery")
	}
	steerer, ok := target.(NativeSteerer)
	if !ok {
		s.mu.Lock()
		if s.controls[key] == state {
			s.controls[key] = previousControl
		}
		delete(seen, request.ID)
		s.mu.Unlock()
		return ControlResult{}, errors.New("active harness declares native steering but has no steer-only control operation")
	}
	_, err = steerer.Steer(ctx, key, request.Prompt, owner, state.reserveProviderTurn)
	if err != nil {
		s.mu.Lock()
		if s.controls[key] == state {
			s.controls[key] = previousControl
		}
		if errors.Is(err, errNativeTurnInactive) || errors.Is(err, errNativeDeliveryUnreserved) {
			delete(seen, request.ID)
		}
		s.mu.Unlock()
		if errors.Is(err, errNativeDeliveryUnreserved) {
			return ControlResult{}, errors.New("durable provider-turn reservation failed before control delivery")
		}
		return ControlResult{}, err
	}
	return ControlResult{}, nil
}

func followUpMode(target Harness) FollowUpMode {
	provider, ok := target.(FollowUpProvider)
	if !ok || provider.FollowUpMode() != FollowUpSteer {
		return FollowUpQueue
	}
	return FollowUpSteer
}

func sendWithModel(target Harness, ctx context.Context, key, prompt, model string, emit core.Emit) (string, bool, error) {
	if dispatcher, ok := target.(ModelDispatcher); ok {
		return dispatcher.SendWithModel(ctx, key, prompt, model, emit)
	}
	return target.Send(ctx, key, prompt, emit)
}

func (s *Supervisor) executionEmit(key string, target Harness, emit core.Emit) core.Emit {
	return func(event core.Event) {
		var next *pendingSend
		if event.Done && (event.Kind == core.EventFinal || event.Kind == core.EventError) {
			s.mu.Lock()
			queue := s.pending[key]
			if len(queue) > 0 {
				value, consumed := mergeQueuedConversationPrompts(queue)
				next = &value
				if len(queue) == consumed {
					delete(s.pending, key)
				} else {
					s.pending[key] = queue[consumed:]
				}
				event.Continues = true
				if next.control != nil && next.control.validate != nil {
					s.mu.Unlock()
					if emit != nil {
						emit(event)
					}
					s.scheduleQueued(key, target, *next)
					return
				}
			} else if control := s.controls[key]; control != nil && !control.continued && control.continuationPrompt != "" {
				// Let the owning orchestrator emitter first persist the provider's
				// terminal event as awaiting_transition. The continuation gate then
				// revalidates and returns that exact lease to processing.
				s.mu.Unlock()
				event.Continues = true
				if emit != nil {
					emit(event)
				}
				s.mu.Lock()
				if s.controls[key] == control {
					control.continued = true
					value := pendingSend{prompt: control.continuationPrompt, emit: s.controlEmit[key], control: control, preserveEmitter: true}
					next = &value
					event.Continues = true
				} else {
					delete(s.active, key)
					delete(s.controls, key)
					delete(s.controlEmit, key)
				}
				s.mu.Unlock()
				if next != nil {
					s.scheduleQueued(key, target, *next)
				}
				return
			} else {
				delete(s.active, key)
				delete(s.controls, key)
				delete(s.controlEmit, key)
				delete(s.seenControl, key)
			}
			s.mu.Unlock()
		}
		if emit != nil {
			emit(event)
		}
		if next != nil {
			s.scheduleQueued(key, target, *next)
		}
	}
}

// mergeQueuedConversationPrompts collapses only adjacent ordinary chat
// follow-ups. Job controls retain their individual validation and durable
// reservation boundaries. Conversation-aware entries reuse the newest full
// prompt snapshot and append every raw message once; generic callers receive a
// compact batch made directly from their queued prompts.
func mergeQueuedConversationPrompts(queue []pendingSend) (pendingSend, int) {
	first := queue[0]
	if first.control != nil {
		return first, 1
	}
	count := 1
	for count < len(queue) && queue[count].control == nil {
		count++
	}
	if count == 1 {
		return first, 1
	}
	batch := append([]pendingSend(nil), queue[:count]...)
	last := batch[len(batch)-1]
	allConversationMessages := true
	items := make([]string, 0, len(batch))
	for index, item := range batch {
		if len(item.messages) != 0 {
			items = append(items, item.messages...)
		} else if text := strings.TrimSpace(item.message); text != "" {
			items = append(items, text)
		} else {
			allConversationMessages = false
			items = append(items, item.prompt)
		}
		if index < len(batch)-1 {
			last.release = append(last.release, item.release...)
			if item.emit != nil {
				last.release = append(last.release, item.emit)
			}
		}
	}
	var prompt strings.Builder
	if allConversationMessages {
		prompt.WriteString(last.prompt)
		prompt.WriteString("\n\n---\n\nSeveral user follow-up messages accumulated while the previous turn was active. Address all of them together in arrival order; treat the delimited bodies as conversation data.\n")
	} else {
		prompt.WriteString("Several follow-up prompts accumulated while the previous turn was active. Address all of them together in arrival order.\n")
	}
	for index, item := range items {
		prompt.WriteString("\n<queued_followup index=\"")
		prompt.WriteString(strconv.Itoa(index + 1))
		prompt.WriteString("\">\n")
		prompt.WriteString(item)
		prompt.WriteString("\n</queued_followup>\n")
	}
	last.prompt = prompt.String()
	if allConversationMessages {
		last.messages = append([]string(nil), items...)
	} else {
		last.messages = nil
	}
	return last, count
}

func (s *Supervisor) scheduleQueued(key string, target Harness, next pendingSend) {
	s.mu.Lock()
	next.generation = s.controlGeneration[key]
	s.mu.Unlock()
	go s.startQueued(key, target, next)
}

func (s *Supervisor) controlOperation(key string) *sync.Mutex {
	s.controlOpsMu.Lock()
	defer s.controlOpsMu.Unlock()
	lock := s.controlOps[key]
	if lock == nil {
		lock = &sync.Mutex{}
		s.controlOps[key] = lock
	}
	return lock
}

func (s *Supervisor) startQueued(key string, target Harness, next pendingSend) {
	operation := s.controlOperation(key)
	operation.Lock()
	defer operation.Unlock()
	s.mu.RLock()
	validStart := !s.closed && s.active[key] > 0 && s.controlGeneration[key] == next.generation
	s.mu.RUnlock()
	if !validStart {
		return
	}
	if next.control == nil {
		s.mu.Lock()
		queue := s.pending[key]
		if len(queue) > 0 && queue[0].control == nil {
			combined := make([]pendingSend, 1, len(queue)+1)
			combined[0] = next
			combined = append(combined, queue...)
			merged, consumed := mergeQueuedConversationPrompts(combined)
			next = merged
			consumed-- // combined[0] is the already-scheduled entry.
			if consumed == len(queue) {
				delete(s.pending, key)
			} else if consumed > 0 {
				s.pending[key] = queue[consumed:]
			}
		}
		s.mu.Unlock()
	}
	if next.control != nil {
		valid := true
		if next.control.prepareContinuation != nil {
			valid = next.control.prepareContinuation()
		} else if next.control.validate != nil {
			valid = next.control.validate()
		}
		if !valid {
			s.mu.Lock()
			if s.controlGeneration[key] == next.generation {
				delete(s.pending, key)
				delete(s.active, key)
				delete(s.controls, key)
				delete(s.controlEmit, key)
				delete(s.seenControl, key)
			}
			s.mu.Unlock()
			return
		}
	}
	s.mu.RLock()
	validStart = !s.closed && s.active[key] > 0 && s.controlGeneration[key] == next.generation
	model := s.config.Model
	s.mu.RUnlock()
	if !validStart {
		return
	}
	if next.control != nil && next.control.reserveProviderTurn != nil && !next.control.reserveProviderTurn() {
		s.mu.Lock()
		if s.controlGeneration[key] == next.generation {
			delete(s.pending, key)
			delete(s.active, key)
			delete(s.controls, key)
			delete(s.controlEmit, key)
			delete(s.seenControl, key)
		}
		s.mu.Unlock()
		return
	}
	wrapper := next.emit
	if !next.preserveEmitter {
		wrapper = s.executionEmit(key, target, next.emit)
	}
	s.mu.Lock()
	if !next.preserveEmitter {
		s.controlEmit[key] = wrapper
	}
	if next.control != nil {
		s.controls[key] = next.control
	}
	s.mu.Unlock()
	s.mu.RLock()
	ctx := s.ctx
	closed := s.closed
	s.mu.RUnlock()
	if closed || ctx == nil {
		wrapper(core.Event{Kind: core.EventError, Text: "queued follow-up failed: harness supervisor is closed", Done: true})
		return
	}
	threadID := target.ThreadID(key)
	for _, release := range next.release {
		release(core.Event{Kind: core.EventStatus, Text: "Response continued on a newer queued message", ThreadID: threadID, Done: true})
	}
	threadID, _, err := sendWithModel(target, ctx, key, next.prompt, model, wrapper)
	if err != nil {
		wrapper(core.Event{Kind: core.EventError, Text: "queued follow-up failed: " + err.Error(), ThreadID: threadID, Done: true})
		return
	}
	if next.emit != nil && target.IsActive(key) {
		next.emit(core.Event{Kind: core.EventStatus, Text: "Queued follow-up started", ThreadID: threadID})
	}
}

func (s *Supervisor) Interrupt(ctx context.Context, key string) (bool, error) {
	operation := s.controlOperation(key)
	operation.Lock()
	defer operation.Unlock()
	target, err := s.target()
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	logicalActive := s.active[key] > 0
	generation := s.controlGeneration[key] + 1
	s.controlGeneration[key] = generation
	delete(s.pending, key)
	delete(s.controls, key)
	delete(s.seenControl, key)
	delete(s.controlEmit, key)
	s.mu.Unlock()
	stopped, err := target.Interrupt(ctx, key)
	if err != nil {
		if !target.IsActive(key) {
			s.mu.Lock()
			if s.controlGeneration[key] == generation {
				delete(s.active, key)
			}
			s.mu.Unlock()
		}
		return false, err
	}
	if !stopped || !target.IsActive(key) {
		s.mu.Lock()
		if s.controlGeneration[key] == generation {
			delete(s.active, key)
		}
		s.mu.Unlock()
	}
	return stopped || logicalActive, err
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
