package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/channel/telegram"
	"github.com/agent0ai/spynel/internal/channel/tui"
	"github.com/agent0ai/spynel/internal/channel/whatsapp"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/extensions"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
	"github.com/agent0ai/spynel/internal/instance"
	"github.com/agent0ai/spynel/internal/instructions"
	"github.com/agent0ai/spynel/internal/localapi"
	"github.com/agent0ai/spynel/internal/media"
	"github.com/agent0ai/spynel/internal/orchestrator"
	startupmanager "github.com/agent0ai/spynel/internal/startup"
	"github.com/agent0ai/spynel/internal/theme"
	"github.com/agent0ai/spynel/internal/updater"
	"github.com/agent0ai/spynel/internal/workspace"
)

const (
	initialTUIHistoryMessages = 500
	initialTUIHistoryChars    = 500000
	restartNoticeDelay        = 200 * time.Millisecond
	npmUpdateExitCode         = 75
)

func Run(args []string, version string) (runErr error) {
	defer func() {
		if value := recover(); value != nil {
			if path, ok := failureConfigPath(args); ok {
				if cfg, err := config.Load(path); err == nil {
					runtimeState := app.NewRuntimeAt(cfg.StatePath("runtime", "logs"), fmt.Sprintf("pid-%d", os.Getpid()))
					runtimeState.LogEvent("fatal", "process", "top_level_panic", fmt.Sprintf("panic: %v\n%s", value, debug.Stack()))
					runtimeState.Close()
				}
			}
			panic(value)
		}
		if runErr != nil {
			if _, controlledExit := runErr.(interface{ ExitCode() int }); !controlledExit {
				recordCommandFailure(args, runErr)
			}
		}
	}()
	runErr = completeRun(run(args, version), replaceCurrentProcess)
	return runErr
}

func recordCommandFailure(args []string, runErr error) {
	path, ok := failureConfigPath(args)
	if !ok {
		return
	}
	cfg, err := config.Load(path)
	if err != nil {
		return
	}
	runtimeState := app.NewRuntimeAt(cfg.StatePath("runtime", "logs"), fmt.Sprintf("pid-%d-error", os.Getpid()))
	runtimeState.LogEvent("error", "process", "command_failed", fmt.Sprintf("Spynel command failed (%T)", runErr))
	runtimeState.Close()
}

func failureConfigPath(args []string) (string, bool) {
	path := configPathArgument(args)
	if len(args) != 0 || path != "" {
		return path, true
	}
	launchRoot, err := canonicalLaunchDirectory()
	if err != nil {
		return "", false
	}
	path = config.PathForRoot(launchRoot)
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	return path, true
}

func configPathArgument(args []string) string {
	for index, argument := range args {
		if argument == "--config" && index+1 < len(args) {
			return args[index+1]
		}
		if strings.HasPrefix(argument, "--config=") {
			return strings.TrimPrefix(argument, "--config=")
		}
	}
	return ""
}

func run(args []string, version string) error {
	if bareInteractiveRequested(args) {
		return runBareInteractive(version)
	}
	switch args[0] {
	case "help", "--help", "-h":
		fmt.Print(helpText)
		return nil
	case "docs":
		return runDocsCommand(args[1:], os.Stdout)
	case "instructions":
		return runInstructionsCommand(args[1:], os.Stdout)
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
			configPath := config.PathForRoot(absolute)
			return runServer(configPath, true, version, []string{"serve", "--tui", "--config", configPath})
		}
		return nil
	case "serve":
		flags := flag.NewFlagSet("serve", flag.ContinueOnError)
		configPath := flags.String("config", "", "path to .spynel/config.yaml")
		withTUI := flags.Bool("tui", false, "also launch the terminal UI")
		flags.Bool("automatic-startup", false, "identify a non-interactive operating-system startup")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		return runServer(*configPath, *withTUI, version, append([]string(nil), args...))
	case "run":
		flags := flag.NewFlagSet("run", flag.ContinueOnError)
		configPath := flags.String("config", "", "path to .spynel/config.yaml")
		once := flags.Bool("once", false, "run one scan and wait for dispatched turns")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if !*once {
			return errors.New("run currently requires --once; use 'spynel serve' for the continuous loop")
		}
		return runOnce(*configPath, version)
	case "send":
		return runSendCommand(args[0], args[1:], version, false)
	case "followup":
		return runSendCommand("followup", args[1:], version, true)
	case "notify":
		return runNotifyCommand(args[1:], version)
	case "command":
		return runFrameworkCLICommand("", args[1:], version)
	case "status":
		return runStatusCLICommand(args[1:], version, os.Stdout)
	case "jobs", "tasks", "goals", "job", "log", "logs", "stop", "new", "clear", "history", "harness", "model", "telegram", "restart", "update":
		return runFrameworkCLICommand(args[0], args[1:], version)
	case "conversation", "conversations":
		return runConversationCommand(args[1:], os.Stdout)
	case "task", "todo", "goal":
		if len(args) >= 2 && args[0] != "goal" && args[1] == "inspect" {
			if len(args) != 3 {
				return errors.New("usage: spynel task inspect FILE")
			}
			return inspectTaskPolicy(args[2], os.Stdout)
		}
		noReview := false
		requestArgs := args[1:]
		if len(requestArgs) > 0 && requestArgs[0] == "--no-review" && args[0] != "goal" {
			noReview = true
			requestArgs = requestArgs[1:]
		}
		if len(requestArgs) == 0 {
			return fmt.Errorf("usage: spynel %s [--no-review] <request>", args[0])
		}
		cfg, err := config.Load("")
		if err != nil {
			return err
		}
		route := "tasks"
		if args[0] == "goal" {
			route = "goals"
		}
		request := strings.Join(requestArgs, " ")
		if route == "tasks" {
			_, _ = history.New(cfg.StatePath("history")).Append("cli", "local", history.Entry{Role: "user", Sender: "cli", Content: "/task " + request})
		}
		options := orchestrator.CreateOptions{}
		if route == "tasks" {
			options = orchestrator.CreateOptions{Notify: true, Origin: "cli/local", Outcomes: []string{"done", "failed", "waiting", "cancelled"}, NoReview: noReview}
		}
		path, err := orchestrator.CreateWithOptions(cfg, route, request, "", options)
		if err != nil {
			return err
		}
		fmt.Println(path)
		return nil
	case "config":
		if len(args) > 1 {
			return runFrameworkCLICommand("config", args[1:], version)
		}
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
		if len(args) == 2 && args[1] == "pair" {
			return pairWhatsApp()
		}
		return runFrameworkCLICommand("whatsapp", args[1:], version)
	default:
		return fmt.Errorf("unknown command %q; run 'spynel help'", args[0])
	}
}

func bareInteractiveRequested(args []string) bool {
	return len(args) == 0
}

func canonicalLaunchDirectory() (string, error) {
	root, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return canonicalDirectory(root)
}

func canonicalDirectory(root string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(root)
}

type bareWorkspaceDiscovery struct {
	localConfig      string
	discoveredConfig string
	parentRoot       string
	ancestorFound    bool
}

func discoverBareWorkspace(launchRoot string) (bareWorkspaceDiscovery, error) {
	result := bareWorkspaceDiscovery{localConfig: config.PathForRoot(launchRoot)}
	if _, err := os.Stat(result.localConfig); err == nil {
		result.discoveredConfig = result.localConfig
		return result, nil
	} else if !os.IsNotExist(err) {
		return bareWorkspaceDiscovery{}, err
	}
	discovered, err := config.Find(launchRoot)
	if errors.Is(err, config.ErrNotInitialized) {
		return result, nil
	}
	if err != nil {
		return bareWorkspaceDiscovery{}, err
	}
	result.discoveredConfig = discovered
	result.parentRoot = filepath.Dir(filepath.Dir(discovered))
	result.ancestorFound = filepath.Clean(result.parentRoot) != filepath.Clean(launchRoot)
	return result, nil
}

func runBareInteractive(version string) error {
	return runBareInteractiveWithRuntime(version, defaultBareInteractiveRuntime(version))
}

type bareInteractiveRuntime struct {
	canonicalLaunchDirectory func() (string, error)
	discoverWorkspace        func(string) (bareWorkspaceDiscovery, error)
	runInitialization        func(context.Context, string, func() error) (bool, error)
	runParentChoice          func(context.Context, string, string, func() error) (tui.WorkspaceChoice, error)
	initializeWorkspace      func(string, bool) error
	changeDirectory          func(string) error
	startServer              func(string, bool, string, []string) error
}

func defaultBareInteractiveRuntime(version string) bareInteractiveRuntime {
	return bareInteractiveRuntime{
		canonicalLaunchDirectory: canonicalLaunchDirectory,
		discoverWorkspace:        discoverBareWorkspace,
		runInitialization: func(ctx context.Context, root string, initialize func() error) (bool, error) {
			return tui.RunInitializationWithVersion(ctx, root, version, initialize)
		},
		runParentChoice: func(ctx context.Context, launchRoot, parentRoot string, initialize func() error) (tui.WorkspaceChoice, error) {
			return tui.RunParentWorkspaceChoiceWithVersion(ctx, launchRoot, parentRoot, version, initialize)
		},
		initializeWorkspace: workspace.Init,
		changeDirectory:     os.Chdir,
		startServer:         runServer,
	}
}

func runBareInteractiveWithRuntime(version string, runtime bareInteractiveRuntime) error {
	launchRoot, err := runtime.canonicalLaunchDirectory()
	if err != nil {
		return err
	}
	discovery, err := runtime.discoverWorkspace(launchRoot)
	if err != nil {
		return err
	}
	if discovery.discoveredConfig == "" {
		initialized, setupErr := runtime.runInitialization(context.Background(), launchRoot, func() error {
			return runtime.initializeWorkspace(launchRoot, false)
		})
		if setupErr != nil || !initialized {
			return setupErr
		}
		if err := runtime.changeDirectory(launchRoot); err != nil {
			return fmt.Errorf("enter initialized workspace: %w", err)
		}
		return runtime.startServer(discovery.localConfig, true, version, nil)
	}
	if !discovery.ancestorFound {
		return runtime.startServer(discovery.localConfig, true, version, nil)
	}

	choice, choiceErr := runtime.runParentChoice(context.Background(), launchRoot, discovery.parentRoot, func() error {
		return runtime.initializeWorkspace(launchRoot, false)
	})
	if choiceErr != nil {
		return choiceErr
	}
	switch choice {
	case tui.WorkspaceChoiceUseParent:
		if err := runtime.changeDirectory(discovery.parentRoot); err != nil {
			return fmt.Errorf("enter parent workspace: %w", err)
		}
		return runtime.startServer(discovery.discoveredConfig, true, version, nil)
	case tui.WorkspaceChoiceInitializeHere:
		if err := runtime.changeDirectory(launchRoot); err != nil {
			return fmt.Errorf("enter initialized workspace: %w", err)
		}
		return runtime.startServer(discovery.localConfig, true, version, nil)
	default:
		return nil
	}
}

func inspectTaskPolicy(path string, output io.Writer) error {
	document, err := orchestrator.ReadDocument(path)
	if err != nil {
		return err
	}
	policy, policyErr := orchestrator.TaskPolicyFromDocument(document)
	effective := policy.ReviewRequired
	if configPath, findErr := config.Find(filepath.Dir(path)); findErr == nil {
		if cfg, loadErr := config.Load(configPath); loadErr == nil {
			effective = cfg.Harness.EffectiveTaskReviewRequired(policy.ReviewRequired)
			_, _ = fmt.Fprintln(output, "Configured task review mode: "+cfg.Harness.Reviews)
		}
	}
	_, _ = fmt.Fprintf(output, "Review required: %t\n", effective)
	if policyErr != nil {
		_, _ = fmt.Fprintln(output, "Policy warning: "+policyErr.Error()+"; treated as review required")
	}
	return nil
}

func runInstructionsCommand(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("instructions", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "path to .spynel/config.yaml")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: spynel instructions [--config PATH]")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	invalid := false
	for _, status := range instructions.Inspect(cfg.StatePath()) {
		state := "missing (valid empty fallback)"
		if status.Present && status.Valid {
			state = fmt.Sprintf("valid (%d bytes)", status.Bytes)
		} else if status.Error != "" {
			state = "invalid (" + status.Error + ")"
			invalid = true
		}
		_, _ = fmt.Fprintf(output, "%s: %s — %s\n", status.Role, status.RelativePath, state)
	}
	if invalid {
		return errors.New("one or more persistent instruction files are unsafe or invalid")
	}
	return nil
}

type restartRequest struct {
	args []string
}

func (r *restartRequest) Error() string {
	return "restart Spynel"
}

type updateRequest struct {
	args []string
}

func (*updateRequest) Error() string { return "update Spynel through npm" }
func (r *updateRequest) ExitCode() int {
	r.writeRestartArgs()
	return npmUpdateExitCode
}

func (r *updateRequest) writeRestartArgs() {
	requestPath := strings.TrimSpace(os.Getenv("SPYNEL_NPM_UPDATE_STATE"))
	if requestPath == "" || filepath.Clean(filepath.Dir(requestPath)) != filepath.Clean(os.TempDir()) || !strings.HasPrefix(filepath.Base(requestPath), "spynel-update-") {
		return
	}
	data, err := json.Marshal(struct {
		Args []string `json:"args"`
	}{Args: append([]string(nil), r.args...)})
	if err != nil {
		return
	}
	file, err := os.OpenFile(requestPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return
	}
	_, _ = file.Write(append(data, '\n'))
	_ = file.Close()
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
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	election, err := instance.New(cfg.StatePath())
	if err != nil {
		return err
	}
	hadHealthyPrimary := false
	if lease, leaseErr := election.Current(); leaseErr == nil {
		hadHealthyPrimary = !election.IsStale(lease)
	}
	// Invocation decides presentation deterministically: the bare command and
	// init continuation pass withTUI=true; `serve` is headless unless --tui.
	launchTUI := withTUI
	var restartScheduled atomic.Bool
	var restarting atomic.Bool
	var updateScheduled atomic.Bool
	var updating atomic.Bool
	requestRestart := func() {
		if !restartScheduled.CompareAndSwap(false, true) {
			return
		}
		go func() {
			timer := time.NewTimer(restartNoticeDelay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
			case <-timer.C:
				restarting.Store(true)
				cancel()
			}
		}()
	}
	requestUpdate := func() {
		if !updateScheduled.CompareAndSwap(false, true) {
			return
		}
		go func() {
			timer := time.NewTimer(restartNoticeDelay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
			case <-timer.C:
				updating.Store(true)
				cancel()
			}
		}()
	}
	ownerResult := make(chan error, 1)
	go func() {
		ownerErr := runOwnerElection(ctx, cfg, version, election, requestRestart, requestUpdate)
		ownerResult <- ownerErr
		if ownerErr != nil {
			cancel()
		}
	}()
	ownerFinished := false
	serverResult := func(err error) error {
		cancel()
		if !ownerFinished {
			ownerErr := <-ownerResult
			ownerFinished = true
			if err == nil && ownerErr != nil {
				err = ownerErr
			}
		}
		if updating.Load() {
			return &updateRequest{args: append([]string(nil), restartArgs...)}
		}
		if restarting.Load() {
			return &restartRequest{args: append([]string(nil), restartArgs...)}
		}
		return err
	}
	client := localapi.NewClient(election)
	connectionStatus := newStartupConnectionStatus(os.Stderr, launchTUI && hadHealthyPrimary && terminalOutput(os.Stderr))
	connectionStatus.connecting()
	ready := make(chan struct {
		state app.SharedState
		err   error
	}, 1)
	go func() {
		state, waitErr := client.WaitReady(ctx)
		ready <- struct {
			state app.SharedState
			err   error
		}{state: state, err: waitErr}
	}()
	var shared app.SharedState
	select {
	case result := <-ready:
		if result.err != nil {
			connectionStatus.failed(result.err)
			return serverResult(result.err)
		}
		connectionStatus.connected()
		shared = result.state
	case err := <-ownerResult:
		ownerFinished = true
		if err != nil {
			connectionStatus.failed(err)
		}
		return serverResult(err)
	case <-ctx.Done():
		return serverResult(nil)
	}
	if launchTUI {
		themes, themeErr := theme.LoadDir(cfg.StatePath("themes"))
		if themeErr != nil {
			return serverResult(fmt.Errorf("load TUI themes: %w", themeErr))
		}
		activeTheme := theme.Selected(themes, shared.Theme)
		if _, found := theme.Find(themes, shared.Theme); !found {
			if saveErr := client.ApplySettings(ctx, map[string]string{"channels.tui.theme": activeTheme.Name}); saveErr != nil {
				return serverResult(fmt.Errorf("restore missing TUI theme: %w", saveErr))
			}
		}
		histories := history.New(cfg.StatePath("history"))
		lease, leaseErr := election.Current()
		becameInitialPrimary := leaseErr == nil && shouldResumeTUIHistory(hadHealthyPrimary, lease, election.ID())
		conversation, err := selectTUIConversation(histories, election.ID(), becameInitialPrimary)
		if err != nil {
			return serverResult(fmt.Errorf("select startup TUI history: %w", err))
		}
		// Admit the selected conversation before reading it. Cleanup serializes
		// this registration with its protection snapshot and deletion, so a
		// history cannot disappear between startup selection and live ownership.
		if err := client.RegisterLiveTUI(ctx, conversation); err != nil {
			return serverResult(fmt.Errorf("register live TUI conversation: %w", err))
		}
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			_ = client.UnregisterLiveTUI(cleanupCtx)
		}()
		initialHistory, _, err := histories.RecentEntries("tui", conversation, initialTUIHistoryMessages, initialTUIHistoryChars)
		if err != nil {
			return serverResult(fmt.Errorf("load TUI history: %w", err))
		}
		hasHistory, historyErr := histories.HasEntries("tui", conversation)
		if historyErr != nil {
			return serverResult(fmt.Errorf("inspect TUI history: %w", historyErr))
		}
		initialScreen, err := client.InitialScreen(ctx, hasHistory, !becameInitialPrimary)
		if err != nil {
			return serverResult(fmt.Errorf("load initial TUI screen: %w", err))
		}
		stateEvents := startTUIStatePolling(ctx, client, shared, cfg.StatePath("themes"))
		notificationEvents := watchTaskNotifications(ctx, histories.Path("tui", conversation))
		err = tui.Run(ctx, shared.Title, client.Handle, app.SlashCommands(), initialHistory, tui.Options{
			Conversation: conversation,
			Version:      version,
			Attachments:  cfg.StatePath("attachments"),
			TitlePath:    cfg.StatePath("tui-title"),
			TitleEvents:  stateEvents.titles,
			Themes:       themes,
			InitialTheme: activeTheme,
			ThemeEvents:  stateEvents.themes,
			SaveTheme: func(name string) error {
				return client.ApplySettings(ctx, map[string]string{"channels.tui.theme": name})
			},
			LoadThemes: func() ([]theme.Theme, error) {
				return theme.LoadDir(cfg.StatePath("themes"))
			},
			ConnectionEvents:   stateEvents.connections,
			PairingEvents:      stateEvents.pairings,
			NoticeEvents:       stateEvents.notices,
			NotificationEvents: notificationEvents,
			AckNotification:    func(id string, after int) error { return client.AckNotification(ctx, "tui/"+conversation, id, after) },
			InitialConnections: shared.Connections,
			RuntimeEvents:      stateEvents.runtime,
			InitialRuntime:     shared.Runtime,
			DurableWorkEvents:  stateEvents.durableWork,
			InitialDurableWork: shared.DurableWork,
			SaveSettings: func(values map[string]string) error {
				return client.ApplySettings(ctx, values)
			},
			InitialScreen: initialScreen,
			ScreenAction:  client.ScreenAction,
			Diagnostic:    client.Diagnostic,
			RegisterLive:  client.RegisterLiveTUI,
		})
		return serverResult(err)
	}
	select {
	case <-ctx.Done():
		return serverResult(nil)
	case err := <-ownerResult:
		ownerFinished = true
		return serverResult(err)
	}
}

type startupConnectionStatus struct {
	output  io.Writer
	enabled bool
}

func newStartupConnectionStatus(output io.Writer, enabled bool) startupConnectionStatus {
	return startupConnectionStatus{output: output, enabled: enabled}
}

func (s startupConnectionStatus) connecting() {
	if s.enabled {
		fmt.Fprintln(s.output, "Connecting to the existing Spynel primary…")
	}
}

func (s startupConnectionStatus) connected() {
	if s.enabled {
		fmt.Fprintln(s.output, "Connected to the existing Spynel primary.")
	}
}

func (s startupConnectionStatus) failed(err error) {
	if s.enabled {
		detail := "connection failed; review the error below and retry or exit"
		if errors.Is(err, localapi.ErrForeignLoopback) {
			detail = "the workspace primary is active in another host/container environment, so its loopback API is unreachable here; stop that primary or run Spynel in the same environment, then retry"
		} else if errors.Is(err, localapi.ErrReadinessTimeout) {
			detail = "the existing primary did not become reachable within the bounded startup interval; review the error below, then retry or exit"
		}
		fmt.Fprintln(s.output, "Could not connect to the existing Spynel primary: "+detail)
	}
}

func terminalOutput(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func shouldResumeTUIHistory(hadHealthyPrimary bool, lease instance.Lease, instanceID string) bool {
	return !hadHealthyPrimary && lease.InstanceID == instanceID && lease.HandoffTo == ""
}

func selectTUIConversation(histories *history.Store, instanceID string, resumeLatest bool) (string, error) {
	if resumeLatest {
		latest, found, err := histories.Latest("tui")
		if err != nil {
			return "", err
		}
		if found {
			return latest.Conversation, nil
		}
	}
	return "local-" + instanceID, nil
}

func interactiveTerminal() bool {
	input, inputErr := os.Stdin.Stat()
	output, outputErr := os.Stdout.Stat()
	return inputErr == nil && outputErr == nil && input.Mode()&os.ModeCharDevice != 0 && output.Mode()&os.ModeCharDevice != 0
}

func runOnce(configPath, version string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if client, active, clientErr := activeWorkspaceClient(ctx, cfg); clientErr != nil {
		return clientErr
	} else if active {
		return client.RunOnce(ctx)
	}
	service, err := buildService(cfg, version)
	if err != nil {
		return err
	}
	defer service.Close()
	if err := service.Start(ctx); err != nil {
		service.Runtime.LogEvent("error", "startup", "service_start_failed", "Service startup failed")
		return err
	}
	if err := service.Orchestrator.ScanOnce(ctx); err != nil {
		return err
	}
	return service.Orchestrator.WaitForIdle(ctx)
}

func runMessageMode(configPath, conversation, text, version string, options messageRunOptions) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	text, err = addCLIAttachments(ctx, cfg, text, options.Attachments)
	if err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("message text or at least one attachment is required")
	}
	if client, active, clientErr := activeWorkspaceClient(ctx, cfg); clientErr != nil {
		return clientErr
	} else if active {
		return runMessageWithOutput(ctx, client.Handle, conversation, text, options)
	} else if options.FollowupOnly {
		return errors.New("followup requires a running Spynel server with an active execution; start `spynel serve` first")
	}
	service, err := buildService(cfg, version)
	if err != nil {
		return err
	}
	// Match server mode: the supervisor retains a harness startup error, but
	// message.received extensions still get the opportunity to answer, reject,
	// or rewrite the input. If no extension completes the operation, Handle
	// reaches the supervisor and returns the original availability failure.
	_ = service.Start(ctx)
	defer service.Close()

	return runMessageWithOutput(ctx, service.Handle, conversation, text, options)
}

func addCLIAttachments(ctx context.Context, cfg config.Config, text string, sources []string) (string, error) {
	for _, source := range sources {
		file, openErr := os.Open(source)
		if openErr != nil {
			return "", fmt.Errorf("open attachment %s: %w", filepath.Base(source), openErr)
		}
		attachment, saveErr := (media.Store{
			Directory: cfg.StatePath("attachments", "cli"),
			MaxBytes:  int64(cfg.Workspace.AttachmentMaxMB) * 1024 * 1024,
		}).Save(ctx, filepath.Base(source), file)
		closeErr := file.Close()
		if saveErr != nil {
			return "", fmt.Errorf("save attachment %s: %w", filepath.Base(source), saveErr)
		}
		if closeErr != nil {
			return "", closeErr
		}
		text = strings.TrimSpace(text + "\n\n" + attachment.Token())
	}
	return text, nil
}

func activeWorkspaceClient(ctx context.Context, cfg config.Config) (*localapi.Client, bool, error) {
	election, err := instance.New(cfg.StatePath())
	if err != nil {
		return nil, false, err
	}
	lease, err := election.Current()
	if os.IsNotExist(err) || (err == nil && election.IsStale(lease)) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	client := localapi.NewClient(election)
	if _, err := client.WaitReady(ctx); err != nil {
		return nil, false, err
	}
	return client, true, nil
}

func runMessageWithHandler(ctx context.Context, handler channel.Handler, conversation, messageText string) error {
	return runMessageWithOutput(ctx, handler, conversation, messageText, messageRunOptions{Output: os.Stdout})
}

func runMessageWithOutput(ctx context.Context, handler channel.Handler, conversation, messageText string, options messageRunOptions) error {
	output := options.Output
	if output == nil {
		output = os.Stdout
	}
	events := make(chan core.Event, 64)
	dispatched := make(chan error, 1)
	go func() {
		dispatched <- handler(ctx, core.Message{
			Channel: "cli", Conversation: conversation, Sender: "cli", Text: messageText, FollowupOnly: options.FollowupOnly,
		}, func(event core.Event) {
			select {
			case events <- event:
			case <-ctx.Done():
			}
		})
	}()

	var streamed strings.Builder
	handlerDone := false
	awaitTerminal := false
	for {
		if handlerDone && !awaitTerminal {
			// Framework hooks may intentionally cancel a message without a
			// visible reply. Drain an event already emitted before Handle
			// returned, but do not leave a script waiting forever for one.
			select {
			case event := <-events:
				if !event.Done {
					awaitTerminal = true
				}
				if done, err := writeCLIEvent(output, &streamed, event, options); done || err != nil {
					return err
				}
				continue
			default:
				return nil
			}
		}
		select {
		case err := <-dispatched:
			dispatched = nil
			if err != nil {
				return err
			}
			handlerDone = true
		case event := <-events:
			if !event.Done {
				awaitTerminal = true
			}
			if done, err := writeCLIEvent(output, &streamed, event, options); done || err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func writeCLIEvent(output io.Writer, streamed *strings.Builder, event core.Event, options messageRunOptions) (bool, error) {
	if options.JSON {
		if err := json.NewEncoder(output).Encode(event); err != nil {
			return false, err
		}
	} else if options.Stream && event.Kind == core.EventDelta {
		if _, err := fmt.Fprint(output, event.Text); err != nil {
			return false, err
		}
		streamed.WriteString(event.Text)
	}
	if !event.Done {
		return false, nil
	}
	switch event.Kind {
	case core.EventError:
		if options.Stream && streamed.Len() > 0 {
			_, _ = fmt.Fprintln(output)
		}
		if strings.TrimSpace(event.Text) == "" {
			return true, errors.New("harness turn failed")
		}
		return true, errors.New(event.Text)
	case core.EventFinal, core.EventStatus:
		if !options.JSON && event.Text != "" {
			if options.Stream && streamed.Len() > 0 {
				if suffix := strings.TrimPrefix(event.Text, streamed.String()); suffix != event.Text {
					_, _ = fmt.Fprint(output, suffix)
				} else if event.Text != streamed.String() {
					_, _ = fmt.Fprint(output, "\n"+event.Text)
				}
				_, _ = fmt.Fprintln(output)
			} else {
				text := event.Text
				if event.Kind == core.EventFinal && event.FinalText != nil {
					text = *event.FinalText
				}
				if _, err := fmt.Fprintln(output, text); err != nil {
					return false, err
				}
			}
		} else if !options.JSON && options.Stream && streamed.Len() > 0 {
			if _, err := fmt.Fprintln(output); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	return false, nil
}

func buildService(cfg config.Config, version string) (*app.Service, error) {
	if err := workspace.Upgrade(cfg.Root); err != nil {
		return nil, fmt.Errorf("upgrade Spynel workspace: %w", err)
	}
	runtimeState := app.NewRuntimeAt(cfg.StatePath("runtime", "logs"), fmt.Sprintf("pid-%d", os.Getpid()))
	registry := harness.NewBuiltinRegistry()
	command, commandErr := harness.ResolveConfiguredCommand(cfg.Harness.Name, cfg.Harness.ACPCommand, nil)
	if commandErr != nil {
		if definition, ok := harness.Lookup(cfg.Harness.Name); ok {
			command = definition.Command
		}
	}
	target := harness.NewSupervisor(registry, harness.HarnessConfig{
		Name: cfg.Harness.Name, Command: command, Args: cfg.HarnessArgs(), Cwd: cfg.Root,
		Model: cfg.Harness.Model, Effort: "medium",
		ApprovalPolicy: "never", Sandbox: cfg.Harness.Sandbox,
		Network: false, SessionsFile: cfg.HarnessSessionsPath(cfg.Harness.Name),
		Version: version, Stderr: runtimeState.Writer("harness"),
	})
	service := app.NewWithRuntime(cfg, target, runtimeState)
	service.Updates = updater.Detect(version)
	startup, err := startupmanager.New("")
	if err != nil {
		service.Runtime.LogEvent("error", "startup", "manager_failed", "Startup manager initialization failed")
		_ = service.Close()
		return nil, fmt.Errorf("initialize startup manager: %w", err)
	}
	startup.Log = service.Runtime.Writer("startup.registration")
	service.Startup = startup
	return service, nil
}

func startChannels(ctx context.Context, service *app.Service, report channel.StatusReporter) (<-chan error, error) {
	initial := service.Settings.Snapshot()
	cacheRoot, cacheErr := media.SpeechCacheDir()
	if cacheErr != nil && initial.Speech.Enabled && strings.TrimSpace(initial.Speech.ModelDir) == "" {
		return nil, cacheErr
	}
	speech := media.NewParakeet(service.Settings, cacheRoot, cacheErr, service.Runtime.Writer("media"))
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
				bot := telegram.NewWithIdentityStore(cfg.Channels.Telegram, cfg.TelegramToken(), cfg.StatePath("runtime", "telegram-identities.json"))
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
	supervisor := channel.NewSupervisor(service.Settings, service.Handle, managed, report, service.Runtime.Writer("channel"))
	supervisor.SetEventLogger(service.Runtime.LogEvent)
	service.PairingControl = supervisor
	service.DeliveryControl = supervisor
	done := make(chan error, 1)
	go func() {
		defer service.Runtime.RecoverPanic("channel", "supervisor_panic")
		err := supervisor.Run(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			service.Runtime.LogEvent("error", "channel", "supervisor_stopped", "Channel supervisor: "+err.Error())
		}
		done <- err
	}()
	return done, nil
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
		runtimeState := app.NewRuntimeAt(cfg.StatePath("runtime", "logs"), fmt.Sprintf("pid-%d-extension", os.Getpid()))
		defer runtimeState.Close()
		path, err := extensions.Install(ctx, directory, args[1], name, runtimeState.Writer("extension.install"))
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
	command, err := harness.ResolveConfiguredCommand(cfg.Harness.Name, cfg.Harness.ACPCommand, nil)
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
	fmt.Println("state directory: writable (" + cfg.StatePath() + ")")
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

const helpText = `Spynel - non-AI orchestration for one human and many coding agents

Usage:
  spynel                         Launch TUI and enabled background services
  spynel serve [--tui]           Run Telegram, WhatsApp, and the loop as a server
  spynel init [--dir DIR]        Initialize and continue into the TUI
    --no-start                   Initialize only (for scripts and automation)
  spynel send [flags] TEXT       Send or stream a message
    --config PATH                Load an explicit workspace configuration
    --conversation NAME          Reuse a durable CLI conversation (default local)
    --stream                     Print response deltas as they arrive
    --json                       Emit every response event as NDJSON
    --stdin                      Read the message body from standard input
    --attach PATH                Copy and attach a file (repeatable)
  spynel followup [flags] TEXT   Steer an active server-side CLI conversation
  spynel notify --workdir PATH (--origin O | --recent-authorized) --message TEXT
                                Queue a proactive assistant notification
  spynel conversations list     List disk-backed conversations
  spynel conversations show     Read a bounded conversation tail
  spynel conversations resume   Branch any saved conversation into CLI
  spynel status [flags]          Show workspace and current conversation status
  spynel command [flags] NAME    Run any non-visual framework slash command
  spynel tasks [flags] [VIEW]   List durable tasks (open by default)
  spynel goals [flags] [VIEW]   List durable goals (open by default)
    VIEW                        open|recent|active|review|waiting|done|failed|all
    --config PATH               Load an explicit workspace configuration
    --conversation NAME         Use a durable CLI command conversation
    --days N                    Restrict updated_at to the last N days
    --limit N                   Render 1 through 100 matching items
    --detail                    Add allowlisted durable details
    --json                      Emit the shared response event as NDJSON
  spynel job message N TEXT     Guide a live orchestrator job in place
  spynel job ping N             Request durable progress from a live job
  spynel docs [TOPIC]            Read curated offline documentation
    search QUERY [page NUMBER]   Search bounded topic sections
    --format text|json           Select plain Markdown or versioned JSON
  spynel instructions            Validate role instruction files without showing contents
  spynel jobs|log...             Other concise framework-command aliases
  spynel update                 Check npm for an update (/update install applies it)
  spynel run --once              Dispatch one orchestration scan and wait
  spynel task [--no-review] REQUEST
                                Create a task (reviewed by default)
  spynel task inspect FILE      Show the task's effective review policy
  spynel goal OBJECTIVE          Create a goal markdown file
  spynel extension ...           List, install, or remove Git extensions
  spynel whatsapp pair           Pair a WhatsApp account by QR code
  spynel config [get|set ...]    Validate config or run the shared config command
  spynel doctor                  Check local configuration and prerequisites
  spynel version                 Print the binary version
`
