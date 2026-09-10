package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/core"
	tea "github.com/charmbracelet/bubbletea"
)

func TestLogoAnimatesGlobalJobsWhileDisplayedConversationIsIdle(t *testing.T) {
	runtime := app.NewRuntime()
	m := testModel()
	m.runtimeEvents = runtime.Updates()
	var delays []time.Duration
	m.logoTick = func(after time.Duration, generation uint64) tea.Cmd {
		delays = append(delays, after)
		return func() tea.Msg { return logoAnimationTickMsg{generation: generation} }
	}
	check := func(want int) {
		t.Helper()
		next, _ := m.Update(m.waitEvent()())
		m = next.(model)
		if m.runtimeStatus.LiveJobs != want || m.working || m.mainAgentActivity != 0 || m.durableWork != (core.DurableWorkCounts{}) {
			t.Fatalf("global jobs %d: runtime=%#v working=%t activity=%d work=%#v", want, m.runtimeStatus, m.working, m.mainAgentActivity, m.durableWork)
		}
		if want == 0 {
			if m.logoAnimation != logoStopped || m.spynelLogo() != "○○" {
				t.Fatalf("settled logo = %d %q", m.logoAnimation, m.spynelLogo())
			}
			return
		}
		if m.logoAnimation != logoBackground {
			t.Fatalf("global job did not start slow loop: %d", m.logoAnimation)
		}
		// Advance more than two loops through the actual update/render boundary.
		advanced := false
		for range 20 {
			before, offset := m.headerView(m.width), m.viewport.YOffset
			next, _ = m.Update(logoAnimationTickMsg{generation: m.logoGeneration})
			m = next.(model)
			if !strings.Contains(m.headerView(m.width), m.spynelLogo()) || m.viewport.YOffset != offset || delays[len(delays)-1] != logoBackgroundInterval {
				t.Fatal("slow logo loop changed scroll position, cadence, or header rendering")
			}
			// The cycle deliberately repeats its first frame at the end.
			if before == m.headerView(m.width) && m.spynelLogo() != "◉◉" {
				t.Fatal("active logo frame did not visibly advance")
			}
			advanced = advanced || before != m.headerView(m.width)
		}
		if !advanced {
			t.Fatal("global job kept a static logo")
		}
	}
	first := runtime.BeginJob("chat:telegram:elsewhere", "telegram", "elsewhere", "global conversation")
	runtime.SetJobRunningIfStarting(first)
	check(1)
	second := runtime.BeginJob("chat:tui:other", "tui", "other", "another window")
	check(2)
	runtime.EndJob(first)
	check(1)
	runtime.UpdateJob(second, core.ExecutionStatus{State: string(app.JobFinishing)})
	check(0) // Still registered for cleanup, but no longer executing.
	runtime.EndJob(second)
	check(0)
	generation, count := m.logoGeneration, len(delays)
	m.Update(logoAnimationTickMsg{generation: generation - 1})
	if len(delays) != count {
		t.Fatal("stale tick restarted a settled logo")
	}
	next, _ := m.Update(durableWorkEvent{counts: core.DurableWorkCounts{Tasks: 1, Goals: 1}})
	if got := next.(model); got.logoAnimation != logoStopped || len(delays) != count {
		t.Fatal("queued task/passive goal started the logo")
	}
}
