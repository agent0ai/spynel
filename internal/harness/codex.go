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

type CodexConfig struct {
	Command        string
	Cwd            string
	Model          string
	Effort         string
	ApprovalPolicy string
	Sandbox        string
	Network        bool
	SessionsFile   string
	Version        string
	Stderr         io.Writer
}

type Codex struct {
	config CodexConfig
	ctx    context.Context
	cancel context.CancelFunc
	cmd    *exec.Cmd
	stdin  io.WriteCloser

	writeMu  sync.Mutex
	keyMu    sync.Mutex
	keyLocks map[string]*sync.Mutex
	mu       sync.Mutex
	nextID   int
	pending  map[int]chan rpcResponse
	session  map[string]string
	loaded   map[string]bool
	active   map[string]*turnState
	deferred map[string][]wireMessage
	closed   bool
}

func (*Codex) FollowUpMode() FollowUpMode { return FollowUpSteer }

type turnState struct {
	key              string
	threadID         string
	turnID           string
	emitMu           sync.RWMutex
	emit             core.Emit
	messages         []string
	currentMessage   strings.Builder
	separatorPending bool
}

func (s *turnState) emitEvent(event core.Event) {
	s.emitMu.RLock()
	emit := s.emit
	s.emitMu.RUnlock()
	if emit != nil {
		emit(event)
	}
}

func (s *turnState) replaceEmit(emit core.Emit) core.Emit {
	s.emitMu.Lock()
	previous := s.emit
	s.emit = emit
	s.emitMu.Unlock()
	return previous
}

type rpcResponse struct {
	Result json.RawMessage
	Error  *rpcError
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type codexModelList struct {
	Data []struct {
		ID                     string `json:"id"`
		Model                  string `json:"model"`
		DisplayName            string `json:"displayName"`
		DefaultReasoningEffort string `json:"defaultReasoningEffort"`
		SupportedEfforts       []struct {
			Effort      string `json:"reasoningEffort"`
			Description string `json:"description"`
		} `json:"supportedReasoningEfforts"`
		IsDefault bool `json:"isDefault"`
	} `json:"data"`
	NextCursor *string `json:"nextCursor"`
}

type wireMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

func NewCodex(cfg CodexConfig) (*Codex, error) {
	if cfg.Command == "" {
		cfg.Command = "codex"
	}
	if cfg.Cwd == "" {
		cfg.Cwd = "."
	}
	if cfg.ApprovalPolicy == "" {
		cfg.ApprovalPolicy = "never"
	}
	if cfg.Sandbox == "" {
		cfg.Sandbox = "dangerFullAccess"
	}
	c := &Codex{
		config: cfg, nextID: 1, pending: map[int]chan rpcResponse{}, session: map[string]string{},
		loaded: map[string]bool{}, active: map[string]*turnState{}, deferred: map[string][]wireMessage{},
		keyLocks: map[string]*sync.Mutex{},
	}
	if err := c.loadSessions(); err != nil {
		return nil, err
	}
	return c, nil
}

// Models uses Codex app-server's picker-visible model catalog instead of a
// static list, so account-specific availability and future models are honored.
func (c *Codex) Models(ctx context.Context) ([]Model, error) {
	var models []Model
	var cursor string
	for {
		params := map[string]any{"limit": 100, "includeHidden": false}
		if cursor != "" {
			params["cursor"] = cursor
		}
		result, err := c.call(ctx, "model/list", params)
		if err != nil {
			return nil, fmt.Errorf("list Codex models: %w", err)
		}
		var page codexModelList
		if err := json.Unmarshal(result, &page); err != nil {
			return nil, fmt.Errorf("decode Codex model catalog: %w", err)
		}
		for _, item := range page.Data {
			id := item.Model
			if id == "" {
				id = item.ID
			}
			if id == "" {
				continue
			}
			model := Model{ID: id, DisplayName: item.DisplayName, DefaultEffort: item.DefaultReasoningEffort, Default: item.IsDefault}
			if model.DisplayName == "" {
				model.DisplayName = id
			}
			for _, effort := range item.SupportedEfforts {
				model.Efforts = append(model.Efforts, effort.Effort)
			}
			models = append(models, model)
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			break
		}
		cursor = *page.NextCursor
	}
	return models, nil
}

func (c *Codex) Start(parent context.Context) error {
	c.mu.Lock()
	if c.cmd != nil {
		c.mu.Unlock()
		return nil
	}
	c.ctx, c.cancel = context.WithCancel(parent)
	c.cmd = exec.CommandContext(c.ctx, c.config.Command, "app-server", "--stdio")
	c.cmd.Dir = c.config.Cwd
	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		c.mu.Unlock()
		return err
	}
	stdin, err := c.cmd.StdinPipe()
	if err != nil {
		c.mu.Unlock()
		return err
	}
	c.stdin = stdin
	if c.config.Stderr != nil {
		c.cmd.Stderr = c.config.Stderr
	}
	if err := c.cmd.Start(); err != nil {
		c.cmd = nil
		c.mu.Unlock()
		return fmt.Errorf("start codex app-server: %w", err)
	}
	c.mu.Unlock()
	go c.readLoop(stdout)
	go c.waitLoop()

	params := map[string]any{
		"clientInfo":   map[string]any{"name": "spynel", "title": "Spynel", "version": c.config.Version},
		"capabilities": map[string]any{"experimentalApi": false},
	}
	if _, err := c.call(parent, "initialize", params); err != nil {
		_ = c.Close()
		return fmt.Errorf("initialize codex app-server: %w", err)
	}
	return c.notify("initialized", map[string]any{})
}

func (c *Codex) Send(ctx context.Context, key, prompt string, emit core.Emit) (string, bool, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", false, errors.New("harness prompt is empty")
	}
	lock := c.lockForKey(key)
	lock.Lock()
	defer lock.Unlock()
	threadID, err := c.ensureThread(ctx, key)
	if err != nil {
		return "", false, err
	}
	c.mu.Lock()
	active := c.active[threadID]
	c.mu.Unlock()
	if active != nil {
		previousEmit := active.replaceEmit(emit)
		params := map[string]any{
			"threadId":       threadID,
			"expectedTurnId": active.turnID,
			"input":          []map[string]any{{"type": "text", "text": prompt}},
		}
		if _, err := c.call(ctx, "turn/steer", params); err != nil {
			c.mu.Lock()
			stillActive := c.active[threadID] == active
			c.mu.Unlock()
			if stillActive {
				active.replaceEmit(previousEmit)
			}
			return threadID, true, err
		}
		// The newest message owns the rest of the streamed response. Release
		// transport-local activity associated with the older message without
		// declaring the underlying harness turn complete.
		if previousEmit != nil {
			previousEmit(core.Event{Kind: core.EventStatus, Done: true, ThreadID: threadID, TurnID: active.turnID})
		}
		if emit != nil {
			emit(core.Event{Kind: core.EventStatus, Text: "Steering active Codex turn", ThreadID: threadID, TurnID: active.turnID})
		}
		return threadID, true, nil
	}

	params := map[string]any{
		"threadId":       threadID,
		"input":          []map[string]any{{"type": "text", "text": prompt}},
		"cwd":            c.config.Cwd,
		"approvalPolicy": c.config.ApprovalPolicy,
		"sandboxPolicy":  c.sandboxPolicy(),
	}
	if c.config.Model != "" {
		params["model"] = c.config.Model
	}
	if c.config.Effort != "" {
		params["effort"] = c.config.Effort
	}
	result, err := c.call(ctx, "turn/start", params)
	if err != nil {
		return threadID, false, err
	}
	var response struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(result, &response); err != nil || response.Turn.ID == "" {
		return threadID, false, fmt.Errorf("invalid turn/start response: %w", err)
	}
	state := &turnState{key: key, threadID: threadID, turnID: response.Turn.ID, emit: emit}
	c.mu.Lock()
	c.active[threadID] = state
	deferred := c.deferred[response.Turn.ID]
	delete(c.deferred, response.Turn.ID)
	c.mu.Unlock()
	for _, message := range deferred {
		c.handleNotification(message)
	}
	if emit != nil {
		emit(core.Event{Kind: core.EventStatus, Text: "Codex turn started", ThreadID: threadID, TurnID: response.Turn.ID})
	}
	return threadID, false, nil
}

func (c *Codex) ensureThread(ctx context.Context, key string) (string, error) {
	c.mu.Lock()
	threadID := c.session[key]
	loaded := c.loaded[threadID]
	c.mu.Unlock()
	if threadID != "" && loaded {
		return threadID, nil
	}
	if threadID != "" {
		result, err := c.call(ctx, "thread/resume", map[string]any{"threadId": threadID})
		if err == nil {
			var response struct {
				Thread struct {
					ID string `json:"id"`
				} `json:"thread"`
			}
			if json.Unmarshal(result, &response) == nil && response.Thread.ID != "" {
				c.mu.Lock()
				c.loaded[response.Thread.ID] = true
				c.mu.Unlock()
				return response.Thread.ID, nil
			}
		}
	}
	params := map[string]any{
		"cwd":            c.config.Cwd,
		"approvalPolicy": c.config.ApprovalPolicy,
		"sandbox":        c.threadSandbox(),
		"serviceName":    "spynel",
	}
	if c.config.Model != "" {
		params["model"] = c.config.Model
	}
	result, err := c.call(ctx, "thread/start", params)
	if err != nil {
		return "", err
	}
	var response struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(result, &response); err != nil || response.Thread.ID == "" {
		return "", fmt.Errorf("invalid thread/start response: %w", err)
	}
	c.mu.Lock()
	c.session[key] = response.Thread.ID
	c.loaded[response.Thread.ID] = true
	err = c.saveSessionsLocked()
	c.mu.Unlock()
	return response.Thread.ID, err
}

func (c *Codex) sandboxPolicy() map[string]any {
	sandbox := c.threadSandbox()
	typeName := "workspaceWrite"
	if sandbox == "read-only" {
		typeName = "readOnly"
	} else if sandbox == "danger-full-access" {
		typeName = "dangerFullAccess"
	}
	result := map[string]any{"type": typeName}
	if sandbox == "workspace-write" {
		result["writableRoots"] = []string{c.config.Cwd}
		result["networkAccess"] = c.config.Network
	}
	return result
}

func (c *Codex) threadSandbox() string {
	switch c.config.Sandbox {
	case "readOnly", "read-only":
		return "read-only"
	case "dangerFullAccess", "danger-full-access":
		return "danger-full-access"
	default:
		return "workspace-write"
	}
}

func (c *Codex) ThreadID(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session[key]
}

func (c *Codex) IsActive(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	threadID := c.session[key]
	return c.active[threadID] != nil
}

func (c *Codex) Interrupt(ctx context.Context, key string) (bool, error) {
	lock := c.lockForKey(key)
	lock.Lock()
	defer lock.Unlock()
	c.mu.Lock()
	threadID := c.session[key]
	state := c.active[threadID]
	c.mu.Unlock()
	if state == nil {
		return false, nil
	}
	_, err := c.call(ctx, "turn/interrupt", map[string]any{
		"threadId": state.threadID,
		"turnId":   state.turnID,
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

func (c *Codex) ResetSession(key string) error {
	lock := c.lockForKey(key)
	lock.Lock()
	defer lock.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if threadID := c.session[key]; threadID != "" && c.active[threadID] != nil {
		return errors.New("cannot reset a session while its turn is active")
	}
	delete(c.session, key)
	return c.saveSessionsLocked()
}

func (c *Codex) lockForKey(key string) *sync.Mutex {
	c.keyMu.Lock()
	defer c.keyMu.Unlock()
	lock := c.keyLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		c.keyLocks[key] = lock
	}
	return lock
}

func (c *Codex) handleNotification(message wireMessage) {
	var params map[string]any
	if json.Unmarshal(message.Params, &params) != nil {
		return
	}
	threadID, _ := params["threadId"].(string)
	turnID, _ := params["turnId"].(string)
	if turn, ok := params["turn"].(map[string]any); ok {
		if id, ok := turn["id"].(string); ok {
			turnID = id
		}
	}
	c.mu.Lock()
	state := c.active[threadID]
	if state == nil && turnID != "" {
		for _, candidate := range c.active {
			if candidate.turnID == turnID {
				state = candidate
				threadID = candidate.threadID
				break
			}
		}
	}
	c.mu.Unlock()
	if state == nil {
		if turnID != "" {
			c.mu.Lock()
			c.deferred[turnID] = append(c.deferred[turnID], message)
			c.mu.Unlock()
		}
		return
	}
	switch message.Method {
	case "item/agentMessage/delta":
		delta, _ := params["delta"].(string)
		if delta != "" {
			if state.separatorPending {
				state.emitEvent(core.Event{Kind: core.EventDelta, Text: "\n", ThreadID: state.threadID, TurnID: state.turnID})
			}
			state.emitEvent(core.Event{Kind: core.EventDelta, Text: delta, ThreadID: state.threadID, TurnID: state.turnID})
			state.separatorPending = false
			state.currentMessage.WriteString(delta)
		}
	case "item/completed":
		item, _ := params["item"].(map[string]any)
		if item["type"] == "agentMessage" {
			text, _ := item["text"].(string)
			if text == "" {
				text = state.currentMessage.String()
			}
			if text != "" {
				state.messages = append(state.messages, text)
				state.separatorPending = true
			}
			state.currentMessage.Reset()
		}
	case "error":
		errorObject, _ := params["error"].(map[string]any)
		text, _ := errorObject["message"].(string)
		state.emitEvent(core.Event{Kind: core.EventError, Text: text, ThreadID: state.threadID, TurnID: state.turnID})
	case "turn/completed":
		turn, _ := params["turn"].(map[string]any)
		status, _ := turn["status"].(string)
		messages := append([]string(nil), state.messages...)
		if text := state.currentMessage.String(); text != "" {
			messages = append(messages, text)
		}
		text := strings.Join(messages, "\n")
		finalText := ""
		if len(messages) > 0 {
			finalText = messages[len(messages)-1]
		}
		kind := core.EventFinal
		if status == "failed" {
			kind = core.EventError
			if errObject, ok := turn["error"].(map[string]any); ok {
				if message, ok := errObject["message"].(string); ok && message != "" {
					text = message
				}
			}
			finalText = text
		}
		state.emitEvent(core.Event{Kind: kind, Text: text, FinalText: &finalText, ThreadID: state.threadID, TurnID: state.turnID, Done: true})
		c.mu.Lock()
		delete(c.active, state.threadID)
		c.mu.Unlock()
	default:
		// Keep internal tool and lifecycle method names out of user-facing status.
	}
}

func (c *Codex) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	if c.closed || c.stdin == nil {
		c.mu.Unlock()
		return nil, errors.New("codex app-server is not running")
	}
	id := c.nextID
	c.nextID++
	response := make(chan rpcResponse, 1)
	c.pending[id] = response
	c.mu.Unlock()
	if err := c.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	select {
	case result := <-response:
		if result.Error != nil {
			return nil, fmt.Errorf("%s (%d)", result.Error.Message, result.Error.Code)
		}
		return result.Result, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (c *Codex) notify(method string, params any) error {
	return c.write(map[string]any{"method": method, "params": params})
}

func (c *Codex) write(message any) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	stdin := c.stdin
	c.mu.Unlock()
	if stdin == nil {
		return errors.New("codex app-server stdin is closed")
	}
	_, err = stdin.Write(append(data, '\n'))
	return err
}

func (c *Codex) readLoop(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var message wireMessage
		if json.Unmarshal(scanner.Bytes(), &message) != nil {
			continue
		}
		if len(message.ID) > 0 && message.Method == "" {
			id, err := strconv.Atoi(strings.Trim(string(message.ID), "\""))
			if err != nil {
				continue
			}
			c.mu.Lock()
			waiter := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if waiter != nil {
				waiter <- rpcResponse{Result: message.Result, Error: message.Error}
			}
			continue
		}
		if len(message.ID) > 0 && message.Method != "" {
			// Spynel defaults to non-interactive approval policy. Decline any
			// unexpected server request instead of granting hidden authority.
			_ = c.write(map[string]any{"id": json.RawMessage(message.ID), "result": "decline"})
			continue
		}
		if message.Method != "" {
			c.handleNotification(message)
		}
	}
	c.failAll(fmt.Errorf("codex app-server stream closed: %v", scanner.Err()))
}

func (c *Codex) waitLoop() {
	c.mu.Lock()
	cmd := c.cmd
	c.mu.Unlock()
	if cmd == nil {
		return
	}
	err := cmd.Wait()
	if err == nil {
		err = errors.New("codex app-server exited")
	}
	c.failAll(err)
}

func (c *Codex) failAll(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	pending := c.pending
	active := c.active
	c.pending = map[int]chan rpcResponse{}
	c.active = map[string]*turnState{}
	c.mu.Unlock()
	for _, waiter := range pending {
		waiter <- rpcResponse{Error: &rpcError{Code: -1, Message: err.Error()}}
	}
	for _, state := range active {
		state.emitEvent(core.Event{Kind: core.EventError, Text: err.Error(), ThreadID: state.threadID, TurnID: state.turnID, Done: true})
	}
}

func (c *Codex) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	stdin := c.stdin
	c.stdin = nil
	cancel := c.cancel
	c.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	if cancel != nil {
		cancel()
	}
	return nil
}

func (c *Codex) loadSessions() error {
	if c.config.SessionsFile == "" {
		return nil
	}
	data, err := os.ReadFile(c.config.SessionsFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &c.session)
}

func (c *Codex) saveSessionsLocked() error {
	if c.config.SessionsFile == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.config.SessionsFile), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c.session, "", "  ")
	if err != nil {
		return err
	}
	return fsx.AtomicWriteFile(c.config.SessionsFile, append(data, '\n'), 0o600)
}
