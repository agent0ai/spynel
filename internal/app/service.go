package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/extensions"
	"github.com/agent0ai/spynel/internal/fsx"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
	"github.com/agent0ai/spynel/internal/media"
	"github.com/agent0ai/spynel/internal/orchestrator"
	"github.com/agent0ai/spynel/internal/shortid"
	"github.com/agent0ai/spynel/internal/theme"
	"github.com/agent0ai/spynel/internal/updater"
)

type Service struct {
	// Config is the immutable structural configuration used to construct
	// histories, hooks, routes, and workspace paths. Live scalar values are
	// always read through Settings, avoiding races with remote form commands.
	Config          config.Config
	Harness         harness.Harness
	History         *history.Store
	Hooks           extensions.Runner
	Orchestrator    *orchestrator.Manager
	Runtime         *Runtime
	Settings        *config.Store
	PairingControl  channel.PairingManager
	DeliveryControl channel.DeliveryRouter
	Startup         interface {
		Sync(config.Config, bool) error
	}
	Updates          *updater.Manager
	configurationMu  sync.Mutex
	instanceMu       sync.RWMutex
	primaryInstance  string
	titleMu          sync.Mutex
	titleChanges     chan string
	themeChanges     chan theme.Theme
	connectionMu     sync.RWMutex
	connections      map[string]channel.ConnectionStatus
	pairingMu        sync.RWMutex
	pairing          map[string]channel.PairingEvent
	pairingEvents    chan channel.PairingEvent
	noticeMu         sync.RWMutex
	noticeSequence   uint64
	lastNotice       channel.Notice
	noticeEvents     chan channel.Notice
	restartRequests  chan struct{}
	updateRequests   chan struct{}
	primaryRequests  chan string
	primaryRequestMu sync.Mutex
	primaryRequested bool
	streamMu         sync.Mutex
	streamText       map[string]string
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
		themeChanges:    make(chan theme.Theme, 1),
		connections:     connections,
		pairing:         map[string]channel.PairingEvent{},
		pairingEvents:   make(chan channel.PairingEvent, 1),
		noticeEvents:    make(chan channel.Notice, 8),
		restartRequests: make(chan struct{}, 1),
		updateRequests:  make(chan struct{}, 1),
		primaryRequests: make(chan string, 1),
		streamText:      map[string]string{},
	}
	manager.Log = runtime.Log
	manager.JobStarted = func(sessionKey, description string) int {
		return runtime.BeginJob(sessionKey, "orchestrator", "markdown", description)
	}
	manager.JobFinished = runtime.EndJob
	manager.SetNotificationDelivery(service.deliverNotification)
	return service
}

func (s *Service) validateOrigin(origin orchestrator.Origin) error {
	if _, err := os.Stat(s.History.Path(origin.Channel, origin.Conversation)); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("origin %s/%s is not a known conversation", origin.Channel, origin.Conversation)
		}
		return err
	}
	cfg := s.Settings.Snapshot()
	switch origin.Channel {
	case "telegram":
		if strings.HasPrefix(origin.Conversation, "TG-group-") {
			if cfg.Channels.Telegram.GroupMode == "off" {
				return errors.New("Telegram group origin is disabled")
			}
			return nil
		}
		if !strings.HasPrefix(origin.Conversation, "TG-") {
			return errors.New("invalid Telegram origin")
		}
		id := strings.TrimPrefix(origin.Conversation, "TG-")
		for _, allowed := range cfg.Channels.Telegram.AllowedUsers {
			if strings.TrimSpace(strings.TrimPrefix(allowed, "@")) == id {
				return nil
			}
		}
		return errors.New("Telegram origin is not authorized by allowed_users")
	case "whatsapp":
		if strings.HasPrefix(origin.Conversation, "WA-group-") {
			if !cfg.Channels.WhatsApp.AllowGroups {
				return errors.New("WhatsApp group origin is disabled")
			}
			return nil
		}
		if !strings.HasPrefix(origin.Conversation, "WA-") {
			return errors.New("invalid WhatsApp origin")
		}
		number := config.NormalizeWhatsAppNumber(strings.TrimPrefix(origin.Conversation, "WA-"))
		for _, allowed := range cfg.Channels.WhatsApp.AllowedNumbers {
			if config.NormalizeWhatsAppNumber(allowed) == number {
				return nil
			}
		}
		return errors.New("WhatsApp origin is not authorized by allowed_numbers")
	case "tui", "cli":
		return nil
	}
	return errors.New("unsupported origin channel")
}

func (s *Service) deliverNotification(ctx context.Context, origin orchestrator.Origin, eventID, text string) error {
	if err := s.validateOrigin(origin); err != nil {
		return err
	}
	if origin.Channel == "telegram" || origin.Channel == "whatsapp" {
		state, stateErr := s.History.DeliveryState(origin.Channel, origin.Conversation, eventID)
		if stateErr != nil {
			return stateErr
		}
		if state == "sent" {
			return nil
		}
		if s.DeliveryControl == nil {
			return fmt.Errorf("%s is disconnected", origin.Channel)
		}
		if _, err := s.History.Append(origin.Channel, origin.Conversation, history.Entry{Role: "notification_sending", EventID: eventID}); err != nil {
			return err
		}
		if err := s.DeliveryControl.Deliver(ctx, origin.Channel, origin.Conversation, eventID, text); err != nil {
			_, _ = s.History.Append(origin.Channel, origin.Conversation, history.Entry{Role: "notification_failed", EventID: eventID, Content: err.Error()})
			return err
		}
	}
	role := "assistant"
	if origin.Channel == "tui" && s.Harness.IsActive(sessionKey(core.Message{Channel: origin.Channel, Conversation: origin.Conversation})) {
		role = "notification_pending"
	}
	_, err := s.History.Append(origin.Channel, origin.Conversation, history.Entry{Role: role, Sender: "Spy", Content: text, EventID: eventID})
	return err
}

func (s *Service) AckNotification(originText, eventID string, afterChars int) error {
	origin, err := orchestrator.ParseOrigin(originText)
	if err != nil {
		return err
	}
	if origin.Channel != "tui" {
		return errors.New("notification interleave acknowledgements are TUI-only")
	}
	if eventID == "" || afterChars < 0 {
		return errors.New("invalid notification acknowledgement")
	}
	_, err = s.History.Append(origin.Channel, origin.Conversation, history.Entry{Role: "notification_ack", EventID: eventID, AfterChars: afterChars})
	return err
}

func (s *Service) Notify(ctx context.Context, originText, text string) (string, error) {
	origin, err := orchestrator.ParseOrigin(originText)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(text) == "" {
		return "", errors.New("notification message is required")
	}
	if err := s.validateOrigin(origin); err != nil {
		return "", err
	}
	id := fmt.Sprintf("manual-%d", time.Now().UTC().UnixNano())
	entry, err := s.Orchestrator.Outbox.Enqueue(id, "manual", originText, strings.TrimSpace(text))
	if err != nil {
		return "", err
	}
	if processErr := s.Orchestrator.Outbox.Process(ctx); processErr != nil {
		s.Runtime.Log("notification retained for retry: " + processErr.Error())
	}
	return entry.ID, nil
}

func (s *Service) Start(ctx context.Context) error {
	return s.Harness.Start(ctx)
}

// SetPrimaryInstanceID records the workspace server owner for shared status
// output. Loopback clients separately identify the process making a request.
func (s *Service) SetPrimaryInstanceID(id string) {
	s.instanceMu.Lock()
	s.primaryInstance = id
	s.instanceMu.Unlock()
}

func (s *Service) primaryInstanceID() string {
	s.instanceMu.RLock()
	id := s.primaryInstance
	s.instanceMu.RUnlock()
	return id
}

func (s *Service) Handle(ctx context.Context, message core.Message, emit core.Emit) error {
	if message.ReceivedAt.IsZero() {
		message.ReceivedAt = time.Now().UTC()
	}
	message.Text = strings.TrimSpace(message.Text)
	if message.Text == "" {
		return nil
	}
	if message.FollowupOnly && !s.Harness.IsActive(sessionKey(message)) {
		return errors.New("there is no active execution for this conversation")
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
	return s.dispatchHarnessPrompt(ctx, message, prompt, emit)
}

func (s *Service) dispatchHarnessPrompt(ctx context.Context, message core.Message, prompt string, emit core.Emit) error {
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
		if !s.Harness.IsActive(key) {
			s.Runtime.EndJob(jobID)
		}
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

func (s *Service) creationCommandPrompt(message core.Message, kind, userMessage string) (string, error) {
	base, err := s.chatPrompt(message)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(s.Config.StatePath("prompts", "create-"+kind+".md"))
	if err != nil {
		return "", err
	}
	directive := string(data)
	directive = strings.ReplaceAll(directive, "{{CHANNEL}}", message.Channel)
	directive = strings.ReplaceAll(directive, "{{CONVERSATION}}", message.Conversation)
	directive = strings.ReplaceAll(directive, "{{TASK_SOURCE}}", s.routeSource("tasks"))
	directive = strings.ReplaceAll(directive, "{{GOAL_SOURCE}}", s.routeSource("goals"))
	// Replace user data last so template-looking text inside the request is
	// never interpreted as another framework placeholder.
	directive = strings.ReplaceAll(directive, "{{USER_MESSAGE}}", userMessage)
	return base + "\n\n---\n\n" + directive, nil
}

func (s *Service) routeSource(name string) string {
	for _, route := range s.Config.Orchestrator.Routes {
		if route.Name == name {
			return s.Config.Resolve(route.Source)
		}
	}
	return "(route not configured)"
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
	prompt = strings.ReplaceAll(prompt, "{{HISTORY_FILE}}", fullPath)
	prompt = strings.ReplaceAll(prompt, "{{CHANNEL}}", message.Channel)
	prompt = strings.ReplaceAll(prompt, "{{CONVERSATION}}", message.Conversation)
	prompt = strings.ReplaceAll(prompt, "{{TASK_SOURCE}}", s.routeSource("tasks"))
	prompt = strings.ReplaceAll(prompt, "{{GOAL_SOURCE}}", s.routeSource("goals"))
	// History is untrusted conversation data. Insert it last so placeholder-like
	// text in prior messages remains literal.
	prompt = strings.ReplaceAll(prompt, "{{RECENT_HISTORY}}", recent)
	if message.Channel == "telegram" || message.Channel == "whatsapp" {
		prompt += "\n\nTo send a readable local file back through this channel, put one directive on its own line in the final response: `[Send attachment](</absolute/path/to/file>)`. For an image displayed as a native photo, use `[Send photo](</absolute/path/to/image.png>)`. Keep any user-facing caption as ordinary text. The path may be outside the active workspace, but it must be absolute and resolve to a regular file within the attachment size limit."
	}
	return prompt, nil
}

func (s *Service) wrapEmit(message core.Message, jobID int, downstream core.Emit) core.Emit {
	return func(event core.Event) {
		key := sessionKey(message)
		if event.Kind == core.EventDelta {
			s.streamMu.Lock()
			s.streamText[key] += event.Text
			s.streamMu.Unlock()
		}
		if event.Done && (event.Kind == core.EventFinal || event.Kind == core.EventError) {
			s.streamMu.Lock()
			streamed := s.streamText[key]
			delete(s.streamText, key)
			s.streamMu.Unlock()
			text := event.Text
			remoteChannel := message.Channel == "telegram" || message.Channel == "whatsapp"
			hookText := text
			if remoteChannel && event.FinalText != nil {
				hookText = *event.FinalText
			}
			if s.Config.Extensions.Enabled {
				output, err := s.Hooks.Run(context.Background(), "harness.after", map[string]any{
					"session_key": sessionKey(message), "text": hookText, "kind": event.Kind,
				})
				if err != nil {
					event.Kind = core.EventError
					text = "harness.after extension hook failed: " + err.Error()
					event.FinalText = &text
				} else if output.Cancel {
					text = output.Message
					event.FinalText = &text
				} else {
					if value, ok := output.Payload["text"].(string); ok {
						if remoteChannel && event.FinalText != nil {
							if value != hookText {
								text = replaceFinalAssistantItem(text, hookText, value)
								event.FinalText = &value
							}
						} else {
							text = value
						}
					}
				}
			}
			if event.Kind == core.EventFinal && remoteChannel {
				cfg := s.Settings.Snapshot()
				maxBytes := int64(cfg.Workspace.AttachmentMaxMB) * 1024 * 1024
				cleaned, attachments, err := media.ParseOutbound(text, maxBytes)
				if err != nil {
					event.Kind = core.EventError
					text = "Spynel outbound attachment error: " + err.Error()
					event.FinalText = &text
					event.Attachments = nil
				} else {
					text = cleaned
					event.Attachments = attachments
					if event.FinalText != nil {
						finalText, finalAttachments, finalErr := media.ParseOutbound(*event.FinalText, maxBytes)
						if finalErr != nil {
							event.Kind = core.EventError
							text = "Spynel outbound attachment error: " + finalErr.Error()
							event.FinalText = &text
							event.Attachments = nil
						} else {
							event.FinalText = &finalText
							event.Attachments = finalAttachments
						}
					}
				}
			}
			event.Text = text
			historyText := text
			for _, attachment := range event.Attachments {
				historyText = strings.TrimSpace(historyText + "\n\n[Sent " + attachment.Kind + " " + attachment.Name + "]")
			}
			if event.Kind == core.EventError && streamed != "" {
				_, _ = s.History.Append(message.Channel, message.Conversation, history.Entry{At: time.Now().UTC(), Role: "assistant", Content: streamed})
			}
			_, _ = s.History.Append(message.Channel, message.Conversation, history.Entry{At: time.Now().UTC(), Role: map[bool]string{true: "error", false: "assistant"}[event.Kind == core.EventError], Content: historyText, Terminal: true})
		}
		if downstream != nil {
			downstream(event)
		}
		if event.Done && !event.Continues && (event.Kind == core.EventFinal || event.Kind == core.EventError) {
			s.Runtime.EndJob(jobID)
		}
	}
}

func replaceFinalAssistantItem(aggregate, previous, replacement string) string {
	if aggregate == previous {
		return replacement
	}
	if previous != "" && strings.HasSuffix(aggregate, previous) {
		return strings.TrimSuffix(aggregate, previous) + replacement
	}
	return aggregate
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
		return s.localReply(message, s.welcomeMessage(message.Channel), emit)
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
	case "primary":
		return s.primaryCommand(message, emit)
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
	case "theme":
		return s.themeCommand(message, remainder, emit)
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
	case "update":
		return s.updateCommand(ctx, message, remainder, emit)
	case "task", "todo":
		if remainder == "" {
			return s.localReply(message, "Usage: /task <title and request>", emit)
		}
		prompt, err := s.creationCommandPrompt(message, "task", remainder)
		if err != nil {
			return err
		}
		return s.dispatchHarnessPrompt(ctx, message, prompt, emit)
	case "goal":
		if remainder == "" {
			return s.localReply(message, "Usage: /goal <objective>", emit)
		}
		prompt, err := s.creationCommandPrompt(message, "goal", remainder)
		if err != nil {
			return err
		}
		return s.dispatchHarnessPrompt(ctx, message, prompt, emit)
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
	usage := "Usage: /log [page <number>|page <start>-<end>|search <text>|clear]"
	parts := strings.Fields(remainder)
	if len(parts) == 0 {
		return s.localReply(message, formatLogs(s.Runtime.Logs()), emit)
	}
	switch strings.ToLower(parts[0]) {
	case "page":
		if len(parts) != 2 {
			return s.localReply(message, usage, emit)
		}
		firstPage, lastPage, err := parseLogPageSpec(parts[1])
		if err != nil {
			return s.localReply(message, err.Error()+"\n\n"+usage, emit)
		}
		return s.localReply(message, formatLogPages(s.Runtime.Logs(), firstPage, lastPage), emit)
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

func parseLogPageSpec(spec string) (int, int, error) {
	if !strings.Contains(spec, "-") {
		page, err := strconv.Atoi(spec)
		if err != nil || page < 1 {
			return 0, 0, fmt.Errorf("log page must be a positive number or inclusive range such as 1-5")
		}
		return page, page, nil
	}
	if strings.Count(spec, "-") != 1 {
		return 0, 0, fmt.Errorf("log page must be a positive number or inclusive range such as 1-5")
	}
	bounds := strings.SplitN(spec, "-", 2)
	firstPage, firstErr := strconv.Atoi(bounds[0])
	lastPage, lastErr := strconv.Atoi(bounds[1])
	if firstErr != nil || lastErr != nil || firstPage < 1 || lastPage < 1 {
		return 0, 0, fmt.Errorf("log page range bounds must be positive numbers")
	}
	if firstPage > lastPage {
		return 0, 0, fmt.Errorf("log page range must run from a lower page to a higher page")
	}
	if lastPage-firstPage+1 > maxLogPageRange {
		return 0, 0, fmt.Errorf("log page range may cover at most %d pages", maxLogPageRange)
	}
	return firstPage, lastPage, nil
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

// UpdateRequests publishes npm update-and-restart requests after the command
// acknowledgement has been persisted. The npm launcher performs the package
// replacement only after the Go process has exited, which is safe on Windows.
func (s *Service) UpdateRequests() <-chan struct{} {
	return s.updateRequests
}

func (s *Service) requestUpdate() {
	select {
	case s.updateRequests <- struct{}{}:
	default:
	}
}

func (s *Service) updateCommand(ctx context.Context, message core.Message, remainder string, emit core.Emit) error {
	if s.Updates == nil {
		return s.localReply(message, "npm updates are unavailable in this build. Install Spynel through npm to use `/update`.", emit)
	}
	action := strings.ToLower(strings.TrimSpace(remainder))
	if action != "" && action != "install" {
		return s.localReply(message, "Usage: /update [install]", emit)
	}
	result, err := s.Updates.Check(ctx)
	if err != nil {
		return s.localReply(message, "Update check skipped: "+err.Error()+". Try `/update` again later.", emit)
	}
	if !result.InstalledViaNPM {
		return s.localReply(message, "This Spynel binary is not managed by npm. Download a newer release using the same installation method.", emit)
	}
	if !result.Available {
		latest := result.Latest
		if latest == "" {
			latest = result.Current
		}
		return s.localReply(message, fmt.Sprintf("Spynel %s is current; npm also reports %s.", result.Current, latest), emit)
	}
	if action == "" {
		if result.CanAutoInstall {
			return s.localReply(message, fmt.Sprintf("Spynel %s is installed through npm; %s is available. Run `/update install` to run npm update and restart Spynel safely.", result.Current, result.Latest), emit)
		}
		return s.localReply(message, fmt.Sprintf("Spynel %s is installed through npm; %s is available. Run `%s`, then `/restart`. This process was not launched by the npm wrapper, so it cannot replace itself safely.", result.Current, result.Latest, result.Command), emit)
	}
	if !result.CanAutoInstall {
		return s.localReply(message, fmt.Sprintf("Run `%s`, then `/restart`. Updating in place is unavailable because this process was not launched by the npm wrapper.", result.Command), emit)
	}
	if err := s.localReply(message, fmt.Sprintf("Updating Spynel from %s to %s with npm, then restarting. Saved workspace state will be migrated on startup.", result.Current, result.Latest), emit); err != nil {
		return err
	}
	s.requestUpdate()
	return nil
}

// PrimaryRequests publishes a target-specific request after /primary has
// acknowledged the requesting local TUI and verified that no jobs are active.
func (s *Service) PrimaryRequests() <-chan string {
	return s.primaryRequests
}

func (s *Service) primaryCommand(message core.Message, emit core.Emit) error {
	if message.Channel != "tui" {
		return s.localReply(message, "/primary is available only from a local TUI instance.", emit)
	}
	primaryID := s.primaryInstanceID()
	if message.InstanceID == "" || primaryID == "" {
		return s.localReply(message, "Cannot identify both this instance and the current primary instance.", emit)
	}
	if message.InstanceID == primaryID {
		return s.localReply(message, "This instance is already the primary instance.", emit)
	}
	if jobs := s.Runtime.Status().Jobs; jobs != 0 {
		activity := fmt.Sprintf("%d agent jobs are", jobs)
		if jobs == 1 {
			activity = "1 agent job is"
		}
		return s.localReply(message, "Cannot transfer primary ownership while "+activity+" running. Use `/jobs` to inspect active work and retry when the workspace is idle.", emit)
	}
	s.primaryRequestMu.Lock()
	if s.primaryRequested {
		s.primaryRequestMu.Unlock()
		return s.localReply(message, "A primary handoff is already in progress.", emit)
	}
	s.primaryRequested = true
	s.primaryRequestMu.Unlock()
	if err := s.localReply(message, "Primary handoff requested. This instance will take ownership after the current primary safely stops its owner-only services.", emit); err != nil {
		s.primaryRequestMu.Lock()
		s.primaryRequested = false
		s.primaryRequestMu.Unlock()
		return err
	}
	s.primaryRequests <- message.InstanceID
	return nil
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

// StatusSnapshot is the bounded, non-secret operational state exposed to
// plain CLI clients and rendered by /status on every channel.
type StatusSnapshot struct {
	Title             string                     `json:"title"`
	Theme             string                     `json:"theme"`
	Instance          string                     `json:"instance,omitempty"`
	PrimaryInstance   string                     `json:"primary_instance,omitempty"`
	Connections       []channel.ConnectionStatus `json:"connections"`
	Runtime           core.RuntimeStatus         `json:"runtime"`
	Harness           string                     `json:"harness,omitempty"`
	HarnessState      string                     `json:"harness_state"`
	HarnessDetail     string                     `json:"harness_detail,omitempty"`
	Model             string                     `json:"model,omitempty"`
	Sandbox           string                     `json:"sandbox"`
	StartupEnabled    bool                       `json:"startup_enabled"`
	Thread            string                     `json:"thread,omitempty"`
	TurnActive        bool                       `json:"turn_active"`
	OrchestratorLease int                        `json:"orchestrator_leases"`
	OrchestratorRuns  int                        `json:"orchestrator_dispatches"`
}

// Status returns the same status contract used by /status without requiring a
// caller to parse presentation Markdown. Opaque identifiers stay shortened.
func (s *Service) Status(message core.Message) (StatusSnapshot, error) {
	leases, dispatches, err := s.Orchestrator.Status()
	if err != nil {
		return StatusSnapshot{}, err
	}
	cfg := s.Settings.Snapshot()
	primaryInstanceID := s.primaryInstanceID()
	instanceID := message.InstanceID
	if instanceID == "" {
		// Telegram and WhatsApp execute inside the primary process rather than
		// crossing the loopback API, so their caller is the owner itself.
		instanceID = primaryInstanceID
	}
	harnessState := "connected"
	harnessDetail := ""
	if availability, ok := s.Harness.(harness.Availability); ok {
		if ready, detail := availability.Available(); !ready {
			harnessState = "unavailable"
			harnessDetail = detail
		}
	}
	thread := shortid.Display(s.Harness.ThreadID(sessionKey(message)))
	return StatusSnapshot{
		Title: s.currentTitle(), Theme: cfg.Channels.TUI.Theme,
		Instance: shortid.Display(instanceID), PrimaryInstance: shortid.Display(primaryInstanceID),
		Connections: []channel.ConnectionStatus{s.connectionStatus("telegram"), s.connectionStatus("whatsapp")},
		Runtime:     s.Runtime.Status(), Harness: cfg.Harness.Name, HarnessState: harnessState,
		HarnessDetail: harnessDetail, Model: cfg.Harness.Model, Sandbox: cfg.Harness.Sandbox,
		StartupEnabled: cfg.Startup.Enabled, Thread: thread, TurnActive: s.Harness.IsActive(sessionKey(message)),
		OrchestratorLease: leases, OrchestratorRuns: dispatches,
	}, nil
}

func (s *Service) statusText(message core.Message) (string, error) {
	status, err := s.Status(message)
	if err != nil {
		return "", err
	}
	return FormatStatus(status), nil
}

// FormatStatus renders a StatusSnapshot for text terminals and chat channels.
func FormatStatus(status StatusSnapshot) string {
	harnessStatus := status.HarnessState
	if status.HarnessDetail != "" {
		harnessStatus += " — " + status.HarnessDetail
	}
	thread := "not started"
	if status.Thread != "" {
		thread = "`" + status.Thread + "`"
	}
	turn := "idle"
	if status.TurnActive {
		turn = "active"
	}
	telegram := channel.ConnectionStatus{Name: "telegram", State: channel.ConnectionUnconfigured}
	whatsapp := channel.ConnectionStatus{Name: "whatsapp", State: channel.ConnectionUnconfigured}
	for _, connection := range status.Connections {
		switch connection.Name {
		case "telegram":
			telegram = connection
		case "whatsapp":
			whatsapp = connection
		}
	}
	return strings.Join([]string{
		"# Status",
		"",
		"- Title: " + status.Title,
		"- Theme: " + status.Theme,
		"- Instance ID: " + statusID(status.Instance),
		"- Primary instance ID: " + statusID(status.PrimaryInstance),
		"- Telegram: " + connectionIndicator(telegram),
		"- WhatsApp: " + connectionIndicator(whatsapp),
		fmt.Sprintf("- Jobs: %d — `/jobs`", status.Runtime.Jobs),
		fmt.Sprintf("- Logs: %d — `/log`", status.Runtime.Logs),
		"- Coding harness: " + emptyAs(status.Harness, "not selected") + " (" + harnessStatus + ")",
		"- Model: " + emptyAs(status.Model, "harness default"),
		"- Agent filesystem access: " + status.Sandbox,
		"- Run at startup: " + enabledText(status.StartupEnabled),
		"- Thread: " + thread,
		"- Turn: " + turn,
		fmt.Sprintf("- Orchestrator: %d leases, %d dispatch goroutines", status.OrchestratorLease, status.OrchestratorRuns),
	}, "\n")
}

func statusID(value string) string {
	display := shortid.Display(value)
	if display == "" {
		return "not available"
	}
	return "`" + display + "`"
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
	{Value: "/primary", Usage: "/primary", Description: "Safely make this TUI instance the workspace primary"},
	{Value: "/welcome", Usage: "/welcome", Description: "Print the Spynel welcome guide in this conversation"},
	{Value: "/config", Usage: "/config", Description: "Open or show Spynel configuration"},
	{Value: "/config get ", Usage: "/config get <key>", Description: "Read one configuration value"},
	{Value: "/config set ", Usage: "/config set <key> <value>", Description: "Persist one configuration value"},
	{Value: "/harness ", Usage: "/harness [name]", Description: "Show or select the coding harness"},
	{Value: "/model ", Usage: "/model [name]", Description: "Show or select the harness model"},
	{Value: "/theme", Usage: "/theme [name]", Description: "Preview or select a color theme"},
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
	{Value: "/update", Usage: "/update", Description: "Check npm for a newer Spynel release"},
	{Value: "/update install", Usage: "/update install", Description: "Install an npm update and restart safely"},
	{Value: "/history", Usage: "/history", Description: "Show the complete history file"},
	{Value: "/resume", Usage: "/resume", Description: "Browse saved conversations and branch one into the TUI"},
	{Value: "/log", Usage: "/log", Description: "Show the newest page of captured runtime logs"},
	{Value: "/log page ", Usage: "/log page <number|start-end>", Description: "Show one or up to five runtime log pages"},
	{Value: "/log search ", Usage: "/log search <text>", Description: "Search captured runtime logs"},
	{Value: "/log clear", Usage: "/log clear", Description: "Clear captured runtime logs"},
	{Value: "/jobs", Usage: "/jobs", Description: "List running agent jobs"},
	{Value: "/job kill ", Usage: "/job kill <number>", Description: "Stop a running agent job by number"},
	{Value: "/clear", Usage: "/clear", Description: "Clear this conversation's history and harness thread"},
	{Value: "/task ", Usage: "/task <request>", Description: "Ask the communication agent to create or refine a finite task"},
	{Value: "/goal ", Usage: "/goal <objective>", Description: "Ask the communication agent to create or refine a measurable goal"},
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
		body:        "# Extensions\n\nExtensions are explicitly installed Git repositories that can run hooks around messages, harness calls, orchestration, and application-version transitions. Update hooks receive exact `from_version` and `to_version` values and must be retry-safe. Their directory and whether hooks are enabled are configured under `extensions` in `spynel.yaml`.\n\n- `/extension list` lists installed extensions.\n- `/extension install <git-url> [name]` installs a repository you trust.\n- `/extension remove <name>` removes an installed extension.",
	},
	{
		name:        "config",
		description: "spynel.yaml settings and path resolution",
		body:        "# Configuration\n\n`spynel.yaml` controls the workspace, harness, channels, speech processing, orchestration routes, and extensions. `/config` shows the shared settings, `/config get <key>` reads one value, and `/config set <key> <value>` atomically validates and persists a change from any channel. `/harness [name]` and `/model [name]` are concise selectors. `/theme [name]` previews/lists or selects a semantic palette from `.spynel/themes`. `harness.sandbox` accepts `danger-full-access`, `workspace-write`, or `read-only`; unrestricted access is the default. Relative paths are resolved from the directory containing `spynel.yaml`, so a project can be moved without rewriting local paths.",
	},
	{
		name:        "channels",
		description: "The TUI, Telegram, and WhatsApp",
		body:        "# Channels\n\nThe TUI, each Telegram chat, and each WhatsApp chat keep independent durable histories and harness threads. All channels share the application slash commands and Markdown-aware responses.\n\nUse `/status` to inspect shared connection, runtime, harness, instance, and orchestrator indicators. From an idle local TUI, `/primary` safely hands workspace ownership to that TUI instance. Use `/history` to locate the current conversation's history file, `/clear` to erase that history and discard its harness thread, `/stop` to interrupt its active execution, and `/new` to start a fresh harness thread without erasing channel history. `/restart` acknowledges the request, cleanly stops the current runtime, and relaunches Spynel with saved configuration and histories intact. `/update` checks npm with a ten-second deadline, and `/update install` lets a supervising npm launcher update after shutdown and then restart. `/log` shows the newest captured runtime output, `/log page <number>` moves backward through it, `/log page <start>-<end>` shows up to five consecutive pages, `/log search <text>` searches it, and `/log clear` erases it. `/jobs` and `/job kill <number>` manage active agent executions across channels.",
	},
	{
		name:        "workflows",
		description: "Durable tasks, goals, and orchestrator scans",
		body:        "# Workflows\n\nA task is one finite, independently verifiable objective. `/task <request>` sends a dedicated creation directive and your request to the communication agent, which creates or refines a complete task in `todo`. Spynel claims it through `working`, then separately claims completed implementation through `review` into `reviewing`; only that fresh review may accept `done`.\n\nA goal is a long-term or multi-round outcome with measurable success criteria. `/goal <objective>` asks the communication agent to create or refine it in `proposed`. A leased planner creates linked finite task rounds, the goal remains unleased in `active` while they run, and a fresh goal review decides against the bar whether to finish, wait, abandon, or plan another round. Finished tasks never complete a goal automatically.\n\nUse `/run` to trigger an orchestrator scan immediately. Claimed `working`, `planning`, and `reviewing` documents have persisted leases and are recovered after crashes.",
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
