package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/frdel/spynel/internal/channel"
	"github.com/frdel/spynel/internal/config"
	"github.com/frdel/spynel/internal/core"
	"github.com/frdel/spynel/internal/harness"
	"github.com/frdel/spynel/internal/history"
	"github.com/frdel/spynel/internal/workspace"
)

type serviceHarness struct {
	mu       sync.Mutex
	prompts  map[string][]string
	threads  map[string]string
	active   map[string]bool
	resetErr error
}

type heldServiceHarness struct {
	*serviceHarness
	emits map[string]core.Emit
}

type reconfigurableServiceHarness struct {
	*serviceHarness
	config harness.HarnessConfig
	calls  int
}

func (r *reconfigurableServiceHarness) HarnessConfig() harness.HarnessConfig {
	return r.config
}

func (r *reconfigurableServiceHarness) Reconfigure(next harness.HarnessConfig) error {
	r.config = next
	r.calls++
	return nil
}

type fakeStartupManager struct {
	calls []bool
	err   error
}

func (m *fakeStartupManager) Sync(_ config.Config, enabled bool) error {
	m.calls = append(m.calls, enabled)
	return m.err
}

func newHeldServiceHarness() *heldServiceHarness {
	return &heldServiceHarness{serviceHarness: newServiceHarness(), emits: map[string]core.Emit{}}
}

func (r *heldServiceHarness) Send(_ context.Context, key, prompt string, emit core.Emit) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prompts[key] = append(r.prompts[key], prompt)
	thread := r.threads[key]
	if thread == "" {
		thread = "thread-" + key
		r.threads[key] = thread
	}
	r.active[key] = true
	r.emits[key] = emit
	return thread, false, nil
}

func (r *heldServiceHarness) finish(key string) {
	r.mu.Lock()
	emit := r.emits[key]
	delete(r.emits, key)
	r.active[key] = false
	r.mu.Unlock()
	if emit != nil {
		emit(core.Event{Kind: core.EventFinal, Text: "finished", Done: true})
	}
}

func newServiceHarness() *serviceHarness {
	return &serviceHarness{prompts: map[string][]string{}, threads: map[string]string{}, active: map[string]bool{}}
}

func (r *serviceHarness) Start(context.Context) error { return nil }
func (r *serviceHarness) Close() error                { return nil }
func (r *serviceHarness) Models(context.Context) ([]harness.Model, error) {
	return []harness.Model{{ID: "sonnet", DisplayName: "Sonnet"}, {ID: "opus", DisplayName: "Opus"}}, nil
}
func (r *serviceHarness) ResetSession(key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.resetErr != nil {
		return r.resetErr
	}
	delete(r.threads, key)
	return nil
}
func (r *serviceHarness) ThreadID(key string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.threads[key]
}
func (r *serviceHarness) IsActive(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active[key]
}

func (r *serviceHarness) Interrupt(_ context.Context, key string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active[key] {
		return false, nil
	}
	r.active[key] = false
	return true, nil
}
func (r *serviceHarness) Send(_ context.Context, key, prompt string, emit core.Emit) (string, bool, error) {
	r.mu.Lock()
	r.prompts[key] = append(r.prompts[key], prompt)
	thread := r.threads[key]
	if thread == "" {
		thread = "thread-" + key
		r.threads[key] = thread
	}
	r.mu.Unlock()
	emit(core.Event{Kind: core.EventFinal, Text: "reply for " + key, ThreadID: thread, Done: true})
	return thread, false, nil
}

func TestServiceKeepsChannelContextsSeparateAndLinksFullHistory(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(root, "spynel.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	harness := newServiceHarness()
	service := New(cfg, harness)
	for _, message := range []core.Message{
		{Channel: "tui", Conversation: "local", Text: "TUI question"},
		{Channel: "telegram", Conversation: "42", Text: "Telegram question"},
	} {
		if err := service.Handle(context.Background(), message, func(core.Event) {}); err != nil {
			t.Fatal(err)
		}
	}
	tuiPrompt := harness.prompts["chat:tui:local"][0]
	telegramPrompt := harness.prompts["chat:telegram:42"][0]
	if strings.Contains(tuiPrompt, "Telegram question") || strings.Contains(telegramPrompt, "TUI question") {
		t.Fatal("channel histories leaked into each other")
	}
	if !strings.Contains(tuiPrompt, service.History.Path("tui", "local")) {
		t.Fatal("prompt does not link the complete history file")
	}
	recent, _, err := service.History.Recent("tui", "local", 10000)
	if err != nil || !strings.Contains(recent, "reply for chat:tui:local") {
		t.Fatalf("assistant response not persisted: %q, %v", recent, err)
	}
}

func TestSlashCommandsCreateDurableTasksWithoutCallingRecipient(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	harness := newServiceHarness()
	service := New(cfg, harness)
	var response core.Event
	err := service.Handle(context.Background(), core.Message{Channel: "telegram", Conversation: "7", Text: "/task inspect the queue"}, func(event core.Event) { response = event })
	if err != nil {
		t.Fatal(err)
	}
	if response.Kind != core.EventFinal || !strings.Contains(response.Text, "Created task") {
		t.Fatalf("unexpected slash response %#v", response)
	}
	if len(harness.prompts) != 0 {
		t.Fatal("local slash command should not call harness")
	}
	entries, err := filepath.Glob(filepath.Join(cfg.Resolve(cfg.Orchestrator.Routes[0].Source), "*.md"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("task files = %#v, %v", entries, err)
	}
}

func TestSlashCommandCatalogBuildsHelpAndReturnsACopy(t *testing.T) {
	commands := SlashCommands()
	if len(commands) == 0 {
		t.Fatal("slash command catalog is empty")
	}
	for _, command := range commands {
		if !strings.Contains(commandHelp, command.Usage) || !strings.Contains(commandHelp, command.Description) {
			t.Fatalf("command help does not contain %#v", command)
		}
	}

	original := commands[0].Value
	commands[0].Value = "/mutated"
	if SlashCommands()[0].Value != original {
		t.Fatal("SlashCommands returned mutable catalog storage")
	}
}

func TestHelpRoutesToBriefIndexAndFocusedTopics(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	target := newServiceHarness()
	service := New(cfg, target)

	help := func(command string) string {
		t.Helper()
		var response core.Event
		if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: command}, func(event core.Event) { response = event }); err != nil {
			t.Fatal(err)
		}
		if response.Kind != core.EventFinal {
			t.Fatalf("%s response = %#v", command, response)
		}
		return response.Text
	}

	overview := help("/help")
	for _, topic := range []string{"about", "commands", "extensions", "config", "channels", "workflows"} {
		if !strings.Contains(overview, "/help "+topic) {
			t.Fatalf("help overview does not link topic %q: %s", topic, overview)
		}
	}
	if strings.Contains(overview, "/extension install") {
		t.Fatalf("bare help should stay brief, got %s", overview)
	}

	tests := map[string]string{
		"/help about":      "# About Spynel",
		"/help commands":   "/stop",
		"/help extensions": "/extension install",
		"/help config":     "spynel.yaml",
		"/help channels":   "Telegram",
		"/help workflows":  "/task <request>",
	}
	for command, want := range tests {
		if response := help(command); !strings.Contains(response, want) {
			t.Errorf("%s response does not contain %q: %s", command, want, response)
		}
	}

	unknown := help("/help nowhere")
	if !strings.Contains(unknown, "Unknown help topic `nowhere`") || !strings.Contains(unknown, "/help commands") {
		t.Fatalf("unknown help response = %s", unknown)
	}
	if len(target.prompts) != 0 {
		t.Fatal("help commands should not call harness")
	}
}

func TestStatusShowsSharedIndicatorsAndShortThreadID(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	target := newServiceHarness()
	service := New(cfg, target)
	key := "chat:telegram:42"
	target.threads[key] = "019c9f42-a3b1-7ced-9e10-123456789abc"
	target.active[key] = true
	service.Runtime.Log("connected")
	service.Runtime.BeginJob(key, "telegram", "42", "inspect production")
	service.SetConnectionStatus(channel.ConnectionStatus{Name: "telegram", State: channel.ConnectionConnected})
	service.SetConnectionStatus(channel.ConnectionStatus{Name: "whatsapp", State: channel.ConnectionError, Detail: "offline"})
	if err := os.WriteFile(cfg.StatePath("tui-title"), []byte("Production\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var response core.Event
	if err := service.Handle(context.Background(), core.Message{Channel: "telegram", Conversation: "42", Text: "/status"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Status", "Title: Production", "Telegram: ● connected", "WhatsApp: ▲ error — offline", "Jobs: 1", "Logs: 1", "Agent filesystem access: danger-full-access", "Thread: `019c9f42`", "Turn: active", "Orchestrator:"} {
		if !strings.Contains(response.Text, want) {
			t.Fatalf("status response does not contain %q:\n%s", want, response.Text)
		}
	}
	if strings.Contains(response.Text, "a3b1-7ced") {
		t.Fatalf("status exposed full thread ID: %s", response.Text)
	}
}

func TestTelegramCommandMentionRoutesToSharedCommand(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	target := newServiceHarness()
	service := New(cfg, target)
	var response core.Event

	if err := service.Handle(context.Background(), core.Message{Channel: "telegram", Conversation: "group", Text: "/help@spynel_bot commands"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if response.Kind != core.EventFinal || !strings.Contains(response.Text, "# Commands") {
		t.Fatalf("Telegram command mention response = %#v", response)
	}
	if len(target.prompts) != 0 {
		t.Fatal("Telegram command mention should not call harness")
	}
}

func TestTitleCommandPersistsFromRemoteChannelsAndNotifiesTUI(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	target := newServiceHarness()
	service := New(cfg, target)

	for _, test := range []struct {
		channel string
		title   string
	}{
		{channel: "telegram", title: "Production API"},
		{channel: "whatsapp", title: "Release Monitor"},
	} {
		var response core.Event
		err := service.Handle(context.Background(), core.Message{Channel: test.channel, Conversation: "remote", Text: "/title " + test.title}, func(event core.Event) { response = event })
		if err != nil {
			t.Fatal(err)
		}
		if response.Kind != core.EventFinal || !strings.Contains(response.Text, test.title) {
			t.Fatalf("%s title response = %#v", test.channel, response)
		}
		select {
		case title := <-service.TitleChanges():
			if title != test.title {
				t.Fatalf("%s title event = %q, want %q", test.channel, title, test.title)
			}
		default:
			t.Fatalf("%s title command did not notify the TUI", test.channel)
		}
		data, err := os.ReadFile(cfg.StatePath("tui-title"))
		if err != nil || strings.TrimSpace(string(data)) != test.title {
			t.Fatalf("%s persisted title = %q, %v", test.channel, data, err)
		}
	}
	if len(target.prompts) != 0 {
		t.Fatal("title commands should not call harness")
	}
}

func TestTitleNotificationsKeepOnlyLatestChange(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	service := New(cfg, newServiceHarness())

	service.publishTitle("Older")
	service.publishTitle("Newest")
	if title := <-service.TitleChanges(); title != "Newest" {
		t.Fatalf("queued title = %q, want Newest", title)
	}
}

func TestQuitIsRecognizedButDoesNotStopRemoteServer(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	service := New(cfg, newServiceHarness())

	for _, channel := range []string{"telegram", "whatsapp"} {
		var response core.Event
		if err := service.Handle(context.Background(), core.Message{Channel: channel, Conversation: "remote", Text: "/quit"}, func(event core.Event) { response = event }); err != nil {
			t.Fatal(err)
		}
		if response.Kind != core.EventFinal || !strings.Contains(response.Text, "interactive TUI only") {
			t.Fatalf("%s quit response = %#v", channel, response)
		}
	}
}

func TestStopCommandInterruptsOnlyAnActiveConversation(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	target := newServiceHarness()
	service := New(cfg, target)
	key := "chat:tui:local"
	target.active[key] = true

	var response core.Event
	err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: "/stop"}, func(event core.Event) { response = event })
	if err != nil {
		t.Fatal(err)
	}
	if target.IsActive(key) || response.Kind != core.EventFinal || !strings.Contains(response.Text, "Stop requested") {
		t.Fatalf("active=%t response=%#v", target.IsActive(key), response)
	}

	response = core.Event{}
	err = service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: "/stop"}, func(event core.Event) { response = event })
	if err != nil || !strings.Contains(response.Text, "no active execution") {
		t.Fatalf("inactive stop response=%#v err=%v", response, err)
	}
}

func TestRestartCommandAcknowledgesAndRequestsProcessRestartAcrossChannels(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	target := newServiceHarness()
	service := New(cfg, target)

	for _, channelName := range []string{"tui", "telegram", "whatsapp"} {
		var response core.Event
		message := core.Message{Channel: channelName, Conversation: "restart-" + channelName, Text: "/restart"}
		if err := service.Handle(context.Background(), message, func(event core.Event) { response = event }); err != nil {
			t.Fatalf("%s restart: %v", channelName, err)
		}
		if response.Kind != core.EventFinal || !response.Done || !response.Local || !strings.Contains(response.Text, "Restarting Spynel") {
			t.Fatalf("%s restart response = %#v", channelName, response)
		}
		select {
		case <-service.RestartRequests():
		default:
			t.Fatalf("%s restart did not publish a process request", channelName)
		}
		recent, _, err := service.History.Recent(channelName, message.Conversation, 1000)
		if err != nil || !strings.Contains(recent, "Saved configuration and conversation history") {
			t.Fatalf("%s restart acknowledgment was not persisted: %q, %v", channelName, recent, err)
		}
	}
	if len(target.prompts) != 0 {
		t.Fatal("restart command should not call the coding harness")
	}
	found := false
	for _, command := range SlashCommands() {
		if command.Value == "/restart" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("restart command is missing from the canonical picker")
	}
}

func TestJobsListAndKillActiveExecutionsByNumericID(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	target := newHeldServiceHarness()
	service := New(cfg, target)

	for _, message := range []core.Message{
		{Channel: "telegram", Conversation: "42", Text: "investigate production"},
		{Channel: "whatsapp", Conversation: "1555", Text: "review the release"},
	} {
		if err := service.Handle(context.Background(), message, func(core.Event) {}); err != nil {
			t.Fatal(err)
		}
	}
	if status := service.Runtime.Status(); status.Jobs != 2 {
		t.Fatalf("runtime status = %#v, want 2 jobs", status)
	}

	var response core.Event
	if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: "/jobs"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Job 1", "telegram/42", "Job 2", "whatsapp/1555", "/job kill <number>"} {
		if !strings.Contains(response.Text, want) {
			t.Fatalf("jobs response does not contain %q: %s", want, response.Text)
		}
	}

	response = core.Event{}
	if err := service.Handle(context.Background(), core.Message{Channel: "telegram", Conversation: "42", Text: "/job kill 1"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Text, "Kill requested for job 1") || service.Runtime.Status().Jobs != 1 {
		t.Fatalf("kill response=%#v status=%#v", response, service.Runtime.Status())
	}
	if target.IsActive("chat:telegram:42") {
		t.Fatal("killed harness session is still active")
	}

	target.finish("chat:whatsapp:1555")
	if service.Runtime.Status().Jobs != 0 {
		t.Fatalf("completed job remains registered: %#v", service.Runtime.Jobs())
	}
}

func TestLogCommandShowsCapturedRuntimeOutput(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	service := New(cfg, newServiceHarness())
	service.Runtime.Log("Spynel started")
	service.Runtime.Log("orchestrator scan complete")
	var response core.Event

	if err := service.Handle(context.Background(), core.Message{Channel: "whatsapp", Conversation: "1555", Text: "/log"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if response.Kind != core.EventFinal || !response.Local || !strings.Contains(response.Text, "2 entries") || !strings.Contains(response.Text, "Spynel started") {
		t.Fatalf("log response = %#v", response)
	}
}

func TestLogCommandSupportsPaginationSearchAndClear(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	service := New(cfg, newServiceHarness())
	for index := 1; index <= 45; index++ {
		service.Runtime.Log(fmt.Sprintf("entry-%02d", index))
	}
	service.Runtime.Log("Needle in the newest haystack")

	run := func(channel, command string) core.Event {
		t.Helper()
		var response core.Event
		if err := service.Handle(context.Background(), core.Message{Channel: channel, Conversation: "test", Text: command}, func(event core.Event) { response = event }); err != nil {
			t.Fatal(err)
		}
		return response
	}

	pageOne := run("tui", "/log page 1").Text
	if !strings.Contains(pageOne, "Page 1 of 3") || !strings.Contains(pageOne, "entry-27") || !strings.Contains(pageOne, "Needle") || strings.Contains(pageOne, "entry-26") {
		t.Fatalf("unexpected newest log page: %s", pageOne)
	}
	pageTwo := run("telegram", "/log page 2").Text
	if !strings.Contains(pageTwo, "Page 2 of 3") || !strings.Contains(pageTwo, "entry-07") || !strings.Contains(pageTwo, "entry-26") || strings.Contains(pageTwo, "entry-06") || strings.Contains(pageTwo, "entry-27") {
		t.Fatalf("unexpected second log page: %s", pageTwo)
	}
	pageThree := run("whatsapp", "/logs page 3").Text
	if !strings.Contains(pageThree, "Page 3 of 3") || !strings.Contains(pageThree, "entry-01") || !strings.Contains(pageThree, "entry-06") || strings.Contains(pageThree, "entry-07") {
		t.Fatalf("unexpected oldest log page: %s", pageThree)
	}
	if output := run("tui", "/log page 100").Text; !strings.Contains(output, "Page 100 does not exist") || !strings.Contains(output, "3 pages") {
		t.Fatalf("unexpected out-of-range response: %s", output)
	}
	if output := run("telegram", "/log search needle").Text; !strings.Contains(output, "1 matches") || !strings.Contains(output, "Needle in the newest haystack") || strings.Contains(output, "entry-45") {
		t.Fatalf("unexpected search response: %s", output)
	}
	if output := run("whatsapp", "/log clear").Text; output != "Cleared 46 runtime log entries." {
		t.Fatalf("unexpected clear response: %s", output)
	}
	if status := service.Runtime.Status(); status.Logs != 0 {
		t.Fatalf("runtime status after clear = %#v", status)
	}
	if output := run("tui", "/log").Text; !strings.Contains(output, "No runtime log entries") {
		t.Fatalf("unexpected empty log response: %s", output)
	}
}

func TestLogCommandRejectsInvalidOptions(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	service := New(cfg, newServiceHarness())
	for _, command := range []string{"/log page 0", "/log page nope", "/log page", "/log search", "/log clear extra", "/log unknown"} {
		var response core.Event
		if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: command}, func(event core.Event) { response = event }); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(response.Text, "Usage: /log") {
			t.Fatalf("%s response = %q", command, response.Text)
		}
	}
}

func TestConfigurationCommandsPersistAcrossChannelsAndProtectOwnChannel(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "spynel.yaml")
	cfg, _ := config.Load(path)
	service := New(cfg, newServiceHarness())
	run := func(channel, command string) core.Event {
		t.Helper()
		var response core.Event
		if err := service.Handle(context.Background(), core.Message{Channel: channel, Conversation: "settings", Text: command}, func(event core.Event) { response = event }); err != nil {
			t.Fatal(err)
		}
		return response
	}

	if response := run("telegram", "/telegram off"); !strings.Contains(response.Text, "cannot be configured from Telegram itself") {
		t.Fatalf("Telegram configured itself: %#v", response)
	}
	if response := run("telegram", "/config set channels.telegram.enabled off"); !strings.Contains(response.Text, "cannot be configured from Telegram itself") {
		t.Fatalf("Telegram bypassed own-channel protection through /config: %#v", response)
	}
	if response := run("whatsapp", "/telegram set token 123:super-secret"); !strings.Contains(response.Text, "`set`") || strings.Contains(response.Text, "super-secret") {
		t.Fatalf("secret setting response was exposed: %#v", response)
	}
	if response := run("whatsapp", "/telegram set allowed_users 42"); !strings.Contains(response.Text, "channels.telegram.allowed_users") {
		t.Fatalf("Telegram whitelist response = %#v", response)
	}
	if response := run("whatsapp", "/telegram on"); !strings.Contains(response.Text, "channels.telegram.enabled") {
		t.Fatalf("Telegram toggle response = %#v", response)
	}
	if response := run("whatsapp", "/whatsapp on"); !strings.Contains(response.Text, "cannot be configured from WhatsApp itself") {
		t.Fatalf("WhatsApp configured itself: %#v", response)
	}
	if response := run("whatsapp", "/config set channels.whatsapp.enabled off"); !strings.Contains(response.Text, "cannot be configured from WhatsApp itself") {
		t.Fatalf("WhatsApp bypassed own-channel protection through /config: %#v", response)
	}
	if response := run("telegram", "/whatsapp on"); !strings.Contains(response.Text, "channels.whatsapp.enabled") {
		t.Fatalf("WhatsApp cross-channel toggle response = %#v", response)
	}
	if response := run("tui", "/config set workspace.history_max_messages 7"); !strings.Contains(response.Text, "history_max_messages") {
		t.Fatalf("config setting response = %#v", response)
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Channels.Telegram.Enabled || reloaded.Channels.Telegram.Token != "123:super-secret" || len(reloaded.Channels.Telegram.AllowedUsers) != 1 || reloaded.Channels.Telegram.AllowedUsers[0] != "42" || !reloaded.Channels.WhatsApp.Enabled || reloaded.Workspace.HistoryMaxMessages != 7 {
		t.Fatalf("configuration commands were not persisted: %#v", reloaded)
	}
	historyEntries, _, err := service.History.Entries("whatsapp", "settings")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range historyEntries {
		if strings.Contains(entry.Content, "super-secret") {
			t.Fatalf("secret leaked into conversation history: %#v", historyEntries)
		}
	}
}

func TestHarnessAndModelCommandsUseSharedSettings(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	service := New(cfg, newServiceHarness())
	for _, test := range []struct {
		command string
		want    string
	}{
		{"/harness claude-code", "harness.name"},
		{"/model opus", "harness.model"},
	} {
		var response core.Event
		if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: test.command}, func(event core.Event) { response = event }); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(response.Text, test.want) {
			t.Fatalf("%s response = %q, want %q", test.command, response.Text, test.want)
		}
	}
	var response core.Event
	if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: "/harness"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if response.Kind != core.EventScreen || response.Screen == nil || response.Screen.ID != "harness" || len(response.Screen.Controls) != 2 || !response.Screen.SaveDisabled || response.Screen.InitialControl != "select:claude-code" || response.Screen.Controls[0].Kind != "action" {
		t.Fatalf("/harness response = %#v", response)
	}
	if next, err := service.ScreenAction(context.Background(), "harness", "select:codex", nil); err != nil || next == nil || next.ID != "welcome" || service.Settings.Snapshot().Harness.Name != "codex" {
		t.Fatalf("harness screen selection = %#v, %v, config %#v", next, err, service.Settings.Snapshot().Harness)
	}
	response = core.Event{}
	if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: "/model"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if response.Kind != core.EventScreen || response.Screen == nil || response.Screen.ID != "model" || len(response.Screen.Controls) != 3 || !response.Screen.SaveDisabled || response.Screen.InitialControl != "select:opus" || response.Screen.Controls[0].Key != "select:" || response.Screen.Controls[1].Kind != "action" {
		t.Fatalf("/model response = %#v", response)
	}
	if next, err := service.ScreenAction(context.Background(), "model", "select:sonnet", nil); err != nil || next != nil || service.Settings.Snapshot().Harness.Model != "sonnet" {
		t.Fatalf("model screen selection = %#v, %v, config %#v", next, err, service.Settings.Snapshot().Harness)
	}
	response = core.Event{}
	if err := service.Handle(context.Background(), core.Message{Channel: "telegram", Conversation: "TG-7", Text: "/harness"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Text, "claude-code") || !strings.Contains(response.Text, "codex") || !strings.Contains(response.Text, "/harness <name>") {
		t.Fatalf("remote /harness response = %#v", response)
	}
}

func TestSandboxSettingReconfiguresHarnessWithoutWorkspaceConfinement(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(root, "spynel.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Harness.Name = "codex"
	target := &reconfigurableServiceHarness{
		serviceHarness: newServiceHarness(),
		config:         harness.HarnessConfig{Name: "codex", Sandbox: "danger-full-access"},
	}
	service := New(cfg, target)
	if _, err := service.ApplySettings(map[string]string{
		"harness.sandbox":              "danger-full-access",
		"workspace.history_char_limit": "13000",
	}); err != nil {
		t.Fatal(err)
	}
	if target.calls != 0 {
		t.Fatalf("unchanged sandbox reconfigured the harness %d time(s)", target.calls)
	}
	var response core.Event
	err = service.Handle(context.Background(), core.Message{Channel: "telegram", Conversation: "TG-7", Text: "/config set harness.sandbox workspace-write"}, func(event core.Event) { response = event })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Text, "Saved `harness.sandbox` = `workspace-write`") || service.Settings.Snapshot().Harness.Sandbox != "workspace-write" || target.config.Sandbox != "workspace-write" || target.calls != 1 {
		t.Fatalf("sandbox update = response %#v, persisted %q, runtime %q", response, service.Settings.Snapshot().Harness.Sandbox, target.config.Sandbox)
	}
	if target.config.Cwd != root || target.config.ApprovalPolicy != "never" {
		t.Fatalf("runtime harness config = %#v", target.config)
	}
}

func TestStartupSettingRegistersAndRollsBackTransactionally(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	service := New(cfg, newServiceHarness())
	manager := &fakeStartupManager{}
	service.Startup = manager
	if _, err := service.ApplySettings(map[string]string{"startup.enabled": "on"}); err != nil {
		t.Fatal(err)
	}
	if len(manager.calls) != 1 || !manager.calls[0] || !service.Settings.Snapshot().Startup.Enabled {
		t.Fatalf("startup enable = calls %#v config %#v", manager.calls, service.Settings.Snapshot().Startup)
	}
	manager.err = fmt.Errorf("registration denied")
	if _, err := service.ApplySettings(map[string]string{"startup.enabled": "off"}); err == nil {
		t.Fatal("failed startup removal unexpectedly persisted")
	}
	if !service.Settings.Snapshot().Startup.Enabled {
		t.Fatal("startup setting was not rolled back after registration failure")
	}
	reloaded, err := config.Load(filepath.Join(root, "spynel.yaml"))
	if err != nil || !reloaded.Startup.Enabled {
		t.Fatalf("persisted startup rollback = %#v, %v", reloaded.Startup, err)
	}
}

func TestUnchangedStartupSettingDoesNotTouchOperatingSystem(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	service := New(cfg, newServiceHarness())
	manager := &fakeStartupManager{err: fmt.Errorf("must not be called")}
	service.Startup = manager
	changed, err := service.ApplySettings(map[string]string{"startup.enabled": "off"})
	if err != nil || len(changed) != 1 || len(manager.calls) != 0 {
		t.Fatalf("unchanged startup = changed %#v, calls %#v, error %v", changed, manager.calls, err)
	}
}

func TestBareTUIConfigurationCommandOpensTypedScreen(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	service := New(cfg, newServiceHarness())
	var response core.Event
	if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: "/telegram"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if response.Kind != core.EventScreen || response.Screen == nil || response.Screen.ID != "telegram" || !response.Screen.StartAtTop || len(response.Screen.Controls) == 0 {
		t.Fatalf("configuration screen response = %#v", response)
	}
	wantFirst := []string{"wizard", "channels.telegram.token", "channels.telegram.allowed_users", "channels.telegram.enabled", "advanced"}
	if len(response.Screen.Controls) < len(wantFirst) {
		t.Fatalf("Telegram controls = %#v", response.Screen.Controls)
	}
	for index, key := range wantFirst {
		if response.Screen.Controls[index].Key != key {
			t.Fatalf("Telegram control %d = %q, want %q", index, response.Screen.Controls[index].Key, key)
		}
	}
	if response.Screen.Controls[4].Kind != "disclosure" || !response.Screen.Controls[5].Advanced {
		t.Fatalf("Telegram advanced disclosure = %#v", response.Screen.Controls)
	}
	foundSecret := false
	foundWhitelistHelp := false
	for _, control := range response.Screen.Controls {
		if control.Key == "channels.telegram.token" {
			foundSecret = control.Secret && control.Value == ""
		}
		if control.Key == telegramAllowedKey {
			foundWhitelistHelp = control.DescriptionMarkdown && strings.Contains(control.Description, "[@userinfobot](https://t.me/userinfobot)")
		}
	}
	if !foundSecret {
		t.Fatalf("Telegram token control is missing or exposed: %#v", response.Screen.Controls)
	}
	if !foundWhitelistHelp {
		t.Fatalf("Telegram whitelist help link is missing: %#v", response.Screen.Controls)
	}
	whatsapp, err := service.Screen("whatsapp")
	if err != nil {
		t.Fatal(err)
	}
	if !whatsapp.StartAtTop {
		t.Fatal("WhatsApp configuration screen does not start at its status section")
	}
	wantFirst = []string{"wizard", "channels.whatsapp.mode", "channels.whatsapp.allowed_numbers", "channels.whatsapp.enabled", "advanced"}
	for index, key := range wantFirst {
		if whatsapp.Controls[index].Key != key {
			t.Fatalf("WhatsApp control %d = %q, want %q", index, whatsapp.Controls[index].Key, key)
		}
	}
}

func TestMainConfigurationStartsWithHarnessModelAndEssentials(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(root, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	service := New(cfg, newServiceHarness())
	screen, err := service.Screen("config")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"harness", "model", "harness.sandbox", "workspace.history_max_messages", "workspace.history_char_limit", "startup.enabled", "advanced"}
	if len(screen.Controls) < len(want)+1 {
		t.Fatalf("main configuration controls = %#v", screen.Controls)
	}
	for index, key := range want {
		if screen.Controls[index].Key != key {
			t.Fatalf("main control %d = %q, want %q", index, screen.Controls[index].Key, key)
		}
	}
	if screen.Controls[0].Kind != "action" || screen.Controls[1].Kind != "action" || screen.Controls[2].Kind != "select" || screen.Controls[6].Kind != "disclosure" || !screen.Controls[7].Advanced {
		t.Fatalf("main control kinds/order = %#v", screen.Controls)
	}
	harnessScreen, err := service.ScreenAction(context.Background(), "config", "harness", nil)
	if err != nil || harnessScreen == nil || harnessScreen.ID != "harness" || harnessScreen.ParentID != "config" || len(harnessScreen.Controls) != 2 {
		t.Fatalf("config harness action = %#v, %v", harnessScreen, err)
	}
	modelScreen, err := service.ScreenAction(context.Background(), "config", "model", nil)
	if err != nil || modelScreen == nil || modelScreen.ID != "model" || modelScreen.ParentID != "config" {
		t.Fatalf("config model action = %#v, %v", modelScreen, err)
	}
}

func TestTelegramSetupWizardCarriesAndAtomicallySavesEssentials(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(root, "spynel.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	service := New(cfg, newServiceHarness())
	screen, err := service.ScreenAction(context.Background(), "telegram", "wizard", nil)
	if err != nil || screen == nil || screen.ID != "wizard:telegram:intro" || !screen.Markdown || !screen.SaveDisabled || !screen.StartAtTop || !strings.Contains(screen.Subtitle, "desktop.\n\nUse Telegram's verified bot-management account: [@BotFather](https://t.me/BotFather)") {
		t.Fatalf("Telegram wizard intro = %#v, %v", screen, err)
	}
	screen, err = service.ScreenAction(context.Background(), screen.ID, "next", map[string]string{})
	if err != nil || screen.ID != "wizard:telegram:create" {
		t.Fatalf("Telegram create step = %#v, %v", screen, err)
	}
	screen, err = service.ScreenAction(context.Background(), screen.ID, "next", map[string]string{})
	if err != nil || screen.ID != "wizard:telegram:token" || screen.Controls[0].Kind != "password" {
		t.Fatalf("Telegram token step = %#v, %v", screen, err)
	}
	if _, err = service.ScreenAction(context.Background(), screen.ID, "next", map[string]string{}); err == nil {
		t.Fatal("Telegram wizard accepted a missing token")
	}
	values := map[string]string{
		telegramTokenKey:   "123456:secret-token",
		telegramAllowedKey: "alice,42",
		telegramEnabledKey: "on",
	}
	screen, err = service.ScreenAction(context.Background(), screen.ID, "next", values)
	if err != nil || screen.ID != "wizard:telegram:access" {
		t.Fatalf("Telegram access step = %#v, %v", screen, err)
	}
	if !strings.Contains(screen.Subtitle, "[@userinfobot](https://t.me/userinfobot)") || !screen.Controls[0].DescriptionMarkdown {
		t.Fatalf("Telegram access help = %#v", screen)
	}
	emptyValues := map[string]string{
		telegramTokenKey:   values[telegramTokenKey],
		telegramAllowedKey: " , ",
		telegramEnabledKey: "on",
	}
	if _, err = service.ScreenAction(context.Background(), screen.ID, "next", emptyValues); err == nil || !strings.Contains(err.Error(), "at least one allowed Telegram user") {
		t.Fatalf("Telegram wizard accepted an empty whitelist: %v", err)
	}
	screen, err = service.ScreenAction(context.Background(), screen.ID, "next", values)
	if err != nil || screen.ID != "wizard:telegram:enable" {
		t.Fatalf("Telegram enable step = %#v, %v", screen, err)
	}
	screen, err = service.ScreenAction(context.Background(), screen.ID, "finish", values)
	if err != nil || screen.ID != "telegram" || !strings.Contains(screen.Status, "setup saved") {
		t.Fatalf("Telegram wizard finish = %#v, %v", screen, err)
	}
	saved := service.Settings.Snapshot().Channels.Telegram
	if !saved.Enabled || saved.Token != values[telegramTokenKey] || len(saved.AllowedUsers) != 2 || saved.AllowedUsers[0] != "alice" {
		t.Fatalf("Telegram wizard saved %#v", saved)
	}
}

func TestWhatsAppSetupWizardEnablesThenShowsPairingStep(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(root, "spynel.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	service := New(cfg, newServiceHarness())
	screen, err := service.ScreenAction(context.Background(), "whatsapp", "wizard", nil)
	if err != nil || screen == nil || screen.ID != "wizard:whatsapp:mode" || screen.Controls[0].Key != whatsappModeKey {
		t.Fatalf("WhatsApp mode step = %#v, %v", screen, err)
	}
	values := map[string]string{
		whatsappModeKey:    "dedicated",
		whatsappAllowedKey: "15551234567",
		whatsappEnabledKey: "on",
	}
	screen, err = service.ScreenAction(context.Background(), screen.ID, "next", values)
	if err != nil || screen.ID != "wizard:whatsapp:access" {
		t.Fatalf("WhatsApp access step = %#v, %v", screen, err)
	}
	screen, err = service.ScreenAction(context.Background(), screen.ID, "next", values)
	if err != nil || screen.ID != "wizard:whatsapp:enable" || !strings.Contains(screen.Subtitle, "youtube.com") {
		t.Fatalf("WhatsApp enable step = %#v, %v", screen, err)
	}
	screen, err = service.ScreenAction(context.Background(), screen.ID, "finish", values)
	if err != nil || screen.ID != "wizard:whatsapp:pair" || !screen.StartAtTop {
		t.Fatalf("WhatsApp pair step = %#v, %v", screen, err)
	}
	saved := service.Settings.Snapshot().Channels.WhatsApp
	if !saved.Enabled || saved.Mode != "dedicated" || len(saved.AllowedNumbers) != 1 {
		t.Fatalf("WhatsApp wizard saved %#v", saved)
	}
}

func TestChatPromptNeutralizesRetiredChannelOverridesInExistingTemplates(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(root, "spynel.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	legacy := "Channel: {{CHANNEL}}\nProject binding: {{PROJECT}}\nChannel instructions: {{INSTRUCTIONS}}\nCustom {{PROJECT}} {{INSTRUCTIONS}}\n{{RECENT_HISTORY}}\n"
	if err := os.WriteFile(cfg.StatePath("prompts", "chat.md"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(cfg, newServiceHarness())
	prompt, err := service.chatPrompt(core.Message{Channel: "telegram", Conversation: "TG-7"})
	if err != nil {
		t.Fatal(err)
	}
	for _, retired := range []string{"{{PROJECT}}", "{{INSTRUCTIONS}}", "Project binding:", "Channel instructions:"} {
		if strings.Contains(prompt, retired) {
			t.Fatalf("retired channel override %q remains in prompt: %q", retired, prompt)
		}
	}
}

func TestResumeBranchesExternalConversationIntoIndependentTUIChat(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	service := New(cfg, newServiceHarness())
	_, _ = service.History.Append("telegram", "TG-7", history.Entry{Role: "user", Content: "remote question"})
	_, _ = service.History.Append("telegram", "TG-7", history.Entry{Role: "assistant", Content: "remote answer"})
	var response core.Event
	if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: "/resume"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if response.Kind != core.EventScreen || response.Screen == nil || response.Screen.ID != "resume" || len(response.Screen.Controls) == 0 {
		t.Fatalf("resume screen = %#v", response)
	}
	var action string
	for _, control := range response.Screen.Controls {
		if strings.Contains(control.Value, "TG-7") {
			action = control.Key
			break
		}
	}
	if action == "" {
		t.Fatalf("Telegram conversation missing from %#v", response.Screen.Controls)
	}
	chat, err := service.ScreenAction(context.Background(), "resume", action, nil)
	if err != nil {
		t.Fatal(err)
	}
	if chat == nil || chat.ID != "chat" || !strings.HasPrefix(chat.Conversation, "resume-") || len(chat.Transcript) != 3 || chat.Transcript[2].Text != "remote answer" || !strings.Contains(chat.Transcript[0].Text, "complete copied transcript") {
		t.Fatalf("branched chat = %#v", chat)
	}
	_, _ = service.History.Append("tui", chat.Conversation, history.Entry{Role: "user", Content: "branch only"})
	source, _, _ := service.History.RecentEntries("telegram", "TG-7", 1, 1000)
	if len(source) != 1 || source[0].Content != "remote answer" {
		t.Fatalf("source history changed: %#v", source)
	}

	response = core.Event{}
	if err := service.Handle(context.Background(), core.Message{Channel: "telegram", Conversation: "TG-7", Text: "/resume"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Text, "TUI only") {
		t.Fatalf("remote resume response = %#v", response)
	}
}

func TestWelcomeScreenIsAutomaticOnlyOnceAndRemainsInvokable(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	service := New(cfg, newServiceHarness())
	first, err := service.InitialWelcome()
	if err != nil || first == nil || first.ID != "welcome" || first.Banner != core.SpynelASCII || len(first.Controls) != 0 || first.Markdown {
		t.Fatalf("first welcome = %#v, %v", first, err)
	}
	for _, command := range []string{"/help", "/config", "/telegram", "/whatsapp"} {
		if !strings.Contains(first.Subtitle, command) {
			t.Fatalf("welcome guidance is missing %s: %q", command, first.Subtitle)
		}
	}
	second, err := service.InitialWelcome()
	if err != nil || second != nil {
		t.Fatalf("welcome was automatic more than once: %#v, %v", second, err)
	}
	var response core.Event
	if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: "/welcome"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if response.Kind != core.EventScreen || response.Screen == nil || response.Screen.ID != "welcome" || len(response.Screen.Controls) != 0 {
		t.Fatalf("manual TUI welcome = %#v", response)
	}
	if err := service.Handle(context.Background(), core.Message{Channel: "telegram", Conversation: "42", Text: "/welcome"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if response.Kind != core.EventFinal || !strings.Contains(response.Text, "Welcome to Spynel") || !strings.Contains(response.Text, "/config") {
		t.Fatalf("remote welcome = %#v", response)
	}
}

func TestClearCommandRemovesDurableConversationHistory(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	harness := newServiceHarness()
	service := New(cfg, harness)
	_, _ = service.History.Append("tui", "local", history.Entry{Role: "assistant", Content: "old response"})
	harness.threads["chat:tui:local"] = "stale-codex-thread"

	var response core.Event
	err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: "/clear"}, func(event core.Event) { response = event })
	if err != nil {
		t.Fatal(err)
	}
	if response.Kind != core.EventFinal || !response.Clear || response.Text == "" {
		t.Fatalf("unexpected clear response %#v", response)
	}
	entries, _, err := service.History.Entries("tui", "local")
	if err != nil || len(entries) != 0 {
		t.Fatalf("history after clear = %#v, %v", entries, err)
	}
	if len(harness.prompts) != 0 {
		t.Fatal("clear command should not call harness")
	}
	if thread := harness.ThreadID("chat:tui:local"); thread != "" {
		t.Fatalf("harness thread after clear = %q", thread)
	}

	response = core.Event{}
	if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: "next message"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if response.ThreadID == "" || response.ThreadID == "stale-codex-thread" {
		t.Fatalf("next message did not start a fresh harness thread: %#v", response)
	}
}

func TestClearCommandPreservesHistoryWhenRecipientResetFails(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	harness := newServiceHarness()
	harness.resetErr = fmt.Errorf("cannot reset while active")
	service := New(cfg, harness)
	_, _ = service.History.Append("telegram", "42", history.Entry{Role: "assistant", Content: "keep me"})

	var response core.Event
	if err := service.Handle(context.Background(), core.Message{Channel: "telegram", Conversation: "42", Text: "/clear"}, func(event core.Event) { response = event }); err != nil {
		t.Fatal(err)
	}
	if response.Clear || !strings.Contains(response.Text, "Cannot clear this conversation") {
		t.Fatalf("unexpected failed-clear response: %#v", response)
	}
	entries, _, err := service.History.Entries("telegram", "42")
	if err != nil || len(entries) < 1 || entries[0].Content != "keep me" {
		t.Fatalf("history changed after failed harness reset: %#v, %v", entries, err)
	}
}
