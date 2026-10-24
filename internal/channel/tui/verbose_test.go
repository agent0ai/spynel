package tui

import (
	"strings"
	"testing"

	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/core"
)

func TestVerboseTUIStreamAndHistoryMatrix(t *testing.T) {
	for _, mode := range []string{"auto", "on", "off"} {
		for _, route := range []string{"stream", "history"} {
			t.Run(mode+"/"+route, func(t *testing.T) {
				m := notificationTestModel()
				m.verbose = mode
				final := "Final answer."
				if route == "stream" {
					for _, e := range []core.Event{{Kind: core.EventDelta, Text: "progress\n"}, {Kind: core.EventFinal, Text: "progress\n" + final, FinalText: &final, Done: true}} {
						next, _ := m.Update(uiEvent{event: e})
						m = next.(model)
					}
				} else {
					m.appendRecoveredResponse(channel.Notification{Recovery: true, Continues: true, Text: "intermediate"})
					m.appendRecoveredResponse(channel.Notification{Recovery: true, Text: "progress\n" + final, FinalText: &final})
				}
				var text string
				for _, entry := range m.transcript {
					text += entry.text
				}
				if strings.Count(text, final) != 1 || (mode == "off" && (strings.Contains(text, "progress") || strings.Contains(text, "intermediate"))) {
					t.Fatal(text)
				}
				if mode != "off" && !strings.Contains(text, "progress") {
					t.Fatal("progress lost", text)
				}
				next, _ := m.Update(uiEvent{event: core.Event{Kind: core.EventFinal, Text: "Command reply", Local: true, Done: true}})
				m = next.(model)
				next, _ = m.Update(uiEvent{event: core.Event{Kind: core.EventError, Text: "Terminal error", Done: true}})
				m = next.(model)
				if m.transcript[len(m.transcript)-2].text != "Command reply" || m.transcript[len(m.transcript)-1].text != "Terminal error" {
					t.Fatal(m.transcript)
				}
			})
		}
	}
}

func TestVerboseTUIMidTurnDoesNotReplayHiddenProgress(t *testing.T) {
	m := notificationTestModel()
	apply := func(e core.Event) { next, _ := m.Update(uiEvent{event: e}); m = next.(model) }
	apply(core.Event{Kind: core.EventDelta, Text: "visible before\n", Verbose: "on"})
	apply(core.Event{Kind: core.EventFinal, Text: "Verbose off", Local: true, Done: true, Verbose: "off"})
	apply(core.Event{Kind: core.EventDelta, Text: "hidden\n", Verbose: "off"})
	apply(core.Event{Kind: core.EventFinal, Text: "hidden continuation", Done: true, Continues: true, Verbose: "off"})
	apply(core.Event{Kind: core.EventFinal, Text: "visible continuation", Continues: true, Done: true, Verbose: "on"})
	apply(core.Event{Kind: core.EventDelta, Text: "answer", Verbose: "on"})
	final := "answer"
	apply(core.Event{Kind: core.EventFinal, Text: "visible before\nhidden\nanswer", FinalText: &final, Done: true, Verbose: "on"})
	var text string
	for _, e := range m.transcript {
		text += e.text
	}
	if strings.Contains(text, "hidden") || !strings.Contains(text, "visible before") || strings.Count(text, "answer") != 1 {
		t.Fatal(text)
	}
}

func verboseFixture(m model) model {
	final := "Verbose final answer."
	for _, e := range []core.Event{
		{Kind: core.EventDelta, Text: "Visible progress before the change.\n", Verbose: "on"},
		{Kind: core.EventFinal, Text: "Global verbose: off.", Local: true, Done: true, Verbose: "off"},
		{Kind: core.EventDelta, Text: "HIDDEN-PROGRESS", Verbose: "off"},
		{Kind: core.EventFinal, Text: "Visible progress before the change.\nHIDDEN-PROGRESS\n" + final, FinalText: &final, Done: true, Verbose: "off"},
	} {
		next, _ := m.Update(uiEvent{event: e})
		m = next.(model)
	}
	m.invalidateHistoryRender()
	m.renderHistory()
	m.viewport.GotoBottom()
	m.refresh()
	return m
}

func TestVerboseTUIRecoveryDoesNotReplaySuppressedAggregate(t *testing.T) {
	m := notificationTestModel()
	m.verbose = "off"
	m.appendRecoveredResponse(channel.Notification{Recovery: true, RequestID: "recovered", Continues: true, Text: "hidden"})
	m.setVerbose("on")
	visible := "Visible continuation."
	m.appendRecoveredResponse(channel.Notification{Recovery: true, RequestID: "recovered", Continues: true, Text: "hidden\n" + visible, FinalText: &visible})
	final := "Final answer."
	m.appendRecoveredResponse(channel.Notification{Recovery: true, RequestID: "recovered", Text: "hidden\n" + visible + "\n" + final, FinalText: &final})
	var text string
	for _, e := range m.transcript {
		text += e.text
	}
	if strings.Contains(text, "hidden") || strings.Count(text, visible) != 1 || strings.Count(text, final) != 1 {
		t.Fatal(text)
	}
}
