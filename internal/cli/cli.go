package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/frdel/spynel/internal/app"
	"github.com/frdel/spynel/internal/channel"
	"github.com/frdel/spynel/internal/channel/telegram"
	"github.com/frdel/spynel/internal/channel/tui"
	"github.com/frdel/spynel/internal/channel/whatsapp"
	"github.com/frdel/spynel/internal/config"
	"github.com/frdel/spynel/internal/core"
	"github.com/frdel/spynel/internal/extensions"
	"github.com/frdel/spynel/internal/harness"
	"github.com/frdel/spynel/internal/media"
	"github.com/frdel/spynel/internal/orchestrator"
	startupmanager "github.com/frdel/spynel/internal/startup"
	"github.com/frdel/spynel/internal/workspace"
)

const (
	initialTUIHistoryMessages = 500
	initialTUIHistoryChars    = 500000
	restartNoticeDelay        = 200 * time.Millisecond
)

func Run(args []string, version string) error {
	return completeRun(run(args, version), replaceCurrentProcess)
}

func run(args []string, version string) error {
	if len(args) == 0 {
		return runServer("", true, version, nil)
	}
	switch args[0] {
	case "help", "--help", "-h":
		fmt.Print(helpText)
		return nil
	case "version", "--version", "-v":
		fmt.Println("spynel " + version)
		return nil
	case "init":
		flags := flag.NewFlagSet("init", flag.ContinueOnError)
		force := flags.Bool("force", false, "restore missing workspace templates")
		root := flags.String("dir", ".", "project directory")
		noStart := flags.Bool("no-start", false, "initialize without launching the TUI")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if err := workspace.Init(*root, *force); err != nil {
			return err
		}
		absolute, _ := filepath.Abs(*root)
		fmt.Println("Initialized Spynel in " + absolute)
		if !*noStart && interactiveTerminal() {
			configPath := filepath.Join(absolute, config.FileName)
			return runServer(configPath, true, version, []string{"serve", "--tui", "--config", configPath})
		}
		return nil
	case "serve":
		flags := flag.NewFlagSet("serve", flag.ContinueOnError)
		configPath := flags.String("config", "", "path to spynel.yaml")
		withTUI := flags.Bool("tui", false, "also launch the terminal UI")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		return runServer(*configPath, *withTUI, version, append([]string(nil), args...))
	case "run":
		flags := flag.NewFlagSet("run", flag.ContinueOnError)
		configPath := flags.String("config", "", "path to spynel.yaml")
		once := flags.Bool("once", false, "run one scan and wait for dispatched turns")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if !*once {
			return errors.New("run currently requires --once; use 'spynel serve' for the continuous loop")
		}
		return runOnce(*configPath, version)
	case "task", "todo", "goal":
		if len(args) < 2 {
			return fmt.Errorf("usage: spynel %s <request>", args[0])
		}
		cfg, err := config.Load("")
		if err != nil {
			return err
		}
		route := "tasks"
		if args[0] == "goal" {
			route = "goals"
		}
		path, err := orchestrator.Create(cfg, route, strings.Join(args[1:], " "), "")
		if err != nil {
			return err
		}
		fmt.Println(path)
		return nil
	case "config":
		cfg, err := config.Load("")
		if err != nil {
			return err
		}
		fmt.Println("valid: " + cfg.Path)
		return nil
	case "doctor":
		return doctor()
	case "extension", "extensions":
		return extensionCommand(args[1:])
	case "whatsapp":
		if len(args) != 2 || args[1] != "pair" {
			return errors.New("usage: spynel whatsapp pair")
		}
		return pairWhatsApp()
	default:
		return fmt.Errorf("unknown command %q; run 'spynel help'", args[0])
	}
}

type restartRequest struct {
	args []string
}

func (r *restartRequest) Error() string {
	return "restart Spynel"
}

func completeRun(err error, restart func([]string) error) error {
	var request *restartRequest
	if !errors.As(err, &request) {
		return err
	}
	return restart(append([]string(nil), request.args...))
}

func runServer(configPath string, withTUI bool, version string, restartArgs []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		if withTUI && configPath == "" && errors.Is(err, config.ErrNotInitialized) {
			root, cwdErr := os.Getwd()
			if cwdErr != nil {
				return cwdErr
			}
			initialized, setupErr := tui.RunInitialization(context.Background(), root, func() error {
				return workspace.Init(root, false)
			})
			if setupErr != nil || !initialized {
				return setupErr
			}
			return runServer(filepath.Join(root, config.FileName), true, version, restartArgs)
		}
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	service, err := buildService(cfg, version)
	if err != nil {
		return err
	}
	launchTUI := withTUI || (cfg.Channels.TUI.Enabled && len(os.Args) == 1)
	if err := service.Start(ctx); err != nil {
		service.Runtime.Log("Harness unavailable: " + err.Error())
	}
	service.Runtime.Log("Spynel started")
	defer service.Harness.Close()
	var restarting atomic.Bool
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-service.RestartRequests():
		}
		timer := time.NewTimer(restartNoticeDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			restarting.Store(true)
			cancel()
		}
	}()
	serverResult := func(err error) error {
		cancel()
		if restarting.Load() {
			return &restartRequest{args: append([]string(nil), restartArgs...)}
		}
		return err
	}
	errorsChannel := make(chan error, 4)
	connectionEvents := make(chan channel.ConnectionStatus, 32)
	reportConnection := func(status channel.ConnectionStatus) {
		service.SetConnectionStatus(status)
		select {
		case connectionEvents <- status:
		default:
		}
	}
	go runOrchestratorWhenReady(ctx, service, errorsChannel)
	startChannels(ctx, service, reportConnection)
	if launchTUI {
		initialHistory, _, err := service.History.RecentEntries("tui", "local", initialTUIHistoryMessages, initialTUIHistoryChars)
		if err != nil {
			cancel()
			return fmt.Errorf("load TUI history: %w", err)
		}
		var initialScreen *core.Screen
		hasHistory, historyErr := service.History.HasEntries("tui", "local")
		if historyErr != nil {
			cancel()
			return fmt.Errorf("inspect TUI history: %w", historyErr)
		}
		if !hasHistory {
			if availability, ok := service.Harness.(harness.Availability); ok {
				if ready, _ := availability.Available(); !ready {
					screen := service.HarnessScreen(true)
					initialScreen = &screen
				}
			}
			if initialScreen == nil {
				initialScreen, err = service.InitialWelcome()
				if err != nil {
					cancel()
					return fmt.Errorf("load welcome screen: %w", err)
				}
			}
		}
		err = tui.Run(ctx, cfg.Channels.TUI.Title, service.Handle, app.SlashCommands(), initialHistory, tui.Options{
			Attachments:        cfg.StatePath("attachments"),
			TitlePath:          cfg.StatePath("tui-title"),
			TitleEvents:        service.TitleChanges(),
			ConnectionEvents:   connectionEvents,
			PairingEvents:      service.PairingEvents(),
			NoticeEvents:       service.NoticeEvents(),
			InitialConnections: initialConnectionStatuses(cfg),
			RuntimeEvents:      service.Runtime.Updates(),
			InitialRuntime:     service.Runtime.Status(),
			SaveSettings: func(values map[string]string) error {
				_, saveErr := service.ApplySettings(values)
				return saveErr
			},
			InitialScreen: initialScreen,
			ScreenAction:  service.ScreenAction,
		})
		return serverResult(err)
	}
	select {
	case <-ctx.Done():
		return serverResult(nil)
	case err := <-errorsChannel:
		if errors.Is(err, context.Canceled) {
			return serverResult(nil)
		}
		return serverResult(err)
	}
}

func interactiveTerminal() bool {
	input, inputErr := os.Stdin.Stat()
	output, outputErr := os.Stdout.Stat()
	return inputErr == nil && outputErr == nil && input.Mode()&os.ModeCharDevice != 0 && output.Mode()&os.ModeCharDevice != 0
}

func runOrchestratorWhenReady(ctx context.Context, service *app.Service, errorsChannel chan<- error) {
	if availability, ok := service.Harness.(harness.Availability); ok {
		if ready, _ := availability.Available(); !ready {
			select {
			case <-ctx.Done():
				errorsChannel <- ctx.Err()
				return
			case <-availability.ReadyEvents():
			}
		}
	}
	errorsChannel <- service.Orchestrator.Run(ctx)
}

func runOnce(configPath, version string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	service, err := buildService(cfg, version)
	if err != nil {
		return err
	}
	if err := service.Start(ctx); err != nil {
		return err
	}
	defer service.Harness.Close()
	if err := service.Orchestrator.ScanOnce(ctx); err != nil {
		return err
	}
	return service.Orchestrator.WaitForIdle(ctx)
}

func buildService(cfg config.Config, version string) (*app.Service, error) {
	runtimeState := app.NewRuntime()
	registry := harness.NewRegistry()
	registry.Register("codex", func(runtimeConfig harness.HarnessConfig) (harness.Harness, error) {
		return harness.NewCodex(harness.CodexConfig{
			Command: runtimeConfig.Command, Cwd: runtimeConfig.Cwd, Model: runtimeConfig.Model,
			Effort: runtimeConfig.Effort, ApprovalPolicy: runtimeConfig.ApprovalPolicy,
			Sandbox: runtimeConfig.Sandbox, Network: runtimeConfig.Network,
			SessionsFile: runtimeConfig.SessionsFile, Version: runtimeConfig.Version, Stderr: runtimeConfig.Stderr,
		})
	})
	registry.Register("claude-code", func(runtimeConfig harness.HarnessConfig) (harness.Harness, error) {
		return harness.NewClaude(runtimeConfig)
	})
	command, commandErr := harness.ResolveCommand(cfg.Harness.Name, nil)
	if commandErr != nil {
		if definition, ok := harness.Lookup(cfg.Harness.Name); ok {
			command = definition.Command
		}
	}
	target := harness.NewSupervisor(registry, harness.HarnessConfig{
		Name: cfg.Harness.Name, Command: command, Cwd: cfg.Root,
		Model: cfg.Harness.Model, Effort: "medium",
		ApprovalPolicy: "never", Sandbox: cfg.Harness.Sandbox,
		Network: false, SessionsFile: cfg.HarnessSessionsPath(cfg.Harness.Name),
		Version: version, Stderr: runtimeState,
	})
	service := app.NewWithRuntime(cfg, target, runtimeState)
	startup, err := startupmanager.New("")
	if err != nil {
		return nil, fmt.Errorf("initialize startup manager: %w", err)
	}
	service.Startup = startup
	return service, nil
}

func startChannels(ctx context.Context, service *app.Service, report channel.StatusReporter) {
	initial := service.Settings.Snapshot()
	speech := media.NewWhisper(service.Settings, initial.StatePath("models", "whisper"), initial.StatePath("runtime", "speech"), service.Runtime)
	managed := []channel.Managed{
		{
			Name:    "telegram",
			Enabled: func(cfg config.Config) bool { return cfg.Channels.Telegram.Enabled },
			Fingerprint: func(cfg config.Config) string {
				return configFingerprint(struct {
					Settings config.Telegram
					Token    string
					Speech   config.Speech
					MaxMB    int
				}{cfg.Channels.Telegram, cfg.TelegramToken(), cfg.Speech, cfg.Workspace.AttachmentMaxMB})
			},
			Build: func(cfg config.Config) (channel.Channel, error) {
				bot := telegram.New(cfg.Channels.Telegram, cfg.TelegramToken())
				bot.SetNoticeReporter(service.SetNotice)
				store := &media.Store{Directory: cfg.StatePath("attachments", "telegram"), MaxBytes: int64(cfg.Workspace.AttachmentMaxMB) * 1024 * 1024}
				var transcriber media.Transcriber
				if cfg.Speech.Enabled {
					transcriber = speech
				}
				bot.SetMedia(store, transcriber)
				return bot, nil
			},
		},
		{
			Name:    "whatsapp",
			Enabled: func(cfg config.Config) bool { return cfg.Channels.WhatsApp.Enabled },
			Fingerprint: func(cfg config.Config) string {
				return configFingerprint(struct {
					Settings config.WhatsApp
					Speech   config.Speech
					MaxMB    int
				}{cfg.Channels.WhatsApp, cfg.Speech, cfg.Workspace.AttachmentMaxMB})
			},
			Build: func(cfg config.Config) (channel.Channel, error) {
				client := whatsapp.New(cfg.Channels.WhatsApp, cfg.Resolve(cfg.Channels.WhatsApp.Database))
				client.SetPairingReporter(service.SetPairing)
				store := &media.Store{Directory: cfg.StatePath("attachments", "whatsapp"), MaxBytes: int64(cfg.Workspace.AttachmentMaxMB) * 1024 * 1024}
				var transcriber media.Transcriber
				if cfg.Speech.Enabled {
					transcriber = speech
				}
				client.SetMedia(store, transcriber)
				return client, nil
			},
		},
	}
	supervisor := channel.NewSupervisor(service.Settings, service.Handle, managed, report, service.Runtime)
	go func() {
		if err := supervisor.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			service.Runtime.Log("channel supervisor: " + err.Error())
		}
	}()
}

func configFingerprint(value any) string {
	data, _ := json.Marshal(value)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func initialConnectionStatuses(cfg config.Config) []channel.ConnectionStatus {
	statuses := []channel.ConnectionStatus{
		{Name: "telegram", State: channel.ConnectionUnconfigured},
		{Name: "whatsapp", State: channel.ConnectionUnconfigured},
	}
	if cfg.Channels.Telegram.Enabled {
		statuses[0].State = channel.ConnectionConnecting
	}
	if cfg.Channels.WhatsApp.Enabled {
		statuses[1].State = channel.ConnectionConnecting
	}
	return statuses
}

func extensionCommand(args []string) error {
	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	directory := cfg.Resolve(cfg.Extensions.Directory)
	if len(args) == 0 || args[0] == "list" {
		names, err := extensions.List(directory)
		if err != nil {
			return err
		}
		if len(names) == 0 {
			fmt.Println("No extensions installed.")
		} else {
			fmt.Println(strings.Join(names, "\n"))
		}
		return nil
	}
	switch args[0] {
	case "install":
		if len(args) < 2 || len(args) > 3 {
			return errors.New("usage: spynel extension install <git-url> [name]")
		}
		name := ""
		if len(args) == 3 {
			name = args[2]
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		path, err := extensions.Install(ctx, directory, args[1], name)
		if err != nil {
			return err
		}
		fmt.Println(path)
		return nil
	case "remove":
		if len(args) != 2 {
			return errors.New("usage: spynel extension remove <name>")
		}
		if err := extensions.Remove(directory, args[1]); err != nil {
			return err
		}
		fmt.Printf("Removed %s; reinstall its Git repository to recover it.\n", args[1])
		return nil
	default:
		return errors.New("usage: spynel extension [list|install <git-url> [name]|remove <name>]")
	}
}

func pairWhatsApp() error {
	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	client := whatsapp.New(cfg.Channels.WhatsApp, cfg.Resolve(cfg.Channels.WhatsApp.Database))
	fmt.Fprintln(os.Stderr, "Starting WhatsApp pairing. Press Ctrl+C after the connection succeeds.")
	err = client.Run(ctx, func(context.Context, core.Message, core.Emit) error { return nil })
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func doctor() error {
	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	fmt.Println("config: ok (" + cfg.Path + ")")
	command, err := harness.ResolveCommand(cfg.Harness.Name, nil)
	if err != nil {
		return err
	}
	fmt.Println("coding harness: " + cfg.Harness.Name + " (" + command + ")")
	if err := os.MkdirAll(cfg.StatePath("runtime"), 0o700); err != nil {
		return err
	}
	testPath := cfg.StatePath("runtime", ".doctor")
	if err := os.WriteFile(testPath, []byte("ok\n"), 0o600); err != nil {
		return fmt.Errorf("state directory is not writable: %w", err)
	}
	_ = os.Remove(testPath)
	fmt.Println("state directory: writable (" + cfg.Resolve(cfg.Workspace.StateDir) + ")")
	if cfg.Channels.Telegram.Enabled && cfg.TelegramToken() == "" {
		return errors.New("Telegram is enabled but its token is empty")
	}
	fmt.Println("telegram: " + enabled(cfg.Channels.Telegram.Enabled))
	fmt.Println("whatsapp: " + enabled(cfg.Channels.WhatsApp.Enabled))
	fmt.Println("doctor: all local checks passed")
	return nil
}

func enabled(value bool) string {
	if value {
		return "enabled"
	}
	return "disabled"
}

const helpText = `Spynel - extensible chat and markdown orchestration

Usage:
  spynel                         Launch TUI and enabled background services
  spynel serve [--tui]           Run Telegram, WhatsApp, and the loop as a server
  spynel init [--dir DIR]        Initialize and continue into the TUI
    --no-start                   Initialize only (for scripts and automation)
  spynel run --once              Dispatch one orchestration scan and wait
  spynel task REQUEST            Create a task markdown file
  spynel goal OBJECTIVE          Create a goal markdown file
  spynel extension ...           List, install, or remove Git extensions
  spynel whatsapp pair           Pair a WhatsApp account by QR code
  spynel config                  Validate and locate spynel.yaml
  spynel doctor                  Check local configuration and prerequisites
  spynel version                 Print the binary version
`
