package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/frdel/spynel/internal/fsx"
	"gopkg.in/yaml.v3"
)

const FileName = "spynel.yaml"

var ErrNotInitialized = errors.New("Spynel is not initialized")

type Config struct {
	Version      int          `yaml:"version"`
	Workspace    Workspace    `yaml:"workspace"`
	Harness      Harness      `yaml:"harness"`
	Channels     Channels     `yaml:"channels"`
	Speech       Speech       `yaml:"speech"`
	Startup      Startup      `yaml:"startup"`
	Orchestrator Orchestrator `yaml:"orchestrator"`
	Extensions   Extensions   `yaml:"extensions"`
	Path         string       `yaml:"-"`
	Root         string       `yaml:"-"`
}

type Workspace struct {
	StateDir           string `yaml:"state_dir"`
	HistoryMaxMessages int    `yaml:"history_max_messages"`
	HistoryCharLimit   int    `yaml:"history_char_limit"`
	AttachmentMaxMB    int    `yaml:"attachment_max_mb"`
}

// Harness contains the user-facing coding-harness choices. Executable
// discovery and the workspace directory remain derived by Spynel.
type Harness struct {
	Name    string `yaml:"name"`
	Model   string `yaml:"model,omitempty"`
	Sandbox string `yaml:"sandbox"`
}

type Channels struct {
	TUI      TUI      `yaml:"tui"`
	Telegram Telegram `yaml:"telegram"`
	WhatsApp WhatsApp `yaml:"whatsapp"`
}

type TUI struct {
	Enabled bool   `yaml:"enabled"`
	Title   string `yaml:"title"`
}

type Telegram struct {
	Enabled               bool     `yaml:"enabled"`
	Name                  string   `yaml:"name"`
	Token                 string   `yaml:"token,omitempty"`
	TokenEnv              string   `yaml:"token_env"`
	Mode                  string   `yaml:"mode"`
	WebhookURL            string   `yaml:"webhook_url,omitempty"`
	WebhookListen         string   `yaml:"webhook_listen"`
	WebhookSecret         string   `yaml:"webhook_secret,omitempty"`
	AllowedUsers          []string `yaml:"allowed_users"`
	PollTimeoutSec        int      `yaml:"poll_timeout_seconds"`
	GroupMode             string   `yaml:"group_mode"`
	WelcomeEnabled        bool     `yaml:"welcome_enabled"`
	WelcomeMessage        string   `yaml:"welcome_message,omitempty"`
	NotifyMessages        bool     `yaml:"notify_messages"`
	AttachmentMaxAgeHours int      `yaml:"attachment_max_age_hours"`
}

type WhatsApp struct {
	Enabled         bool     `yaml:"enabled"`
	Mode            string   `yaml:"mode"`
	Database        string   `yaml:"database"`
	AllowedNumbers  []string `yaml:"allowed_numbers"`
	AllowGroups     bool     `yaml:"allow_groups"`
	PollIntervalSec int      `yaml:"poll_interval_seconds"`
}

type Speech struct {
	Enabled        bool   `yaml:"enabled"`
	Command        string `yaml:"command"`
	FFmpegCommand  string `yaml:"ffmpeg_command"`
	Model          string `yaml:"model"`
	ModelPath      string `yaml:"model_path,omitempty"`
	Language       string `yaml:"language"`
	MaxFileMB      int    `yaml:"max_file_mb"`
	MaxDurationSec int    `yaml:"max_duration_seconds"`
	ChunkSeconds   int    `yaml:"chunk_seconds"`
}

type Startup struct {
	Enabled bool `yaml:"enabled"`
}

type Orchestrator struct {
	Enabled     bool    `yaml:"enabled"`
	IntervalSec int     `yaml:"interval_seconds"`
	MaxParallel int     `yaml:"max_parallel"`
	Routes      []Route `yaml:"routes"`
}

type Route struct {
	Name           string   `yaml:"name"`
	Source         string   `yaml:"source"`
	Working        string   `yaml:"working"`
	Prompt         string   `yaml:"prompt"`
	RecoveryPrompt string   `yaml:"recovery_prompt"`
	StaleAfter     string   `yaml:"stale_after"`
	AllowedNext    []string `yaml:"allowed_next"`
}

func (r Route) StaleDuration() time.Duration {
	d, err := time.ParseDuration(r.StaleAfter)
	if err != nil || d <= 0 {
		return 30 * time.Minute
	}
	return d
}

type Extensions struct {
	Enabled     bool   `yaml:"enabled"`
	Directory   string `yaml:"directory"`
	HookTimeout string `yaml:"hook_timeout"`
}

func (e Extensions) Timeout() time.Duration {
	d, err := time.ParseDuration(e.HookTimeout)
	if err != nil || d <= 0 {
		return 30 * time.Second
	}
	return d
}

func Default() Config {
	return Config{
		Version:   1,
		Workspace: Workspace{StateDir: ".spynel", HistoryMaxMessages: 50, HistoryCharLimit: 12000, AttachmentMaxMB: 100},
		Harness:   Harness{Name: "", Model: "", Sandbox: "danger-full-access"},
		Channels: Channels{
			TUI:      TUI{Enabled: true, Title: "Spynel"},
			Telegram: Telegram{Name: "spynel", TokenEnv: "SPYNEL_TELEGRAM_TOKEN", Mode: "polling", WebhookListen: "127.0.0.1:8787", PollTimeoutSec: 30, GroupMode: "mention", WelcomeMessage: "Welcome, {name}!"},
			WhatsApp: WhatsApp{Mode: "self-chat", Database: ".spynel/whatsapp.db", PollIntervalSec: 3},
		},
		Speech:  Speech{Enabled: true, Command: "whisper-cli", FFmpegCommand: "ffmpeg", Model: "small", Language: "auto", MaxFileMB: 100, MaxDurationSec: 1800, ChunkSeconds: 600},
		Startup: Startup{},
		Orchestrator: Orchestrator{
			Enabled: true, IntervalSec: 10, MaxParallel: 4,
			Routes: []Route{
				{Name: "tasks", Source: ".spynel/tasks/todo", Working: ".spynel/tasks/working", Prompt: ".spynel/prompts/task.md", RecoveryPrompt: ".spynel/prompts/recovery.md", StaleAfter: "30m", AllowedNext: []string{"todo", "working", "waiting", "done", "failed"}},
				{Name: "goals", Source: ".spynel/goals/active", Working: ".spynel/goals/working", Prompt: ".spynel/prompts/goal.md", RecoveryPrompt: ".spynel/prompts/recovery.md", StaleAfter: "2h", AllowedNext: []string{"active", "working", "waiting", "done"}},
			},
		},
		Extensions: Extensions{Enabled: true, Directory: ".spynel/extensions", HookTimeout: "30s"},
	}
}

func Find(start string) (string, error) {
	root, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	if info, statErr := os.Stat(root); statErr == nil && !info.IsDir() {
		root = filepath.Dir(root)
	}
	for {
		candidate := filepath.Join(root, FileName)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(root)
		if parent == root {
			return "", fmt.Errorf("%w: %s not found from %s", ErrNotInitialized, FileName, start)
		}
		root = parent
	}
}

func Load(path string) (Config, error) {
	if path == "" {
		var err error
		path, err = Find(".")
		if err != nil {
			return Config{}, err
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return Config{}, err
	}
	cfg := Default()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", abs, err)
	}
	// Version-one workspaces used `recipient`. Read that shape as a legacy
	// alias, but Save always emits the simpler canonical `harness` section.
	var compatibility struct {
		Harness *Harness `yaml:"harness"`
		Legacy  *struct {
			Name  string `yaml:"name"`
			Model string `yaml:"model,omitempty"`
		} `yaml:"recipient"`
	}
	if err := yaml.Unmarshal(data, &compatibility); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", abs, err)
	}
	if compatibility.Harness == nil && compatibility.Legacy != nil {
		cfg.Harness.Name = compatibility.Legacy.Name
		cfg.Harness.Model = compatibility.Legacy.Model
	}
	cfg.Harness.Sandbox = normalizeSandbox(cfg.Harness.Sandbox)
	cfg.Path = abs
	cfg.Root = filepath.Dir(abs)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	var problems []string
	if c.Version != 1 {
		problems = append(problems, "version must be 1")
	}
	if c.Workspace.StateDir == "" {
		problems = append(problems, "workspace.state_dir is required")
	}
	if c.Workspace.HistoryCharLimit < 0 {
		problems = append(problems, "workspace.history_char_limit cannot be negative")
	}
	if c.Workspace.HistoryMaxMessages < 0 {
		problems = append(problems, "workspace.history_max_messages cannot be negative")
	}
	if c.Workspace.AttachmentMaxMB <= 0 {
		problems = append(problems, "workspace.attachment_max_mb must be positive")
	}
	if c.Harness.Name != "" && c.Harness.Name != "codex" && c.Harness.Name != "claude-code" {
		problems = append(problems, "harness.name must be codex or claude-code")
	}
	switch c.Harness.Sandbox {
	case "read-only", "workspace-write", "danger-full-access":
	default:
		problems = append(problems, "harness.sandbox must be read-only, workspace-write, or danger-full-access")
	}
	if c.Orchestrator.Enabled && c.Orchestrator.IntervalSec <= 0 {
		problems = append(problems, "orchestrator.interval_seconds must be positive")
	}
	if c.Orchestrator.MaxParallel <= 0 {
		problems = append(problems, "orchestrator.max_parallel must be positive")
	}
	seen := map[string]bool{}
	for i, route := range c.Orchestrator.Routes {
		prefix := fmt.Sprintf("orchestrator.routes[%d]", i)
		if route.Name == "" || route.Source == "" || route.Working == "" || route.Prompt == "" || route.RecoveryPrompt == "" {
			problems = append(problems, prefix+" requires name, source, working, prompt, and recovery_prompt")
		}
		if seen[route.Name] {
			problems = append(problems, "duplicate route name "+route.Name)
		}
		seen[route.Name] = true
		if route.StaleAfter != "" {
			if _, err := time.ParseDuration(route.StaleAfter); err != nil {
				problems = append(problems, prefix+".stale_after is invalid")
			}
		}
	}
	if c.Channels.WhatsApp.Mode != "" && c.Channels.WhatsApp.Mode != "self-chat" && c.Channels.WhatsApp.Mode != "dedicated" {
		problems = append(problems, "channels.whatsapp.mode must be self-chat or dedicated")
	}
	if c.Channels.Telegram.Mode != "polling" && c.Channels.Telegram.Mode != "webhook" {
		problems = append(problems, "channels.telegram.mode must be polling or webhook")
	}
	if c.Channels.Telegram.Mode == "webhook" && c.Channels.Telegram.Enabled && c.Channels.Telegram.WebhookURL == "" {
		problems = append(problems, "channels.telegram.webhook_url is required in webhook mode")
	}
	if c.Channels.Telegram.Mode == "webhook" && c.Channels.Telegram.Enabled && strings.TrimSpace(c.Channels.Telegram.WebhookListen) == "" {
		problems = append(problems, "channels.telegram.webhook_listen is required in webhook mode")
	}
	if c.Channels.Telegram.Enabled && !hasNonemptyValue(c.Channels.Telegram.AllowedUsers) {
		problems = append(problems, "channels.telegram.allowed_users requires at least one user when Telegram is enabled")
	}
	switch c.Channels.Telegram.GroupMode {
	case "mention", "all", "off":
	default:
		problems = append(problems, "channels.telegram.group_mode must be mention, all, or off")
	}
	if c.Channels.Telegram.AttachmentMaxAgeHours < 0 {
		problems = append(problems, "channels.telegram.attachment_max_age_hours cannot be negative")
	}
	if c.Channels.WhatsApp.PollIntervalSec < 2 {
		problems = append(problems, "channels.whatsapp.poll_interval_seconds must be at least 2")
	}
	if c.Speech.MaxFileMB <= 0 || c.Speech.MaxDurationSec <= 0 || c.Speech.ChunkSeconds <= 0 {
		problems = append(problems, "speech resource limits must be positive")
	}
	if c.Speech.Enabled && (strings.TrimSpace(c.Speech.Command) == "" || strings.TrimSpace(c.Speech.FFmpegCommand) == "") {
		problems = append(problems, "speech.command and speech.ffmpeg_command are required when speech is enabled")
	}
	if strings.TrimSpace(c.Extensions.Directory) == "" {
		problems = append(problems, "extensions.directory is required")
	}
	if duration, err := time.ParseDuration(c.Extensions.HookTimeout); err != nil || duration <= 0 {
		problems = append(problems, "extensions.hook_timeout must be a positive duration")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func hasNonemptyValue(values []string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

func normalizeSandbox(value string) string {
	compact := strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(value)))
	switch compact {
	case "readonly":
		return "read-only"
	case "workspacewrite", "restricted":
		return "workspace-write"
	case "dangerfullaccess", "unrestricted", "fullaccess":
		return "danger-full-access"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func (c Config) Resolve(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(c.Root, filepath.FromSlash(path))
}

func (c Config) StatePath(parts ...string) string {
	base := c.Resolve(c.Workspace.StateDir)
	return filepath.Join(append([]string{base}, parts...)...)
}

// HarnessSessionsPath uses the canonical harness filename for new sessions
// while continuing an existing version-one recipient session map in place.
func (c Config) HarnessSessionsPath(name string) string {
	name = strings.NewReplacer("/", "-", "\\", "-").Replace(strings.ToLower(strings.TrimSpace(name)))
	if name == "" {
		name = "unselected"
	}
	canonical := c.StatePath("runtime", "harness-"+name+"-sessions.json")
	legacyName := "recipient-" + name + "-sessions.json"
	if name == "codex" {
		legacyName = "recipient-sessions.json"
	}
	legacy := c.StatePath("runtime", legacyName)
	if _, err := os.Stat(canonical); os.IsNotExist(err) {
		if _, legacyErr := os.Stat(legacy); legacyErr == nil {
			return legacy
		}
	}
	return canonical
}

func (c Config) TelegramToken() string {
	if c.Channels.Telegram.Token != "" {
		return c.Channels.Telegram.Token
	}
	if c.Channels.Telegram.TokenEnv != "" {
		return os.Getenv(c.Channels.Telegram.TokenEnv)
	}
	return ""
}

// Save validates and atomically persists a loaded configuration. Runtime-only
// path metadata is excluded by its YAML tags.
func Save(cfg Config) error {
	if cfg.Path == "" {
		return errors.New("configuration path is empty")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode %s: %w", cfg.Path, err)
	}
	if err := fsx.AtomicWriteFile(cfg.Path, data, 0o600); err != nil {
		return fmt.Errorf("save %s: %w", cfg.Path, err)
	}
	return nil
}

// Store serializes validated configuration changes and publishes the newest
// persisted snapshot without retaining an unbounded update queue.
type Store struct {
	mu      sync.RWMutex
	current Config
	updates chan Config
}

func NewStore(cfg Config) *Store {
	return &Store{current: cfg, updates: make(chan Config, 1)}
}

func (s *Store) Snapshot() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

func (s *Store) Update(change func(*Config) error) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.current
	if err := change(&next); err != nil {
		return s.current, err
	}
	if err := Save(next); err != nil {
		return s.current, err
	}
	s.current = next
	select {
	case <-s.updates:
	default:
	}
	s.updates <- next
	return next, nil
}

func (s *Store) Updates() <-chan Config { return s.updates }
