package cli

import (
	"testing"

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
