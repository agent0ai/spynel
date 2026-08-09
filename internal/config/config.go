package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/agent0ai/spynel/internal/fsx"
	"github.com/agent0ai/spynel/internal/harness"
	"gopkg.in/yaml.v3"
)

const (
	// FileName is the canonical workspace-relative configuration path.
	FileName           = ".spynel/config.yaml"
	LegacyFileName     = "spynel.yaml"
	StateDirectoryName = ".spynel"

	TaskReviewsSkipTrivial  = "skip-trivial"
	TaskReviewsAlways       = "always"
	TaskReviewsNever        = "never"
	TaskNotificationsOff    = "off"
	TaskNotificationsDecide = "decide"
	TaskNotificationsAlways = "always"
)

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
	HistoryMaxMessages int `yaml:"history_max_messages"`
	HistoryCharLimit   int `yaml:"history_char_limit"`
	AttachmentMaxMB    int `yaml:"attachment_max_mb"`
}

// Harness contains the user-facing coding-harness choices. Built-in
// executables and the workspace directory remain derived by Spynel; only the
// explicit custom ACP profile accepts a command and shell-free argument list.
type Harness struct {
	Name                 string   `yaml:"name"`
	Model                string   `yaml:"model,omitempty"`
	Sandbox              string   `yaml:"sandbox"`
	ChatAgentPrefix      string   `yaml:"chat_agent_prefix"`
	DeveloperAgentPrefix string   `yaml:"developer_agent_prefix"`
	ReviewerAgentPrefix  string   `yaml:"reviewer_agent_prefix"`
	HeartbeatAgentPrefix string   `yaml:"heartbeat_agent_prefix"`
	Reviews              string   `yaml:"reviews"`
	ACPCommand           string   `yaml:"acp_command,omitempty"`
	ACPArgs              []string `yaml:"acp_args,omitempty"`
}

// EffectiveTaskReviewRequired applies the workspace-wide task review mode to
// the per-document choice. Goal outcome review remains a separate mandatory
// lifecycle phase.
func (h Harness) EffectiveTaskReviewRequired(documentRequiresReview bool) bool {
	switch h.Reviews {
	case TaskReviewsAlways:
		return true
	case TaskReviewsNever:
		return false
	default:
		return documentRequiresReview
	}
}

// PrependAgentPrefix separates a harness-native command from the prompt with
// exactly one ASCII space.
// An empty prefix deliberately preserves the existing prompt byte-for-byte.
func PrependAgentPrefix(prefix, prompt string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return prompt
	}
	return prefix + " " + prompt
}

type Channels struct {
	TUI      TUI      `yaml:"tui"`
	Telegram Telegram `yaml:"telegram"`
	WhatsApp WhatsApp `yaml:"whatsapp"`
}

type TUI struct {
	Enabled bool   `yaml:"enabled"`
	Title   string `yaml:"title"`
	Theme   string `yaml:"theme"`
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
	ModelDir       string `yaml:"model_dir,omitempty"`
	Language       string `yaml:"language"`
	NumThreads     int    `yaml:"num_threads"`
	MaxFileMB      int    `yaml:"max_file_mb"`
	MaxDurationSec int    `yaml:"max_duration_seconds"`
	ChunkSeconds   int    `yaml:"chunk_seconds"`
}

var speechLanguages = []string{
	"auto", "en", "bg", "hr", "cs", "da", "nl", "et", "fi", "fr", "de", "el", "hu",
	"it", "lv", "lt", "mt", "pl", "pt", "ro", "sk", "sl", "es", "sv", "ru", "uk",
}

// SpeechLanguages returns the language values supported by the bundled
// Parakeet models. English uses the English-only model; auto and every other
// language use the multilingual model.
func SpeechLanguages() []string {
	return append([]string(nil), speechLanguages...)
}

func IsSpeechLanguage(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, language := range speechLanguages {
		if value == language {
			return true
		}
	}
	return false
}

type Startup struct {
	Enabled bool `yaml:"enabled"`
}

type Orchestrator struct {
	Enabled                  bool    `yaml:"enabled"`
	IntervalSec              int     `yaml:"interval_seconds"`
	SemanticHeartbeatMinutes int     `yaml:"semantic_heartbeat_minutes"`
	TaskNotifications        string  `yaml:"task_notifications"`
	MaxParallel              int     `yaml:"max_parallel"`
	Routes                   []Route `yaml:"routes"`
}

type Route struct {
	Name           string   `yaml:"name"`
	Source         string   `yaml:"source"`
	Working        string   `yaml:"working"`
	Prompt         string   `yaml:"prompt"`
	RecoveryPrompt string   `yaml:"recovery_prompt"`
	ReviewPrompt   string   `yaml:"review_prompt,omitempty"`
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
		Workspace: Workspace{HistoryMaxMessages: 50, HistoryCharLimit: 12000, AttachmentMaxMB: 100},
		Harness: Harness{
			Name: "", Model: "", Sandbox: "danger-full-access",
			DeveloperAgentPrefix: "/goal", ReviewerAgentPrefix: "/goal", HeartbeatAgentPrefix: "/goal",
			Reviews: TaskReviewsSkipTrivial,
		},
		Channels: Channels{
			TUI:      TUI{Enabled: true, Title: "Spynel", Theme: "spynel"},
			Telegram: Telegram{Name: "spynel", TokenEnv: "SPYNEL_TELEGRAM_TOKEN", Mode: "polling", WebhookListen: "127.0.0.1:8787", PollTimeoutSec: 30, GroupMode: "mention", WelcomeMessage: "Welcome, {name}!"},
			WhatsApp: WhatsApp{Mode: "self-chat", Database: ".spynel/whatsapp.db", PollIntervalSec: 3},
		},
		Speech:  Speech{Enabled: true, Language: "en", NumThreads: 2, MaxFileMB: 100, MaxDurationSec: 1800, ChunkSeconds: 600},
		Startup: Startup{},
		Orchestrator: Orchestrator{
			Enabled: true, IntervalSec: 10, SemanticHeartbeatMinutes: 15, TaskNotifications: TaskNotificationsDecide, MaxParallel: 4,
			Routes: []Route{
				{Name: "tasks", Source: ".spynel/tasks/todo", Working: ".spynel/tasks/working", Prompt: ".spynel/prompts/task.md", RecoveryPrompt: ".spynel/prompts/recovery.md", ReviewPrompt: ".spynel/prompts/review.md", StaleAfter: "30m", AllowedNext: []string{"todo", "working", "review", "reviewing", "waiting", "done", "failed", "cancelled"}},
				{Name: "goals", Source: ".spynel/goals/proposed", Working: ".spynel/goals/planning", Prompt: ".spynel/prompts/goal.md", RecoveryPrompt: ".spynel/prompts/recovery.md", ReviewPrompt: ".spynel/prompts/goal-review.md", StaleAfter: "2h", AllowedNext: []string{"proposed", "planning", "active", "review", "reviewing", "waiting", "done", "abandoned"}},
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
		candidate := PathForRoot(root)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		legacy := filepath.Join(root, LegacyFileName)
		if _, err := os.Stat(legacy); err == nil {
			return legacy, nil
		}
		parent := filepath.Dir(root)
		if parent == root {
			return "", fmt.Errorf("%w: %s not found from %s", ErrNotInitialized, filepath.ToSlash(FileName), start)
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
	if rootForConfigPath(abs) != filepath.Dir(abs) {
		if _, statErr := os.Stat(abs); os.IsNotExist(statErr) {
			legacy := filepath.Join(rootForConfigPath(abs), LegacyFileName)
			if _, legacyErr := os.Stat(legacy); legacyErr == nil {
				return migrateLegacyConfig(legacy, abs)
			} else if !os.IsNotExist(legacyErr) {
				return Config{}, legacyErr
			}
		} else if statErr != nil {
			return Config{}, statErr
		}
	}
	if filepath.Base(abs) == LegacyFileName {
		canonical := PathForRoot(filepath.Dir(abs))
		if _, statErr := os.Stat(canonical); statErr == nil {
			abs = canonical
		} else if !os.IsNotExist(statErr) {
			return Config{}, statErr
		} else {
			return migrateLegacyConfig(abs, canonical)
		}
	}
	return loadAt(abs)
}

func loadAt(abs string) (Config, error) {
	data, err := os.ReadFile(abs)
	if err != nil {
		return Config{}, err
	}
	return decode(data, abs)
}

func decode(data []byte, abs string) (Config, error) {
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
	cfg.Harness.Name = harness.NormalizeName(cfg.Harness.Name)
	cfg.Harness.Sandbox = normalizeSandbox(cfg.Harness.Sandbox)
	cfg.Harness.ChatAgentPrefix = strings.TrimSpace(cfg.Harness.ChatAgentPrefix)
	cfg.Harness.DeveloperAgentPrefix = strings.TrimSpace(cfg.Harness.DeveloperAgentPrefix)
	cfg.Harness.ReviewerAgentPrefix = strings.TrimSpace(cfg.Harness.ReviewerAgentPrefix)
	cfg.Harness.HeartbeatAgentPrefix = strings.TrimSpace(cfg.Harness.HeartbeatAgentPrefix)
	cfg.Harness.Reviews = normalizeTaskReviewMode(cfg.Harness.Reviews)
	cfg.Speech.Language = strings.ToLower(strings.TrimSpace(cfg.Speech.Language))
	// Upgrade version-one built-in workflow values in memory without
	// overwriting the user's configuration file. Workspace.Upgrade restores
	// missing prompts and directories separately and non-destructively.
	for i := range cfg.Orchestrator.Routes {
		route := &cfg.Orchestrator.Routes[i]
		switch route.Name {
		case "tasks":
			if route.ReviewPrompt == "" {
				route.ReviewPrompt = ".spynel/prompts/review.md"
			}
			route.AllowedNext = []string{"todo", "working", "review", "reviewing", "waiting", "done", "failed", "cancelled"}
		case "goals":
			if filepath.Base(filepath.Clean(route.Source)) == "active" {
				route.Source = filepath.Join(filepath.Dir(route.Source), "proposed")
			}
			if filepath.Base(filepath.Clean(route.Working)) == "working" {
				route.Working = filepath.Join(filepath.Dir(route.Working), "planning")
			}
			if route.ReviewPrompt == "" {
				route.ReviewPrompt = ".spynel/prompts/goal-review.md"
			}
			route.AllowedNext = []string{"proposed", "planning", "active", "review", "reviewing", "waiting", "done", "abandoned"}
		}
	}
	cfg.Path = abs
	cfg.Root = rootForConfigPath(abs)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// PathForRoot returns the fixed configuration path for a workspace root.
func PathForRoot(root string) string {
	return filepath.Join(root, filepath.FromSlash(FileName))
}

func rootForConfigPath(path string) string {
	directory := filepath.Dir(path)
	if filepath.Base(path) == filepath.Base(filepath.FromSlash(FileName)) && filepath.Base(directory) == StateDirectoryName {
		return filepath.Dir(directory)
	}
	return directory
}

func migrateLegacyConfig(legacyPath, canonicalPath string) (Config, error) {
	data, err := os.ReadFile(legacyPath)
	if err != nil {
		return Config{}, err
	}
	var legacy struct {
		Workspace struct {
			StateDir string `yaml:"state_dir"`
		} `yaml:"workspace"`
	}
	if err := yaml.Unmarshal(data, &legacy); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", legacyPath, err)
	}
	workspaceRoot := filepath.Dir(legacyPath)
	legacyState := strings.TrimSpace(legacy.Workspace.StateDir)
	if legacyState != "" {
		resolved := legacyState
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(workspaceRoot, filepath.FromSlash(resolved))
		}
		if filepath.Clean(resolved) != filepath.Clean(filepath.Join(workspaceRoot, StateDirectoryName)) {
			return Config{}, fmt.Errorf("cannot automatically migrate legacy workspace.state_dir %q; move that state to %s and retry", legacyState, filepath.Join(workspaceRoot, StateDirectoryName))
		}
	}
	data, err = removeLegacyStateDir(data)
	if err != nil {
		return Config{}, fmt.Errorf("migrate %s: %w", legacyPath, err)
	}
	cfg, err := decode(data, canonicalPath)
	if err != nil {
		return Config{}, err
	}
	if err := os.MkdirAll(filepath.Dir(canonicalPath), 0o700); err != nil {
		return Config{}, err
	}
	if err := fsx.AtomicWriteFile(canonicalPath, data, 0o600); err != nil {
		return Config{}, fmt.Errorf("migrate configuration to %s: %w", canonicalPath, err)
	}
	if err := os.Remove(legacyPath); err != nil {
		if os.IsNotExist(err) {
			// Another process may have completed the same one-time migration
			// after this process read the legacy file. Keep the atomically
			// published canonical file and use its validated contents.
			return loadAt(canonicalPath)
		}
		return Config{}, fmt.Errorf("remove migrated legacy configuration %s: %w", legacyPath, err)
	}
	return cfg, nil
}

func removeLegacyStateDir(data []byte) ([]byte, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
		return data, nil
	}
	root := document.Content[0]
	for index := 0; index+1 < len(root.Content); index += 2 {
		if root.Content[index].Value != "workspace" || root.Content[index+1].Kind != yaml.MappingNode {
			continue
		}
		workspace := root.Content[index+1]
		for child := 0; child+1 < len(workspace.Content); child += 2 {
			if workspace.Content[child].Value == "state_dir" {
				workspace.Content = append(workspace.Content[:child], workspace.Content[child+2:]...)
				break
			}
		}
		break
	}
	return yaml.Marshal(&document)
}

func (c Config) Validate() error {
	var problems []string
	if c.Version != 1 {
		problems = append(problems, "version must be 1")
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
	if strings.TrimSpace(c.Channels.TUI.Theme) == "" {
		problems = append(problems, "channels.tui.theme is required")
	}
	if c.Harness.Name != "" {
		if _, ok := harness.Lookup(c.Harness.Name); !ok {
			problems = append(problems, "harness.name is not a supported coding harness")
		}
	}
	if c.Harness.Name == "acp" && strings.TrimSpace(c.Harness.ACPCommand) == "" {
		problems = append(problems, "harness.acp_command is required when harness.name is acp")
	}
	if strings.ContainsRune(c.Harness.ACPCommand, '\x00') {
		problems = append(problems, "harness.acp_command contains an invalid NUL byte")
	}
	for _, argument := range c.Harness.ACPArgs {
		if strings.ContainsRune(argument, '\x00') {
			problems = append(problems, "harness.acp_args contains an invalid NUL byte")
			break
		}
	}
	switch c.Harness.Sandbox {
	case "read-only", "workspace-write", "danger-full-access":
	default:
		problems = append(problems, "harness.sandbox must be read-only, workspace-write, or danger-full-access")
	}
	for _, field := range []struct{ name, value string }{
		{name: "chat_agent_prefix", value: c.Harness.ChatAgentPrefix},
		{name: "developer_agent_prefix", value: c.Harness.DeveloperAgentPrefix},
		{name: "reviewer_agent_prefix", value: c.Harness.ReviewerAgentPrefix},
		{name: "heartbeat_agent_prefix", value: c.Harness.HeartbeatAgentPrefix},
	} {
		invalidControl := strings.IndexFunc(field.value, unicode.IsControl) >= 0
		if len(field.value) > 256 || invalidControl {
			problems = append(problems, "harness."+field.name+" must be one line of at most 256 bytes")
		}
	}
	switch c.Harness.Reviews {
	case TaskReviewsSkipTrivial, TaskReviewsAlways, TaskReviewsNever:
	default:
		problems = append(problems, "harness.reviews must be skip-trivial, always, or never")
	}
	if c.Orchestrator.Enabled && c.Orchestrator.IntervalSec <= 0 {
		problems = append(problems, "orchestrator.interval_seconds must be positive")
	}
	if c.Orchestrator.MaxParallel <= 0 {
		problems = append(problems, "orchestrator.max_parallel must be positive")
	}
	if minutes := c.Orchestrator.SemanticHeartbeatMinutes; minutes != 0 && (minutes < 5 || minutes > 1440) {
		problems = append(problems, "orchestrator.semantic_heartbeat_minutes must be 0 (disabled) or between 5 and 1440")
	}
	switch c.Orchestrator.TaskNotifications {
	case TaskNotificationsOff, TaskNotificationsDecide, TaskNotificationsAlways:
	default:
		problems = append(problems, "orchestrator.task_notifications must be off, decide, or always")
	}
	seen := map[string]bool{}
	for i, route := range c.Orchestrator.Routes {
		prefix := fmt.Sprintf("orchestrator.routes[%d]", i)
		if route.Name == "" || route.Source == "" || route.Working == "" || route.Prompt == "" || route.RecoveryPrompt == "" {
			problems = append(problems, prefix+" requires name, source, working, prompt, and recovery_prompt")
		}
		if (route.Name == "tasks" || route.Name == "goals") && strings.TrimSpace(route.ReviewPrompt) == "" {
			problems = append(problems, prefix+".review_prompt is required for built-in task and goal routes")
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
	if c.Channels.Telegram.Mode == "webhook" && c.Channels.Telegram.Enabled && strings.TrimSpace(c.Channels.Telegram.WebhookSecret) == "" {
		problems = append(problems, "channels.telegram.webhook_secret is required in webhook mode")
	}
	if c.Channels.Telegram.Enabled && !HasAllowedTelegramUser(c.Channels.Telegram.AllowedUsers) {
		problems = append(problems, "channels.telegram.allowed_users requires at least one user when Telegram is enabled")
	}
	if c.Channels.WhatsApp.Enabled && !HasAllowedWhatsAppNumber(c.Channels.WhatsApp.AllowedNumbers) {
		problems = append(problems, "channels.whatsapp.allowed_numbers requires at least one number when WhatsApp is enabled")
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
	if c.Speech.MaxFileMB <= 0 || c.Speech.MaxDurationSec <= 0 || c.Speech.ChunkSeconds <= 0 || c.Speech.NumThreads <= 0 {
		problems = append(problems, "speech resource limits must be positive")
	}
	if !IsSpeechLanguage(c.Speech.Language) {
		problems = append(problems, "speech.language must be auto or a supported Parakeet language code")
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

// HasAllowedTelegramUser reports whether an allow-list contains at least one
// canonical numeric ID or username. Whitespace, punctuation-only values, and
// malformed entries do not make an enabled transport safe to start.
func HasAllowedTelegramUser(values []string) bool {
	for _, value := range values {
		if NormalizeTelegramUser(value) != "" {
			return true
		}
	}
	return false
}

// NormalizeTelegramUser returns the canonical value used for both startup
// validation and sender authorization. Positive decimal IDs and ASCII
// usernames containing letters, digits, or underscores are accepted.
func NormalizeTelegramUser(value string) string {
	value = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "@")))
	if value == "" || len(value) > 32 {
		return ""
	}
	if id, err := strconv.ParseInt(value, 10, 64); err == nil {
		if id <= 0 {
			return ""
		}
		return strconv.FormatInt(id, 10)
	}
	hasLetterOrDigit := false
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			hasLetterOrDigit = true
			continue
		}
		if character != '_' {
			return ""
		}
	}
	if !hasLetterOrDigit {
		return ""
	}
	return value
}

// HasAllowedWhatsAppNumber reports whether an allow-list contains at least
// one canonical digit-bearing phone number. Prefix- or punctuation-only
// entries remain empty so every configuration and adapter boundary fails
// closed in the same way.
func HasAllowedWhatsAppNumber(values []string) bool {
	for _, value := range values {
		if NormalizeAllowedWhatsAppNumber(value) != "" {
			return true
		}
	}
	return false
}

// NormalizeAllowedWhatsAppNumber validates a configured allow-list entry and
// returns the same canonical digits used for JID authorization. Formatting
// punctuation and whitespace are accepted, while letters, controls, empty
// values, and numbers beyond E.164's 15-digit maximum fail closed.
func NormalizeAllowedWhatsAppNumber(value string) string {
	for _, character := range value {
		if character >= '0' && character <= '9' || unicode.IsSpace(character) || unicode.IsPunct(character) || character == '+' {
			continue
		}
		return ""
	}
	normalized := NormalizeWhatsAppNumber(value)
	if normalized == "" || len(normalized) > 15 {
		return ""
	}
	return normalized
}

// NormalizeWhatsAppNumber returns the canonical digits used by WhatsApp JIDs
// and allow-list comparisons. A leading international 00 access prefix is
// equivalent to +; other domestic trunk prefixes are preserved because they
// cannot be interpreted safely without country-specific configuration.
func NormalizeWhatsAppNumber(value string) string {
	var digits strings.Builder
	for _, character := range value {
		if character >= '0' && character <= '9' {
			digits.WriteRune(character)
		}
	}
	normalized := digits.String()
	if strings.HasPrefix(normalized, "00") {
		normalized = normalized[2:]
	}
	return normalized
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
	base := filepath.Join(c.Root, StateDirectoryName)
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

// HarnessArgs returns the shell-free process arguments for the selected
// built-in profile or the user-configured custom ACP process.
func (c Config) HarnessArgs() []string {
	return harness.CommandArgs(c.Harness.Name, c.Harness.ACPArgs)
}

// ACPArgsJSON provides the stable scalar representation used by shared
// configuration commands and forms.
func (c Config) ACPArgsJSON() string {
	data, _ := json.Marshal(c.Harness.ACPArgs)
	return string(data)
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
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o700); err != nil {
		return fmt.Errorf("prepare %s: %w", cfg.Path, err)
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
