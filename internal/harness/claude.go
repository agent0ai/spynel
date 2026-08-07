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
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/fsx"
)

// Claude is a lightweight adapter around Claude Code print mode. It uses
// streaming JSON input when the selected permission mode supports it, and
// bounded text input when Claude requires its ordinary permission pipeline for
// autonomous tools. Claude Code owns the agent loop and durable transcript;
// Spynel stores only conversation-to-session mappings.
type Claude struct {
	config HarnessConfig
	ctx    context.Context
	cancel context.CancelFunc

	keyMu    sync.Mutex
	keyLocks map[string]*sync.Mutex
	mu       sync.Mutex
	sessions map[string]claudeSession
	active   map[string]*claudeTurn
	closed   bool
}

type claudeSession struct {
	ID     string `json:"id"`
	Policy string `json:"policy"`
}

type claudeTurn struct {
	key       string
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	ready     chan error
	done      chan struct{}
	readyOnce sync.Once
	doneOnce  sync.Once
	sessionID string
	text      strings.Builder
	finalText strings.Builder
	sawText   bool
	separator bool
	stderr    *tailBuffer

	emitMu sync.RWMutex
	emit   core.Emit

	inputMu   sync.Mutex
	stdin     io.WriteCloser
	accepting bool
}

var errClaudeTurnClosing = errors.New("Claude Code turn is finishing")

type claudeEvent struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	IsError   bool            `json:"is_error"`
	Result    string          `json:"result"`
	Error     json.RawMessage `json:"error"`
	Event     struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`
}

func NewClaude(cfg HarnessConfig) (*Claude, error) {
	if cfg.Command == "" {
		cfg.Command = "claude"
	}
	if cfg.Cwd == "" {
		cfg.Cwd = "."
	}
	c := &Claude{config: cfg, keyLocks: map[string]*sync.Mutex{}, sessions: map[string]claudeSession{}, active: map[string]*claudeTurn{}}
	if err := c.loadSessions(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Claude) FollowUpMode() FollowUpMode {
	if claudeUsesStreamingInput(c.config) {
		return FollowUpSteer
	}
	return FollowUpQueue
}

func (c *Claude) Start(parent context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("Claude Code harness is closed")
	}
	if c.ctx != nil {
		return nil
	}
	if _, err := exec.LookPath(c.config.Command); err != nil {
		return fmt.Errorf("find Claude Code executable %q: %w", c.config.Command, err)
	}
	info, err := os.Stat(c.config.Cwd)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("Claude Code working directory %q is unavailable", c.config.Cwd)
	}
	c.ctx, c.cancel = context.WithCancel(parent)
	if claudeUsesPrivilegedFallback(c.config) && c.config.Stderr != nil {
		_, _ = fmt.Fprintln(c.config.Stderr, "Claude Code refuses --dangerously-skip-permissions for a privileged account; using acceptEdits with all tools allowed")
	}
	return nil
}

// Models returns Claude Code's documented dynamic aliases. Claude Code does
// not expose its interactive picker as a machine-readable CLI API, so aliases
// are safer than hard-coding provider- and account-specific model versions.
func (c *Claude) Models(context.Context) ([]Model, error) {
	allEfforts := []string{"low", "medium", "high", "xhigh", "max"}
	models := []Model{
		{ID: "default", DisplayName: "Default", Description: "Recommended model for this account", Default: true},
		{ID: "best", DisplayName: "Best", Description: "Most capable available model"},
		{ID: "fable", DisplayName: "Fable", Description: "Fable for the hardest and longest-running tasks", Efforts: allEfforts, DefaultEffort: "high"},
		{ID: "sonnet", DisplayName: "Sonnet", Description: "Latest Sonnet for daily coding", Efforts: allEfforts},
		{ID: "opus", DisplayName: "Opus", Description: "Latest Opus for complex reasoning", Efforts: allEfforts},
		{ID: "haiku", DisplayName: "Haiku", Description: "Fast model for simple tasks", Efforts: allEfforts},
		{ID: "sonnet[1m]", DisplayName: "Sonnet (1M)", Description: "Sonnet with extended context", Efforts: allEfforts},
		{ID: "opus[1m]", DisplayName: "Opus (1M)", Description: "Opus with extended context", Efforts: allEfforts},
		{ID: "opusplan", DisplayName: "Opus Plan", Description: "Opus for planning and Sonnet for execution", Efforts: allEfforts},
	}
	configured := strings.TrimSpace(c.config.Model)
	if configured != "" {
		found := false
		for index := range models {
			if models[index].ID == configured {
				found = true
				break
			}
		}
		if !found {
			models = append(models, Model{ID: configured, DisplayName: configured, Description: "Configured custom model", Efforts: allEfforts})
		}
	}
	return models, nil
}

func (c *Claude) Send(ctx context.Context, key, prompt string, emit core.Emit) (string, bool, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", false, errors.New("harness prompt is empty")
	}
	lock := c.lockForKey(key)
	lock.Lock()
	defer lock.Unlock()

	for {
		c.mu.Lock()
		if c.closed || c.ctx == nil {
			c.mu.Unlock()
			return "", false, errors.New("Claude Code harness is not running")
		}
		active := c.active[key]
		session := c.sessions[key]
		threadID := session.ID
		if active != nil {
			c.mu.Unlock()
			previous, err := active.writePrompt(prompt, emit, true)
			if errors.Is(err, errClaudeTurnClosing) {
				select {
				case <-active.done:
					continue
				case <-ctx.Done():
					return threadID, true, ctx.Err()
				}
			}
			if err != nil {
				return threadID, true, fmt.Errorf("steer active Claude Code turn: %w", err)
			}
			if previous != nil {
				previous(core.Event{Kind: core.EventStatus, Text: "Response continued on a newer message", ThreadID: threadID, Done: true})
			}
			if emit != nil {
				emit(core.Event{Kind: core.EventStatus, Text: "Steering active Claude Code turn", ThreadID: threadID})
			}
			return threadID, true, nil
		}
		baseContext := c.ctx
		cfg := c.config
		previousSession := c.resumeSessionLocked(key, cfg)
		c.mu.Unlock()
		return c.startTurn(ctx, baseContext, key, prompt, previousSession, cfg, emit)
	}
}

func (c *Claude) resumeSessionLocked(key string, cfg HarnessConfig) string {
	session := c.sessions[key]
	if session.Policy != claudeSessionPolicy(cfg) {
		return ""
	}
	return session.ID
}

func (c *Claude) startTurn(ctx, baseContext context.Context, key, prompt, previousSession string, cfg HarnessConfig, emit core.Emit) (string, bool, error) {
	turnContext, cancel := context.WithCancel(baseContext)
	streamingInput := claudeUsesStreamingInput(cfg)
	args := []string{"-p"}
	if streamingInput {
		args = append(args, "--input-format", "stream-json")
	}
	args = append(args, "--output-format", "stream-json", "--verbose", "--include-partial-messages")
	if previousSession != "" {
		args = append(args, "--resume", previousSession)
	}
	if cfg.Model != "" && cfg.Model != "default" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.Effort != "" && cfg.Effort != "auto" {
		args = append(args, "--effort", cfg.Effort)
	}
	args = append(args, claudePermissionArgs(cfg)...)
	cmd := exec.CommandContext(turnContext, cfg.Command, args...)
	cmd.Dir = cfg.Cwd
	var stdin io.WriteCloser
	if streamingInput {
		var err error
		stdin, err = cmd.StdinPipe()
		if err != nil {
			cancel()
			return "", false, err
		}
	} else {
		cmd.Stdin = strings.NewReader(prompt)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", false, err
	}
	stderr := newTailBuffer(64 * 1024)
	if cfg.Stderr != nil {
		cmd.Stderr = io.MultiWriter(cfg.Stderr, stderr)
	} else {
		cmd.Stderr = stderr
	}
	turn := &claudeTurn{
		key: key, cmd: cmd, cancel: cancel, emit: emit, stdin: stdin, accepting: streamingInput,
		ready: make(chan error, 1), done: make(chan struct{}), stderr: stderr,
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return "", false, fmt.Errorf("start Claude Code: %w", err)
	}
	c.mu.Lock()
	c.active[key] = turn
	c.mu.Unlock()
	go c.runTurn(turn, stdout)
	if streamingInput {
		if _, err := turn.writePrompt(prompt, nil, false); err != nil {
			cancel()
			return "", false, fmt.Errorf("send initial Claude Code prompt: %w", err)
		}
	}

	select {
	case err := <-turn.ready:
		if err != nil {
			return "", false, err
		}
		return turn.sessionID, false, nil
	case <-ctx.Done():
		cancel()
		return "", false, ctx.Err()
	case <-baseContext.Done():
		return "", false, baseContext.Err()
	}
}

func claudePermissionArgs(cfg HarnessConfig) []string {
	return claudePermissionArgsFor(cfg, claudeRunningPrivileged())
}

func claudePermissionArgsFor(cfg HarnessConfig, privileged bool) []string {
	switch cfg.ApprovalPolicy {
	case "untrusted", "plan":
		return []string{"--permission-mode", "plan"}
	}
	switch cfg.Sandbox {
	case "readOnly", "read-only":
		return []string{"--permission-mode", "plan"}
	case "workspaceWrite", "workspace-write":
		return []string{"--permission-mode", "acceptEdits", "--allowedTools", "Bash(*)"}
	case "dangerFullAccess", "danger-full-access":
		if cfg.ApprovalPolicy == "never" && !privileged {
			return []string{"--dangerously-skip-permissions"}
		}
		if cfg.ApprovalPolicy == "never" && privileged {
			return []string{"--permission-mode", "acceptEdits", "--allowedTools", "*", "Bash(*)"}
		}
	}
	if cfg.ApprovalPolicy == "never" {
		return []string{"--permission-mode", "dontAsk"}
	}
	return nil
}

func claudeRunningPrivileged() bool {
	current, err := user.Current()
	return err == nil && current.Uid == "0"
}

func claudeUsesPrivilegedFallback(cfg HarnessConfig) bool {
	return cfg.ApprovalPolicy == "never" &&
		(cfg.Sandbox == "dangerFullAccess" || cfg.Sandbox == "danger-full-access") &&
		claudeRunningPrivileged()
}

func claudeUsesStreamingInput(cfg HarnessConfig) bool {
	return claudeUsesStreamingInputFor(cfg, claudeRunningPrivileged())
}

func claudeUsesStreamingInputFor(cfg HarnessConfig, privileged bool) bool {
	if cfg.ApprovalPolicy == "untrusted" || cfg.ApprovalPolicy == "plan" {
		return true
	}
	switch cfg.Sandbox {
	case "workspaceWrite", "workspace-write":
		// Claude Code 2.1.x does not honor allowedTools for approval-gated
		// commands under stream-json input. Ordinary text input does.
		return false
	case "dangerFullAccess", "danger-full-access":
		// Non-privileged bypass needs no approval callback. Claude refuses
		// bypass under root/sudo, whose all-tools fallback needs text input.
		return !privileged
	default:
		return true
	}
}

func claudeSessionPolicy(cfg HarnessConfig) string {
	return strings.Join([]string{
		cfg.Model,
		cfg.Effort,
		cfg.ApprovalPolicy,
		cfg.Sandbox,
		strconv.FormatBool(cfg.Network),
		strings.Join(claudePermissionArgs(cfg), "\x1f"),
		strconv.FormatBool(claudeUsesStreamingInput(cfg)),
	}, "\x1e")
}

func (turn *claudeTurn) writePrompt(prompt string, emit core.Emit, replace bool) (core.Emit, error) {
	message := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]string{{"type": "text", "text": prompt}},
		},
	}
	data, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	turn.inputMu.Lock()
	defer turn.inputMu.Unlock()
	if !turn.accepting || turn.stdin == nil {
		return nil, errClaudeTurnClosing
	}
	var previous core.Emit
	if replace {
		previous = turn.replaceEmit(emit)
	}
	if _, err := turn.stdin.Write(append(data, '\n')); err != nil {
		if replace {
			turn.replaceEmit(previous)
		}
		return nil, err
	}
	return previous, nil
}

func (turn *claudeTurn) closeInput() {
	turn.inputMu.Lock()
	defer turn.inputMu.Unlock()
	if !turn.accepting {
		return
	}
	turn.accepting = false
	if turn.stdin != nil {
		_ = turn.stdin.Close()
		turn.stdin = nil
	}
}

func (turn *claudeTurn) emitEvent(event core.Event) {
	turn.emitMu.RLock()
	emit := turn.emit
	turn.emitMu.RUnlock()
	if emit != nil {
		emit(event)
	}
}

func (turn *claudeTurn) replaceEmit(emit core.Emit) core.Emit {
	turn.emitMu.Lock()
	previous := turn.emit
	turn.emit = emit
	turn.emitMu.Unlock()
	return previous
}

func (c *Claude) runTurn(turn *claudeTurn, reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	completed := false
	var terminal core.Event
	for scanner.Scan() {
		var event claudeEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		if event.SessionID != "" && turn.sessionID == "" {
			turn.sessionID = event.SessionID
			c.rememberSession(turn.key, event.SessionID)
			turn.readyOnce.Do(func() { turn.ready <- nil })
		}
		if event.Type == "stream_event" {
			switch event.Event.Type {
			case "message_start":
				if turn.sawText {
					turn.separator = true
					turn.finalText.Reset()
				}
			case "content_block_delta":
				if event.Event.Delta.Type == "text_delta" && event.Event.Delta.Text != "" {
					if turn.separator {
						c.emitClaudeDelta(turn, "\n")
						turn.finalText.Reset()
						turn.separator = false
					}
					c.emitClaudeDelta(turn, event.Event.Delta.Text)
					turn.sawText = true
				}
			}
			continue
		}
		if event.Type == "result" {
			text := turn.text.String()
			if text == "" || event.IsError {
				text = event.Result
			}
			finalText := turn.finalText.String()
			if finalText == "" || event.IsError {
				finalText = event.Result
			}
			if finalText == "" {
				finalText = text
			}
			kind := core.EventFinal
			if event.IsError {
				kind = core.EventError
			}
			terminal = core.Event{Kind: kind, Text: text, FinalText: &finalText, ThreadID: turn.sessionID, Done: true}
			completed = true
			// Streaming input keeps print mode alive for more user messages.
			// Close it after the provider result so this finite Spynel turn can
			// exit; a follow-up written before this lock is included first.
			turn.closeInput()
		}
	}
	waitErr := turn.cmd.Wait()
	turn.cancel()
	if completed {
		// Wait for print mode to exit before releasing the session. Closing its
		// streaming input after the result keeps each Spynel execution bounded.
		c.finishClaudeTurn(turn, terminal)
		return
	}
	err := scanner.Err()
	if err == nil {
		err = waitErr
	}
	message := "Claude Code stopped before returning a final response"
	if detail := strings.TrimSpace(turn.stderr.String()); detail != "" {
		message += ": " + detail
	} else if err != nil {
		message += ": " + err.Error()
	}
	c.finishClaudeTurn(turn, core.Event{Kind: core.EventError, Text: message, ThreadID: turn.sessionID, Done: true})
}

func (c *Claude) emitClaudeDelta(turn *claudeTurn, text string) {
	turn.text.WriteString(text)
	turn.finalText.WriteString(text)
	turn.emitEvent(core.Event{Kind: core.EventDelta, Text: text, ThreadID: turn.sessionID})
}

func (c *Claude) finishClaudeTurn(turn *claudeTurn, event core.Event) {
	turn.doneOnce.Do(func() {
		turn.readyOnce.Do(func() {
			if turn.sessionID == "" {
				turn.ready <- errors.New(event.Text)
			} else {
				turn.ready <- nil
			}
		})
		c.mu.Lock()
		if c.active[turn.key] == turn {
			delete(c.active, turn.key)
		}
		c.mu.Unlock()
		turn.emitEvent(event)
		close(turn.done)
	})
}

func (c *Claude) rememberSession(key, sessionID string) {
	c.mu.Lock()
	c.sessions[key] = claudeSession{ID: sessionID, Policy: claudeSessionPolicy(c.config)}
	err := c.saveSessionsLocked()
	c.mu.Unlock()
	if err != nil && c.config.Stderr != nil {
		_, _ = fmt.Fprintf(c.config.Stderr, "save Claude Code session: %v\n", err)
	}
}

func (c *Claude) Interrupt(_ context.Context, key string) (bool, error) {
	lock := c.lockForKey(key)
	lock.Lock()
	defer lock.Unlock()
	c.mu.Lock()
	turn := c.active[key]
	c.mu.Unlock()
	if turn == nil {
		return false, nil
	}
	turn.cancel()
	if runtime.GOOS != "windows" && turn.cmd.Process != nil {
		_ = turn.cmd.Process.Signal(os.Interrupt)
	}
	return true, nil
}

func (c *Claude) ResetSession(key string) error {
	lock := c.lockForKey(key)
	lock.Lock()
	defer lock.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active[key] != nil {
		return errors.New("cannot reset a session while its turn is active")
	}
	delete(c.sessions, key)
	return c.saveSessionsLocked()
}

func (c *Claude) ThreadID(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessions[key].ID
}

func (c *Claude) IsActive(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active[key] != nil
}

func (c *Claude) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	cancel := c.cancel
	turns := make([]*claudeTurn, 0, len(c.active))
	for _, turn := range c.active {
		turns = append(turns, turn)
	}
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, turn := range turns {
		turn.cancel()
	}
	return nil
}

func (c *Claude) lockForKey(key string) *sync.Mutex {
	c.keyMu.Lock()
	defer c.keyMu.Unlock()
	lock := c.keyLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		c.keyLocks[key] = lock
	}
	return lock
}

func (c *Claude) loadSessions() error {
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
	var values map[string]json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	for key, value := range values {
		var session claudeSession
		if err := json.Unmarshal(value, &session); err != nil {
			var legacy string
			if legacyErr := json.Unmarshal(value, &legacy); legacyErr != nil {
				return err
			}
			session.ID = legacy
		}
		if strings.TrimSpace(session.ID) != "" {
			c.sessions[key] = session
		}
	}
	return nil
}

func (c *Claude) saveSessionsLocked() error {
	if c.config.SessionsFile == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.config.SessionsFile), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c.sessions, "", "  ")
	if err != nil {
		return err
	}
	return fsx.AtomicWriteFile(c.config.SessionsFile, append(data, '\n'), 0o600)
}

type tailBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func newTailBuffer(limit int) *tailBuffer { return &tailBuffer{limit: limit} }

func (b *tailBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, data...)
	if len(b.data) > b.limit {
		b.data = append([]byte(nil), b.data[len(b.data)-b.limit:]...)
	}
	return len(data), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}
