package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/history"
)

const maxSubscribers = 32

type EventEnvelope struct {
	Schema string                     `json:"schema"`
	Event  *history.ConversationEvent `json:"event,omitempty"`
	Cursor string                     `json:"cursor,omitempty"`
	Error  string                     `json:"error,omitempty"`
	Code   string                     `json:"code,omitempty"`
}

type ConversationSnapshot struct {
	Schema  string                      `json:"schema"`
	Events  []history.ConversationEvent `json:"events"`
	Cursor  string                      `json:"cursor"`
	Bounded bool                        `json:"bounded"`
}

func (s *Server) conversation(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("conversation")
	if err := app.ValidateConversationName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	events, cursor, err := s.Service.History.EventSnapshot("cli", name)
	if err != nil {
		writeJSON(w, http.StatusConflict, eventError(err))
		return
	}
	writeJSON(w, http.StatusOK, ConversationSnapshot{Schema: "spynel.events/v1", Events: events, Cursor: cursor, Bounded: true})
}

func eventError(err error) EventEnvelope {
	code := "history_unavailable"
	if errors.Is(err, history.ErrEventCursor) {
		code = "invalid_cursor"
	}
	if errors.Is(err, history.ErrEventExpired) {
		code = "cursor_expired"
	}
	if errors.Is(err, history.ErrEventLag) {
		code = "replay_limit"
	}
	return EventEnvelope{Schema: "spynel.events/v1", Code: code, Error: "Conversation events unavailable: " + code + "; resynchronize using conversation history"}
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	conversation, cursor := r.URL.Query().Get("conversation"), r.URL.Query().Get("after")
	if err := app.ValidateConversationName(conversation); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(cursor) > 256 {
		writeJSON(w, http.StatusBadRequest, eventError(history.ErrEventCursor))
		return
	}
	if s.subscribers.Add(1) > maxSubscribers {
		s.subscribers.Add(-1)
		http.Error(w, "subscriber limit reached", http.StatusTooManyRequests)
		return
	}
	defer s.subscribers.Add(-1)
	events, next, err := s.Service.History.Events("cli", conversation, cursor)
	if err != nil {
		writeJSON(w, http.StatusConflict, eventError(err))
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	encoder, controller := json.NewEncoder(w), http.NewResponseController(w)
	write := func(value EventEnvelope) error {
		value.Schema = "spynel.events/v1"
		_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := encoder.Encode(value); err != nil {
			return err
		}
		return controller.Flush()
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	lastCheckpoint := time.Now()
	initial := true
	for {
		for _, event := range events {
			if write(EventEnvelope{Event: &event}) != nil {
				return
			}
		}
		if initial || cursor != next || time.Since(lastCheckpoint) >= 15*time.Second {
			if write(EventEnvelope{Cursor: next}) != nil {
				return
			}
			lastCheckpoint = time.Now()
			initial = false
		}
		cursor = next
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
		events, next, err = s.Service.History.Events("cli", conversation, cursor)
		if err != nil {
			_ = write(eventError(err))
			return
		}
	}
}

// Events makes one subscription attempt. Callers persist a cursor only after
// processing its preceding events and decide explicitly when to reconnect.
func (c *Client) Events(ctx context.Context, conversation, after string, emit func(EventEnvelope) error) error {
	query := url.Values{"conversation": {conversation}, "after": {after}}
	response, err := c.request(ctx, http.MethodGet, "/v1/events?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := responseError(response); err != nil {
		return err
	}
	decoder := json.NewDecoder(response.Body)
	for {
		var envelope EventEnvelope
		if err := decoder.Decode(&envelope); err != nil {
			if errors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
		if envelope.Schema != "spynel.events/v1" {
			return errors.New("unsupported event schema")
		}
		if err := emit(envelope); err != nil {
			return err
		}
		if envelope.Error != "" {
			return errors.New(envelope.Error)
		}
	}
}
