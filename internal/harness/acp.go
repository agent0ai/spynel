package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/fsx"
)

const (
	acpProtocolVersion = 1
	acpMaxRecord       = 16 * 1024 * 1024
)

// ACP adapts the stable v1 Agent Client Protocol over its standard JSON-RPC
// stdio transport. One agent process owns independent ACP sessions for every
// Spynel conversation.
type ACP struct {
	config HarnessConfig

	keyMu    sync.Mutex
	keyLocks map[string]*sync.Mutex
	writeMu  sync.Mutex
	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	nextID   uint64
	pending  map[string]chan acpResponse
	sessions map[string]acpSession
	live     map[string]bool
	active   map[string]*acpTurn
	caps     acpAgentCapabilities
	closed   bool
}

type acpSession struct {
	ID     string `json:"id"`
	Policy string `json:"policy"`
}

type acpTurn struct {
	emit core.Emit

	mu        sync.Mutex
	text      strings.Builder
	cancelled bool
	completed bool
}

type acpResponse struct {
	result json.RawMessage
	err    error
}

type acpRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *acpRPCError) Error() string {
	if e == nil {
		return "ACP request failed"
	}
	if strings.TrimSpace(e.Message) == "" {
		return fmt.Sprintf("ACP error %d", e.Code)
	}
	return fmt.Sprintf("ACP error %d: %s", e.Code, e.Message)
}

type acpEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *acpRPCError    `json:"error,omitempty"`
}

type acpAgentCapabilities struct {
	LoadSession         bool `json:"loadSession"`
	SessionCapabilities struct {
		Resume json.RawMessage `json:"resume,omitempty"`
		Close  json.RawMessage `json:"close,omitempty"`
	} `json:"sessionCapabilities"`
}

type acpSessionResult struct {
	SessionID     string            `json:"sessionId"`
	ConfigOptions []acpConfigOption `json:"configOptions"`
}

type acpConfigOption struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Type     string `json:"type"`
}

func NewACP(cfg HarnessConfig) (*ACP, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, errors.New("ACP command is empty")
	}
	if strings.TrimSpace(cfg.Cwd) == "" {
		cfg.Cwd = "."
	}
	root, err := filepath.Abs(cfg.Cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve ACP working directory: %w", err)
	}
	cfg.Cwd = root
	adapter := &ACP{
		config: cfg, keyLocks: map[string]*sync.Mutex{}, pending: map[string]chan acpResponse{},
		sessions: map[string]acpSession{}, live: map[string]bool{}, active: map[string]*acpTurn{},
	}
	if err := adapter.loadSessions(); err != nil {
		return nil, err
	}
	return adapter, nil
}

func (a *ACP) FollowUpMode() FollowUpMode { return FollowUpQueue }

func (a *ACP) Start(parent context.Context) error {
	a.mu.Lock()
	if a.ctx != nil {
		a.mu.Unlock()
		return nil
	}
	if a.closed {
		a.mu.Unlock()
		return errors.New("ACP harness is closed")
	}
	info, err := os.Stat(a.config.Cwd)
	if err != nil || !info.IsDir() {
		a.mu.Unlock()
		return fmt.Errorf("ACP working directory %q is unavailable", a.config.Cwd)
	}
	a.ctx, a.cancel = context.WithCancel(parent)
	command := exec.CommandContext(a.ctx, a.config.Command, a.config.Args...)
	command.Dir = a.config.Cwd
	if len(a.config.Env) != 0 {
		command.Env = append(os.Environ(), a.config.Env...)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		a.cancel()
		a.ctx = nil
		a.cancel = nil
		a.mu.Unlock()
		return err
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		a.cancel()
		a.ctx = nil
		a.cancel = nil
		a.mu.Unlock()
		return err
	}
	if a.config.Stderr != nil {
		command.Stderr = a.config.Stderr
	}
	if err := command.Start(); err != nil {
		a.cancel()
		a.ctx = nil
		a.cancel = nil
		a.mu.Unlock()
		return fmt.Errorf("start ACP agent %q: %w", a.config.Command, err)
	}
	a.cmd = command
	a.stdin = stdin
	a.nextID = 1
	a.mu.Unlock()
	go a.readLoop(stdout)
	go a.waitLoop()

	version := strings.TrimSpace(a.config.Version)
	if version == "" {
		version = "dev"
	}
	result, err := a.call(parent, "initialize", map[string]any{
		"protocolVersion": acpProtocolVersion,
		"clientCapabilities": map[string]any{
			"fs":       map[string]bool{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
		"clientInfo": map[string]string{"name": "spynel", "title": "Spynel", "version": version},
	})
	if err != nil {
		_ = a.Close()
		return fmt.Errorf("initialize ACP agent %q: %w", a.config.Command, err)
	}
	var initialized struct {
		ProtocolVersion   int                  `json:"protocolVersion"`
		AgentCapabilities acpAgentCapabilities `json:"agentCapabilities"`
	}
	if err := json.Unmarshal(result, &initialized); err != nil {
		_ = a.Close()
		return fmt.Errorf("decode ACP initialize response: %w", err)
	}
	if initialized.ProtocolVersion != acpProtocolVersion {
		_ = a.Close()
		return fmt.Errorf("ACP agent selected protocol version %d; Spynel requires stable v1", initialized.ProtocolVersion)
	}
	a.mu.Lock()
	a.caps = initialized.AgentCapabilities
	a.mu.Unlock()
	return nil
}

func (a *ACP) Send(ctx context.Context, key, prompt string, emit core.Emit) (string, bool, error) {
	a.mu.Lock()
	model := a.config.Model
	a.mu.Unlock()
	return a.SendWithModel(ctx, key, prompt, model, emit)
}

func (a *ACP) SetModel(model string) {
	a.mu.Lock()
	a.config.Model = model
	a.mu.Unlock()
}

func (a *ACP) SendWithModel(ctx context.Context, key, prompt, model string, emit core.Emit) (string, bool, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", false, errors.New("harness prompt is empty")
	}
	lock := a.lockForKey(key)
	lock.Lock()
	defer lock.Unlock()
	session, err := a.ensureSession(ctx, key, model)
	if err != nil {
		return "", false, err
	}
	turn := &acpTurn{emit: emit}
	a.mu.Lock()
	if a.closed || a.ctx == nil {
		a.mu.Unlock()
		return session.ID, false, errors.New("ACP harness is not running")
	}
	if a.active[key] != nil {
		a.mu.Unlock()
		return session.ID, false, errors.New("ACP session already has an active prompt turn")
	}
	a.active[key] = turn
	a.mu.Unlock()
	_, waiter, err := a.beginCall("session/prompt", map[string]any{
		"sessionId": session.ID,
		"prompt":    []map[string]string{{"type": "text", "text": prompt}},
	})
	if err != nil {
		a.mu.Lock()
		if a.active[key] == turn {
			delete(a.active, key)
		}
		a.mu.Unlock()
		return session.ID, false, err
	}
	if emit != nil {
		emit(core.Event{Kind: core.EventStatus, Text: "ACP turn started", ThreadID: session.ID,
			Execution: &core.ExecutionStatus{State: "running"}})
	}
	go a.awaitPrompt(key, session.ID, turn, waiter)
	return session.ID, false, nil
}

func (a *ACP) awaitPrompt(key, sessionID string, turn *acpTurn, waiter <-chan acpResponse) {
	a.mu.Lock()
	ctx := a.ctx
	a.mu.Unlock()
	var response acpResponse
	select {
	case response = <-waiter:
	case <-ctx.Done():
		response.err = ctx.Err()
	}
	if response.err != nil {
		a.finishPrompt(key, sessionID, turn, "", response.err)
		return
	}
	var result struct {
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(response.result, &result); err != nil {
		a.finishPrompt(key, sessionID, turn, "", fmt.Errorf("decode ACP prompt response: %w", err))
		return
	}
	if result.StopReason == "cancelled" {
		a.finishPrompt(key, sessionID, turn, result.StopReason, errors.New("ACP turn cancelled"))
		return
	}
	a.finishPrompt(key, sessionID, turn, result.StopReason, nil)
}

func (a *ACP) finishPrompt(key, sessionID string, turn *acpTurn, stopReason string, turnErr error) {
	turn.mu.Lock()
	if turn.completed {
		turn.mu.Unlock()
		return
	}
	turn.completed = true
	text := turn.text.String()
	emit := turn.emit
	turn.mu.Unlock()
	a.mu.Lock()
	if a.active[key] == turn {
		delete(a.active, key)
	}
	a.mu.Unlock()
	if emit == nil {
		return
	}
	if turnErr != nil {
		emit(core.Event{Kind: core.EventError, Text: turnErr.Error(), ThreadID: sessionID, Done: true,
			Execution: &core.ExecutionStatus{State: "error", Detail: turnErr.Error()}})
		return
	}
	finalText := text
	detail := stopReason
	if detail == "" {
		detail = "end_turn"
	}
	emit(core.Event{Kind: core.EventFinal, Text: text, FinalText: &finalText, ThreadID: sessionID, Done: true,
		Execution: &core.ExecutionStatus{State: "finishing", Detail: detail}})
}

func (a *ACP) Interrupt(_ context.Context, key string) (bool, error) {
	lock := a.lockForKey(key)
	lock.Lock()
	defer lock.Unlock()
	a.mu.Lock()
	turn := a.active[key]
	session := a.sessions[key]
	a.mu.Unlock()
	if turn == nil {
		return false, nil
	}
	turn.mu.Lock()
	turn.cancelled = true
	turn.mu.Unlock()
	if err := a.notify("session/cancel", map[string]any{"sessionId": session.ID}); err != nil {
		return false, fmt.Errorf("cancel ACP turn: %w", err)
	}
	return true, nil
}

func (a *ACP) ResetSession(key string) error {
	lock := a.lockForKey(key)
	lock.Lock()
	defer lock.Unlock()
	a.mu.Lock()
	if a.active[key] != nil {
		a.mu.Unlock()
		return errors.New("cannot reset an ACP session while its turn is active")
	}
	delete(a.sessions, key)
	delete(a.live, key)
	err := a.saveSessionsLocked()
	a.mu.Unlock()
	return err
}

func (a *ACP) ThreadID(key string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[key].ID
}

func (a *ACP) IsActive(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.active[key] != nil
}

func (a *ACP) Close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	cancel := a.cancel
	stdin := a.stdin
	a.stdin = nil
	a.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	if cancel != nil {
		cancel()
	}
	return nil
}

func (a *ACP) ensureSession(ctx context.Context, key, model string) (acpSession, error) {
	a.mu.Lock()
	if a.closed || a.ctx == nil {
		a.mu.Unlock()
		return acpSession{}, errors.New("ACP harness is not running")
	}
	cfg := a.config
	cfg.Model = model
	session := a.sessions[key]
	if session.Policy != acpSessionPolicy(cfg) {
		session = acpSession{}
		delete(a.sessions, key)
		delete(a.live, key)
	}
	if session.ID != "" && a.live[key] {
		a.mu.Unlock()
		return session, nil
	}
	caps := a.caps
	a.mu.Unlock()

	params := map[string]any{"cwd": cfg.Cwd, "mcpServers": []any{}}
	method := "session/new"
	if session.ID != "" {
		params["sessionId"] = session.ID
		switch {
		case capabilityPresent(caps.SessionCapabilities.Resume):
			method = "session/resume"
		case caps.LoadSession:
			method = "session/load"
		default:
			session = acpSession{}
			delete(params, "sessionId")
		}
	}
	usedMethod := method
	result, err := a.call(ctx, method, params)
	if err != nil && method != "session/new" {
		delete(params, "sessionId")
		usedMethod = "session/new"
		result, err = a.call(ctx, "session/new", params)
	}
	if err != nil {
		return acpSession{}, fmt.Errorf("create or resume ACP session: %w", err)
	}
	var setup acpSessionResult
	if err := json.Unmarshal(result, &setup); err != nil {
		return acpSession{}, fmt.Errorf("decode ACP session response: %w", err)
	}
	if setup.SessionID != "" {
		session.ID = setup.SessionID
	}
	if session.ID == "" {
		return acpSession{}, errors.New("ACP session response omitted sessionId")
	}
	if err := a.applySessionOptions(ctx, session.ID, setup.ConfigOptions, usedMethod == "session/new", cfg); err != nil {
		return acpSession{}, err
	}
	session.Policy = acpSessionPolicy(cfg)
	a.mu.Lock()
	a.sessions[key] = session
	a.live[key] = true
	err = a.saveSessionsLocked()
	a.mu.Unlock()
	if err != nil {
		return acpSession{}, err
	}
	return session, nil
}

func capabilityPresent(value json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(value))
	return trimmed != "" && trimmed != "null" && trimmed != "false"
}

func (a *ACP) applySessionOptions(ctx context.Context, sessionID string, options []acpConfigOption, requireModel bool, cfg HarnessConfig) error {
	modelSet := strings.TrimSpace(cfg.Model) == ""
	effortSet := false
	for _, option := range options {
		value := ""
		switch option.Category {
		case "model":
			if modelSet {
				continue
			}
			value = strings.TrimSpace(cfg.Model)
		case "thought_level":
			if effortSet {
				continue
			}
			value = strings.TrimSpace(cfg.Effort)
		}
		if value == "" || option.Type != "select" {
			continue
		}
		if _, err := a.call(ctx, "session/set_config_option", map[string]any{
			"sessionId": sessionID, "configId": option.ID, "value": value,
		}); err != nil {
			return fmt.Errorf("set ACP %s option %q: %w", option.Category, option.ID, err)
		}
		if option.Category == "model" {
			modelSet = true
		}
		if option.Category == "thought_level" {
			effortSet = true
		}
	}
	if requireModel && !modelSet {
		return fmt.Errorf("ACP agent does not expose a model config option; clear harness.model or configure the agent command itself")
	}
	return nil
}

func (a *ACP) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	key, waiter, err := a.beginCall(method, params)
	if err != nil {
		return nil, err
	}
	select {
	case response := <-waiter:
		return response.result, response.err
	case <-ctx.Done():
		a.mu.Lock()
		if a.pending[key] == waiter {
			delete(a.pending, key)
		}
		a.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (a *ACP) beginCall(method string, params any) (string, <-chan acpResponse, error) {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	a.mu.Lock()
	if a.closed || a.stdin == nil {
		a.mu.Unlock()
		return "", nil, errors.New("ACP agent stdin is closed")
	}
	id := a.nextID
	a.nextID++
	key := strconv.FormatUint(id, 10)
	waiter := make(chan acpResponse, 1)
	a.pending[key] = waiter
	stdin := a.stdin
	a.mu.Unlock()
	err := writeACP(stdin, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		a.mu.Lock()
		delete(a.pending, key)
		a.mu.Unlock()
		return "", nil, err
	}
	return key, waiter, nil
}

func (a *ACP) notify(method string, params any) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	a.mu.Lock()
	if a.closed || a.stdin == nil {
		a.mu.Unlock()
		return errors.New("ACP agent stdin is closed")
	}
	stdin := a.stdin
	a.mu.Unlock()
	return writeACP(stdin, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func writeACP(writer io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = writer.Write(append(data, '\n'))
	return err
}

func (a *ACP) readLoop(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), acpMaxRecord)
	for scanner.Scan() {
		var envelope acpEnvelope
		if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil {
			a.fail(fmt.Errorf("ACP agent emitted incompatible non-JSON output: %w", err))
			return
		}
		if envelope.JSONRPC != "2.0" {
			a.fail(fmt.Errorf("ACP agent emitted a message without jsonrpc 2.0"))
			return
		}
		if envelope.Method != "" {
			a.handleAgentMessage(envelope)
			continue
		}
		key := acpIDKey(envelope.ID)
		a.mu.Lock()
		waiter := a.pending[key]
		delete(a.pending, key)
		a.mu.Unlock()
		if waiter != nil {
			response := acpResponse{result: envelope.Result}
			if envelope.Error != nil {
				response.err = envelope.Error
			}
			waiter <- response
		}
	}
	err := scanner.Err()
	if err == nil {
		err = errors.New("ACP agent stdout closed")
	}
	a.fail(err)
}

func acpIDKey(id json.RawMessage) string {
	return strings.Trim(string(id), "\"")
}

func (a *ACP) handleAgentMessage(envelope acpEnvelope) {
	switch envelope.Method {
	case "session/update":
		a.handleSessionUpdate(envelope.Params)
	case "session/request_permission":
		a.handlePermissionRequest(envelope.ID, envelope.Params)
	default:
		if len(envelope.ID) != 0 {
			_ = a.respondError(envelope.ID, -32601, "method not supported by Spynel")
		}
	}
}

func (a *ACP) handleSessionUpdate(raw json.RawMessage) {
	var params struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string          `json:"sessionUpdate"`
			Content       json.RawMessage `json:"content"`
			Title         string          `json:"title"`
			Kind          string          `json:"kind"`
			Status        string          `json:"status"`
			ToolCallID    string          `json:"toolCallId"`
		} `json:"update"`
	}
	if json.Unmarshal(raw, &params) != nil {
		return
	}
	_, turn := a.turnForSession(params.SessionID)
	if turn == nil {
		return
	}
	switch params.Update.SessionUpdate {
	case "agent_message_chunk":
		var content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(params.Update.Content, &content) == nil && content.Type == "text" && content.Text != "" {
			turn.mu.Lock()
			turn.text.WriteString(content.Text)
			emit := turn.emit
			turn.mu.Unlock()
			if emit != nil {
				emit(core.Event{Kind: core.EventDelta, Text: content.Text, ThreadID: params.SessionID})
			}
		}
	case "tool_call", "tool_call_update":
		detail := strings.TrimSpace(params.Update.Title)
		if detail == "" {
			detail = strings.TrimSpace(params.Update.Kind)
		}
		if detail == "" {
			detail = "tool"
		}
		toolStatus := strings.TrimSpace(params.Update.Status)
		executionDetail := detail
		if toolStatus != "" {
			executionDetail += " (" + toolStatus + ")"
		}
		turn.mu.Lock()
		emit := turn.emit
		turn.mu.Unlock()
		if emit != nil {
			emit(core.Event{Kind: core.EventStatus, Text: "ACP tool: " + detail, ThreadID: params.SessionID,
				Execution: &core.ExecutionStatus{State: "running", Detail: executionDetail}})
		}
	}
}

func (a *ACP) turnForSession(sessionID string) (string, *acpTurn) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, turn := range a.active {
		if a.sessions[key].ID == sessionID {
			return key, turn
		}
	}
	return "", nil
}

func (a *ACP) handlePermissionRequest(id, raw json.RawMessage) {
	var params struct {
		SessionID string `json:"sessionId"`
		ToolCall  struct {
			Kind string `json:"kind"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	if json.Unmarshal(raw, &params) != nil {
		_ = a.respondError(id, -32602, "invalid permission request")
		return
	}
	_, turn := a.turnForSession(params.SessionID)
	active := false
	allow := false
	if turn != nil {
		turn.mu.Lock()
		active = !turn.cancelled && !turn.completed
		turn.mu.Unlock()
		allow = active && a.permissionAllowed(params.ToolCall.Kind)
	}
	preferred := []string{"reject_once", "reject_always"}
	if allow {
		preferred = []string{"allow_once", "allow_always"}
	}
	selected := ""
	if active {
		for _, kind := range preferred {
			for _, option := range params.Options {
				if option.Kind == kind {
					selected = option.OptionID
					break
				}
			}
			if selected != "" {
				break
			}
		}
	}
	outcome := map[string]any{"outcome": "cancelled"}
	if selected != "" {
		outcome = map[string]any{"outcome": "selected", "optionId": selected}
	}
	_ = a.respondResult(id, map[string]any{"outcome": outcome})
}

func (a *ACP) permissionAllowed(kind string) bool {
	if a.config.Sandbox != "read-only" {
		return true
	}
	switch kind {
	case "read", "search", "think":
		return true
	case "fetch":
		return a.config.Network
	default:
		return false
	}
}

func (a *ACP) respondResult(id json.RawMessage, result any) error {
	return a.respond(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result})
}

func (a *ACP) respondError(id json.RawMessage, code int, message string) error {
	return a.respond(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "error": map[string]any{"code": code, "message": message}})
}

func (a *ACP) respond(value any) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	a.mu.Lock()
	stdin := a.stdin
	closed := a.closed
	a.mu.Unlock()
	if closed || stdin == nil {
		return errors.New("ACP agent stdin is closed")
	}
	return writeACP(stdin, value)
}

func (a *ACP) waitLoop() {
	err := a.cmd.Wait()
	if err == nil {
		err = errors.New("ACP agent process exited")
	}
	a.fail(err)
}

func (a *ACP) fail(err error) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	pending := a.pending
	a.pending = map[string]chan acpResponse{}
	active := a.active
	a.active = map[string]*acpTurn{}
	a.stdin = nil
	cancel := a.cancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, waiter := range pending {
		waiter <- acpResponse{err: err}
	}
	for key, turn := range active {
		a.mu.Lock()
		sessionID := a.sessions[key].ID
		a.mu.Unlock()
		turn.mu.Lock()
		if turn.completed {
			turn.mu.Unlock()
			continue
		}
		turn.completed = true
		emit := turn.emit
		turn.mu.Unlock()
		if emit != nil {
			emit(core.Event{Kind: core.EventError, Text: err.Error(), ThreadID: sessionID, Done: true,
				Execution: &core.ExecutionStatus{State: "error", Detail: err.Error()}})
		}
	}
}

func (a *ACP) lockForKey(key string) *sync.Mutex {
	a.keyMu.Lock()
	defer a.keyMu.Unlock()
	lock := a.keyLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		a.keyLocks[key] = lock
	}
	return lock
}

func acpSessionPolicy(cfg HarnessConfig) string {
	arguments, _ := json.Marshal(cfg.Args)
	environment, _ := json.Marshal(cfg.Env)
	return strings.Join([]string{
		cfg.Command, string(arguments), string(environment), cfg.Cwd, cfg.Model, cfg.Effort,
		cfg.Sandbox, strconv.FormatBool(cfg.Network),
	}, "\x1f")
}

func (a *ACP) loadSessions() error {
	if a.config.SessionsFile == "" {
		return nil
	}
	data, err := os.ReadFile(a.config.SessionsFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &a.sessions); err != nil {
		return fmt.Errorf("decode ACP session map: %w", err)
	}
	return nil
}

func (a *ACP) saveSessionsLocked() error {
	if a.config.SessionsFile == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(a.config.SessionsFile), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(a.sessions, "", "  ")
	if err != nil {
		return err
	}
	return fsx.AtomicWriteFile(a.config.SessionsFile, append(data, '\n'), 0o600)
}
