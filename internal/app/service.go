package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/frdel/spynel/internal/channel"
	"github.com/frdel/spynel/internal/config"
	"github.com/frdel/spynel/internal/core"
	"github.com/frdel/spynel/internal/extensions"
	"github.com/frdel/spynel/internal/fsx"
	"github.com/frdel/spynel/internal/harness"
	"github.com/frdel/spynel/internal/history"
	"github.com/frdel/spynel/internal/orchestrator"
	"github.com/frdel/spynel/internal/shortid"
)

type Service struct {
	// Config is the immutable structural configuration used to construct
	// histories, hooks, routes, and workspace paths. Live scalar values are
	// always read through Settings, avoiding races with remote form commands.
	Config       config.Config
	Harness      harness.Harness
	History      *history.Store
	Hooks        extensions.Runner
	Orchestrator *orchestrator.Manager
	Runtime      *Runtime
	Settings     *config.Store
	Startup      interface {
		Sync(config.Config, bool) error
	}
	configurationMu sync.Mutex
	titleMu         sync.Mutex
	titleChanges    chan string
	connectionMu    sync.RWMutex
	connections     map[string]channel.ConnectionStatus
	pairingMu       sync.RWMutex
	pairing         map[string]channel.PairingEvent
	pairingEvents   chan channel.PairingEvent
	noticeEvents    chan channel.Notice
	restartRequests chan struct{}
}

func New(cfg config.Config, target harness.Harness) *Service {
	return NewWithRuntime(cfg, target, NewRuntime())
}

func NewWithRuntime(cfg config.Config, target harness.Harness, runtime *Runtime) *Service {
	hooks := extensions.Runner{Directory: cfg.Resolve(cfg.Extensions.Directory), Timeout: cfg.Extensions.Timeout()}
	store := history.New(cfg.StatePath("history"))
	manager := orchestrator.New(cfg, target, hooks)
	connections := map[string]channel.ConnectionStatus{
		"telegram": {Name: "telegram", State: channel.ConnectionUnconfigured},
		"whatsapp": {Name: "whatsapp", State: channel.ConnectionUnconfigured},
	}
	if cfg.Channels.Telegram.Enabled {
		connections["telegram"] = channel.ConnectionStatus{Name: "telegram", State: channel.ConnectionConnecting}
	}
	if cfg.Channels.WhatsApp.Enabled {
		connections["whatsapp"] = channel.ConnectionStatus{Name: "whatsapp", State: channel.ConnectionConnecting}
	}
	service := &Service{
		Config:          cfg,
		Harness:         target,
		History:         store,
		Hooks:           hooks,
		Orchestrator:    manager,
		Runtime:         runtime,
		Settings:        config.NewStore(cfg),
		titleChanges:    make(chan string, 1),
		connections:     connections,
		pairing:         map[string]channel.PairingEvent{},
		pairingEvents:   make(chan channel.PairingEvent, 1),
		noticeEvents:    make(chan channel.Notice, 8),
		restartRequests: make(chan struct{}, 1),
	}
	manager.Log = runtime.Log
	manager.JobStarted = func(sessionKey, description string) int {
		return runtime.BeginJob(sessionKey, "orchestrator", "markdown", description)
	}
	manager.JobFinished = runtime.EndJob
	return service
}

func (s *Service) Start(ctx context.Context) error {
	return s.Harness.Start(ctx)
}

func (s *Service) Handle(ctx context.Context, message core.Message, emit core.Emit) error {
	if message.ReceivedAt.IsZero() {
		message.ReceivedAt = time.Now().UTC()
	}
	message.Text = strings.TrimSpace(message.Text)
	if message.Text == "" {
		return nil
	}
	if s.Config.Extensions.Enabled {
		output, err := s.Hooks.Run(ctx, "message.received", map[string]any{
			"channel": message.Channel, "conversation": message.Conversation,
			"sender": message.Sender, "text": message.Text,
		})
		if err != nil {
			return err
		}
		if output.Cancel {
			if output.Message != "" && emit != nil {
				emit(core.Event{Kind: core.EventFinal, Text: output.Message, Done: true, Local: true})
			}
			return nil
		}
		if text, ok := output.Payload["text"].(string); ok {
			message.Text = text
		}
	}
	if _, err := s.History.Append(message.Channel, message.Conversation, history.Entry{
		At: message.ReceivedAt, Role: "user", Sender: message.Sender, Content: redactSensitiveCommand(message.Text),
	}); err != nil {
		return err
	}
	if strings.HasPrefix(message.Text, "/") {
		return s.handleCommand(ctx, message, emit)
	}
	prompt, err := s.chatPrompt(message)
	if err != nil {
		return err
	}
	if s.Config.Extensions.Enabled {
		output, err := s.Hooks.Run(ctx, "harness.before", map[string]any{
			"session_key": sessionKey(message), "prompt": prompt, "channel": message.Channel,
		})
		if err != nil {
			return err
		}
		if output.Cancel {
			return s.localReply(message, output.Message, emit)
		}
		if value, ok := output.Payload["prompt"].(string); ok {
			prompt = value
		}
	}
	key := sessionKey(message)
	jobID := s.Runtime.BeginJob(key, message.Channel, message.Conversation, message.Text)
	wrapped := s.wrapEmit(message, jobID, emit)
	threadID, steered, err := s.Harness.Send(ctx, key, prompt, wrapped)
	if err != nil {
		s.Runtime.EndJob(jobID)
		return err
	}
	if emit != nil {
		verb := "Harness turn started"
		if steered {
			verb = "Message steered into active harness turn"
		}
		emit(core.Event{Kind: core.EventStatus, Text: verb, ThreadID: threadID})
	}
	return nil
}

func (s *Service) chatPrompt(message core.Message) (string, error) {
	cfg := s.Settings.Snapshot()
	recent, fullPath, err := s.History.RecentBounded(message.Channel, message.Conversation, cfg.Workspace.HistoryMaxMessages, cfg.Workspace.HistoryCharLimit)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(s.Config.StatePath("prompts", "chat.md"))
	if err != nil {
		return "", err
	}
	prompt := string(data)
	// Workspaces initialized before channel prompt overrides were retired may
	// still carry the old, user-overridable template. Remove the complete stock
	// lines first, then neutralize tokens in customized variants.
	for _, retired := range []string{
		"Project binding: {{PROJECT}}\r\n",
		"Project binding: {{PROJECT}}\n",
		"Channel instructions: {{INSTRUCTIONS}}\r\n",
		"Channel instructions: {{INSTRUCTIONS}}\n",
	} {
		prompt = strings.ReplaceAll(prompt, retired, "")
	}
	prompt = strings.ReplaceAll(prompt, "{{PROJECT}}", "")
	prompt = strings.ReplaceAll(prompt, "{{INSTRUCTIONS}}", "")
	replacements := map[string]string{
		"{{HISTORY_FILE}}":   fullPath,
		"{{CHANNEL}}":        message.Channel,
		"{{CONVERSATION}}":   message.Conversation,
		"{{RECENT_HISTORY}}": recent,
	}
	for from, to := range replacements {
		prompt = strings.ReplaceAll(prompt, from, to)
	}
	return prompt, nil
}

func (s *Service) wrapEmit(message core.Message, jobID int, downstream core.Emit) core.Emit {
	return func(event core.Event) {
		if event.Done && (event.Kind == core.EventFinal || event.Kind == core.EventError) {
			text := event.Text
			if s.Config.Extensions.Enabled {
				output, err := s.Hooks.Run(context.Background(), "harness.after", map[string]any{
					"session_key": sessionKey(message), "text": text, "kind": event.Kind,
				})
				if err != nil {
					event.Kind = core.EventError
					text = "harness.after extension hook failed: " + err.Error()
				} else if output.Cancel {
					text = output.Message
				} else {
					if value, ok := output.Payload["text"].(string); ok {
						text = value
					}
				}
			}
			event.Text = text
			_, _ = s.History.Append(message.Channel, message.Conversation, history.Entry{
				At: time.Now().UTC(), Role: "assistant", Content: text,
			})
		}
		if downstream != nil {
			downstream(event)
		}
		if event.Done {
			s.Runtime.EndJob(jobID)
		}
	}
}

func (s *Service) handleCommand(ctx context.Context, message core.Message, emit core.Emit) error {
	commandLine := strings.TrimSpace(strings.TrimPrefix(message.Text, "/"))
	parts := strings.Fields(commandLine)
	if len(parts) == 0 {
		return s.localReply(message, commandHelp, emit)
	}
	command := strings.ToLower(parts[0])
	if message.Channel == "telegram" {
		command = strings.SplitN(command, "@", 2)[0]
	}
	remainder := strings.TrimSpace(strings.TrimPrefix(commandLine, parts[0]))
	switch command {
	case "help":
		return s.localReply(message, helpFor(remainder), emit)
	case "commands":
		return s.localReply(message, commandHelp, emit)
	case "welcome":
		if message.Channel == "tui" && emit != nil {
			screen := s.WelcomeScreen()
			emit(core.Event{Kind: core.EventScreen, Screen: &screen, Local: true})
			return nil
		}
		return s.localReply(message, s.welcomeText(), emit)
	case "new":
		if err := s.Harness.ResetSession(sessionKey(message)); err != nil {
			return s.localReply(message, "Cannot start a new thread: "+err.Error(), emit)
		}
		return s.localReply(message, "A new harness thread will be created for the next message. This channel history remains on disk.", emit)
	case "status":
		status, err := s.statusText(message)
		if err != nil {
			return err
		}
		return s.localReply(message, status, emit)
	case "config":
		return s.configurationCommand(message, "config", remainder, emit)
	case "telegram":
		if message.Channel == "telegram" {
			return s.localReply(message, "Telegram cannot be configured from Telegram itself. Use the TUI or WhatsApp so a bad setting cannot lock you out of this channel.", emit)
		}
		return s.configurationCommand(message, "telegram", remainder, emit)
	case "whatsapp":
		if message.Channel == "whatsapp" {
			return s.localReply(message, "WhatsApp cannot be configured from WhatsApp itself. Use the TUI or Telegram so a bad setting cannot lock you out of this channel.", emit)
		}
		return s.configurationCommand(message, "whatsapp", remainder, emit)
	case "harness":
		return s.harnessCommand(message, remainder, emit)
	case "model":
		return s.modelCommand(ctx, message, remainder, emit)
	case "title":
		if remainder == "" {
			return s.localReply(message, "Usage: /title <name>", emit)
		}
		if len([]rune(remainder)) > maxTitleChars {
			return s.localReply(message, fmt.Sprintf("Title is too long (maximum %d characters).", maxTitleChars), emit)
		}
		if err := fsx.AtomicWriteFile(s.Config.StatePath("tui-title"), []byte(remainder+"\n"), 0o600); err != nil {
			return s.localReply(message, "Cannot save title: "+err.Error(), emit)
		}
		s.publishTitle(remainder)
		return s.localReply(message, "Title changed to "+remainder+".", emit)
	case "history":
		_, path, err := s.History.Recent(message.Channel, message.Conversation, 1)
		if err != nil {
			return err
		}
		return s.localReply(message, "Full independent history: "+path, emit)
	case "resume":
		return s.resumeCommand(message, emit)
	case "log", "logs":
		return s.logCommand(message, remainder, emit)
	case "jobs":
		return s.localReply(message, formatJobs(s.Runtime.Jobs()), emit)
	case "job":
		return s.jobCommand(ctx, message, remainder, emit)
	case "clear":
		if err := s.Harness.ResetSession(sessionKey(message)); err != nil {
			return s.localReply(message, "Cannot clear this conversation: "+err.Error(), emit)
		}
		if err := s.History.Clear(message.Channel, message.Conversation); err != nil {
			return err
		}
		if emit != nil {
			emit(core.Event{Kind: core.EventFinal, Text: "Conversation history and harness thread cleared.", Clear: true, Done: true, Local: true})
		}
		return nil
	case "steer":
		if remainder == "" {
			return s.localReply(message, "Usage: /steer <message>", emit)
		}
		if !s.Harness.IsActive(sessionKey(message)) {
			return s.localReply(message, "There is no active turn. Send the message normally to start a turn.", emit)
		}
		key := sessionKey(message)
		job, ok := s.Runtime.JobForSession(key)
		if !ok {
			job.ID = s.Runtime.BeginJob(key, message.Channel, message.Conversation, remainder)
		}
		threadID, _, err := s.Harness.Send(ctx, key, remainder, s.wrapEmit(message, job.ID, emit))
		if err != nil {
			s.Runtime.EndJob(job.ID)
			return err
		}
		if emit != nil {
			emit(core.Event{Kind: core.EventStatus, Text: "Steered active turn", ThreadID: threadID})
		}
		return nil
	case "stop":
		stopped, err := s.Harness.Interrupt(ctx, sessionKey(message))
		if err != nil {
			return s.localReply(message, "Cannot stop the active execution: "+err.Error(), emit)
		}
		if !stopped {
			return s.localReply(message, "There is no active execution to stop.", emit)
		}
		if job, ok := s.Runtime.JobForSession(sessionKey(message)); ok {
			s.Runtime.EndJob(job.ID)
		}
		return s.localReply(message, "Stop requested for the active execution.", emit)
	case "restart":
		if err := s.localReply(message, "Restarting Spynel. Saved configuration and conversation history will be restored.", emit); err != nil {
			return err
		}
		s.requestRestart()
		return nil
	case "task", "todo":
		if remainder == "" {
			return s.localReply(message, "Usage: /task <title and request>", emit)
		}
		path, err := orchestrator.Create(s.Config, "tasks", remainder, "")
		if err != nil {
			return err
		}
		return s.localReply(message, "Created task: "+path, emit)
	case "goal":
		if remainder == "" {
			return s.localReply(message, "Usage: /goal <objective>", emit)
		}
		path, err := orchestrator.Create(s.Config, "goals", remainder, "")
		if err != nil {
			return err
		}
		return s.localReply(message, "Created goal: "+path, emit)
	case "run":
		if err := s.Orchestrator.ScanOnce(ctx); err != nil {
			return err
		}
		return s.localReply(message, "Orchestrator scan started.", emit)
	case "extension", "extensions":
		return s.extensionCommand(ctx, message, remainder, emit)
	case "quit", "exit":
		return s.localReply(message, "/quit exits an interactive TUI only; it does not stop the Telegram or WhatsApp server.", emit)
	default:
		return s.localReply(message, "Unknown command /"+command+". Use /help.", emit)
	}
}

func (s *Service) logCommand(message core.Message, remainder string, emit core.Emit) error {
	usage := "Usage: /log [page <number>|search <text>|clear]"
	parts := strings.Fields(remainder)
	if len(parts) == 0 {
		return s.localReply(message, formatLogs(s.Runtime.Logs()), emit)
	}
	switch strings.ToLower(parts[0]) {
	case "page":
		if len(parts) != 2 {
			return s.localReply(message, usage, emit)
		}
		page, err := strconv.Atoi(parts[1])
		if err != nil || page < 1 {
			return s.localReply(message, "Log page must be a positive number.\n\n"+usage, emit)
		}
		return s.localReply(message, formatLogPage(s.Runtime.Logs(), page), emit)
	case "search":
		query := strings.TrimSpace(strings.TrimPrefix(remainder, parts[0]))
		if query == "" {
			return s.localReply(message, "Usage: /log search <text>", emit)
		}
		return s.localReply(message, formatLogSearch(s.Runtime.Logs(), query), emit)
	case "clear":
		if len(parts) != 1 {
			return s.localReply(message, usage, emit)
		}
		count := s.Runtime.ClearLogs()
		return s.localReply(message, fmt.Sprintf("Cleared %d runtime log entries.", count), emit)
	default:
		return s.localReply(message, usage, emit)
	}
}

func (s *Service) jobCommand(ctx context.Context, message core.Message, remainder string, emit core.Emit) error {
	parts := strings.Fields(remainder)
	if len(parts) != 2 || strings.ToLower(parts[0]) != "kill" {
		return s.localReply(message, "Usage: /job kill <number>", emit)
	}
	id, err := strconv.Atoi(parts[1])
	if err != nil || id <= 0 {
		return s.localReply(message, "Job number must be positive. Use /jobs to list running jobs.", emit)
	}
	job, ok := s.Runtime.Job(id)
	if !ok {
		return s.localReply(message, fmt.Sprintf("Job %d is not running. Use /jobs to list running jobs.", id), emit)
	}
	stopped, err := s.Harness.Interrupt(ctx, job.SessionKey)
	if err != nil {
		return s.localReply(message, fmt.Sprintf("Cannot kill job %d: %v", id, err), emit)
	}
	if !stopped {
		s.Runtime.EndJob(id)
		return s.localReply(message, fmt.Sprintf("Job %d was already finished.", id), emit)
	}
	s.Runtime.EndJob(id)
	return s.localReply(message, fmt.Sprintf("Kill requested for job %d.", id), emit)
}

// TitleChanges publishes persisted title updates so a running TUI can reflect
// commands received from any channel.
func (s *Service) TitleChanges() <-chan string {
	return s.titleChanges
}

func (s *Service) publishTitle(title string) {
	s.titleMu.Lock()
	defer s.titleMu.Unlock()
	select {
	case <-s.titleChanges:
	default:
	}
	s.titleChanges <- title
}

// RestartRequests publishes process-wide restart requests after the command
// acknowledgment has been persisted and emitted to the issuing channel.
func (s *Service) RestartRequests() <-chan struct{} {
	return s.restartRequests
}

func (s *Service) requestRestart() {
	select {
	case s.restartRequests <- struct{}{}:
	default:
	}
}

// SetConnectionStatus keeps shared channel health available to commands on
// every transport, not only to the TUI that renders the live badges.
func (s *Service) SetConnectionStatus(status channel.ConnectionStatus) {
	if status.Name == "" {
		return
	}
	s.connectionMu.Lock()
	s.connections[status.Name] = status
	s.connectionMu.Unlock()
}

func (s *Service) connectionStatus(name string) channel.ConnectionStatus {
	s.connectionMu.RLock()
	status, ok := s.connections[name]
	s.connectionMu.RUnlock()
	if !ok {
		return channel.ConnectionStatus{Name: name, State: channel.ConnectionUnconfigured}
	}
	return status
}

func (s *Service) statusText(message core.Message) (string, error) {
	leases, dispatches, err := s.Orchestrator.Status()
	if err != nil {
		return "", err
	}
	runtimeStatus := s.Runtime.Status()
	thread := "not started"
	if threadID := s.Harness.ThreadID(sessionKey(message)); threadID != "" {
		thread = "`" + emptyAs(shortid.Display(threadID), "unknown") + "`"
	}
	turn := "idle"
	if s.Harness.IsActive(sessionKey(message)) {
		turn = "active"
	}
	cfg := s.Settings.Snapshot()
	harnessStatus := "connected"
	if availability, ok := s.Harness.(harness.Availability); ok {
		if ready, detail := availability.Available(); !ready {
			harnessStatus = "unavailable"
			if detail != "" {
				harnessStatus += " — " + detail
			}
		}
	}
	return strings.Join([]string{
		"# Status",
		"",
		"- Title: " + s.currentTitle(),
		"- Telegram: " + connectionIndicator(s.connectionStatus("telegram")),
		"- WhatsApp: " + connectionIndicator(s.connectionStatus("whatsapp")),
		fmt.Sprintf("- Jobs: %d — `/jobs`", runtimeStatus.Jobs),
		fmt.Sprintf("- Logs: %d — `/log`", runtimeStatus.Logs),
		"- Coding harness: " + emptyAs(cfg.Harness.Name, "not selected") + " (" + harnessStatus + ")",
		"- Model: " + emptyAs(cfg.Harness.Model, "harness default"),
		"- Agent filesystem access: " + cfg.Harness.Sandbox,
		"- Run at startup: " + enabledText(cfg.Startup.Enabled),
		"- Thread: " + thread,
		"- Turn: " + turn,
		fmt.Sprintf("- Orchestrator: %d leases, %d dispatch goroutines", leases, dispatches),
	}, "\n"), nil
}

func enabledText(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func (s *Service) currentTitle() string {
	title := s.Settings.Snapshot().Channels.TUI.Title
	data, err := os.ReadFile(s.Config.StatePath("tui-title"))
	if err == nil && strings.TrimSpace(string(data)) != "" {
		title = strings.TrimSpace(string(data))
	}
	return title
}

func connectionIndicator(status channel.ConnectionStatus) string {
	switch status.State {
	case channel.ConnectionConnected:
		return "● connected"
	case channel.ConnectionConnecting:
		return "◐ connecting"
	case channel.ConnectionError:
		if detail := strings.TrimSpace(strings.ReplaceAll(status.Detail, "\n", " ")); detail != "" {
			return "▲ error — " + detail
		}
		return "▲ error"
	default:
		return "○ not configured"
	}
}

func (s *Service) extensionCommand(ctx context.Context, message core.Message, remainder string, emit core.Emit) error {
	parts := strings.Fields(remainder)
	directory := s.Config.Resolve(s.Config.Extensions.Directory)
	if len(parts) == 0 || parts[0] == "list" {
		names, err := extensions.List(directory)
		if err != nil {
			return err
		}
		if len(names) == 0 {
			return s.localReply(message, "No project extensions are installed.", emit)
		}
		return s.localReply(message, "Installed extensions:\n- "+strings.Join(names, "\n- "), emit)
	}
	switch parts[0] {
	case "install":
		if len(parts) < 2 {
			return s.localReply(message, "Usage: /extension install <git-url> [name]", emit)
		}
		name := ""
		if len(parts) > 2 {
			name = parts[2]
		}
		path, err := extensions.Install(ctx, directory, parts[1], name)
		if err != nil {
			return err
		}
		return s.localReply(message, "Installed trusted extension: "+path, emit)
	case "remove":
		if len(parts) != 2 {
			return s.localReply(message, "Usage: /extension remove <name>", emit)
		}
		if err := extensions.Remove(directory, parts[1]); err != nil {
			return err
		}
		return s.localReply(message, "Removed extension "+parts[1]+". This cannot be recovered from Spynel; reinstall the Git repository if needed.", emit)
	default:
		return s.localReply(message, "Usage: /extension [list|install <git-url> [name]|remove <name>]", emit)
	}
}

func (s *Service) localReply(message core.Message, text string, emit core.Emit) error {
	if text == "" {
		text = "Command completed."
	}
	_, err := s.History.Append(message.Channel, message.Conversation, history.Entry{
		At: time.Now().UTC(), Role: "assistant", Content: text,
	})
	if err != nil {
		return err
	}
	if emit != nil {
		emit(core.Event{Kind: core.EventFinal, Text: text, Done: true, Local: true})
	}
	return nil
}

func sessionKey(message core.Message) string {
	return "chat:" + message.Channel + ":" + message.Conversation
}

func emptyAs(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

var slashCommands = []core.SlashCommand{
	{Value: "/help", Usage: "/help", Description: "Show the help topic index"},
	{Value: "/help about", Usage: "/help about", Description: "Learn what Spynel does and where it stores state"},
	{Value: "/help commands", Usage: "/help commands", Description: "Show the complete slash-command reference"},
	{Value: "/help extensions", Usage: "/help extensions", Description: "Learn how trusted project extensions work"},
	{Value: "/help config", Usage: "/help config", Description: "Understand spynel.yaml and path resolution"},
	{Value: "/help channels", Usage: "/help channels", Description: "Learn about the TUI, Telegram, and WhatsApp"},
	{Value: "/help workflows", Usage: "/help workflows", Description: "Learn how tasks, goals, and scans work"},
	{Value: "/status", Usage: "/status", Description: "Show channel, runtime, thread, and orchestrator state"},
	{Value: "/welcome", Usage: "/welcome", Description: "Open or show the Spynel welcome guide"},
	{Value: "/config", Usage: "/config", Description: "Open or show Spynel configuration"},
	{Value: "/config get ", Usage: "/config get <key>", Description: "Read one configuration value"},
	{Value: "/config set ", Usage: "/config set <key> <value>", Description: "Persist one configuration value"},
	{Value: "/harness ", Usage: "/harness [name]", Description: "Show or select the coding harness"},
	{Value: "/model ", Usage: "/model [name]", Description: "Show or select the harness model"},
	{Value: "/telegram", Usage: "/telegram", Description: "Open or show Telegram configuration"},
	{Value: "/telegram on", Usage: "/telegram on", Description: "Enable Telegram"},
	{Value: "/telegram off", Usage: "/telegram off", Description: "Disable Telegram"},
	{Value: "/telegram set ", Usage: "/telegram set <key> <value>", Description: "Persist a Telegram setting"},
	{Value: "/whatsapp", Usage: "/whatsapp", Description: "Open or show WhatsApp configuration"},
	{Value: "/whatsapp on", Usage: "/whatsapp on", Description: "Enable WhatsApp"},
	{Value: "/whatsapp off", Usage: "/whatsapp off", Description: "Disable WhatsApp"},
	{Value: "/whatsapp set ", Usage: "/whatsapp set <key> <value>", Description: "Persist a WhatsApp setting"},
	{Value: "/title ", Usage: "/title <name>", Description: "Rename and persist this TUI window"},
	{Value: "/new", Usage: "/new", Description: "Start a new harness thread for this channel"},
	{Value: "/steer ", Usage: "/steer <message>", Description: "Explicitly steer the active harness turn"},
	{Value: "/stop", Usage: "/stop", Description: "Stop the active execution for this conversation"},
	{Value: "/restart", Usage: "/restart", Description: "Restart Spynel and restore saved state"},
	{Value: "/history", Usage: "/history", Description: "Show the complete history file"},
	{Value: "/resume", Usage: "/resume", Description: "Browse saved conversations and branch one into the TUI"},
	{Value: "/log", Usage: "/log", Description: "Show the newest page of captured runtime logs"},
	{Value: "/log page ", Usage: "/log page <number>", Description: "Show an older runtime log page"},
	{Value: "/log search ", Usage: "/log search <text>", Description: "Search captured runtime logs"},
	{Value: "/log clear", Usage: "/log clear", Description: "Clear captured runtime logs"},
	{Value: "/jobs", Usage: "/jobs", Description: "List running agent jobs"},
	{Value: "/job kill ", Usage: "/job kill <number>", Description: "Stop a running agent job by number"},
	{Value: "/clear", Usage: "/clear", Description: "Clear this conversation's history and harness thread"},
	{Value: "/task ", Usage: "/task <request>", Description: "Create a markdown task"},
	{Value: "/goal ", Usage: "/goal <objective>", Description: "Create a markdown goal"},
	{Value: "/run", Usage: "/run", Description: "Trigger an orchestrator scan"},
	{Value: "/extension list", Usage: "/extension list", Description: "List installed project extensions"},
	{Value: "/extension install ", Usage: "/extension install URL", Description: "Install a trusted Git extension"},
	{Value: "/extension remove ", Usage: "/extension remove NAME", Description: "Remove an installed extension"},
	{Value: "/quit", Usage: "/quit", Description: "Exit the TUI only (Telegram and WhatsApp use server lifecycle)"},
}

const maxTitleChars = 80

var commandHelp = formatCommandHelp(slashCommands)

var helpTopics = []struct {
	name        string
	description string
	body        string
}{
	{
		name:        "about",
		description: "What Spynel does and where it stores state",
		body:        "# About Spynel\n\nSpynel connects local coding agents to a terminal UI, Telegram, and WhatsApp while keeping each conversation independent. It also turns durable Markdown task and goal files into agent work.\n\nThe project configuration is `spynel.yaml`. Runtime state, histories, harness sessions, attachments, and local UI preferences live under the configured state directory, `.spynel` by default.",
	},
	{
		name:        "commands",
		description: "The complete slash-command reference",
		body:        commandHelp,
	},
	{
		name:        "extensions",
		description: "Trusted project extensions and their hooks",
		body:        "# Extensions\n\nExtensions are explicitly installed Git repositories that can run hooks around messages, harness calls, and orchestration. Their directory and whether hooks are enabled are configured under `extensions` in `spynel.yaml`.\n\n- `/extension list` lists installed extensions.\n- `/extension install <git-url> [name]` installs a repository you trust.\n- `/extension remove <name>` removes an installed extension.",
	},
	{
		name:        "config",
		description: "spynel.yaml settings and path resolution",
		body:        "# Configuration\n\n`spynel.yaml` controls the workspace, harness, channels, speech processing, orchestration routes, and extensions. `/config` shows the shared settings, `/config get <key>` reads one value, and `/config set <key> <value>` atomically validates and persists a change from any channel. `/harness [name]` and `/model [name]` are concise selectors. `harness.sandbox` accepts `danger-full-access`, `workspace-write`, or `read-only`; unrestricted access is the default. Relative paths are resolved from the directory containing `spynel.yaml`, so a project can be moved without rewriting local paths.",
	},
	{
		name:        "channels",
		description: "The TUI, Telegram, and WhatsApp",
		body:        "# Channels\n\nThe TUI, each Telegram chat, and each WhatsApp chat keep independent durable histories and harness threads. All channels share the application slash commands and Markdown-aware responses.\n\nUse `/status` to inspect shared connection, runtime, harness, and orchestrator indicators, `/history` to locate the current conversation's history file, `/clear` to erase that history and discard its harness thread, `/stop` to interrupt its active execution, and `/new` to start a fresh harness thread without erasing channel history. `/restart` acknowledges the request, cleanly stops the current runtime, and relaunches Spynel with saved configuration and histories intact. `/log` shows the newest captured runtime output, `/log page <number>` moves backward through it, `/log search <text>` searches it, and `/log clear` erases it. `/jobs` and `/job kill <number>` manage active agent executions across channels.",
	},
	{
		name:        "workflows",
		description: "Durable tasks, goals, and orchestrator scans",
		body:        "# Workflows\n\n`/task <request>` and `/goal <objective>` create durable Markdown files in the configured route directories. Their front matter is machine-readable; their bodies and agent logs remain readable and editable by people.\n\nUse `/run` to trigger an orchestrator scan immediately. The orchestrator claims eligible files, dispatches them to the configured harness, records progress, and applies the route's completion behavior.",
	},
}

var helpOverview = formatHelpOverview(helpTopics)

// SlashCommands returns the canonical command catalog used by interactive
// channel affordances. The returned slice is safe for the caller to modify.
func SlashCommands() []core.SlashCommand {
	return append([]core.SlashCommand(nil), slashCommands...)
}

func formatCommandHelp(commands []core.SlashCommand) string {
	lines := []string{"# Commands", ""}
	for _, command := range commands {
		lines = append(lines, fmt.Sprintf("- `%s` — %s", command.Usage, command.Description))
	}
	return strings.Join(lines, "\n")
}

func formatHelpOverview(topics []struct {
	name        string
	description string
	body        string
}) string {
	lines := []string{
		"# Spynel help",
		"",
		"Spynel connects coding agents to local and remote chat channels and can run durable Markdown task and goal workflows.",
		"",
		"Choose a help topic:",
		"",
	}
	for _, topic := range topics {
		lines = append(lines, fmt.Sprintf("- `/help %s` — %s", topic.name, topic.description))
	}
	return strings.Join(lines, "\n")
}

func helpFor(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return helpOverview
	}
	if name == "configuration" {
		name = "config"
	} else if name == "workflow" || name == "tasks" || name == "goals" {
		name = "workflows"
	}
	for _, topic := range helpTopics {
		if topic.name == name {
			return topic.body
		}
	}
	return fmt.Sprintf("Unknown help topic `%s`.\n\n%s", name, helpOverview)
}

func (s *Service) HistoryPath(channel, conversation string) string {
	return filepath.Clean(s.History.Path(channel, conversation))
}
