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
	"runtime"
	"strings"
	"sync"

	"github.com/frdel/spynel/internal/core"
	"github.com/frdel/spynel/internal/fsx"
)

// Claude is a lightweight adapter around Claude Code print mode. Claude Code
// owns the agent loop and durable transcript; Spynel stores only the mapping
// from its conversation keys to Claude's opaque session IDs.
type Claude struct {
	config HarnessConfig
	ctx    context.Context
	cancel context.CancelFunc

	keyMu    sync.Mutex
	keyLocks map[string]*sync.Mutex
	mu       sync.Mutex
	sessions map[string]string
	active   map[string]*claudeTurn
	closed   bool
}

type claudeTurn struct {
	key       string
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	emit      core.Emit
	ready     chan error
	readyOnce sync.Once
	doneOnce  sync.Once
	sessionID string
	text      strings.Builder
	sawText   bool
	separator bool
	stderr    *tailBuffer
}

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
	if cfg.Command == "" || cfg.Command == "codex" {
		cfg.Command = "claude"
	}
	if cfg.Cwd == "" {
		cfg.Cwd = "."
	}
	c := &Claude{config: cfg, keyLocks: map[string]*sync.Mutex{}, sessions: map[string]string{}, active: map[string]*claudeTurn{}}
	if err := c.loadSessions(); err != nil {
		return nil, err
	}
	return c, nil
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

	c.mu.Lock()
	if c.closed || c.ctx == nil {
		c.mu.Unlock()
		return "", false, errors.New("Claude Code harness is not running")
	}
	if c.active[key] != nil {
		c.mu.Unlock()
		return c.sessions[key], true, errors.New("Claude Code print mode cannot steer an active turn; use /stop or wait for it to finish")
	}
	baseContext := c.ctx
	previousSession := c.sessions[key]
	cfg := c.config
	c.mu.Unlock()

	turnContext, cancel := context.WithCancel(baseContext)
	args := []string{"-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages"}
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
	cmd.Stdin = strings.NewReader(prompt)
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
	turn := &claudeTurn{key: key, cmd: cmd, cancel: cancel, emit: emit, ready: make(chan error, 1), stderr: stderr}
	if err := cmd.Start(); err != nil {
		cancel()
		return "", false, fmt.Errorf("start Claude Code: %w", err)
	}
	c.mu.Lock()
	c.active[key] = turn
	c.mu.Unlock()
	go c.runTurn(turn, stdout)

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
	if cfg.Sandbox == "dangerFullAccess" || cfg.Sandbox == "danger-full-access" {
		if cfg.ApprovalPolicy == "never" {
			return []string{"--dangerously-skip-permissions"}
		}
	}
	switch cfg.ApprovalPolicy {
	case "never":
		return []string{"--permission-mode", "dontAsk"}
	case "untrusted", "plan":
		return []string{"--permission-mode", "plan"}
	default:
		return nil
	}
}

func (c *Claude) runTurn(turn *claudeTurn, reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	completed := false
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
				}
			case "content_block_delta":
				if event.Event.Delta.Type == "text_delta" && event.Event.Delta.Text != "" {
					if turn.separator {
						c.emitClaudeDelta(turn, "\n")
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
			if text == "" {
				text = event.Result
			}
			kind := core.EventFinal
			if event.IsError {
				kind = core.EventError
			}
			c.finishClaudeTurn(turn, core.Event{Kind: kind, Text: text, ThreadID: turn.sessionID, Done: true})
			completed = true
		}
	}
	waitErr := turn.cmd.Wait()
	turn.cancel()
	if completed {
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
	if turn.emit != nil {
		turn.emit(core.Event{Kind: core.EventDelta, Text: text, ThreadID: turn.sessionID})
	}
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
		if turn.emit != nil {
			turn.emit(event)
		}
	})
}

func (c *Claude) rememberSession(key, sessionID string) {
	c.mu.Lock()
	c.sessions[key] = sessionID
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
	return c.sessions[key]
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
	return json.Unmarshal(data, &c.sessions)
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
