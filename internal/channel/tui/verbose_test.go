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
	hidden := "HIDDEN-PROGRESS"
	for _, e := range []core.Event{
		{Kind: core.EventDelta, Text: "Visible progress before the change.\n", Verbose: "on"},
		{Kind: core.EventFinal, Text: "Global verbose: off.", Local: true, Done: true, Verbose: "off"},
		{Kind: core.EventDelta, Text: hidden, Verbose: "off"},
		{Kind: core.EventFinal, Text: "Global verbose: on.", Local: true, Done: true, Verbose: "on"},
		{Kind: core.EventFinal, Text: "Visible progress before the change.\n" + hidden, FinalText: &hidden, Done: true, Continues: true, Verbose: "on"},
		{Kind: core.EventDelta, Text: "Verbose ", Verbose: "on"},
		{Kind: core.EventDelta, Text: "final ", Verbose: "off"},
		{Kind: core.EventDelta, Text: "answer.", Verbose: "on"},
		{Kind: core.EventFinal, Text: final, FinalText: &final, Done: true, Verbose: "on"},
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

func TestVerboseToggleInsideFinalItem(t *testing.T) {
	m := notificationTestModel()
	m.requests = map[string]requestPresentation{"request": {active: true}}
	apply := func(e core.Event) { e.RequestID = "request"; next, _ := m.Update(uiEvent{event: e}); m = next.(model) }
	m.setVerbose("on")
	apply(core.Event{Kind: core.EventDelta, Text: "The ", Verbose: "on"})
	m.setVerbose("off")
	apply(core.Event{Kind: core.EventDelta, Text: "result ", Verbose: "off"})
	m.setVerbose("on")
	apply(core.Event{Kind: core.EventDelta, Text: "is 42.", Verbose: "on"})
	m.input.SetValue("Draft selection")
	m.input.SelectAll()
	final := "The result is 42."
	apply(core.Event{Kind: core.EventFinal, Text: final, FinalText: &final, Done: true, Verbose: "on"})
	var text string
	for _, e := range m.transcript {
		text += e.text
	}
	if text != final {
		t.Fatalf("final answer must be complete and singular; transcript=%q", text)
	}
	if m.input.SelectedText() != "Draft selection" {
		t.Fatal("final reconciliation cleared the composer selection")
	}
}

func TestVerboseTUIContinuationDoesNotReplayHiddenProgress(t *testing.T) {
	m := notificationTestModel()
	m.requests = map[string]requestPresentation{"request": {active: true}}
	apply := func(e core.Event) { e.RequestID = "request"; next, _ := m.Update(uiEvent{event: e}); m = next.(model) }
	m.setVerbose("off")
	apply(core.Event{Kind: core.EventDelta, Text: "HIDDEN commentary", Verbose: "off"})
	m.setVerbose("on")
	item := "HIDDEN commentary"
	apply(core.Event{Kind: core.EventFinal, Text: item, FinalText: &item, Done: true, Continues: true, Verbose: "on"})
	for _, e := range m.transcript {
		if strings.Contains(e.text, "HIDDEN") {
			t.Fatalf("enabling replayed previously suppressed progress: %q", e.text)
		}
	}
}

func TestVerboseTUIContinuationSnapshotAfterToggle(t *testing.T) {
	for _, route := range []string{"stream", "history"} {
		for _, suffix := range []string{"", " Visible suffix."} {
			t.Run(route+suffix, func(t *testing.T) {
				m := notificationTestModel()
				m.requests = map[string]requestPresentation{"request": {active: true, started: true}}
				apply := func(e core.Event) { e.RequestID = "request"; next, _ := m.Update(uiEvent{event: e}); m = next.(model) }
				m.setVerbose("off")
				apply(core.Event{Kind: core.EventDelta, Text: "HIDDEN commentary"})
				m.setVerbose("on")
				item := "HIDDEN commentary" + suffix
				if route == "stream" {
					apply(core.Event{Kind: core.EventFinal, Text: item, FinalText: &item, ResponseID: "continuing", Done: true, Continues: true})
				} else {
					m.receiveRecoveredResponse(channel.Notification{Recovery: true, RequestID: "request", ResponseID: "continuing", Text: item, FinalText: &item, Continues: true})
					apply(core.Event{Kind: requestFinished})
				}
				final := "Next turn final."
				apply(core.Event{Kind: core.EventDelta, Text: final})
				apply(core.Event{Kind: core.EventFinal, Text: final, FinalText: &final, Done: true})
				var text string
				for _, e := range m.transcript {
					text += e.text
				}
				if strings.Contains(text, "HIDDEN") || (suffix != "" && strings.Count(text, suffix) != 1) || strings.Count(text, final) != 1 {
					t.Fatalf("replayed, lost or duplicated continuation: %q", text)
				}
			})
		}
	}
}

func TestVerboseTUIFinalFragmentsAcrossCommandsAndRecovery(t *testing.T) {
	for _, route := range []string{"stream", "history", "newer-request"} {
		t.Run(route, func(t *testing.T) {
			m := notificationTestModel()
			m.requests = map[string]requestPresentation{"request": {active: true, started: true}}
			apply := func(e core.Event) { e.RequestID = "request"; next, _ := m.Update(uiEvent{event: e}); m = next.(model) }
			delta := func(mode, text string) { m.setVerbose(mode); apply(core.Event{Kind: core.EventDelta, Text: text}) }
			delta("on", "Earlier visible.\n")
			delta("off", "HIDDEN progress\n")
			delta("on", "The ")
			delta("off", "result ")
			m.appendTranscript(transcriptEntry{role: "user", text: "/status"}, transcriptEntry{role: "assistant", text: "Command reply."})
			delta("on", "is ")
			delta("off", "42.")
			m.setVerbose("on")
			final := "The result is 42."
			all := "Earlier visible.\nHIDDEN progress\n" + final
			if route == "stream" {
				apply(core.Event{Kind: core.EventFinal, Text: all, FinalText: &final, ResponseID: "terminal", Done: true})
			} else {
				if route == "newer-request" {
					m.working = true
					m.dispatchMessage("Newer user turn", "Newer user turn")
					m.streaming, m.responseText = "Newer stream", "Newer stream"
				}
				m.receiveRecoveredResponse(channel.Notification{Recovery: true, RequestID: "request", ResponseID: "terminal", Text: all, FinalText: &final})
				apply(core.Event{Kind: requestFinished})
				if route == "newer-request" && (m.streaming != "Newer stream" || m.responseText != "Newer stream") {
					t.Fatal("older final changed newer stream")
				}
			}
			var text, answer string
			for _, e := range m.transcript {
				text += e.text
				if e.requestID == "request" && e.role == "assistant" {
					answer += e.text
				}
			}
			if answer != "Earlier visible.\n"+final || strings.Contains(text, "HIDDEN") || strings.Count(text, "Command reply.") != 1 {
				t.Fatalf("final fragments or command order changed: %q", text)
			}
			if strings.Index(text, "The ") > strings.Index(text, "/status") || strings.Index(text, "result is") < strings.Index(text, "Command reply.") {
				t.Fatal("command interleave changed", text)
			}
		})
	}
}

func TestVerboseTUIContinuingErrorResetsObservedPosition(t *testing.T) {
	m := notificationTestModel()
	apply := func(e core.Event) { next, _ := m.Update(uiEvent{event: e}); m = next.(model) }
	m.setVerbose("off")
	apply(core.Event{Kind: core.EventDelta, Text: "HIDDEN failed turn"})
	apply(core.Event{Kind: core.EventError, Text: "continuing error", Continues: true, Done: true})
	m.setVerbose("on")
	apply(core.Event{Kind: core.EventDelta, Text: "The "})
	m.setVerbose("off")
	apply(core.Event{Kind: core.EventDelta, Text: "result "})
	m.setVerbose("on")
	apply(core.Event{Kind: core.EventDelta, Text: "is 42."})
	final := "The result is 42."
	apply(core.Event{Kind: core.EventFinal, Text: final, FinalText: &final, Done: true})
	if len(m.transcript) != 1 || m.transcript[0].text != final {
		t.Fatal(m.transcript)
	}
}
