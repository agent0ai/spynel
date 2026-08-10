package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/channel/tui"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
	"github.com/agent0ai/spynel/internal/instance"
	"github.com/agent0ai/spynel/internal/localapi"
	"github.com/agent0ai/spynel/internal/orchestrator"
	"github.com/agent0ai/spynel/internal/workspace"
)

type heldCLIHarness struct {
	mu      sync.Mutex
	emits   map[string]core.Emit
	prompts map[string][]string
	threads map[string]string
}

func TestRecordCommandFailurePersistsGenericEvidenceWithoutErrorContent(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	configPath := config.PathForRoot(root)
	recordCommandFailure([]string{"serve", "--config", configPath}, errors.New("authorization: Bearer must-not-persist"))
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	runtimeState := app.NewRuntimeAt(cfg.StatePath("runtime", "logs"), "command-failure-reader")
	defer runtimeState.Close()
	found := false
	for _, entry := range runtimeState.Logs() {
		found = found || entry.Component == "process" && entry.Event == "command_failed"
		if strings.Contains(entry.Text, "must-not-persist") {
			t.Fatalf("top-level error content leaked into runtime log: %#v", entry)
		}
	}
	if !found {
		t.Fatalf("command failure evidence missing: %#v", runtimeState.Logs())
	}
}

func TestConfigPathArgument(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{args: []string{"status"}, want: ""},
		{args: []string{"status", "--config", "/tmp/project/.spynel/config.yaml"}, want: "/tmp/project/.spynel/config.yaml"},
		{args: []string{"send", "--config=/tmp/other.yaml", "hello"}, want: "/tmp/other.yaml"},
	} {
		if got := configPathArgument(test.args); got != test.want {
			t.Errorf("configPathArgument(%q) = %q, want %q", test.args, got, test.want)
		}
	}
}

func TestBareWorkspaceDiscoveryDistinguishesLocalAncestorAndSymlink(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "nested", "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	discovery, err := discoverBareWorkspace(child)
	if err != nil {
		t.Fatal(err)
	}
	if !discovery.ancestorFound || discovery.parentRoot != root || discovery.discoveredConfig != config.PathForRoot(root) || discovery.localConfig != config.PathForRoot(child) {
		t.Fatalf("ancestor discovery = %#v", discovery)
	}

	if err := workspace.Init(child, false); err != nil {
		t.Fatal(err)
	}
	discovery, err = discoverBareWorkspace(child)
	if err != nil {
		t.Fatal(err)
	}
	if discovery.ancestorFound || discovery.discoveredConfig != config.PathForRoot(child) {
		t.Fatalf("local discovery = %#v", discovery)
	}

	linkParent := t.TempDir()
	link := filepath.Join(linkParent, "workspace-link")
	if err := os.Symlink(child, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	canonical, err := canonicalDirectory(link)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(child)
	if err != nil {
		t.Fatal(err)
	}
	if canonical != want {
		t.Fatalf("canonical symlink directory = %q, want %q", canonical, want)
	}
}

func TestBareInteractiveStartupOrdersChoiceDirectoryAndServerConstruction(t *testing.T) {
	const (
		launchRoot   = "/canonical/parent/child"
		parentRoot   = "/canonical/parent"
		localConfig  = "/canonical/parent/child/.spynel/config.yaml"
		parentConfig = "/canonical/parent/.spynel/config.yaml"
	)
	tests := []struct {
		name       string
		choice     tui.WorkspaceChoice
		wantEvents []string
	}{
		{
			name: "parent changes directory before server construction", choice: tui.WorkspaceChoiceUseParent,
			wantEvents: []string{"canonical", "discover:" + launchRoot, "choice:" + launchRoot + ":" + parentRoot, "chdir:" + parentRoot, "server:" + parentConfig},
		},
		{
			name: "initialize creates only launch workspace before directory and server", choice: tui.WorkspaceChoiceInitializeHere,
			wantEvents: []string{"canonical", "discover:" + launchRoot, "choice:" + launchRoot + ":" + parentRoot, "init:" + launchRoot, "chdir:" + launchRoot, "server:" + localConfig},
		},
		{
			name:       "exit has no filesystem or server effects",
			choice:     tui.WorkspaceChoiceExit,
			wantEvents: []string{"canonical", "discover:" + launchRoot, "choice:" + launchRoot + ":" + parentRoot},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var events []string
			runtime := bareInteractiveRuntime{
				canonicalLaunchDirectory: func() (string, error) {
					events = append(events, "canonical")
					return launchRoot, nil
				},
				discoverWorkspace: func(root string) (bareWorkspaceDiscovery, error) {
					events = append(events, "discover:"+root)
					return bareWorkspaceDiscovery{localConfig: localConfig, discoveredConfig: parentConfig, parentRoot: parentRoot, ancestorFound: true}, nil
				},
				runParentChoice: func(_ context.Context, launch, parent string, initialize func() error) (tui.WorkspaceChoice, error) {
					events = append(events, "choice:"+launch+":"+parent)
					if test.choice == tui.WorkspaceChoiceInitializeHere {
						if err := initialize(); err != nil {
							return tui.WorkspaceChoiceExit, err
						}
					}
					return test.choice, nil
				},
				initializeWorkspace: func(root string, force bool) error {
					if force {
						t.Fatal("startup initialization unexpectedly forced an overwrite")
					}
					events = append(events, "init:"+root)
					return nil
				},
				changeDirectory: func(root string) error {
					events = append(events, "chdir:"+root)
					return nil
				},
				startServer: func(path string, withTUI bool, version string, restartArgs []string) error {
					if !withTUI || version != "test" || restartArgs != nil {
						t.Fatalf("server arguments = %q, %t, %q, %#v", path, withTUI, version, restartArgs)
					}
					events = append(events, "server:"+path)
					return nil
				},
			}
			if err := runBareInteractiveWithRuntime("test", runtime); err != nil {
				t.Fatal(err)
			}
			if strings.Join(events, "\n") != strings.Join(test.wantEvents, "\n") {
				t.Fatalf("startup events = %#v, want %#v", events, test.wantEvents)
			}
		})
	}
}

func TestBareInteractiveStartupHandlesLocalUninitializedAndInitializationFailure(t *testing.T) {
	const launchRoot = "/canonical/new"
	const localConfig = "/canonical/new/.spynel/config.yaml"
	initFailure := errors.New("cannot initialize")
	for _, test := range []struct {
		name        string
		initialized bool
		initErr     error
		wantEvents  []string
		wantErr     error
	}{
		{name: "successful initialization", initialized: true, wantEvents: []string{"screen", "init", "chdir", "server"}},
		{name: "cancelled initialization", wantEvents: []string{"screen"}},
		{name: "failed initialization", initErr: initFailure, wantEvents: []string{"screen", "init"}, wantErr: initFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			var events []string
			runtime := bareInteractiveRuntime{
				canonicalLaunchDirectory: func() (string, error) { return launchRoot, nil },
				discoverWorkspace: func(string) (bareWorkspaceDiscovery, error) {
					return bareWorkspaceDiscovery{localConfig: localConfig}, nil
				},
				runInitialization: func(_ context.Context, root string, initialize func() error) (bool, error) {
					events = append(events, "screen")
					if test.initErr != nil || test.initialized {
						if err := initialize(); err != nil {
							return false, err
						}
					}
					return test.initialized, nil
				},
				initializeWorkspace: func(root string, force bool) error {
					events = append(events, "init")
					return test.initErr
				},
				changeDirectory: func(string) error { events = append(events, "chdir"); return nil },
				startServer:     func(string, bool, string, []string) error { events = append(events, "server"); return nil },
			}
			err := runBareInteractiveWithRuntime("test", runtime)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("startup error = %v, want %v", err, test.wantErr)
			}
			if strings.Join(events, "\n") != strings.Join(test.wantEvents, "\n") {
				t.Fatalf("startup events = %#v, want %#v", events, test.wantEvents)
			}
		})
	}
}

func TestBareInteractiveChoiceAlignsRealProcessDirectoryBeforeServer(t *testing.T) {
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })
	parent := t.TempDir()
	child := filepath.Join(parent, "child")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		choice tui.WorkspaceChoice
		want   string
	}{
		{name: "parent", choice: tui.WorkspaceChoiceUseParent, want: parent},
		{name: "initialized child", choice: tui.WorkspaceChoiceInitializeHere, want: child},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.Chdir(original); err != nil {
				t.Fatal(err)
			}
			runtime := bareInteractiveRuntime{
				canonicalLaunchDirectory: func() (string, error) { return child, nil },
				discoverWorkspace: func(string) (bareWorkspaceDiscovery, error) {
					return bareWorkspaceDiscovery{
						localConfig: config.PathForRoot(child), discoveredConfig: config.PathForRoot(parent),
						parentRoot: parent, ancestorFound: true,
					}, nil
				},
				runParentChoice: func(_ context.Context, _, _ string, initialize func() error) (tui.WorkspaceChoice, error) {
					if test.choice == tui.WorkspaceChoiceInitializeHere {
						if err := initialize(); err != nil {
							return tui.WorkspaceChoiceExit, err
						}
					}
					return test.choice, nil
				},
				initializeWorkspace: func(root string, force bool) error {
					if root != child || force {
						t.Fatalf("initialize request = %q, force %t", root, force)
					}
					return nil
				},
				changeDirectory: os.Chdir,
				startServer: func(string, bool, string, []string) error {
					got, err := os.Getwd()
					if err != nil {
						return err
					}
					if got != test.want {
						return fmt.Errorf("server CWD = %q, want %q", got, test.want)
					}
					return nil
				},
			}
			if err := runBareInteractiveWithRuntime("test", runtime); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBareInteractiveInitializedRootStartsWithoutChoice(t *testing.T) {
	const root = "/canonical/local"
	const configPath = "/canonical/local/.spynel/config.yaml"
	choiceCalled := false
	serverCalled := false
	runtime := bareInteractiveRuntime{
		canonicalLaunchDirectory: func() (string, error) { return root, nil },
		discoverWorkspace: func(string) (bareWorkspaceDiscovery, error) {
			return bareWorkspaceDiscovery{localConfig: configPath, discoveredConfig: configPath}, nil
		},
		runParentChoice: func(context.Context, string, string, func() error) (tui.WorkspaceChoice, error) {
			choiceCalled = true
			return tui.WorkspaceChoiceExit, nil
		},
		startServer: func(path string, withTUI bool, version string, restartArgs []string) error {
			serverCalled = path == configPath && withTUI && version == "test" && restartArgs == nil
			return nil
		},
	}
	if err := runBareInteractiveWithRuntime("test", runtime); err != nil {
		t.Fatal(err)
	}
	if choiceCalled || !serverCalled {
		t.Fatalf("choice called = %t, server called correctly = %t", choiceCalled, serverCalled)
	}
}

func TestOnlyBareCommandRequestsInteractiveWorkspaceChoice(t *testing.T) {
	if !bareInteractiveRequested(nil) {
		t.Fatal("bare command did not request interactive startup")
	}
	for _, args := range [][]string{
		{"serve"}, {"serve", "--tui"}, {"serve", "--config", "/explicit/.spynel/config.yaml"},
		{"init", "--dir", "/explicit"}, {"status", "--config", "/explicit/.spynel/config.yaml"},
	} {
		if bareInteractiveRequested(args) {
			t.Fatalf("explicit or noninteractive command unexpectedly requested workspace choice: %#v", args)
		}
	}
}

func TestBareFailureLoggingDoesNotAdoptAncestorBeforeChoice(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(child); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	if path, ok := failureConfigPath(nil); ok || path != "" {
		t.Fatalf("bare failure path = %q, %t; ancestor must remain untouched", path, ok)
	}
}

func TestWorkflowListAliasPreservesSharedAndListOptions(t *testing.T) {
	for _, test := range []struct {
		input []string
		want  []string
	}{
		{input: []string{"--limit", "50"}, want: []string{"open", "--limit", "50"}},
		{input: []string{"--config", "workspace.yml", "--days", "14", "--detail"}, want: []string{"--config", "workspace.yml", "open", "--days", "14", "--detail"}},
		{input: []string{"--json", "waiting", "--limit", "2"}, want: []string{"--json", "waiting", "--limit", "2"}},
		{input: []string{"review", "--days", "7"}, want: []string{"review", "--days", "7"}},
		{input: []string{"failed", "--detail"}, want: []string{"failed", "--detail"}},
	} {
		if got := workflowListAliasArgs(test.input); strings.Join(got, "\x00") != strings.Join(test.want, "\x00") {
			t.Errorf("workflowListAliasArgs(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

func TestWorkflowListAliasesAreDocumentedForExternalPrograms(t *testing.T) {
	for _, want := range []string{
		"spynel tasks [flags] [VIEW]",
		"spynel goals [flags] [VIEW]",
		"open|recent|active|review|waiting|done|failed|all",
		"--config PATH",
		"--conversation NAME",
		"--days N",
		"--limit N",
		"--detail",
		"shared response event as NDJSON",
	} {
		if !strings.Contains(helpText, want) {
			t.Fatalf("CLI help does not expose %q:\n%s", want, helpText)
		}
	}
}

func TestInstructionsCommandReportsValidationWithoutContents(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfgPath := config.PathForRoot(root)
	cfg, _ := config.Load(cfgPath)
	secret := "do-not-print-in-status"
	if err := os.WriteFile(cfg.StatePath("instructions", "agent-chat.md"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if err := runInstructionsCommand([]string{"--config", cfgPath}, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), secret) || !strings.Contains(output.String(), "chat: .spynel/instructions/agent-chat.md — valid") {
		t.Fatalf("instruction inspection output = %q", output.String())
	}
}

func TestInstructionsCommandRejectsEscapingInstructionsDirectory(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfgPath := config.PathForRoot(root)
	instructionsPath := filepath.Join(root, ".spynel", "instructions")
	outsidePath := filepath.Join(t.TempDir(), "external-instructions")
	if err := os.Rename(instructionsPath, outsidePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsidePath, instructionsPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	var output strings.Builder
	err := runInstructionsCommand([]string{"--config", cfgPath}, &output)
	if err == nil || !strings.Contains(err.Error(), "unsafe or invalid") {
		t.Fatalf("symlinked instructions command error = %v", err)
	}
	if strings.Count(output.String(), "invalid (.spynel/instructions path must not be a symbolic link)") != 5 {
		t.Fatalf("symlinked instructions command output = %q", output.String())
	}
}

func TestTaskInspectShowsEffectiveFailSafeReviewPolicy(t *testing.T) {
	for _, test := range []struct {
		name  string
		front string
		want  []string
	}{
		{name: "explicit false", front: "review_required: false\n", want: []string{"Review required: false"}},
		{name: "missing", front: "id: task\n", want: []string{"Review required: true"}},
		{name: "malformed", front: "review_required: nope\n", want: []string{"Review required: true", "Policy warning:", "treated as review required"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "task.md")
			if err := os.WriteFile(path, []byte("---\n"+test.front+"---\n# Task\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if err := inspectTaskPolicy(path, &output); err != nil {
				t.Fatal(err)
			}
			for _, want := range test.want {
				if !strings.Contains(output.String(), want) {
					t.Fatalf("inspection = %q, missing %q", output.String(), want)
				}
			}
		})
	}
}

func TestTaskInspectAppliesWorkspaceReviewMode(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Harness.Reviews = config.TaskReviewsNever
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.StatePath("tasks", "todo"), "inspect.md")
	if err := os.WriteFile(path, []byte("---\nid: task\nreview_required: true\n---\n# Task\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := inspectTaskPolicy(path, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Configured task review mode: never") || !strings.Contains(output.String(), "Review required: false") {
		t.Fatalf("inspection = %q", output.String())
	}
}

func TestNotifyCommandUsesDurableHistoryWithoutHarness(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	store := history.New(cfg.StatePath("history"))
	if _, err := store.Append("cli", "alerts", history.Entry{Role: "user", Content: "known"}); err != nil {
		t.Fatal(err)
	}
	if err := runNotifyCommand([]string{"--config", cfg.Path, "--origin", "cli/alerts", "complete"}, "test"); err != nil {
		t.Fatal(err)
	}
	entries, _, err := store.RecentEntries("cli", "alerts", 10, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[1].Sender != "Spy" || entries[1].Content != "complete" {
		t.Fatalf("history = %#v", entries)
	}
}

func TestNotificationAgentPreparedCLIActionsSendOrExplicitlyDecline(t *testing.T) {
	setup := func(t *testing.T) (config.Config, string) {
		t.Helper()
		root := t.TempDir()
		if err := workspace.Init(root, false); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(config.PathForRoot(root))
		if err != nil {
			t.Fatal(err)
		}
		store := history.New(cfg.StatePath("history"))
		if _, err := store.Append("cli", "alerts", history.Entry{Role: "user", Content: "known"}); err != nil {
			t.Fatal(err)
		}
		taskFile := filepath.Join(cfg.StatePath("tasks", "done"), "task.md")
		document := orchestrator.Document{FrontMatter: map[string]any{
			"id": "task-1", "title": "Report", "status": "done", "attempt": 1,
			"notify": map[string]any{"enabled": true, "origin": "cli/alerts", "on": []any{"done"}},
		}, Body: "# Report\n\n## Progress\n\n- Complete.\n"}
		if err := orchestrator.WriteDocument(taskFile, document); err != nil {
			t.Fatal(err)
		}
		eventDirectory := cfg.StatePath("runtime", "notification-agents")
		if err := os.MkdirAll(eventDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
		event := map[string]any{
			"id": "event-1", "task_id": "task-1", "outcome": "done", "task_file": taskFile,
			"transition": "task_implementation:1", "origin": "cli/alerts", "mode": "decide", "state": "pending",
		}
		data, _ := json.Marshal(event)
		if err := os.WriteFile(filepath.Join(eventDirectory, "event-1.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		return cfg, filepath.Join(eventDirectory, "event-1.json")
	}

	t.Run("send", func(t *testing.T) {
		cfg, eventPath := setup(t)
		args := []string{"--config", cfg.Path, "--origin", "cli/alerts", "--event-key", "task-notification:event-1", "--outcome", "done", "report ready"}
		if err := runNotifyCommand(args, "test"); err != nil {
			t.Fatal(err)
		}
		outboxID := orchestrator.NotificationOutboxID("task-notification:event-1", "done")
		if _, err := os.Stat(filepath.Join(cfg.StatePath("runtime", "outbox"), outboxID+".json")); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(eventPath)
		if strings.Contains(string(data), `"state":"declined"`) || !strings.Contains(string(data), `"journaled": true`) {
			t.Fatalf("send did not record its transition-specific journal receipt: %s", data)
		}
		document, err := orchestrator.ReadDocument(filepath.Join(cfg.StatePath("tasks", "done"), "task.md"))
		if err != nil || !strings.Contains(document.Body, "Sent the user a notification: report ready") {
			t.Fatalf("send journal = %#v, %v", document.Body, err)
		}
	})

	t.Run("decline", func(t *testing.T) {
		cfg, eventPath := setup(t)
		args := []string{"--config", cfg.Path, "--origin", "cli/alerts", "--event-key", "task-notification:event-1", "--outcome", "done", "--decline"}
		if err := runNotifyCommand(args, "test"); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(eventPath)
		if err != nil || !strings.Contains(string(data), `"state": "declined"`) {
			t.Fatalf("decline event = %s, %v", data, err)
		}
		entries, err := os.ReadDir(cfg.StatePath("runtime", "outbox"))
		if err == nil && len(entries) != 0 {
			t.Fatalf("decline queued %d outbox entries", len(entries))
		}
	})

	t.Run("failed action", func(t *testing.T) {
		cfg, eventPath := setup(t)
		args := []string{"--config", cfg.Path, "--origin", "cli/other", "--event-key", "task-notification:event-1", "--outcome", "done", "--decline"}
		if err := runNotifyCommand(args, "test"); err == nil {
			t.Fatal("redirected decline unexpectedly succeeded")
		}
		data, err := os.ReadFile(eventPath)
		if err != nil || !strings.Contains(string(data), `"state":"pending"`) {
			t.Fatalf("failed action event = %s, %v", data, err)
		}
	})
}

func newHeldCLIHarness() *heldCLIHarness {
	return &heldCLIHarness{
		emits: map[string]core.Emit{}, prompts: map[string][]string{}, threads: map[string]string{},
	}
}

func (h *heldCLIHarness) Start(context.Context) error { return nil }

func (h *heldCLIHarness) Send(_ context.Context, key, prompt string, emit core.Emit) (string, bool, error) {
	h.mu.Lock()
	previous := h.emits[key]
	steered := previous != nil
	thread := h.threads[key]
	if thread == "" {
		thread = "thread-" + key
		h.threads[key] = thread
	}
	h.prompts[key] = append(h.prompts[key], prompt)
	h.emits[key] = emit
	h.mu.Unlock()
	if previous != nil {
		previous(core.Event{Kind: core.EventStatus, Done: true, ThreadID: thread})
	}
	emit(core.Event{Kind: core.EventStatus, Text: "active", ThreadID: thread})
	return thread, steered, nil
}

func (h *heldCLIHarness) Interrupt(_ context.Context, key string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.emits[key] == nil {
		return false, nil
	}
	delete(h.emits, key)
	return true, nil
}

func (h *heldCLIHarness) ResetSession(key string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.threads, key)
	return nil
}

func (h *heldCLIHarness) ThreadID(key string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.threads[key]
}

func (h *heldCLIHarness) IsActive(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.emits[key] != nil
}

func (h *heldCLIHarness) Close() error { return nil }

func (h *heldCLIHarness) finish(key, text string) {
	h.mu.Lock()
	emit := h.emits[key]
	delete(h.emits, key)
	h.mu.Unlock()
	if emit != nil {
		emit(core.Event{Kind: core.EventFinal, Text: text, Done: true})
	}
}

func TestCompleteRunReplacesProcessForRestartRequest(t *testing.T) {
	want := []string{"serve", "--tui", "--config", "/tmp/project/.spynel/config.yaml"}
	request := &restartRequest{args: append([]string(nil), want...)}
	called := false
	err := completeRun(fmt.Errorf("server stopped: %w", request), func(args []string) error {
		called = true
		if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("restart arguments = %#v, want %#v", args, want)
		}
		args[0] = "mutated"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("restart process function was not called")
	}
	if request.args[0] != "serve" {
		t.Fatal("restart request arguments were exposed to mutation")
	}
}

func TestCompleteRunPreservesOrdinaryErrors(t *testing.T) {
	want := errors.New("failed")
	called := false
	got := completeRun(want, func([]string) error {
		called = true
		return nil
	})
	if !errors.Is(got, want) || called {
		t.Fatalf("completeRun error = %v, restart called = %t", got, called)
	}
}

func TestNPMUpdateRequestReturnsLauncherExitCodeWithoutReplacingProcess(t *testing.T) {
	called := false
	err := completeRun(&updateRequest{}, func([]string) error {
		called = true
		return nil
	})
	exit, ok := err.(interface{ ExitCode() int })
	if !ok || exit.ExitCode() != npmUpdateExitCode || called {
		t.Fatalf("update request = %T %v, restart called = %t", err, err, called)
	}
}

func TestNPMUpdateRequestPublishesInitContinuationArguments(t *testing.T) {
	placeholder, err := os.CreateTemp(os.TempDir(), "spynel-update-")
	if err != nil {
		t.Fatal(err)
	}
	statePath := placeholder.Name()
	if err := placeholder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(statePath) })
	t.Setenv("SPYNEL_NPM_UPDATE_STATE", statePath)
	want := []string{"serve", "--tui", "--config", "/tmp/project/.spynel/config.yaml"}
	request := &updateRequest{args: want}
	if code := request.ExitCode(); code != npmUpdateExitCode {
		t.Fatalf("update exit code = %d", code)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Args []string `json:"args"`
	}
	if err := json.Unmarshal(data, &state); err != nil || strings.Join(state.Args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("update state = %#v, %v", state, err)
	}
}

func TestOfflineUpdateInstallReturnsControlToNPMLauncher(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	packageRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(packageRoot, "npm", "vendor"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageRoot, "package.json"), []byte(`{"name":"spynel","version":"1.2.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageRoot, "npm", "vendor", ".installed.json"), []byte(`{"version":"1.2.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"name":"spynel","version":"1.3.0"}`))
	}))
	defer registry.Close()
	t.Setenv("SPYNEL_NPM_PACKAGE_ROOT", packageRoot)
	t.Setenv("SPYNEL_NPM_LAUNCHER_MANAGED", "1")
	t.Setenv("SPYNEL_NPM_REGISTRY_URL", registry.URL)
	var output bytes.Buffer
	err := runFrameworkMessageMode(config.PathForRoot(root), "updates", "/update install", "1.2.0", messageRunOptions{Output: &output})
	exit, ok := err.(interface{ ExitCode() int })
	if !ok || exit.ExitCode() != npmUpdateExitCode || !strings.Contains(output.String(), "Updating Spynel") {
		t.Fatalf("offline update = %T %v, output %q", err, err, output.String())
	}
}

func TestInitialConnectionStatusesReflectConfiguration(t *testing.T) {
	cfg := config.Default()
	cfg.Channels.Telegram.Enabled = true

	statuses := initialConnectionStatuses(cfg)
	if len(statuses) != 2 {
		t.Fatalf("status count = %d", len(statuses))
	}
	if statuses[0].Name != "telegram" || statuses[0].State != channel.ConnectionConnecting {
		t.Fatalf("Telegram status = %#v", statuses[0])
	}
	if statuses[1].Name != "whatsapp" || statuses[1].State != channel.ConnectionUnconfigured {
		t.Fatalf("WhatsApp status = %#v", statuses[1])
	}
}

func TestInitNoStartCreatesWorkspaceWithoutEnteringTUI(t *testing.T) {
	root := filepath.Join(t.TempDir(), "new-workspace")
	if err := Run([]string{"init", "--no-start", "--dir", root}, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(config.PathForRoot(root)); err != nil {
		t.Fatalf("initialized config: %v", err)
	}
	if !strings.Contains(helpText, "--no-start") || !strings.Contains(helpText, "continue into the TUI") {
		t.Fatalf("init continuation is not documented in help:\n%s", helpText)
	}
}

func TestSendCommandValidatesScriptableArguments(t *testing.T) {
	if err := run([]string{"send"}, "test"); err == nil || !strings.Contains(err.Error(), "usage: spynel send") {
		t.Fatalf("missing send text error = %v", err)
	}
	if err := run([]string{"send", "--conversation", "", "hello"}, "test"); err == nil || !strings.Contains(err.Error(), "cannot be empty") {
		t.Fatalf("empty conversation error = %v", err)
	}
	if !strings.Contains(helpText, "spynel send") || !strings.Contains(helpText, "--conversation") {
		t.Fatalf("send command is not documented in CLI help:\n%s", helpText)
	}
}

func TestCLIMessageSupportsStdinStreamingAndJSONEvents(t *testing.T) {
	text, err := cliMessageText(nil, true, strings.NewReader("first line\nsecond line\n"))
	if err != nil || text != "first line\nsecond line" {
		t.Fatalf("stdin message = %q, %v", text, err)
	}
	if _, err := cliMessageText([]string{"body"}, true, strings.NewReader("stdin")); err == nil {
		t.Fatal("CLI accepted both positional text and --stdin")
	}
	if _, err := cliMessageText(nil, true, strings.NewReader(strings.Repeat("x", maxCLIInputBytes+1))); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized stdin error = %v", err)
	}

	handler := func(_ context.Context, message core.Message, emit core.Emit) error {
		if message.Conversation != "stream-test" || !message.FollowupOnly {
			t.Fatalf("message = %#v", message)
		}
		emit(core.Event{Kind: core.EventStatus, Text: "steered"})
		emit(core.Event{Kind: core.EventDelta, Text: "routine preamble\nhello "})
		emit(core.Event{Kind: core.EventDelta, Text: "world"})
		finalText := "hello world"
		emit(core.Event{Kind: core.EventFinal, Text: "routine preamble\nhello world", FinalText: &finalText, Done: true})
		return nil
	}
	var finalOnly bytes.Buffer
	if err := runMessageWithOutput(context.Background(), handler, "stream-test", "follow", messageRunOptions{FollowupOnly: true, Output: &finalOnly}); err != nil {
		t.Fatal(err)
	}
	if finalOnly.String() != "hello world\n" {
		t.Fatalf("final-only output = %q", finalOnly.String())
	}
	var streamed bytes.Buffer
	if err := runMessageWithOutput(context.Background(), handler, "stream-test", "follow", messageRunOptions{Stream: true, FollowupOnly: true, Output: &streamed}); err != nil {
		t.Fatal(err)
	}
	if streamed.String() != "routine preamble\nhello world\n" {
		t.Fatalf("streamed output = %q", streamed.String())
	}

	var events bytes.Buffer
	if err := runMessageWithOutput(context.Background(), handler, "stream-test", "follow", messageRunOptions{JSON: true, FollowupOnly: true, Output: &events}); err != nil {
		t.Fatal(err)
	}
	var decoded []core.Event
	for _, line := range strings.Split(strings.TrimSpace(events.String()), "\n") {
		var event core.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		decoded = append(decoded, event)
	}
	if len(decoded) != 4 || decoded[1].Kind != core.EventDelta || decoded[3].Kind != core.EventFinal || !decoded[3].Done || decoded[3].FinalText == nil || *decoded[3].FinalText != "hello world" {
		t.Fatalf("NDJSON events = %#v", decoded)
	}
}

func TestCLIMessageCopiesRepeatableAttachmentsIntoWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(source, []byte("attachment body"), 0o600); err != nil {
		t.Fatal(err)
	}
	text, err := addCLIAttachments(context.Background(), cfg, "inspect this", []string{source})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "inspect this\n\n[Attachment notes.txt]") || !strings.Contains(text, filepath.ToSlash(cfg.StatePath("attachments", "cli"))) {
		t.Fatalf("attachment message = %q", text)
	}
	matches, err := filepath.Glob(cfg.StatePath("attachments", "cli", "notes*.txt"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("copied attachments = %#v, %v", matches, err)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil || string(data) != "attachment body" {
		t.Fatalf("copied attachment = %q, %v", data, err)
	}
}

func TestConversationCLIListsShowsAndBranchesDiskBackedHistory(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	store := history.New(cfg.StatePath("history"))
	if _, err := store.Append("telegram", "TG-42", history.Entry{At: time.Now().UTC(), Role: "user", Sender: "alice", Content: "inspect this"}); err != nil {
		t.Fatal(err)
	}

	var listed bytes.Buffer
	if err := listCLIConversations([]string{"--config", cfg.Path, "--json"}, &listed); err != nil {
		t.Fatal(err)
	}
	var records []conversationListRecord
	if err := json.Unmarshal(listed.Bytes(), &records); err != nil || len(records) != 1 || records[0].Channel != "telegram" || records[0].Conversation != "TG-42" {
		t.Fatalf("conversation list = %#v, %v", records, err)
	}

	var shown bytes.Buffer
	if err := showCLIConversation([]string{"--config", cfg.Path, "--json", "telegram", "TG-42"}, &shown); err != nil {
		t.Fatal(err)
	}
	var tail conversationTail
	if err := json.Unmarshal(shown.Bytes(), &tail); err != nil || len(tail.Entries) != 1 || tail.Entries[0].Content != "inspect this" {
		t.Fatalf("conversation tail = %#v, %v", tail, err)
	}

	var resumed bytes.Buffer
	if err := resumeCLIConversation([]string{"--config", cfg.Path, "--json", "telegram", "TG-42"}, &resumed); err != nil {
		t.Fatal(err)
	}
	var branch conversationBranch
	if err := json.Unmarshal(resumed.Bytes(), &branch); err != nil || branch.Channel != "cli" || !strings.HasPrefix(branch.Conversation, "resume-") {
		t.Fatalf("conversation branch = %#v, %v", branch, err)
	}
	entries, _, err := store.Entries("cli", branch.Conversation)
	if err != nil || len(entries) != 1 || entries[0].Content != "inspect this" {
		t.Fatalf("branched entries = %#v, %v", entries, err)
	}
}

func TestStatusCLIEmitsStructuredNonSecretState(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	waitingPath := filepath.Join(root, ".spynel", "tasks", "waiting", "waiting.md")
	if err := os.WriteFile(waitingPath, []byte("---\nid: waiting\nstatus: waiting\n---\n# Waiting\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runStatusCLICommand([]string{"--config", cfg.Path, "--conversation", "automation", "--json"}, "test", &output); err != nil {
		t.Fatal(err)
	}
	var status app.StatusSnapshot
	if err := json.Unmarshal(output.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Title == "" || status.Sandbox == "" || status.HarnessState == "" || len(status.Connections) != 2 {
		t.Fatalf("CLI status = %#v", status)
	}
	if status.TasksActive != 1 || status.TasksWaiting != 1 {
		t.Fatalf("CLI durable waiting count = active %d waiting %d", status.TasksActive, status.TasksWaiting)
	}
	if strings.Contains(output.String(), "token") {
		t.Fatalf("CLI status exposed configuration secrets: %s", output.String())
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(output.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["theme"]; ok {
		t.Fatalf("structured CLI status retained theme: %s", output.String())
	}
	if _, ok := fields["thread"]; ok {
		t.Fatalf("structured CLI status retained thread: %s", output.String())
	}
	for _, field := range []string{"tasks_active", "tasks_waiting", "goals_active", "heartbeat_state"} {
		if _, ok := fields[field]; !ok {
			t.Fatalf("structured CLI status is missing %q: %s", field, output.String())
		}
	}
}

func TestOfflineFrameworkCommandDoesNotRequireStartedHarness(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runFrameworkMessageMode(config.PathForRoot(root), "framework", "/help commands", "test", messageRunOptions{Output: &output}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "/status") || !strings.Contains(output.String(), "/extension list") {
		t.Fatalf("framework command output = %q", output.String())
	}
}

func TestOfflineCLIExtensionCanHandleMessageWithoutAHarness(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell extension fixture")
	}
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Harness.Name = ""
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	extension := filepath.Join(cfg.Resolve(cfg.Extensions.Directory), "offline-tool")
	if err := os.MkdirAll(extension, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "name: offline-tool\nhooks:\n  message.received: [\"./hook.sh\"]\n"
	script := "#!/bin/sh\nIFS= read -r input\ncase \"$input\" in\n  *\"handle offline\"*) printf '%s\\n' '{\"cancel\":true,\"message\":\"handled without harness\"}' ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(extension, ".spynel-extension.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extension, "hook.sh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := runMessageMode(cfg.Path, "offline", "handle offline", "test", messageRunOptions{Output: &output}); err != nil {
		t.Fatal(err)
	}
	if output.String() != "handled without harness\n" {
		t.Fatalf("offline extension output = %q", output.String())
	}
	if err := runMessageMode(cfg.Path, "offline", "requires a harness", "test", messageRunOptions{Output: &bytes.Buffer{}}); err == nil || !strings.Contains(err.Error(), "harness unavailable") {
		t.Fatalf("unhandled offline message error = %v", err)
	}
}

func TestCLIJoinsWorkspaceOwnerAndStrictlySteersActiveConversation(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		extension := filepath.Join(cfg.Resolve(cfg.Extensions.Directory), "cli-tool")
		if err := os.MkdirAll(extension, 0o700); err != nil {
			t.Fatal(err)
		}
		manifest := "name: cli-tool\nhooks:\n  message.received: [\"./hook.sh\"]\n"
		script := "#!/bin/sh\ninput=$(cat)\ncase \"$input\" in\n  *\"invoke custom tool\"*) printf '%s\\n' '{\"cancel\":true,\"message\":\"extension handled CLI\"}' ;;\nesac\n"
		if err := os.WriteFile(filepath.Join(extension, ".spynel-extension.yaml"), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(extension, "hook.sh"), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	target := newHeldCLIHarness()
	service := app.New(cfg, target)
	election, err := instance.New(cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	service.SetPrimaryInstanceID(election.ID())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	token, err := election.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, acquired, err := election.TryAcquire(listener.Addr().String(), token); err != nil || !acquired {
		t.Fatalf("acquire workspace owner = %t, %v", acquired, err)
	}
	serverContext, stopServer := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- (&localapi.Server{Service: service, Token: token}).Serve(serverContext, listener)
	}()
	defer func() {
		stopServer()
		if err := <-serverDone; err != nil {
			t.Errorf("local API: %v", err)
		}
		_ = election.Release(token)
	}()
	readyContext, stopWaiting := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopWaiting()
	if _, err := localapi.NewClient(election).WaitReady(readyContext); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		var toolOutput bytes.Buffer
		if err := runMessageMode(cfg.Path, "tool", "invoke custom tool", "test", messageRunOptions{Output: &toolOutput}); err != nil {
			t.Fatal(err)
		}
		if toolOutput.String() != "extension handled CLI\n" || target.IsActive("chat:cli:tool") {
			t.Fatalf("CLI extension output = %q, harness active = %t", toolOutput.String(), target.IsActive("chat:cli:tool"))
		}
	}

	var firstOutput bytes.Buffer
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- runMessageMode(cfg.Path, "active", "start the work", "test", messageRunOptions{Stream: true, Output: &firstOutput})
	}()
	key := "chat:cli:active"
	deadline := time.Now().Add(5 * time.Second)
	for !target.IsActive(key) {
		if time.Now().After(deadline) {
			t.Fatal("initial CLI turn did not become active through the owner")
		}
		time.Sleep(5 * time.Millisecond)
	}
	var jobsOutput bytes.Buffer
	if err := runFrameworkMessageMode(cfg.Path, "inspect", "/jobs", "test", messageRunOptions{Output: &jobsOutput}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jobsOutput.String(), "1▶ · running · cli/active") || strings.Contains(jobsOutput.String(), "↻") || !strings.Contains(jobsOutput.String(), "Use `/job info <number>`") || !strings.Contains(jobsOutput.String(), "Use `/job kill <number>`") {
		t.Fatalf("plain CLI live jobs output = %q", jobsOutput.String())
	}
	jobs := service.Runtime.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("owner jobs = %#v", jobs)
	}
	var infoOutput bytes.Buffer
	if err := runFrameworkMessageMode(cfg.Path, "inspect", fmt.Sprintf("/job info %d", jobs[0].ID), "test", messageRunOptions{Output: &infoOutput}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(infoOutput.String(), "Provider steps (▶): 1 (live conversation)") || strings.Contains(infoOutput.String(), "Implementation attempts") {
		t.Fatalf("plain CLI live job info = %q", infoOutput.String())
	}

	var followupOutput bytes.Buffer
	followupDone := make(chan error, 1)
	go func() {
		followupDone <- runMessageMode(cfg.Path, "active", "focus on the API", "test", messageRunOptions{FollowupOnly: true, Stream: true, Output: &followupOutput})
	}()
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follow-up did not release the first CLI response")
	}
	target.mu.Lock()
	prompts := append([]string(nil), target.prompts[key]...)
	target.mu.Unlock()
	if len(prompts) != 2 || !strings.Contains(prompts[1], "focus on the API") {
		t.Fatalf("owner harness prompts = %#v", prompts)
	}
	target.finish(key, "follow-up complete")
	select {
	case err := <-followupDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follow-up CLI did not receive the final response")
	}
	if firstOutput.String() != "" || followupOutput.String() != "follow-up complete\n" {
		t.Fatalf("CLI outputs = first %q, follow-up %q", firstOutput.String(), followupOutput.String())
	}
	if err := runMessageMode(cfg.Path, "active", "too late", "test", messageRunOptions{FollowupOnly: true, Output: &bytes.Buffer{}}); err == nil || !strings.Contains(err.Error(), "no active execution") {
		t.Fatalf("inactive follow-up error = %v", err)
	}
}

func TestRunMessageCompletesWhenFollowUpReleasesItsEmitter(t *testing.T) {
	handler := func(_ context.Context, _ core.Message, emit core.Emit) error {
		emit(core.Event{Kind: core.EventStatus, Done: true})
		return nil
	}
	if err := runMessageWithHandler(context.Background(), handler, "follow-up", "message"); err != nil {
		t.Fatal(err)
	}
}

func TestRunMessageCompletesWhenExtensionCancelsWithoutReply(t *testing.T) {
	handler := func(context.Context, core.Message, core.Emit) error { return nil }
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runMessageWithOutput(ctx, handler, "cancelled", "message", messageRunOptions{Output: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
}

func TestBuildServiceUsesConfiguredHarnessSandbox(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Harness.Sandbox = "read-only"
	service, err := buildService(cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	runtimeHarness, ok := service.Harness.(interface {
		HarnessConfig() harness.HarnessConfig
	})
	if !ok || runtimeHarness.HarnessConfig().Sandbox != "read-only" {
		t.Fatalf("runtime sandbox = %#v, configurable = %t", runtimeHarness, ok)
	}
}

func TestTUIStartupResumesHistoryOnlyForInitialElectionWinner(t *testing.T) {
	store := history.New(t.TempDir())
	if _, err := store.Append("tui", "local-old", history.Entry{At: time.Now().Add(-time.Hour), Role: "user", Content: "old"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("tui", "local-latest", history.Entry{At: time.Now(), Role: "assistant", Content: "latest"}); err != nil {
		t.Fatal(err)
	}
	ownerLease := instance.Lease{InstanceID: "winner"}
	if !shouldResumeTUIHistory(false, ownerLease, "winner") {
		t.Fatal("initial election winner did not qualify to resume TUI history")
	}
	for _, test := range []struct {
		name       string
		hadPrimary bool
		lease      instance.Lease
	}{
		{name: "existing owner", hadPrimary: true, lease: ownerLease},
		{name: "initial race loser", lease: instance.Lease{InstanceID: "other"}},
		{name: "handoff record", lease: instance.Lease{InstanceID: "winner", HandoffTo: "target"}},
	} {
		if shouldResumeTUIHistory(test.hadPrimary, test.lease, "winner") {
			t.Fatalf("%s unexpectedly qualified to resume TUI history", test.name)
		}
	}
	conversation, err := selectTUIConversation(store, "winner", true)
	if err != nil || conversation != "local-latest" {
		t.Fatalf("resumed conversation = %q, %v", conversation, err)
	}
	conversation, err = selectTUIConversation(store, "secondary", false)
	if err != nil || conversation != "local-secondary" {
		t.Fatalf("secondary conversation = %q, %v", conversation, err)
	}
}

func TestOwnerElectionRunsOneServerAndHandsOffOnExit(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.Root = root
	cfg.Path = config.PathForRoot(root)
	cfg.Harness.Name = ""
	cfg.Channels.Telegram.Enabled = false
	cfg.Channels.WhatsApp.Enabled = false
	cfg.Orchestrator.Enabled = false
	cfg.Extensions.Enabled = false
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	first, err := instance.New(cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	second, err := instance.New(cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	firstContext, stopFirst := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() { firstDone <- runOwnerElection(firstContext, cfg, "test", first, func() {}, func() {}) }()
	waitContext, stopWaiting := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopWaiting()
	client := localapi.NewClient(first)
	if _, err := client.WaitReady(waitContext); err != nil {
		stopFirst()
		t.Fatal(err)
	}
	lease, err := first.Current()
	if err != nil || lease.InstanceID != first.ID() {
		stopFirst()
		t.Fatalf("first lease = %#v, %v", lease, err)
	}

	secondContext, stopSecond := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() { secondDone <- runOwnerElection(secondContext, cfg, "test", second, func() {}, func() {}) }()
	time.Sleep(2 * instance.RetryInterval)
	lease, err = second.Current()
	if err != nil || lease.InstanceID != first.ID() {
		stopFirst()
		stopSecond()
		t.Fatalf("second contender displaced healthy owner: %#v, %v", lease, err)
	}
	updated := cfg
	updated.Channels.TUI.Title = "Reloaded by successor"
	if err := config.Save(updated); err != nil {
		stopFirst()
		stopSecond()
		t.Fatal(err)
	}

	stopFirst()
	if err := <-firstDone; err != nil {
		stopSecond()
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		lease, err = second.Current()
		if err == nil && lease.InstanceID == second.ID() {
			break
		}
		if time.Now().After(deadline) {
			stopSecond()
			t.Fatalf("secondary did not take over: %#v, %v", lease, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	state, err := client.WaitReady(waitContext)
	if err != nil {
		stopSecond()
		t.Fatal(err)
	}
	if state.Title != "Reloaded by successor" {
		stopSecond()
		t.Fatalf("successor used stale startup config: %#v", state)
	}
	stopSecond()
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func TestOwnerElectionPromotesAfterObservedPrimaryBecomesStale(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.Root = root
	cfg.Path = config.PathForRoot(root)
	cfg.Harness.Name = ""
	cfg.Channels.Telegram.Enabled = false
	cfg.Channels.WhatsApp.Enabled = false
	cfg.Orchestrator.Enabled = false
	cfg.Extensions.Enabled = false
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	stateDirectory := cfg.StatePath()
	if err := os.MkdirAll(filepath.Join(stateDirectory, "runtime"), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	stalled := instance.Lease{
		InstanceID:  "stalled-primary",
		PID:         1,
		Endpoint:    "127.0.0.1:1",
		Token:       "stalled-primary-token",
		StartedAt:   now.Add(-time.Hour),
		HeartbeatAt: now.Add(-instance.StaleAfter + 250*time.Millisecond),
	}
	data, err := json.Marshal(stalled)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDirectory, "runtime", "primary.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	contender, err := instance.New(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runOwnerElection(ctx, cfg, "test", contender, func() {}, func() {}) }()

	waitContext, stopWaiting := context.WithTimeout(context.Background(), 6*time.Second)
	defer stopWaiting()
	if _, err := localapi.NewClient(contender).WaitReady(waitContext); err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}
	lease, err := contender.Current()
	if err != nil || lease.InstanceID != contender.ID() {
		cancel()
		<-done
		t.Fatalf("promoted lease = %#v, %v", lease, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPrimaryCommandHandsOwnershipToRequestingTUI(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.Root = root
	cfg.Path = config.PathForRoot(root)
	cfg.Harness.Name = ""
	cfg.Channels.Telegram.Enabled = false
	cfg.Channels.WhatsApp.Enabled = false
	cfg.Orchestrator.Enabled = false
	cfg.Extensions.Enabled = false
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	first, err := instance.New(cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	second, err := instance.New(cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	firstContext, stopFirst := context.WithCancel(context.Background())
	secondContext, stopSecond := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() { firstDone <- runOwnerElection(firstContext, cfg, "test", first, func() {}, func() {}) }()
	waitContext, stopWaiting := context.WithTimeout(context.Background(), 8*time.Second)
	defer stopWaiting()
	if _, err := localapi.NewClient(first).WaitReady(waitContext); err != nil {
		stopFirst()
		t.Fatal(err)
	}
	go func() { secondDone <- runOwnerElection(secondContext, cfg, "test", second, func() {}, func() {}) }()
	defer func() {
		stopFirst()
		stopSecond()
		if err := <-firstDone; err != nil {
			t.Errorf("first election: %v", err)
		}
		if err := <-secondDone; err != nil {
			t.Errorf("second election: %v", err)
		}
	}()
	client := localapi.NewClient(second)
	var response core.Event
	if err := client.Handle(waitContext, core.Message{Channel: "tui", Conversation: "local-" + second.ID(), Text: "/primary"}, func(event core.Event) {
		if event.Kind == core.EventFinal {
			response = event
		}
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Text, "Primary handoff requested") {
		t.Fatalf("primary response = %#v", response)
	}
	for {
		lease, err := second.Current()
		if err == nil && lease.InstanceID == second.ID() && lease.HandoffTo == "" {
			break
		}
		select {
		case <-waitContext.Done():
			t.Fatalf("requesting TUI did not become primary: lease %#v, error %v", lease, err)
		case <-time.After(25 * time.Millisecond):
		}
	}
	if _, err := client.WaitReady(waitContext); err != nil {
		t.Fatalf("new primary API did not become ready: %v", err)
	}
}

func TestPromotionRecordsConfigurationReloadFailure(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.Root = root
	cfg.Path = config.PathForRoot(root)
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	election, err := instance.New(cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.WriteFile(cfg.Path, []byte("invalid: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	term, startErr := startPrimaryTerm(context.Background(), cfg, "test", election, listener, "unused", func() {}, func() {})
	if term != nil || startErr == nil {
		t.Fatalf("promotion with invalid config = %#v, %v", term, startErr)
	}
	runtimeState := app.NewRuntimeAt(cfg.StatePath("runtime", "logs"), "reload-reader")
	defer runtimeState.Close()
	found := false
	for _, entry := range runtimeState.Logs() {
		found = found || entry.Component == "config" && entry.Event == "reload_failed"
	}
	if !found {
		t.Fatalf("configuration reload failure evidence missing: %#v", runtimeState.Logs())
	}
}
