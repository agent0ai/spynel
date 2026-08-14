// Package localapi connects every TUI process to the one elected workspace
// server over an authenticated loopback HTTP endpoint.
package localapi

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
)

// This accommodates the plain CLI's bounded message plus JSON framing while
// keeping every authenticated loopback request comfortably finite.
const maxRequestBytes = 1 << 20

const shutdownTimeout = 5 * time.Second

type Server struct {
	Service *app.Service
	Token   string
}

type streamEnvelope struct {
	Event *core.Event `json:"event,omitempty"`
	Error string      `json:"error,omitempty"`
}

type screenActionRequest struct {
	InstanceID string            `json:"instance_id"`
	ScreenID   string            `json:"screen_id"`
	Action     string            `json:"action"`
	Values     map[string]string `json:"values"`
}

type screenResponse struct {
	Screen *core.Screen `json:"screen,omitempty"`
}

type settingsRequest struct {
	Values map[string]string `json:"values"`
}
type notifyRequest struct {
	Origin  string `json:"origin"`
	Message string `json:"message"`
}
type notifyResponse struct {
	ID string `json:"id"`
}
type notificationAckRequest struct {
	Origin     string `json:"origin"`
	ID         string `json:"id"`
	AfterChars int    `json:"after_chars"`
}
type diagnosticRequest struct {
	Event   string `json:"event"`
	Message string `json:"message"`
}
type liveTUIRequest struct {
	InstanceID   string `json:"instance_id"`
	Conversation string `json:"conversation"`
}

func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	if s.Service == nil || s.Token == "" {
		return errors.New("local API requires a service and token")
	}
	// The owner may have spent an unbounded interval starting its harness and
	// channels after election. Anchor the cleanup fence to the point when live
	// TUI clients can actually renew, before accepting any request.
	s.Service.FenceCleanupForLiveTUIReadmission()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.authorize(s.health))
	mux.HandleFunc("GET /v1/state", s.authorize(s.state))
	mux.HandleFunc("GET /v1/status", s.authorize(s.status))
	mux.HandleFunc("GET /v1/initial-screen", s.authorize(s.initialScreen))
	mux.HandleFunc("POST /v1/message", s.authorize(s.message))
	mux.HandleFunc("POST /v1/screen-action", s.authorize(s.screenAction))
	mux.HandleFunc("POST /v1/settings", s.authorize(s.settings))
	mux.HandleFunc("POST /v1/run-once", s.authorize(s.runOnce))
	mux.HandleFunc("POST /v1/notify", s.authorize(s.notify))
	mux.HandleFunc("POST /v1/notification-ack", s.authorize(s.notificationAck))
	mux.HandleFunc("POST /v1/diagnostic", s.authorize(s.diagnostic))
	mux.HandleFunc("POST /v1/tui-live", s.authorize(s.liveTUI))
	mux.HandleFunc("DELETE /v1/tui-live", s.authorize(s.liveTUI))
	serverContext, cancelServer := context.WithCancel(ctx)
	defer cancelServer()
	server := &http.Server{
		Handler: mux, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second,
		BaseContext: func(net.Listener) context.Context { return serverContext },
	}
	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		cancelServer()
		shutdownServer(ctx, server)
		close(shutdownDone)
	}()
	err := server.Serve(listener)
	if ctx.Err() != nil {
		<-shutdownDone
	} else {
		// A listener failure also cancels and drains already accepted requests
		// before ownership is relinquished.
		cancelServer()
		shutdownServer(ctx, server)
	}
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (s *Server) liveTUI(response http.ResponseWriter, request *http.Request) {
	var input liveTUIRequest
	if err := decodeJSON(request.Body, &input); err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	if request.Method == http.MethodDelete {
		s.Service.UnregisterLiveTUI(input.InstanceID)
		response.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.Service.RegisterLiveTUI(input.InstanceID, input.Conversation, time.Now().UTC()); err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) diagnostic(response http.ResponseWriter, request *http.Request) {
	var input diagnosticRequest
	if err := decodeJSON(request.Body, &input); err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	if len(input.Event) > 64 || len(input.Message) > 1024 {
		http.Error(response, "diagnostic exceeds bounds", http.StatusBadRequest)
		return
	}
	s.Service.Runtime.LogEvent("warning", "tui", input.Event, input.Message)
	response.WriteHeader(http.StatusNoContent)
}

func shutdownServer(parent context.Context, server *http.Server) {
	shutdownContext, cancel := context.WithTimeout(context.WithoutCancel(parent), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		_ = server.Close()
	}
}

func (s *Server) notificationAck(response http.ResponseWriter, request *http.Request) {
	var input notificationAckRequest
	if err := decodeJSON(request.Body, &input); err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.Service.AckNotification(input.Origin, input.ID, input.AfterChars); err != nil {
		writeError(response, err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) notify(response http.ResponseWriter, request *http.Request) {
	var input notifyRequest
	if err := decodeJSON(request.Body, &input); err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	id, err := s.Service.Notify(request.Context(), input.Origin, input.Message)
	if err != nil {
		writeError(response, err)
		return
	}
	writeJSON(response, http.StatusAccepted, notifyResponse{ID: id})
}

func (s *Server) authorize(next http.HandlerFunc) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		provided := request.Header.Get("Authorization")
		want := "Bearer " + s.Token
		if len(provided) != len(want) || subtle.ConstantTimeCompare([]byte(provided), []byte(want)) != 1 {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(response, request)
	}
}

func (s *Server) health(response http.ResponseWriter, _ *http.Request) {
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) state(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, s.Service.SharedState())
}

func (s *Server) status(response http.ResponseWriter, request *http.Request) {
	conversation := request.URL.Query().Get("conversation")
	if conversation == "" {
		conversation = "local"
	}
	status, err := s.Service.Status(core.Message{
		Channel: "cli", Conversation: conversation, Sender: "cli",
		InstanceID: request.URL.Query().Get("instance_id"),
	})
	if err != nil {
		writeError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, status)
}

func (s *Server) initialScreen(response http.ResponseWriter, request *http.Request) {
	hasHistory, _ := strconv.ParseBool(request.URL.Query().Get("has_history"))
	forceWelcome, _ := strconv.ParseBool(request.URL.Query().Get("welcome"))
	if forceWelcome {
		screen := s.Service.WelcomeScreen()
		writeJSON(response, http.StatusOK, screenResponse{Screen: &screen})
		return
	}
	if hasHistory {
		writeJSON(response, http.StatusOK, screenResponse{})
		return
	}
	if availability, ok := s.Service.Harness.(harness.Availability); ok {
		if ready, _ := availability.Available(); !ready {
			screen := s.Service.HarnessScreen(true)
			writeJSON(response, http.StatusOK, screenResponse{Screen: &screen})
			return
		}
	}
	screen, err := s.Service.InitialWelcome()
	if err != nil {
		writeError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, screenResponse{Screen: screen})
}

func (s *Server) message(response http.ResponseWriter, request *http.Request) {
	var message core.Message
	if err := decodeJSON(request.Body, &message); err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	if message.Channel == "tui" {
		if err := s.Service.RegisterLiveTUI(message.InstanceID, message.Conversation, time.Now().UTC()); err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
	}
	response.Header().Set("Content-Type", "application/x-ndjson")
	response.Header().Set("X-Content-Type-Options", "nosniff")
	flusher, _ := response.(http.Flusher)
	events := make(chan core.Event, 256)
	emit := func(event core.Event) {
		if message.Channel == "tui" && event.Screen != nil && event.Screen.Conversation != "" {
			_ = s.Service.RegisterLiveTUI(message.InstanceID, event.Screen.Conversation, time.Now().UTC())
		}
		select {
		case events <- event:
		case <-request.Context().Done():
		}
	}
	if err := s.Service.Handle(request.Context(), message, emit); err != nil {
		_ = json.NewEncoder(response).Encode(streamEnvelope{Error: err.Error()})
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	encoder := json.NewEncoder(response)
	awaitTerminal := false
	writeEvent := func(event core.Event) bool {
		// Service.Handle returns after starting a harness turn. Remember that
		// synchronous start/delta event instead of polling IsActive: providers
		// are allowed to clear active bookkeeping immediately before emitting
		// their terminal event.
		if !event.Done && (event.Kind == core.EventStatus || event.Kind == core.EventDelta) {
			awaitTerminal = true
		}
		if err := encoder.Encode(streamEnvelope{Event: &event}); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	for {
		select {
		case event := <-events:
			if !writeEvent(event) || event.Done {
				return
			}
		default:
			if !awaitTerminal {
				return
			}
			select {
			case event := <-events:
				if !writeEvent(event) || event.Done {
					return
				}
			case <-request.Context().Done():
				return
			}
		}
	}
}

func (s *Server) screenAction(response http.ResponseWriter, request *http.Request) {
	var input screenActionRequest
	if err := decodeJSON(request.Body, &input); err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	screen, err := s.Service.ScreenActionForInstance(request.Context(), input.InstanceID, input.ScreenID, input.Action, input.Values)
	if err != nil {
		writeError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, screenResponse{Screen: screen})
}

func (s *Server) settings(response http.ResponseWriter, request *http.Request) {
	var input settingsRequest
	if err := decodeJSON(request.Body, &input); err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := s.Service.ApplySettings(input.Values); err != nil {
		writeError(response, err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) runOnce(response http.ResponseWriter, request *http.Request) {
	if err := s.Service.Orchestrator.ScanOnce(request.Context()); err != nil {
		writeError(response, err)
		return
	}
	if err := s.Service.Orchestrator.WaitForIdle(request.Context()); err != nil {
		writeError(response, err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func decodeJSON(body io.Reader, target any) error {
	data, err := io.ReadAll(io.LimitReader(body, maxRequestBytes+1))
	if err != nil {
		return fmt.Errorf("read request: %w", err)
	}
	if len(data) > maxRequestBytes {
		return fmt.Errorf("request exceeds the %d byte limit", maxRequestBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode request: multiple JSON values are not allowed")
		}
		return fmt.Errorf("decode request: %w", err)
	}
	return nil
}

func writeError(response http.ResponseWriter, err error) {
	http.Error(response, err.Error(), http.StatusUnprocessableEntity)
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}
