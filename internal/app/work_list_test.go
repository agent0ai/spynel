package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/orchestrator"
	"github.com/agent0ai/spynel/internal/workspace"
)

func TestTaskAndGoalCommandsDefaultToOpenAndOfferSemanticViews(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	writeWorkflowListFixture(t, cfg, "tasks", "working", "working.md", map[string]any{
		"id": "task-working", "title": "Implement *list* commands", "status": "working", "review_required": true,
		"created_at": now.Add(-2 * time.Hour).Format(time.RFC3339), "updated_at": now.Add(-time.Hour).Format(time.RFC3339),
		"provider_iterations": 4, "attempt": 2,
	}, "## Progress\n\n- Earlier step.\n- Implement filters and tests.\n")
	writeWorkflowListFixture(t, cfg, "tasks", "done", "done.md", map[string]any{
		"id": "task-done", "title": "Document listings", "status": "done", "review_required": true,
		"created_at": now.Add(-72 * time.Hour).Format(time.RFC3339), "updated_at": now.Add(-48 * time.Hour).Format(time.RFC3339),
		"attempt": 1, "review_attempt": 1,
	}, "## Progress\n\n- Accepted by independent review.\n")
	writeWorkflowListFixture(t, cfg, "tasks", "waiting", "waiting.md", map[string]any{
		"id": "task-waiting", "title": "Wait for production access", "status": "waiting", "review_required": true,
		"created_at": now.Add(-120 * time.Hour).Format(time.RFC3339), "updated_at": now.Add(-96 * time.Hour).Format(time.RFC3339),
		"waiting_for": "Production credentials", "wake_at": now.Add(24 * time.Hour).Format(time.RFC3339),
	}, "# Waiting\n")
	writeWorkflowListFixture(t, cfg, "goals", "active", "active.md", map[string]any{
		"id": "goal-active", "title": "Keep releases healthy", "status": "active",
		"created_at": now.Add(-240 * time.Hour).Format(time.RFC3339), "updated_at": now.Add(-192 * time.Hour).Format(time.RFC3339),
		"round": 2, "round_task_ids": []string{"task-a", "task-b"}, "provider_iterations": 5, "planning_attempt": 2,
	}, "## Progress\n\n- Round two tasks are running.\n")
	writeWorkflowListFixture(t, cfg, "goals", "done", "done.md", map[string]any{
		"id": "goal-done", "title": "Ship version one", "status": "done",
		"created_at": now.Add(-300 * time.Hour).Format(time.RFC3339), "updated_at": now.Add(-144 * time.Hour).Format(time.RFC3339),
		"round": 3, "review_attempt": 1,
	}, "## Progress\n\n- All criteria verified.\n")

	target := newServiceHarness()
	service := New(cfg, target)
	openTasks := runJobCommand(t, service, "/tasks")
	for _, want := range []string{"# Tasks · open", "Implement \\*list\\* commands", "working · updated", "4▶", "2↻", "step: Implement filters and tests.", "Wait for production access", "Showing 2 tasks.", "Run `/tasks recent`, `/tasks active`, `/tasks review`", "`/tasks failed`, `/tasks all` for other views", "Add `--days N`, `--limit N`, or `--detail`."} {
		if !strings.Contains(openTasks, want) {
			t.Fatalf("default open task output missing %q:\n%s", want, openTasks)
		}
	}
	if strings.Contains(openTasks, "Document listings") {
		t.Fatalf("terminal task appeared in default open view:\n%s", openTasks)
	}

	explicitOpenTasks := runJobCommand(t, service, "/tasks open")
	if !strings.Contains(explicitOpenTasks, "Implement \\*list\\* commands") || !strings.Contains(explicitOpenTasks, "Wait for production access") || strings.Contains(explicitOpenTasks, "Document listings") {
		t.Fatalf("explicit open task view did not select all nonterminal work:\n%s", explicitOpenTasks)
	}
	recentTasks := runJobCommand(t, service, "/tasks recent")
	if !strings.Contains(recentTasks, "# Tasks · recent 3d") || !strings.Contains(recentTasks, "Implement \\*list\\* commands") || !strings.Contains(recentTasks, "Document listings") || strings.Contains(recentTasks, "Wait for production access") {
		t.Fatalf("recent task view did not apply its activity window:\n%s", recentTasks)
	}
	allTasks := runJobCommand(t, service, "/tasks all --limit 2")
	if !strings.Contains(allTasks, "Implement \\*list\\* commands") || !strings.Contains(allTasks, "Document listings") || strings.Contains(allTasks, "Wait for production access") || !strings.Contains(allTasks, "Showing 2 of 3 matching tasks.") {
		t.Fatalf("all task view did not sort and limit the complete inventory:\n%s", allTasks)
	}
	doneTasks := runJobCommand(t, service, "/tasks done")
	if !strings.Contains(doneTasks, "# Tasks · done") || !strings.Contains(doneTasks, "Document listings") || strings.Contains(doneTasks, "Implement \\*list\\* commands") || strings.Contains(doneTasks, "Wait for production access") {
		t.Fatalf("done task view did not isolate successful terminal work:\n%s", doneTasks)
	}
	waitingDetails := runJobCommand(t, service, "/tasks waiting --detail --limit 1")
	for _, want := range []string{"# Tasks · waiting", "Production credentials", "wake in", "ID task-waiting", "file waiting.md", "review required", now.Add(-96 * time.Hour).Format(time.RFC3339)} {
		if !strings.Contains(waitingDetails, want) {
			t.Fatalf("detailed semantic view output missing %q:\n%s", want, waitingDetails)
		}
	}
	windowedTasks := runJobCommand(t, service, "/tasks recent --days 5")
	if !strings.Contains(windowedTasks, "Wait for production access") || !strings.Contains(windowedTasks, "# Tasks · recent 5d") {
		t.Fatalf("custom recent window failed:\n%s", windowedTasks)
	}

	openGoals := runJobCommand(t, service, "/goals")
	if !strings.Contains(openGoals, "Keep releases healthy") || strings.Contains(openGoals, "Ship version one") || !strings.Contains(openGoals, "# Goals · open") || !strings.Contains(openGoals, "Run `/goals recent`, `/goals active`, `/goals review`") {
		t.Fatalf("goal default open view failed:\n%s", openGoals)
	}
	recentGoals := runJobCommand(t, service, "/goals recent")
	if !strings.Contains(recentGoals, "Ship version one") || strings.Contains(recentGoals, "Keep releases healthy") || !strings.Contains(recentGoals, "# Goals · recent 7d") {
		t.Fatalf("goal recent window failed:\n%s", recentGoals)
	}
	activeGoals := runJobCommand(t, service, "/goals active")
	for _, want := range []string{"# Goals · active", "Keep releases healthy", "round 2", "2 tasks", "5▶", "plan 2", "step: Round two tasks are running."} {
		if !strings.Contains(activeGoals, want) {
			t.Fatalf("active goal status output missing %q:\n%s", want, activeGoals)
		}
	}
	if output := runJobCommand(t, service, "/goals open --days 9"); !strings.Contains(output, "Keep releases healthy") || strings.Contains(output, "Ship version one") {
		t.Fatalf("windowed open goal view failed:\n%s", output)
	}
	if output := runJobCommand(t, service, "/tasks --days 0"); !strings.Contains(output, "days must be a whole number from 1") || !strings.Contains(output, "Usage: /tasks") {
		t.Fatalf("invalid options did not return bounded usage:\n%s", output)
	}
	if output := runJobCommand(t, service, "/tasks --status waiting"); !strings.Contains(output, "Unknown option --status") || !strings.Contains(output, "open|recent|active|review|waiting|done|failed|all") {
		t.Fatalf("precise status filtering was not rejected with semantic-view usage:\n%s", output)
	}
	if len(target.prompts) != 0 {
		t.Fatalf("deterministic list commands invoked the harness: %#v", target.prompts)
	}
}

func TestWorkflowSemanticViewsGroupLifecycleStatuses(t *testing.T) {
	for _, test := range []struct {
		kind   string
		view   string
		status string
		want   bool
	}{
		{kind: "tasks", view: "open", status: "reviewing", want: true},
		{kind: "tasks", view: "open", status: "cancelled", want: false},
		{kind: "tasks", view: "active", status: "todo", want: true},
		{kind: "tasks", view: "active", status: "working", want: true},
		{kind: "tasks", view: "active", status: "review", want: false},
		{kind: "tasks", view: "review", status: "review", want: true},
		{kind: "tasks", view: "review", status: "reviewing", want: true},
		{kind: "tasks", view: "failed", status: "failed", want: true},
		{kind: "tasks", view: "failed", status: "cancelled", want: true},
		{kind: "tasks", view: "failed", status: "done", want: false},
		{kind: "goals", view: "active", status: "proposed", want: true},
		{kind: "goals", view: "active", status: "planning", want: true},
		{kind: "goals", view: "active", status: "active", want: true},
		{kind: "goals", view: "review", status: "reviewing", want: true},
		{kind: "goals", view: "failed", status: "abandoned", want: true},
		{kind: "goals", view: "failed", status: "done", want: false},
	} {
		t.Run(test.kind+"/"+test.view+"/"+test.status, func(t *testing.T) {
			got := workflowItemMatchesView(test.kind, test.view, test.status, workflowTerminalStatuses(test.kind))
			if got != test.want {
				t.Fatalf("workflowItemMatchesView(%q, %q, %q) = %v, want %v", test.kind, test.view, test.status, got, test.want)
			}
		})
	}
}

func TestWorkflowListCommandsUseSharedApplicationContractAcrossChannels(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	target := newServiceHarness()
	service := New(cfg, target)
	for _, test := range []struct {
		channel      string
		conversation string
		command      string
		want         string
	}{
		{channel: "cli", conversation: "automation", command: "/tasks failed --limit 1", want: "# Tasks · failed"},
		{channel: "telegram", conversation: "TG-7", command: "/tasks@spynel_bot recent --days 2", want: "# Tasks · recent 2d"},
		{channel: "whatsapp", conversation: "WA-15557654321", command: "/goals review --detail", want: "# Goals · review"},
	} {
		t.Run(test.channel, func(t *testing.T) {
			var response core.Event
			err := service.Handle(context.Background(), core.Message{Channel: test.channel, Conversation: test.conversation, Text: test.command}, func(event core.Event) {
				if event.Kind == core.EventFinal {
					response = event
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			if !response.Done || !response.Local || !strings.Contains(response.Text, test.want) {
				t.Fatalf("shared %s response = %#v, want %q", test.channel, response, test.want)
			}
		})
	}
	if len(target.prompts) != 0 {
		t.Fatalf("shared workflow listing invoked the harness: %#v", target.prompts)
	}
}

func TestTaskAndGoalListingsIgnoreConversationAndNotificationOrigin(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	// Keep the relative-age field stable while the interfaces are invoked
	// sequentially; production formatting clamps future skew to zero.
	now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second).Format(time.RFC3339)
	writeWorkflowListFixture(t, cfg, "tasks", "waiting", "global-task.md", map[string]any{
		"id": "global-task", "title": "Global task", "status": "waiting", "review_required": true,
		"created_at": now, "updated_at": now,
		"notify": map[string]any{"enabled": true, "origin": "telegram/TG-private", "on": []any{"waiting"}},
	}, "## Progress\n\n- Waiting for input.\n")
	writeWorkflowListFixture(t, cfg, "goals", "active", "global-goal.md", map[string]any{
		"id": "global-goal", "title": "Global goal", "status": "active", "created_at": now, "updated_at": now,
		"notify": map[string]any{"enabled": true, "origin": "whatsapp/WA-private", "on": []any{"done"}},
	}, "## Progress\n\n- Round is active.\n")
	service := New(cfg, newServiceHarness())
	callers := []core.Message{
		{Channel: "tui", Conversation: "local"},
		{Channel: "telegram", Conversation: "TG-other"},
		{Channel: "whatsapp", Conversation: "WA-other"},
		{Channel: "cli", Conversation: "automation"},
	}
	for _, command := range []string{"/tasks all --detail", "/goals all --detail"} {
		var expected string
		for _, caller := range callers {
			caller.Text = command
			var response core.Event
			if err := service.Handle(context.Background(), caller, func(event core.Event) { response = event }); err != nil {
				t.Fatal(err)
			}
			if expected == "" {
				expected = response.Text
			} else if response.Text != expected {
				t.Fatalf("%s %s listing differs from global workspace output:\n%s\n--- want ---\n%s", caller.Channel, command, response.Text, expected)
			}
		}
		for _, private := range []string{"TG-private", "WA-private"} {
			if strings.Contains(expected, private) {
				t.Fatalf("%s exposed notification routing metadata %q:\n%s", command, private, expected)
			}
		}
	}
}

func TestWorkflowListShowsBoundedUnavailableDocumentWarnings(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	path := cfg.StatePath("tasks", "working", "bad[*].md")
	if err := os.WriteFile(path, []byte("not front matter\x1b[31m"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := runJobCommand(t, New(cfg, newServiceHarness()), "/tasks")
	for _, want := range []string{"bad\\[\\*\\]", "details unavailable", "## Warnings", "has invalid front matter"} {
		if !strings.Contains(output, want) {
			t.Fatalf("unavailable document output missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "\x1b") || strings.Contains(output, root) {
		t.Fatalf("workflow list exposed controls or a full path:\n%s", output)
	}
}

func TestTaskDetailUsesGlobalReviewModeInsteadOfStaleDocumentChoice(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Harness.Reviews = config.TaskReviewsNever
	now := time.Now().UTC().Truncate(time.Second)
	writeWorkflowListFixture(t, cfg, "tasks", "todo", "policy.md", map[string]any{
		"id": "task-policy", "title": "Policy task", "status": "todo", "review_required": true,
		"created_at": now.Format(time.RFC3339), "updated_at": now.Format(time.RFC3339),
	}, "## Progress\n")
	output := runJobCommand(t, New(cfg, newServiceHarness()), "/tasks --detail")
	if !strings.Contains(output, "direct low-risk completion allowed") || strings.Contains(output, "review required") {
		t.Fatalf("task detail did not apply never mode:\n%s", output)
	}
}

func writeWorkflowListFixture(t *testing.T, cfg config.Config, kind, status, name string, front map[string]any, body string) {
	t.Helper()
	path := cfg.StatePath(kind, status, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.WriteDocument(path, orchestrator.Document{FrontMatter: front, Body: body}); err != nil {
		t.Fatal(err)
	}
}
