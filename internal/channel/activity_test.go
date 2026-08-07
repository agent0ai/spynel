package channel

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type activityTestSignal struct {
	key    string
	active bool
}

func TestActivityIndicatorRefreshesAndWaitsForOverlappingWork(t *testing.T) {
	events := make(chan activityTestSignal, 32)
	indicator := NewActivityIndicator(5*time.Millisecond, func(_ context.Context, key string, active bool) error {
		events <- activityTestSignal{key: key, active: active}
		return nil
	})
	first := indicator.Start(context.Background(), "chat")
	second := indicator.Start(context.Background(), "chat")

	waitActivitySignal(t, events, true)
	waitActivitySignal(t, events, true)
	first()
	select {
	case event := <-events:
		if !event.active {
			t.Fatal("first overlapping completion stopped the activity indicator")
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("activity refresh stopped while overlapping work remained")
	}
	second()
	waitActivitySignal(t, events, false)
}

func TestActivityIndicatorKeepsRefreshingWhenFirstContextEnds(t *testing.T) {
	events := make(chan bool, 32)
	indicator := NewActivityIndicator(5*time.Millisecond, func(_ context.Context, _ string, active bool) error {
		events <- active
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	indicator.Start(ctx, "chat")
	stopSecond := indicator.Start(context.Background(), "chat")
	waitBooleanSignal(t, events, true)
	cancel()
	waitBooleanSignal(t, events, true)
	stopSecond()
	waitBooleanSignal(t, events, false)
}

func TestActivityIndicatorDoesNotBlockStartAndRecoversAfterTimedOutSignal(t *testing.T) {
	firstStarted := make(chan struct{})
	var attempts atomic.Int32
	refreshed := make(chan struct{}, 1)
	indicator := NewActivityIndicator(5*time.Millisecond, func(ctx context.Context, _ string, active bool) error {
		if !active {
			return nil
		}
		if attempts.Add(1) == 1 {
			close(firstStarted)
			<-ctx.Done()
			return ctx.Err()
		}
		select {
		case refreshed <- struct{}{}:
		default:
		}
		return errors.New("transient action failure")
	})

	started := make(chan func(), 1)
	go func() { started <- indicator.Start(context.Background(), "chat") }()
	waitChannel(t, firstStarted, "first activity attempt")
	var stop func()
	select {
	case stop = <-started:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Start waited for the slow activity request")
	}
	waitChannel(t, refreshed, "activity refresh after timed-out request")
	stop()
}

func TestActivityIndicatorStopsWhenItsContextEnds(t *testing.T) {
	events := make(chan bool, 8)
	indicator := NewActivityIndicator(time.Hour, func(_ context.Context, _ string, active bool) error {
		events <- active
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	indicator.Start(ctx, "chat")
	waitBooleanSignal(t, events, true)
	cancel()
	waitBooleanSignal(t, events, false)
}

func waitChannel(t *testing.T, channel <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func waitActivitySignal(t *testing.T, events <-chan activityTestSignal, active bool) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case event := <-events:
			if event.key != "chat" {
				t.Fatalf("activity signal = %#v, want chat", event)
			}
			if event.active == active {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for active=%t", active)
		}
	}
}

func waitBooleanSignal(t *testing.T, events <-chan bool, active bool) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case event := <-events:
			if event == active {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for active=%t", active)
		}
	}
}
