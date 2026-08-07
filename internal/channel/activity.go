package channel

import (
	"context"
	"sync"
	"time"
)

const activityStopTimeout = 2 * time.Second

// ActivityIndicator keeps a best-effort remote activity signal alive while
// one or more operations are active for the same conversation. Callers must
// invoke the returned function when their operation emits its terminal event.
type ActivityIndicator[K comparable] struct {
	interval time.Duration
	signal   func(context.Context, K, bool) error

	mu     sync.Mutex
	active map[K]*activityState
}

type activityState struct {
	references int
	cancel     context.CancelFunc
	done       <-chan struct{}
	signalMu   sync.Mutex
}

func NewActivityIndicator[K comparable](interval time.Duration, signal func(context.Context, K, bool) error) *ActivityIndicator[K] {
	if interval <= 0 {
		interval = 4 * time.Second
	}
	return &ActivityIndicator[K]{interval: interval, signal: signal, active: make(map[K]*activityState)}
}

// Start signals activity immediately and refreshes it until the returned stop
// function is called, the parent context ends, or the final overlapping user
// of the same key stops. The stop function is idempotent.
func (a *ActivityIndicator[K]) Start(parent context.Context, key K) func() {
	if parent == nil {
		parent = context.Background()
	}
	a.mu.Lock()
	if current := a.active[key]; current != nil {
		current.references++
		a.mu.Unlock()
		return a.reference(parent, key, current)
	}
	ctx, cancel := context.WithCancel(context.Background())
	state := &activityState{references: 1, cancel: cancel, done: ctx.Done()}
	a.active[key] = state
	a.mu.Unlock()

	stop := a.reference(parent, key, state)
	a.send(ctx, key, state, true)
	go a.refresh(ctx, key, state, stop)
	return stop
}

func (a *ActivityIndicator[K]) reference(parent context.Context, key K, state *activityState) func() {
	var once sync.Once
	stop := func() { once.Do(func() { a.release(key, state) }) }
	if done := parent.Done(); done != nil {
		go func() {
			select {
			case <-done:
				stop()
			case <-state.done:
			}
		}()
	}
	return stop
}

func (a *ActivityIndicator[K]) refresh(ctx context.Context, key K, state *activityState, stop func()) {
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			stop()
			return
		case <-ticker.C:
			a.send(ctx, key, state, true)
		}
	}
}

func (a *ActivityIndicator[K]) release(key K, state *activityState) {
	a.mu.Lock()
	current := a.active[key]
	if current != state {
		a.mu.Unlock()
		return
	}
	state.references--
	if state.references > 0 {
		a.mu.Unlock()
		return
	}
	delete(a.active, key)
	state.cancel()
	a.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), activityStopTimeout)
	a.send(ctx, key, state, false)
	cancel()

	// A replacement may have started while the inactive signal was in flight.
	// Reassert it so an old completion can never silence new work in the chat.
	a.mu.Lock()
	replacement := a.active[key]
	a.mu.Unlock()
	if replacement != nil {
		ctx, cancel = context.WithTimeout(context.Background(), activityStopTimeout)
		a.send(ctx, key, replacement, true)
		cancel()
	}
}

func (a *ActivityIndicator[K]) send(ctx context.Context, key K, state *activityState, active bool) {
	if a.signal == nil {
		return
	}
	state.signalMu.Lock()
	defer state.signalMu.Unlock()
	_ = a.signal(ctx, key, active)
}
