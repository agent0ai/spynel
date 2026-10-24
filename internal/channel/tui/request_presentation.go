package tui

import (
	"slices"

	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/core"
)

// This local event follows all request callbacks in the same FIFO, including
// asynchronous handler-return errors; it never crosses the application API.
const requestFinished = "tui_request_finished"

type requestPresentation struct {
	active, started, ended, retired bool
	progressSuppressed              bool
}

func (m *model) observeRequest(event core.Event) {
	r := m.requests[event.RequestID]
	if event.Kind == core.EventActivity {
		if event.Active {
			r.started = true
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
