package cli

import (
	"context"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/core"
)

func TestPublishTUIStateChangesCarriesLatestDurableCounts(t *testing.T) {
	events := tuiStateEvents{durableWork: make(chan core.DurableWorkCounts, 1)}
	previous := app.SharedState{DurableWork: core.DurableWorkCounts{Tasks: 1}}
	first := app.SharedState{DurableWork: core.DurableWorkCounts{Tasks: 2, Goals: 1}}
	latest := app.SharedState{DurableWork: core.DurableWorkCounts{Tasks: 4, Goals: 3}}

	publishTUIStateChanges(events, previous, first, "")
	publishTUIStateChanges(events, first, latest, "")

	if got := <-events.durableWork; got != latest.DurableWork {
		t.Fatalf("durable work update = %#v, want %#v", got, latest.DurableWork)
	}
	previous = latest
	latest.WorkDiagnostics = []string{"task count is a lower bound"}
	publishTUIStateChanges(events, previous, latest, "")
	select {
	case got := <-events.durableWork:
		t.Fatalf("diagnostic-only change published a noisy header update: %#v", got)
	default:
	}
}

func TestPublishTUIStateChangesCarriesGlobalJobActivity(t *testing.T) {
	events := tuiStateEvents{runtime: make(chan core.RuntimeStatus, 1)}
	previous := app.SharedState{}
	for _, live := range []int{1, 2, 1, 0} {
		// Keep the registered count unchanged to catch running/settling edges.
		next := app.SharedState{Runtime: core.RuntimeStatus{Jobs: 2, LiveJobs: live}}
		publishTUIStateChanges(events, previous, next, "")
		select {
		case got := <-events.runtime:
			if got != next.Runtime {
				t.Fatalf("global activity update = %#v, want %#v", got, next.Runtime)
			}
		default:
			t.Fatalf("live job transition to %d was not published", live)
		}
		previous = next
	}
}

func TestPublishTUIStateChangesRoutesOnlySelectedConversationActivity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := tuiStateEvents{activity: make(chan core.Event, 4), activitySet: make(chan int, 1)}
	go bridgeTUIActivity(ctx, events.activity, events.activitySet, 1)
	if event := receiveTUIActivity(t, events.activity); event.Kind != core.EventActivity || !event.Active {
		t.Fatalf("initial selected activity event = %#v", event)
	}
	previous := app.SharedState{ConversationActivity: 1}
	next := app.SharedState{ConversationActivity: 2}
	publishTUIStateChanges(events, previous, next, "", "selected")
	if event := receiveTUIActivity(t, events.activity); event.Kind != core.EventActivity || !event.Active {
		t.Fatalf("selected activity event = %#v", event)
	}
	select {
	case event := <-events.activity:
		t.Fatalf("other conversation affected selected TUI: %#v", event)
	default:
	}

	publishTUIStateChanges(events, next, app.SharedState{}, "", "selected")
	for index := 0; index < 2; index++ {
		if event := receiveTUIActivity(t, events.activity); event.Kind != core.EventActivity || event.Active {
			t.Fatalf("selected activity stop %d = %#v", index, event)
		}
	}
}

func TestTUIActivityBridgeDoesNotBlockAboveEventCapacity(t *testing.T) {
	startupContext, stopStartup := context.WithCancel(context.Background())
	started := make(chan tuiStateEvents, 1)
	go func() {
		started <- startTUIStatePolling(startupContext, nil, app.SharedState{ConversationActivity: 64}, "", "selected")
	}()
	select {
	case <-started:
		stopStartup()
	case <-time.After(2 * time.Second):
		stopStartup()
		t.Fatal("TUI state polling blocked while publishing initial activity above event capacity")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := tuiStateEvents{activity: make(chan core.Event, 16), activitySet: make(chan int, 1)}
	go bridgeTUIActivity(ctx, events.activity, events.activitySet, 64)
	for index := 0; index < 64; index++ {
		if event := receiveTUIActivity(t, events.activity); !event.Active {
			t.Fatalf("initial activity %d was inactive: %#v", index, event)
		}
	}

	publishLatest(events.activitySet, 0)
	for index := 0; index < 64; index++ {
		if event := receiveTUIActivity(t, events.activity); event.Active {
			t.Fatalf("activity stop %d was active: %#v", index, event)
		}
	}
}

func receiveTUIActivity(t *testing.T, events <-chan core.Event) core.Event {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TUI activity event")
		return core.Event{}
	}
}
