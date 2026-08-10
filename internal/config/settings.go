package config

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/agent0ai/spynel/internal/harness"
)

// Setting is the shared metadata used by command-based configuration and TUI
// form controls. Keys are stable public command identifiers.
type Setting struct {
	Key                 string
	Section             string
	Description         string
	DescriptionMarkdown bool
	Value               string
	Secret              bool
	Choices             []string
	Restart             bool
	Advanced            bool
}

// Settings returns every user-facing scalar setting in deterministic order.
func Settings(cfg Config) []Setting {
	values := []Setting{
		{Key: "harness.name", Section: "harness", Description: "Active coding harness", Value: cfg.Harness.Name, Choices: harness.Names()},
		{Key: "harness.model", Section: "harness", Description: "Harness model override", Value: cfg.Harness.Model},
		{Key: "harness.sandbox", Section: "harness", Description: "Coding-agent filesystem access; danger-full-access removes workspace confinement", Value: cfg.Harness.Sandbox, Choices: []string{"danger-full-access", "workspace-write", "read-only"}},
		{Key: "harness.reviews", Section: "harness", Description: "Task review policy; skip-trivial lets agents decide, always forces review, and never disables task review", Value: cfg.Harness.Reviews, Choices: []string{TaskReviewsSkipTrivial, TaskReviewsAlways, TaskReviewsNever}},
		{Key: "workspace.history_max_messages", Section: "config", Description: "Maximum recent messages passed to the harness (0 disables history)", Value: strconv.Itoa(cfg.Workspace.HistoryMaxMessages)},
		{Key: "workspace.history_char_limit", Section: "config", Description: "Maximum total history characters passed to the harness", Value: strconv.Itoa(cfg.Workspace.HistoryCharLimit)},
		{Key: "startup.enabled", Section: "config", Description: "Run Spynel automatically for this workspace", Value: formatBool(cfg.Startup.Enabled), Choices: []string{"on", "off"}},
		{Key: "harness.chat_agent_prefix", Section: "harness", Description: "Optionally prefix communication-agent messages with harness-native commands like `/goal`", Value: cfg.Harness.ChatAgentPrefix, Advanced: true},
		{Key: "harness.developer_agent_prefix", Section: "harness", Description: "Optionally prefix implementation and planning agent messages with harness-native commands like `/goal`", Value: cfg.Harness.DeveloperAgentPrefix, Advanced: true},
		{Key: "harness.reviewer_agent_prefix", Section: "harness", Description: "Optionally prefix task and goal reviewer messages with harness-native commands like `/goal`", Value: cfg.Harness.ReviewerAgentPrefix, Advanced: true},
		{Key: "harness.heartbeat_agent_prefix", Section: "harness", Description: "Optionally prefix semantic-heartbeat audit agent messages with harness-native commands like `/goal`", Value: cfg.Harness.HeartbeatAgentPrefix, Advanced: true},
		{Key: "harness.acp_command", Section: "harness", Description: "Executable name or absolute path for the custom ACP stdio agent", Value: cfg.Harness.ACPCommand, Advanced: true},
		{Key: "harness.acp_args", Section: "harness", Description: "Command-line arguments for custom ACP (quotes and escapes group values; no shell expansion)", Value: cfg.ACPArgsText(), Advanced: true},
		{Key: "workspace.attachment_max_mb", Section: "config", Description: "Maximum downloaded attachment size", Value: strconv.Itoa(cfg.Workspace.AttachmentMaxMB), Advanced: true},
		{Key: "channels.tui.title", Section: "config", Description: "Default TUI title", Value: cfg.Channels.TUI.Title, Advanced: true},
		{Key: "channels.tui.theme", Section: "config", Description: "Active color theme from .spynel/themes", Value: cfg.Channels.TUI.Theme, Advanced: true},
		{Key: "orchestrator.enabled", Section: "config", Description: "Run Markdown task and goal routes", Value: formatBool(cfg.Orchestrator.Enabled), Choices: []string{"on", "off"}, Advanced: true},
		{Key: "orchestrator.interval_seconds", Section: "config", Description: "Live route scan interval; saving resets the next scan deadline", Value: strconv.Itoa(cfg.Orchestrator.IntervalSec), Advanced: true},
		{Key: "orchestrator.semantic_heartbeat_minutes", Section: "config", Description: "Fixed delay after each agent workflow audit completes; 0 disables it", Value: strconv.Itoa(cfg.Orchestrator.SemanticHeartbeatMinutes), Advanced: true},
		{Key: "orchestrator.task_notifications", Section: "config", Description: "Live automatic task notification policy: off, agent-decided, or always send", Value: cfg.Orchestrator.TaskNotifications, Choices: []string{TaskNotificationsOff, TaskNotificationsDecide, TaskNotificationsAlways}, Advanced: true},
		{Key: "orchestrator.max_parallel", Section: "config", Description: "Live maximum concurrent Markdown jobs; lowering never cancels active work", Value: strconv.Itoa(cfg.Orchestrator.MaxParallel), Advanced: true},
		{Key: "orchestrator.routes", Section: "config", Description: "JSON array of live Markdown route definitions", Value: routesJSON(cfg.Orchestrator.Routes), Advanced: true},
		{Key: "extensions.enabled", Section: "config", Description: "Run trusted extension hooks after restart", Value: formatBool(cfg.Extensions.Enabled), Choices: []string{"on", "off"}, Restart: true, Advanced: true},
		{Key: "extensions.directory", Section: "config", Description: "Installed extension directory after restart", Value: cfg.Extensions.Directory, Restart: true, Advanced: true},
		{Key: "extensions.hook_timeout", Section: "config", Description: "Per-hook timeout after restart", Value: cfg.Extensions.HookTimeout, Restart: true, Advanced: true},
		{Key: "channels.telegram.token", Section: "telegram", Description: "Telegram bot token", Value: secretState(cfg.Channels.Telegram.Token), Secret: true},
		{Key: "channels.telegram.allowed_users", Section: "telegram", Description: "Required when enabled; find your numeric ID with [@userinfobot](https://t.me/userinfobot) (third-party)", DescriptionMarkdown: true, Value: strings.Join(cfg.Channels.Telegram.AllowedUsers, ",")},
		{Key: "channels.telegram.enabled", Section: "telegram", Description: "Run the Telegram integration", Value: formatBool(cfg.Channels.Telegram.Enabled), Choices: []string{"on", "off"}},
		{Key: "channels.telegram.name", Section: "telegram", Description: "Friendly bot name", Value: cfg.Channels.Telegram.Name, Advanced: true},
		{Key: "channels.telegram.token_env", Section: "telegram", Description: "Environment variable containing the bot token", Value: cfg.Channels.Telegram.TokenEnv, Advanced: true},
		{Key: "channels.telegram.mode", Section: "telegram", Description: "Telegram delivery mode", Value: cfg.Channels.Telegram.Mode, Choices: []string{"polling", "webhook"}, Advanced: true},
		{Key: "channels.telegram.webhook_url", Section: "telegram", Description: "Public HTTPS base URL for webhook mode", Value: cfg.Channels.Telegram.WebhookURL, Advanced: true},
		{Key: "channels.telegram.webhook_listen", Section: "telegram", Description: "Local HTTP address behind the webhook reverse proxy", Value: cfg.Channels.Telegram.WebhookListen, Advanced: true},
		{Key: "channels.telegram.webhook_secret", Section: "telegram", Description: "Webhook verification secret (required in webhook mode)", Value: secretState(cfg.Channels.Telegram.WebhookSecret), Secret: true, Advanced: true},
		{Key: "channels.telegram.poll_timeout_seconds", Section: "telegram", Description: "Long-poll timeout", Value: strconv.Itoa(cfg.Channels.Telegram.PollTimeoutSec), Advanced: true},
		{Key: "channels.telegram.group_mode", Section: "telegram", Description: "Group response policy", Value: cfg.Channels.Telegram.GroupMode, Choices: []string{"mention", "all", "off"}, Advanced: true},
		{Key: "channels.telegram.welcome_enabled", Section: "telegram", Description: "Welcome new group members", Value: formatBool(cfg.Channels.Telegram.WelcomeEnabled), Choices: []string{"on", "off"}, Advanced: true},
		{Key: "channels.telegram.welcome_message", Section: "telegram", Description: "Group welcome template ({name} is replaced)", Value: cfg.Channels.Telegram.WelcomeMessage, Advanced: true},
		{Key: "channels.telegram.notify_messages", Section: "telegram", Description: "Show a TUI notification for incoming messages", Value: formatBool(cfg.Channels.Telegram.NotifyMessages), Choices: []string{"on", "off"}, Advanced: true},
		{Key: "channels.telegram.attachment_max_age_hours", Section: "telegram", Description: "Downloaded attachment retention; 0 keeps files", Value: strconv.Itoa(cfg.Channels.Telegram.AttachmentMaxAgeHours), Advanced: true},
		{Key: "channels.whatsapp.mode", Section: "whatsapp", Description: "Personal self-chat or dedicated number", Value: cfg.Channels.WhatsApp.Mode, Choices: []string{"self-chat", "dedicated"}},
		{Key: "channels.whatsapp.allowed_numbers", Section: "whatsapp", Description: "Required when enabled; comma-separated phone numbers allowed to message Spynel", Value: strings.Join(cfg.Channels.WhatsApp.AllowedNumbers, ",")},
		{Key: "channels.whatsapp.enabled", Section: "whatsapp", Description: "Run the WhatsApp integration", Value: formatBool(cfg.Channels.WhatsApp.Enabled), Choices: []string{"on", "off"}},
		{Key: "channels.whatsapp.database", Section: "whatsapp", Description: "Persistent WhatsApp session database", Value: cfg.Channels.WhatsApp.Database, Advanced: true},
		{Key: "channels.whatsapp.allow_groups", Section: "whatsapp", Description: "Respond in groups when supported", Value: formatBool(cfg.Channels.WhatsApp.AllowGroups), Choices: []string{"on", "off"}, Advanced: true},
		{Key: "channels.whatsapp.poll_interval_seconds", Section: "whatsapp", Description: "Connection health-check interval", Value: strconv.Itoa(cfg.Channels.WhatsApp.PollIntervalSec), Advanced: true},
		{Key: "speech.enabled", Section: "config", Description: "Transcribe incoming voice messages", Value: formatBool(cfg.Speech.Enabled), Choices: []string{"on", "off"}, Advanced: true},
		{Key: "speech.language", Section: "config", Description: "Parakeet transcription language; English uses the English model and other values use multilingual auto-detection", Value: cfg.Speech.Language, Choices: SpeechLanguages(), Advanced: true},
		{Key: "speech.model_dir", Section: "config", Description: "Optional explicit Parakeet model directory; otherwise use the shared OS user cache", Value: cfg.Speech.ModelDir, Advanced: true},
		{Key: "speech.num_threads", Section: "config", Description: "CPU threads used for local transcription", Value: strconv.Itoa(cfg.Speech.NumThreads), Advanced: true},
		{Key: "speech.max_file_mb", Section: "config", Description: "Maximum accepted voice file size", Value: strconv.Itoa(cfg.Speech.MaxFileMB), Advanced: true},
		{Key: "speech.max_duration_seconds", Section: "config", Description: "Maximum voice duration processed", Value: strconv.Itoa(cfg.Speech.MaxDurationSec), Advanced: true},
		{Key: "speech.chunk_seconds", Section: "config", Description: "Maximum transcription chunk duration", Value: strconv.Itoa(cfg.Speech.ChunkSeconds), Advanced: true},
	}
	return values
}

func SettingByKey(cfg Config, key string) (Setting, bool) {
	key = normalizeSettingKey(key)
	for _, setting := range Settings(cfg) {
		if setting.Key == key {
			return setting, true
		}
	}
	return Setting{}, false
}

// SetSetting parses one command value transactionally into the strongly typed
// config model; invalid input never leaves a partially changed value behind.
func SetSetting(cfg *Config, key, value string) (Setting, error) {
	next := *cfg
	setting, err := setSetting(&next, key, value)
	if err != nil {
		return Setting{}, err
	}
	if err := next.Validate(); err != nil {
		return Setting{}, err
	}
	*cfg = next
	return setting, nil
}

// SetSettings applies a form submission as one validated transaction, which
// allows related values (for example webhook mode and URL) to change together.
func SetSettings(cfg *Config, values map[string]string) ([]Setting, error) {
	next := *cfg
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	changed := make([]Setting, 0, len(keys))
	for _, key := range keys {
		setting, err := setSetting(&next, key, values[key])
		if err != nil {
			return nil, err
		}
		changed = append(changed, setting)
	}
	if err := next.Validate(); err != nil {
		return nil, err
	}
	*cfg = next
	for index := range changed {
		changed[index], _ = SettingByKey(next, changed[index].Key)
	}
	return changed, nil
}

func setSetting(cfg *Config, key, value string) (Setting, error) { //nolint:gocyclo
	key = normalizeSettingKey(key)
	rawValue := value
	value = strings.TrimSpace(value)
	parseInteger := func(minimum int) (int, error) {
		number, err := strconv.Atoi(value)
		if err != nil || number < minimum {
			return 0, fmt.Errorf("%s must be an integer of at least %d", key, minimum)
		}
		return number, nil
	}
	parseBoolean := func() (bool, error) { return parseBool(value, key) }
	var err error
	switch key {
	case "workspace.attachment_max_mb":
		cfg.Workspace.AttachmentMaxMB, err = parseInteger(1)
	case "workspace.history_max_messages":
		cfg.Workspace.HistoryMaxMessages, err = parseInteger(0)
	case "workspace.history_char_limit":
		cfg.Workspace.HistoryCharLimit, err = parseInteger(0)
	case "harness.name":
		cfg.Harness.Name = harness.NormalizeName(value)
	case "harness.model":
		cfg.Harness.Model = value
	case "harness.sandbox":
		cfg.Harness.Sandbox = normalizeSandbox(value)
	case "harness.chat_agent_prefix":
		cfg.Harness.ChatAgentPrefix = value
	case "harness.developer_agent_prefix":
		cfg.Harness.DeveloperAgentPrefix = value
	case "harness.reviewer_agent_prefix":
		cfg.Harness.ReviewerAgentPrefix = value
	case "harness.heartbeat_agent_prefix":
		cfg.Harness.HeartbeatAgentPrefix = value
	case "harness.reviews":
		cfg.Harness.Reviews = normalizeTaskReviewMode(value)
	case "harness.acp_command":
		cfg.Harness.ACPCommand = value
	case "harness.acp_args":
		cfg.Harness.ACPArgs, err = ParseCommandLineArguments(rawValue)
	case "channels.tui.title":
		cfg.Channels.TUI.Title = value
	case "channels.tui.theme":
		cfg.Channels.TUI.Theme = value
	case "orchestrator.enabled":
		cfg.Orchestrator.Enabled, err = parseBoolean()
	case "orchestrator.interval_seconds":
		cfg.Orchestrator.IntervalSec, err = parseInteger(1)
	case "orchestrator.semantic_heartbeat_minutes":
		cfg.Orchestrator.SemanticHeartbeatMinutes, err = parseInteger(0)
	case "orchestrator.task_notifications":
		cfg.Orchestrator.TaskNotifications = strings.ToLower(value)
	case "orchestrator.max_parallel":
		cfg.Orchestrator.MaxParallel, err = parseInteger(1)
	case "orchestrator.routes":
		var routes []Route
		if err = json.Unmarshal([]byte(value), &routes); err == nil {
			cfg.Orchestrator.Routes = routes
		} else {
			err = fmt.Errorf("orchestrator.routes must be a JSON route array: %w", err)
		}
	case "extensions.enabled":
		cfg.Extensions.Enabled, err = parseBoolean()
	case "extensions.directory":
		cfg.Extensions.Directory = value
	case "extensions.hook_timeout":
		cfg.Extensions.HookTimeout = value
	case "channels.telegram.enabled":
		cfg.Channels.Telegram.Enabled, err = parseBoolean()
	case "channels.telegram.name":
		cfg.Channels.Telegram.Name = value
	case "channels.telegram.token":
		cfg.Channels.Telegram.Token = value
	case "channels.telegram.token_env":
		cfg.Channels.Telegram.TokenEnv = value
	case "channels.telegram.mode":
		cfg.Channels.Telegram.Mode = strings.ToLower(value)
	case "channels.telegram.webhook_url":
		cfg.Channels.Telegram.WebhookURL = value
	case "channels.telegram.webhook_listen":
		cfg.Channels.Telegram.WebhookListen = value
	case "channels.telegram.webhook_secret":
		cfg.Channels.Telegram.WebhookSecret = value
	case "channels.telegram.allowed_users":
		cfg.Channels.Telegram.AllowedUsers = parseList(value)
	case "channels.telegram.poll_timeout_seconds":
		cfg.Channels.Telegram.PollTimeoutSec, err = parseInteger(1)
	case "channels.telegram.group_mode":
		cfg.Channels.Telegram.GroupMode = strings.ToLower(value)
	case "channels.telegram.welcome_enabled":
		cfg.Channels.Telegram.WelcomeEnabled, err = parseBoolean()
	case "channels.telegram.welcome_message":
		cfg.Channels.Telegram.WelcomeMessage = value
	case "channels.telegram.notify_messages":
		cfg.Channels.Telegram.NotifyMessages, err = parseBoolean()
	case "channels.telegram.attachment_max_age_hours":
		cfg.Channels.Telegram.AttachmentMaxAgeHours, err = parseInteger(0)
	case "channels.whatsapp.enabled":
		cfg.Channels.WhatsApp.Enabled, err = parseBoolean()
	case "channels.whatsapp.mode":
		cfg.Channels.WhatsApp.Mode = strings.ToLower(value)
	case "channels.whatsapp.database":
		cfg.Channels.WhatsApp.Database = value
	case "channels.whatsapp.allowed_numbers":
		cfg.Channels.WhatsApp.AllowedNumbers = parseList(value)
	case "channels.whatsapp.allow_groups":
		cfg.Channels.WhatsApp.AllowGroups, err = parseBoolean()
	case "channels.whatsapp.poll_interval_seconds":
		cfg.Channels.WhatsApp.PollIntervalSec, err = parseInteger(2)
	case "speech.enabled":
		cfg.Speech.Enabled, err = parseBoolean()
	case "speech.language":
		cfg.Speech.Language = strings.ToLower(value)
	case "speech.model_dir":
		cfg.Speech.ModelDir = value
	case "speech.num_threads":
		cfg.Speech.NumThreads, err = parseInteger(1)
	case "speech.max_file_mb":
		cfg.Speech.MaxFileMB, err = parseInteger(1)
	case "speech.max_duration_seconds":
		cfg.Speech.MaxDurationSec, err = parseInteger(1)
	case "speech.chunk_seconds":
		cfg.Speech.ChunkSeconds, err = parseInteger(1)
	case "startup.enabled":
		cfg.Startup.Enabled, err = parseBoolean()
	default:
		return Setting{}, fmt.Errorf("unknown setting %q", key)
	}
	if err != nil {
		return Setting{}, err
	}
	setting, _ := SettingByKey(*cfg, key)
	return setting, nil
}

func normalizeSettingKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	switch key {
	case "harness", "harness.name":
		return "harness.name"
	case "model", "harness.model":
		return "harness.model"
	default:
		return key
	}
}

func normalizeTaskReviewMode(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func IsSecretSetting(key string) bool {
	key = normalizeSettingKey(key)
	return key == "channels.telegram.token" || key == "channels.telegram.webhook_secret"
}

func parseBool(value, key string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on", "enabled":
		return true, nil
	case "0", "false", "no", "off", "disabled":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be on or off", key)
	}
}

func parseList(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func formatBool(value bool) string {
	if value {
		return "on"
	}
	return "off"
}

func routesJSON(routes []Route) string {
	data, _ := json.Marshal(routes)
	return string(data)
}

func secretState(value string) string {
	if value == "" {
		return "not set"
	}
	return "set"
}
