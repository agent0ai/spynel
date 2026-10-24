package tui

import (
	"slices"
	"strings"

	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/core"
)

// This local event follows all request callbacks in the same FIFO, including
// asynchronous handler-return errors; it never crosses the application API.
const requestFinished = "tui_request_finished"

type requestPresentation struct {
	active, started, ended, retired bool
	progressSuppressed              bool
	deltaBytes                      int
}

// Native steering replaces the request emitter without completing the response.
func (m *model) continueResponse(request string) {
	if m.requests == nil {
		m.requests = make(map[string]requestPresentation)
	}
	if previous := m.responseSourceID; previous != request {
		prior := m.requests[previous]
		next := m.requests[request]
		next.deltaBytes = prior.deltaBytes
		next.progressSuppressed = next.progressSuppressed || m.responseSuppressed
		m.requests[request] = next
		if prior.deltaBytes > 0 {
			prior.deltaBytes = 0
			m.requests[previous] = prior
		}
		for i := range m.transcript {
			if e := &m.transcript[i]; e.requestID == previous && e.streamEnd > 0 && e.responseID == "" {
				e.requestID = request
			}
		}
	}
	m.responseSourceID = request
	m.responseRequestID = request
}

// Final items restore their hidden portions without repeating visible fragments.
// Continuing snapshots only add bytes not already observed in this turn.
func (m *model) finishFilteredResponse(request, text string, final *string, continues, showProgress bool) {
	if !continues && request == m.responseRequestID && m.requests[request].deltaBytes == len(m.responseText) {
		if final != nil {
			text = *final
		}
		m.finishRecipientResponse(text)
		return
	}
	if request == m.responseRequestID {
		m.commitStreamingResponse()
	}
	remaining := ""
	if continues {
		if showProgress {
			remaining = channel.UnstreamedFinal(text, final, m.requests[request].deltaBytes)
		}
	} else {
		start := 0
		if final != nil && strings.HasSuffix(text, *final) {
			start = len(text) - len(*final)
		}
		cursor := start
		last := -1
		for i := range m.transcript {
			e := &m.transcript[i]
			if e.requestID != request || e.streamEnd <= start || e.streamEnd > len(text) || e.streamStart < 0 || e.text != text[e.streamStart:e.streamEnd] {
				continue
			}
			prefix := e.text[:max(0, start-e.streamStart)]
			e.text = prefix + text[cursor:e.streamEnd]
			cursor = e.streamEnd
			if last == i-1 && last >= 0 {
				e.text = m.transcript[last].text + e.text
				m.transcript[last].text = ""
			}
			last = i
		}
		remaining = text[cursor:]
		if last >= 0 && last == len(m.transcript)-1 {
			m.transcript[last].text += remaining
			remaining = ""
		}
		m.transcript = slices.DeleteFunc(m.transcript, func(e transcriptEntry) bool {
			return e.requestID == request && e.streamEnd > 0 && e.text == ""
		})
		m.transcript = boundTranscript(m.transcript)
		if last >= 0 {
			m.selection = outputSelection{}
			if m.drag.pane == outputPane {
				m.cancelDrag()
			}
		}
		m.invalidateHistoryRender()
	}
	if remaining != "" {
		m.appendTranscript(transcriptEntry{role: "assistant", text: remaining, requestID: request})
	}
	if request == m.responseRequestID {
		m.resetResponse()
	} else {
		m.finishResponsePresentation(request)
	}
}

func (m *model) finishResponsePresentation(request string) {
	if request == m.responseSourceID {
		m.responseSourceID = ""
	}
	for i := range m.transcript {
		if m.transcript[i].requestID == request {
			m.transcript[i].streamStart, m.transcript[i].streamEnd = 0, 0
		}
	}
	if r, ok := m.requests[request]; ok {
		r.deltaBytes = 0
		m.requests[request] = r
	}
}

func (m *model) observeRequest(event core.Event) {
	r := m.requests[event.RequestID]
	if event.Kind == core.EventActivity {
		if event.Active {
			r.started = true
			m.responseRequestID = event.RequestID
		} else {
			r.ended = true
		}
	}
	if event.Done && !event.Continues {
		r.ended = true
	}
	m.requests[event.RequestID] = r
	m.retireRequest(event.RequestID)
}

func (m *model) retireRequest(id string) {
	r := m.requests[id]
	if r.active || !r.ended || r.retired {
		return
	}
	r.retired = true
	m.requests[id] = r
	m.finishedRequests = append(m.finishedRequests, id)
	if len(m.finishedRequests) > maxTranscriptRows {
		delete(m.requests, m.finishedRequests[0])
		m.finishedRequests = m.finishedRequests[1:]
	}
}

func (m *model) tagResponse(request, response string) {
	if request == "" || response == "" {
		return
	}
	for i := range m.transcript {
		if m.transcript[i].requestID == request && m.transcript[i].responseID == "" {
			m.transcript[i].responseID = response
		}
	}
}

func (m *model) rememberResponse(id string) {
	m.pendingRecovery = slices.DeleteFunc(m.pendingRecovery, func(n channel.Notification) bool { return n.ResponseID == id })
	m.presentedResponses = append(m.presentedResponses, id)
	if len(m.presentedResponses) > maxTranscriptRows {
		m.presentedResponses = m.presentedResponses[1:]
	}
}

func (m *model) receiveRecoveredResponse(n channel.Notification) {
	if n.ResponseID != "" && slices.Contains(m.presentedResponses, n.ResponseID) {
		return
	}
	// History can overtake queued stream events. Keep the stream's ordering
	// until its handler and every preceding callback have reached the model.
	if m.requests[n.RequestID].active {
		m.pendingRecovery = append(m.pendingRecovery, n)
		return
	}
	m.appendRecoveredResponse(n)
}

func (m *model) finishRequest(id string) {
	r := m.requests[id]
	r.active = false
	if !r.started {
		r.ended = true
	}
	m.requests[id] = r
	m.retireRequest(id)
	pending := m.pendingRecovery
	m.pendingRecovery = nil
	for _, n := range pending {
		m.receiveRecoveredResponse(n)
	}
}
