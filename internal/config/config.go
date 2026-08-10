package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/agent0ai/spynel/internal/fsx"
	"github.com/agent0ai/spynel/internal/harness"
	"gopkg.in/yaml.v3"
)

const (
	// FileName is the canonical workspace-relative configuration path.
	FileName           = ".spynel/config.yaml"
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
	Title string `yaml:"title"`
	Theme string `yaml:"theme"`
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
	Name           string   `yaml:"name" json:"name"`
	Source         string   `yaml:"source" json:"source"`
	Working        string   `yaml:"working" json:"working"`
	Prompt         string   `yaml:"prompt" json:"prompt"`
	RecoveryPrompt string   `yaml:"recovery_prompt" json:"recovery_prompt"`
	ReviewPrompt   string   `yaml:"review_prompt,omitempty" json:"review_prompt,omitempty"`
	StaleAfter     string   `yaml:"stale_after" json:"stale_after"`
	AllowedNext    []string `yaml:"allowed_next" json:"allowed_next"`
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
			Reviews: TaskReviewsSkipTrivial,
		},
		Channels: Channels{
			TUI:      TUI{Title: "Spynel", Theme: "spynel"},
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
	data, _, err := normalizeLegacyConfig(data)
	if err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", abs, err)
	}
	cfg := Default()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", abs, err)
	}
	cfg.Harness.Name = harness.NormalizeName(cfg.Harness.Name)
	cfg.Harness.Sandbox = normalizeSandbox(cfg.Harness.Sandbox)
	cfg.Harness.ChatAgentPrefix = strings.TrimSpace(cfg.Harness.ChatAgentPrefix)
	cfg.Harness.DeveloperAgentPrefix = strings.TrimSpace(cfg.Harness.DeveloperAgentPrefix)
	cfg.Harness.ReviewerAgentPrefix = strings.TrimSpace(cfg.Harness.ReviewerAgentPrefix)
	cfg.Harness.HeartbeatAgentPrefix = strings.TrimSpace(cfg.Harness.HeartbeatAgentPrefix)
	cfg.Harness.Reviews = normalizeTaskReviewMode(cfg.Harness.Reviews)
	cfg.Speech.Language = strings.ToLower(strings.TrimSpace(cfg.Speech.Language))
	cfg.Path = abs
	cfg.Root = rootForConfigPath(abs)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// normalizeLegacyConfig accepts the one retired launch preference long enough
// to remove it from the decoded representation. A subsequent ordinary save
// writes only the canonical schema; all other unknown fields still fail closed.
func normalizeLegacyConfig(data []byte) ([]byte, bool, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, false, err
	}
	if len(document.Content) == 0 {
		return data, false, nil
	}
	root := document.Content[0]
	channels := mappingValue(root, "channels")
	tui := mappingValue(channels, "tui")
	changed := removeMappingKey(tui, "enabled")
	if !changed {
		return data, false, nil
	}
	normalized, err := yaml.Marshal(&document)
	return normalized, true, err
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}

func removeMappingKey(node *yaml.Node, key string) bool {
	if node == nil || node.Kind != yaml.MappingNode {
		return false
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			node.Content = append(node.Content[:index], node.Content[index+2:]...)
			return true
		}
	}
	return false
}

// NormalizeLegacyFile performs the one supported one-time schema cleanup.
// Current-schema files are not rewritten.
func NormalizeLegacyFile(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	normalized, changed, err := normalizeLegacyConfig(data)
	if err != nil || !changed {
		return false, err
	}
	if _, err := decode(normalized, path); err != nil {
		return false, err
	}
	if err := fsx.AtomicWriteFile(path, normalized, 0o600); err != nil {
		return false, err
	}
	return true, nil
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
		if !utf8.ValidString(argument) {
			problems = append(problems, "harness.acp_args contains invalid UTF-8")
			break
		}
		if strings.ContainsAny(argument, "\r\n") {
			problems = append(problems, "harness.acp_args cannot contain multiline arguments")
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
	return strings.ToLower(strings.TrimSpace(value))
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

// HarnessSessionsPath returns the canonical session map for one harness.
func (c Config) HarnessSessionsPath(name string) string {
	name = strings.NewReplacer("/", "-", "\\", "-").Replace(strings.ToLower(strings.TrimSpace(name)))
	if name == "" {
		name = "unselected"
	}
	return c.StatePath("runtime", "harness-"+name+"-sessions.json")
}

// HarnessArgs returns the shell-free process arguments for the selected
// built-in profile or the user-configured custom ACP process.
func (c Config) HarnessArgs() []string {
	return harness.CommandArgs(c.Harness.Name, c.Harness.ACPArgs)
}

// ACPArgsText provides the stable command-line scalar representation used by
// every shared settings surface. Validate guarantees it is representable.
func (c Config) ACPArgsText() string {
	text, _ := FormatCommandLineArguments(c.Harness.ACPArgs)
	return text
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
	writeMu sync.Mutex
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
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.current
	if err := change(&next); err != nil {
		return s.current, err
	}
	if err := Save(next); err != nil {
		return s.current, err
	}
	reloaded, err := Load(next.Path)
	if err != nil {
		return s.current, fmt.Errorf("reload saved configuration: %w", err)
	}
	s.current = reloaded
	s.publishLocked(reloaded)
	return reloaded, nil
}

func (s *Store) publishLocked(next Config) {
	select {
	case <-s.updates:
	default:
	}
	s.updates <- next
}

func (s *Store) Updates() <-chan Config { return s.updates }
