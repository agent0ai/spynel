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
	"time"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/fsx"
)

const piRPCMaxRecord = 16 * 1024 * 1024

// Pi is a native adapter for Pi's documented JSONL RPC mode. Pi owns one
// current session per process, so Spynel keeps one idle-capable process per
// active conversation and persists its session file for restart/resume.
type Pi struct {
	config HarnessConfig

	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	closed    bool
	processes map[string]*piProcess
	sessions  map[string]piSession
	keyMu     sync.Mutex
	keyLocks  map[string]*sync.Mutex
}

type piSession struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Policy string `json:"policy"`
}

type piProcess struct {
	owner  *Pi
	key    string
	cmd    *exec.Cmd
	cancel context.CancelFunc
	stdin  io.WriteCloser

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  uint64
	pending map[string]chan piResponse
	active  *piTurn
	session piSession
	closed  bool
}

type piTurn struct {
	emit core.Emit

	mu             sync.Mutex
	text           strings.Builder
	currentMessage strings.Builder
	lastMessage    string
	assistantOpen  bool
	errorText      string
	completed      bool
	deliveryMu     sync.Mutex
}

type piResponse struct {
	Data  json.RawMessage
	Error error
}

type piWireMessage struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Command string          `json:"command,omitempty"`
	Success bool            `json:"success,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`
}

type piState struct {
	SessionFile string `json:"sessionFile"`
	SessionID   string `json:"sessionId"`
	IsStreaming bool   `json:"isStreaming"`
}

func NewPi(cfg HarnessConfig) (*Pi, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		cfg.Command = "pi"
	}
	if strings.TrimSpace(cfg.Cwd) == "" {
		cfg.Cwd = "."
	}
	if cfg.Sandbox == "" {
		cfg.Sandbox = "danger-full-access"
	}
	adapter := &Pi{
		config: cfg, processes: map[string]*piProcess{}, sessions: map[string]piSession{}, keyLocks: map[string]*sync.Mutex{},
	}
	if err := adapter.loadSessions(); err != nil {
		return nil, err
	}
	return adapter, nil
}

func (p *Pi) FollowUpMode() FollowUpMode { return FollowUpSteer }

func (p *Pi) Start(parent context.Context) error {
	p.mu.Lock()
	if p.ctx != nil {
		p.mu.Unlock()
		return nil
	}
	if p.closed {
		p.mu.Unlock()
		return errors.New("Pi harness is closed")
	}
	info, err := os.Stat(p.config.Cwd)
	if err != nil || !info.IsDir() {
		p.mu.Unlock()
		return fmt.Errorf("Pi working directory %q is unavailable", p.config.Cwd)
	}
	p.ctx, p.cancel = context.WithCancel(parent)
	checkContext, cancelCheck := context.WithTimeout(p.ctx, 5*time.Second)
	p.mu.Unlock()
	defer cancelCheck()

	output := newTailBuffer(16 * 1024)
	command := exec.CommandContext(checkContext, p.config.Command, "--version")
	command.Dir = p.config.Cwd
	command.Stdout = output
	command.Stderr = output
	if err := command.Run(); err != nil {
		_ = p.Close()
		return fmt.Errorf("Pi executable %q failed --version capability check: %w (%s)", p.config.Command, err, strings.TrimSpace(output.String()))
	}
	if strings.TrimSpace(output.String()) == "" {
		_ = p.Close()
		return fmt.Errorf("Pi executable %q returned an incompatible empty --version result", p.config.Command)
	}
	return nil
}

func (p *Pi) Send(ctx context.Context, key, prompt string, emit core.Emit) (string, bool, error) {
	p.mu.Lock()
	model := p.config.Model
	p.mu.Unlock()
	return p.SendWithModel(ctx, key, prompt, model, emit)
}

func (p *Pi) SetModel(model string) {
	p.mu.Lock()
	p.config.Model = model
	p.mu.Unlock()
}

func (p *Pi) SendWithModel(ctx context.Context, key, prompt, model string, emit core.Emit) (string, bool, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", false, errors.New("harness prompt is empty")
	}
	lock := p.lockForKey(key)
	lock.Lock()
	defer lock.Unlock()
	process, err := p.ensureProcess(ctx, key, model)
	if err != nil {
		return "", false, err
	}
	process.mu.Lock()
	active := process.active
	process.mu.Unlock()
	if active != nil {
		threadID, err := p.steerLocked(ctx, process, prompt, emit, nil)
		return threadID, true, err
	}
	turn := &piTurn{emit: emit}
	process.mu.Lock()
	if process.closed {
		process.mu.Unlock()
		return process.session.ID, false, errors.New("Pi RPC process is closed")
	}
	process.active = turn
	process.mu.Unlock()
	_, err = process.call(ctx, map[string]any{"type": "prompt", "message": prompt}, nil)
	if err != nil {
		process.mu.Lock()
		if process.active == turn {
			process.active = nil
		}
		process.mu.Unlock()
		return process.session.ID, false, fmt.Errorf("Pi rejected prompt: %w", err)
	}
	if emit != nil {
		emit(core.Event{Kind: core.EventStatus, Text: "Pi turn started", ThreadID: process.session.ID,
			Execution: &core.ExecutionStatus{State: "running"}})
	}
	return process.session.ID, false, nil
}

func (p *Pi) Steer(ctx context.Context, key, prompt string, emit core.Emit, beforeDelivery func() bool) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", errors.New("harness prompt is empty")
	}
	lock := p.lockForKey(key)
	lock.Lock()
	defer lock.Unlock()
	p.mu.Lock()
	process := p.processes[key]
	p.mu.Unlock()
	if process == nil {
		return p.ThreadID(key), fmt.Errorf("Pi turn is no longer active: %w", errNativeTurnInactive)
	}
	return p.steerLocked(ctx, process, prompt, emit, beforeDelivery)
}

func (p *Pi) steerLocked(ctx context.Context, process *piProcess, prompt string, emit core.Emit, beforeDelivery func() bool) (string, error) {
	process.mu.Lock()
	turn := process.active
	process.mu.Unlock()
	if turn == nil {
		return process.session.ID, fmt.Errorf("Pi turn is no longer active: %w", errNativeTurnInactive)
	}
	turn.deliveryMu.Lock()
	defer turn.deliveryMu.Unlock()
	turn.mu.Lock()
	completed := turn.completed
	turn.mu.Unlock()
	if completed {
		return process.session.ID, fmt.Errorf("Pi turn is no longer active: %w", errNativeTurnInactive)
	}
	var previous core.Emit
	reserved := false
	_, err := process.call(ctx, map[string]any{"type": "steer", "message": prompt}, func() bool {
		if beforeDelivery != nil && !beforeDelivery() {
			return false
		}
		reserved = true
		turn.mu.Lock()
		previous = turn.emit
		turn.emit = emit
		turn.mu.Unlock()
		return true
	})
	if err != nil {
		turn.mu.Lock()
		if !turn.completed {
			turn.emit = previous
		}
		turn.mu.Unlock()
		if !reserved && beforeDelivery != nil {
			return process.session.ID, errNativeDeliveryUnreserved
		}
		return process.session.ID, err
	}
	if previous != nil {
		previous(core.Event{Kind: core.EventStatus, Text: "Response continued on a newer message", ThreadID: process.session.ID, Done: true})
	}
	if emit != nil {
		emit(core.Event{Kind: core.EventStatus, Text: "Steering active Pi turn", ThreadID: process.session.ID})
	}
	return process.session.ID, nil
}

func (p *Pi) Interrupt(ctx context.Context, key string) (bool, error) {
	lock := p.lockForKey(key)
	lock.Lock()
	defer lock.Unlock()
	p.mu.Lock()
	process := p.processes[key]
	p.mu.Unlock()
	if process == nil {
		return false, nil
	}
	process.mu.Lock()
	active := process.active != nil
	process.mu.Unlock()
	if !active {
		return false, nil
	}
	if _, err := process.call(ctx, map[string]any{"type": "abort"}, nil); err != nil {
		return false, fmt.Errorf("abort Pi turn: %w", err)
	}
	return true, nil
}

func (p *Pi) ResetSession(key string) error {
	lock := p.lockForKey(key)
	lock.Lock()
	defer lock.Unlock()
	p.mu.Lock()
	process := p.processes[key]
	if process != nil {
		process.mu.Lock()
		active := process.active != nil
		process.mu.Unlock()
		if active {
			p.mu.Unlock()
			return errors.New("cannot reset a Pi session while its turn is active")
		}
		delete(p.processes, key)
	}
	delete(p.sessions, key)
	err := p.saveSessionsLocked()
	p.mu.Unlock()
	if process != nil {
		process.close()
	}
	return err
}

func (p *Pi) ThreadID(key string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessions[key].ID
}

func (p *Pi) IsActive(key string) bool {
	p.mu.Lock()
	process := p.processes[key]
	p.mu.Unlock()
	if process == nil {
		return false
	}
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.active != nil
}

func (p *Pi) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	cancel := p.cancel
	processes := make([]*piProcess, 0, len(p.processes))
	for _, process := range p.processes {
		processes = append(processes, process)
	}
	p.processes = map[string]*piProcess{}
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, process := range processes {
		process.close()
	}
	return nil
}

func (p *Pi) Models(ctx context.Context) ([]Model, error) {
	p.mu.Lock()
	model := p.config.Model
	p.mu.Unlock()
	process, err := p.startProcess(ctx, "", piSession{}, true, model)
	if err != nil {
		return nil, err
	}
	defer process.close()
	data, err := process.call(ctx, map[string]any{"type": "get_available_models"}, nil)
	if err != nil {
		return nil, fmt.Errorf("list Pi models: %w", err)
	}
	var response struct {
		Models []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Provider  string `json:"provider"`
			Reasoning bool   `json:"reasoning"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("decode Pi model catalog: %w", err)
	}
	models := make([]Model, 0, len(response.Models))
	for _, item := range response.Models {
		id := item.ID
		if item.Provider != "" && !strings.Contains(id, "/") {
			id = item.Provider + "/" + id
		}
		if id == "" {
			continue
		}
		model := Model{ID: id, DisplayName: item.Name}
		if model.DisplayName == "" {
			model.DisplayName = id
		}
		if item.Reasoning {
			model.Efforts = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}
			model.DefaultEffort = "medium"
		}
		models = append(models, model)
	}
	return models, nil
}

func (p *Pi) ensureProcess(ctx context.Context, key, model string) (*piProcess, error) {
	p.mu.Lock()
	if p.closed || p.ctx == nil {
		p.mu.Unlock()
		return nil, errors.New("Pi harness is not running")
	}
	if process := p.processes[key]; process != nil {
		process.mu.Lock()
		active := process.active != nil
		policyMatches := process.session.Policy == p.sessionPolicyLocked(model)
		process.mu.Unlock()
		if active || policyMatches {
			p.mu.Unlock()
			return process, nil
		}
		delete(p.processes, key)
		p.mu.Unlock()
		process.close()
		p.mu.Lock()
	}
	session := p.sessions[key]
	if session.Policy != p.sessionPolicyLocked(model) {
		session = piSession{}
		delete(p.sessions, key)
	}
	p.mu.Unlock()
	process, err := p.startProcess(ctx, key, session, false, model)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		process.close()
		return nil, errors.New("Pi harness closed while starting a session")
	}
	if existing := p.processes[key]; existing != nil {
		p.mu.Unlock()
		process.close()
		return existing, nil
	}
	p.processes[key] = process
	p.mu.Unlock()
	return process, nil
}

func (p *Pi) startProcess(ctx context.Context, key string, session piSession, ephemeral bool, model string) (*piProcess, error) {
	p.mu.Lock()
	baseContext := p.ctx
	closed := p.closed
	cfg := p.config
	cfg.Model = model
	p.mu.Unlock()
	if ephemeral {
		baseContext = ctx
	}
	if closed || baseContext == nil {
		return nil, errors.New("Pi harness is not running")
	}
	processContext, cancel := context.WithCancel(baseContext)
	args := []string{"--mode", "rpc", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes"}
	if ephemeral {
		args = append(args, "--no-session")
	} else {
		sessionDir := filepath.Join(filepath.Dir(cfg.SessionsFile), "pi-sessions")
		if cfg.SessionsFile == "" {
			sessionDir = filepath.Join(cfg.Cwd, ".spynel", "runtime", "pi-sessions")
		}
		if err := os.MkdirAll(sessionDir, 0o700); err != nil {
			cancel()
			return nil, err
		}
		args = append(args, "--session-dir", sessionDir)
		if session.Path != "" {
			if _, err := os.Stat(session.Path); err == nil {
				args = append(args, "--session", session.Path)
			}
		}
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.Effort != "" {
		args = append(args, "--thinking", cfg.Effort)
	}
	if cfg.Sandbox == "read-only" {
		args = append(args, "--tools", "read,grep,find,ls")
	}
	command := exec.CommandContext(processContext, cfg.Command, args...)
	command.Dir = cfg.Cwd
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if cfg.Stderr != nil {
		command.Stderr = cfg.Stderr
	}
	if err := command.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start Pi RPC process: %w", err)
	}
	process := &piProcess{owner: p, key: key, cmd: command, cancel: cancel, stdin: stdin, nextID: 1, pending: map[string]chan piResponse{}}
	go process.readLoop(stdout)
	go process.waitLoop()
	data, err := process.call(ctx, map[string]any{"type": "get_state"}, nil)
	if err != nil {
		process.close()
		return nil, fmt.Errorf("Pi executable %q failed RPC get_state negotiation: %w", cfg.Command, err)
	}
	var state piState
	if err := json.Unmarshal(data, &state); err != nil || (!ephemeral && (state.SessionID == "" || state.SessionFile == "")) {
		process.close()
		return nil, fmt.Errorf("Pi executable %q returned an incompatible get_state result with missing sessionId or sessionFile", cfg.Command)
	}
	process.session = piSession{ID: state.SessionID, Path: state.SessionFile, Policy: piSessionPolicy(cfg)}
	if _, err := process.call(ctx, map[string]any{"type": "set_steering_mode", "mode": "all"}, nil); err != nil {
		process.close()
		return nil, fmt.Errorf("configure Pi steering queue: %w", err)
	}
	if _, err := process.call(ctx, map[string]any{"type": "set_follow_up_mode", "mode": "all"}, nil); err != nil {
		process.close()
		return nil, fmt.Errorf("configure Pi follow-up queue: %w", err)
	}
	if !ephemeral {
		p.mu.Lock()
		p.sessions[key] = process.session
		err = p.saveSessionsLocked()
		p.mu.Unlock()
		if err != nil {
			process.close()
			return nil, err
		}
	}
	return process, nil
}

func (process *piProcess) call(ctx context.Context, message map[string]any, beforeWrite func() bool) (json.RawMessage, error) {
	process.writeMu.Lock()
	process.mu.Lock()
	if process.closed || process.stdin == nil {
		process.mu.Unlock()
		process.writeMu.Unlock()
		return nil, errors.New("Pi RPC stdin is closed")
	}
	id := "spynel-" + strconv.FormatUint(process.nextID, 10)
	process.nextID++
	waiter := make(chan piResponse, 1)
	process.pending[id] = waiter
	stdin := process.stdin
	process.mu.Unlock()
	if beforeWrite != nil && !beforeWrite() {
		process.mu.Lock()
		delete(process.pending, id)
		process.mu.Unlock()
		process.writeMu.Unlock()
		return nil, errNativeDeliveryUnreserved
	}
	message["id"] = id
	data, err := json.Marshal(message)
	if err == nil {
		_, err = stdin.Write(append(data, '\n'))
	}
	if err != nil {
		process.mu.Lock()
		delete(process.pending, id)
		process.mu.Unlock()
	}
	process.writeMu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case response := <-waiter:
		return response.Data, response.Error
	case <-ctx.Done():
		process.mu.Lock()
		delete(process.pending, id)
		process.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (process *piProcess) readLoop(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), piRPCMaxRecord)
	for scanner.Scan() {
		var envelope piWireMessage
		if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil {
			process.fail(fmt.Errorf("Pi RPC emitted incompatible non-JSON output: %w", err))
			return
		}
		if envelope.Type == "response" {
			process.mu.Lock()
			waiter := process.pending[envelope.ID]
			delete(process.pending, envelope.ID)
			process.mu.Unlock()
			if waiter != nil {
				response := piResponse{Data: envelope.Data}
				if !envelope.Success {
					response.Error = errors.New(emptyPiError(envelope.Error))
				}
				waiter <- response
			}
			continue
		}
		var event map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &event); err == nil {
			process.handleEvent(envelope.Type, event)
		}
	}
	process.fail(fmt.Errorf("Pi RPC stream closed: %v", scanner.Err()))
}

func emptyPiError(value string) string {
	if strings.TrimSpace(value) == "" {
		return "Pi RPC command failed"
	}
	return value
}

func (process *piProcess) handleEvent(kind string, event map[string]json.RawMessage) {
	process.mu.Lock()
	turn := process.active
	process.mu.Unlock()
	if turn == nil {
		return
	}
	switch kind {
	case "message_start":
		var message struct {
			Role string `json:"role"`
		}
		_ = json.Unmarshal(event["message"], &message)
		if message.Role == "assistant" {
			turn.startAssistant(process.session.ID)
		}
	case "message_update":
		var update struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}
		_ = json.Unmarshal(event["assistantMessageEvent"], &update)
		if update.Type == "text_delta" && update.Delta != "" {
			turn.appendText(process.session.ID, update.Delta)
		}
	case "message_end":
		var message struct {
			Role         string          `json:"role"`
			StopReason   string          `json:"stopReason"`
			ErrorMessage string          `json:"errorMessage"`
			Content      json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(event["message"], &message)
		if message.Role == "assistant" {
			turn.finishMessage(process.session.ID, piMessageText(message.Content))
			if message.ErrorMessage != "" || message.StopReason == "error" {
				turn.mu.Lock()
				turn.errorText = message.ErrorMessage
				if turn.errorText == "" {
					turn.errorText = "Pi assistant message failed"
				}
				turn.mu.Unlock()
			}
		}
	case "tool_execution_start":
		var toolName string
		_ = json.Unmarshal(event["toolName"], &toolName)
		turn.emitEvent(core.Event{Kind: core.EventStatus, Text: "Pi tool: " + emptyAsHarness(toolName, "running"), ThreadID: process.session.ID,
			Execution: &core.ExecutionStatus{State: "running", Detail: toolName}})
	case "auto_retry_start", "summarization_retry_scheduled":
		turn.emitEvent(core.Event{Kind: core.EventStatus, Text: "Pi is retrying", ThreadID: process.session.ID,
			Execution: &core.ExecutionStatus{State: "reconnecting"}})
	case "agent_settled":
		process.finishTurn(turn)
	}
}

func piMessageText(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var result strings.Builder
	for _, block := range blocks {
		if block.Type == "text" {
			result.WriteString(block.Text)
		}
	}
	return result.String()
}

func (turn *piTurn) startAssistant(threadID string) {
	turn.mu.Lock()
	prefix := ""
	if turn.text.Len() > 0 && !strings.HasSuffix(turn.text.String(), "\n") {
		prefix = "\n"
		turn.text.WriteByte('\n')
	}
	turn.currentMessage.Reset()
	turn.assistantOpen = true
	emit := turn.emit
	turn.mu.Unlock()
	if emit != nil && prefix != "" {
		emit(core.Event{Kind: core.EventDelta, Text: prefix, ThreadID: threadID})
	}
}

func (turn *piTurn) appendText(threadID, text string) {
	turn.mu.Lock()
	prefix := ""
	if !turn.assistantOpen && turn.text.Len() > 0 {
		turn.text.WriteByte('\n')
		prefix = "\n"
		turn.currentMessage.Reset()
	}
	turn.assistantOpen = true
	turn.text.WriteString(text)
	turn.currentMessage.WriteString(text)
	emit := turn.emit
	turn.mu.Unlock()
	if emit != nil {
		emit(core.Event{Kind: core.EventDelta, Text: prefix + text, ThreadID: threadID})
	}
}

func (turn *piTurn) finishMessage(threadID, authoritative string) {
	turn.mu.Lock()
	current := turn.currentMessage.String()
	missing := ""
	if authoritative != "" && current != authoritative {
		switch {
		case strings.HasPrefix(authoritative, current):
			missing = strings.TrimPrefix(authoritative, current)
		case strings.HasSuffix(current, authoritative):
			// The complete authoritative item was already present in the
			// streamed text, possibly with provider diagnostic prose before it.
		default:
			if current != "" {
				missing = "\n"
			}
			missing += authoritative
		}
		turn.text.WriteString(missing)
		turn.currentMessage.WriteString(missing)
	}
	turn.lastMessage = authoritative
	if turn.lastMessage == "" {
		turn.lastMessage = turn.currentMessage.String()
	}
	turn.assistantOpen = false
	emit := turn.emit
	turn.mu.Unlock()
	if emit != nil && missing != "" {
		emit(core.Event{Kind: core.EventDelta, Text: missing, ThreadID: threadID})
	}
}

func (turn *piTurn) emitEvent(event core.Event) {
	turn.mu.Lock()
	emit := turn.emit
	turn.mu.Unlock()
	if emit != nil {
		emit(event)
	}
}

func (process *piProcess) finishTurn(turn *piTurn) {
	turn.deliveryMu.Lock()
	turn.mu.Lock()
	if turn.completed {
		turn.mu.Unlock()
		turn.deliveryMu.Unlock()
		return
	}
	turn.completed = true
	text := turn.text.String()
	last := turn.lastMessage
	if last == "" {
		last = text
	}
	errorText := turn.errorText
	emit := turn.emit
	turn.mu.Unlock()
	process.mu.Lock()
	if process.active == turn {
		process.active = nil
	}
	process.mu.Unlock()
	turn.deliveryMu.Unlock()
	if emit == nil {
		return
	}
	if errorText != "" {
		emit(core.Event{Kind: core.EventError, Text: errorText, ThreadID: process.session.ID, Done: true,
			Execution: &core.ExecutionStatus{State: "error", Detail: errorText}})
		return
	}
	emit(core.Event{Kind: core.EventFinal, Text: text, FinalText: &last, ThreadID: process.session.ID, Done: true,
		Execution: &core.ExecutionStatus{State: "finishing"}})
}

func (process *piProcess) waitLoop() {
	err := process.cmd.Wait()
	if err == nil {
		err = errors.New("Pi RPC process exited")
	}
	process.fail(err)
}

func (process *piProcess) fail(err error) {
	process.mu.Lock()
	if process.closed {
		process.mu.Unlock()
		return
	}
	process.closed = true
	pending := process.pending
	process.pending = map[string]chan piResponse{}
	turn := process.active
	process.active = nil
	process.stdin = nil
	cancel := process.cancel
	process.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, waiter := range pending {
		waiter <- piResponse{Error: err}
	}
	if turn != nil {
		turn.emitEvent(core.Event{Kind: core.EventError, Text: err.Error(), ThreadID: process.session.ID, Done: true,
			Execution: &core.ExecutionStatus{State: "error", Detail: err.Error()}})
	}
	if process.owner != nil && process.key != "" {
		process.owner.mu.Lock()
		if process.owner.processes[process.key] == process {
			delete(process.owner.processes, process.key)
		}
		process.owner.mu.Unlock()
	}
}

func (process *piProcess) close() {
	process.mu.Lock()
	if process.closed {
		process.mu.Unlock()
		return
	}
	process.closed = true
	stdin := process.stdin
	process.stdin = nil
	cancel := process.cancel
	process.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	if cancel != nil {
		cancel()
	}
}

func (p *Pi) lockForKey(key string) *sync.Mutex {
	p.keyMu.Lock()
	defer p.keyMu.Unlock()
	lock := p.keyLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		p.keyLocks[key] = lock
	}
	return lock
}

func (p *Pi) sessionPolicyLocked(model string) string {
	cfg := p.config
	cfg.Model = model
	return piSessionPolicy(cfg)
}

func piSessionPolicy(cfg HarnessConfig) string {
	return strings.Join([]string{cfg.Command, cfg.Cwd, cfg.Model, cfg.Effort, cfg.Sandbox}, "\x1f")
}

func (p *Pi) loadSessions() error {
	if p.config.SessionsFile == "" {
		return nil
	}
	data, err := os.ReadFile(p.config.SessionsFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &p.sessions); err != nil {
		return fmt.Errorf("decode Pi session map: %w", err)
	}
	return nil
}

func (p *Pi) saveSessionsLocked() error {
	if p.config.SessionsFile == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p.config.SessionsFile), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p.sessions, "", "  ")
	if err != nil {
		return err
	}
	return fsx.AtomicWriteFile(p.config.SessionsFile, append(data, '\n'), 0o600)
}

func emptyAsHarness(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
