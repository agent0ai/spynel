package orchestrator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/extensions"
	"github.com/agent0ai/spynel/internal/workspace"
)

func TestTaskPolicyDefaultsAndFailsSafe(t *testing.T) {
	tests := []struct {
		name      string
		value     any
		set       bool
		required  bool
		wantError bool
	}{
		{name: "missing", required: true},
		{name: "true", set: true, value: true, required: true},
		{name: "false", set: true, value: false, required: false},
		{name: "string", set: true, value: "false", required: true, wantError: true},
		{name: "number", set: true, value: 0, required: true, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			front := map[string]any{}
			if test.set {
				front["review_required"] = test.value
			}
			policy, err := TaskPolicyFromDocument(Document{FrontMatter: front})
			if policy.ReviewRequired != test.required || (err != nil) != test.wantError {
				t.Fatalf("policy = %#v, error = %v", policy, err)
			}
		})
	}
}

func TestTaskCreationSetsReviewPolicyDeliberately(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	for _, noReview := range []bool{false, true} {
		path, err := CreateWithOptions(cfg, "tasks", "collect inventory", "", CreateOptions{NoReview: noReview})
		if err != nil {
			t.Fatal(err)
		}
		document, err := ReadDocument(path)
		if err != nil || document.FrontMatter["review_required"] != !noReview {
			t.Fatalf("review_required = %#v, %v", document.FrontMatter["review_required"], err)
		}
		if noReview && (!strings.Contains(document.Body, "proportionate verification") || strings.Contains(document.Body, "independently verified")) {
			t.Fatalf("direct task acceptance criteria = %q", document.Body)
		}
	}
	path, err := CreateWithOptions(cfg, "tasks", "goal work", "", CreateOptions{GoalID: "goal-1", GoalRound: 1, NoReview: true})
	if err != nil {
		t.Fatal(err)
	}
	document, _ := ReadDocument(path)
	if document.FrontMatter["review_required"] != false {
		t.Fatal("goal-derived task did not preserve the planner's no-review choice")
	}
}

func TestTaskCreationReviewModeOverridesPerTaskChoice(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	for _, test := range []struct {
		mode     string
		noReview bool
		want     bool
	}{
		{mode: config.TaskReviewsAlways, noReview: true, want: true},
		{mode: config.TaskReviewsNever, noReview: false, want: false},
	} {
		cfg.Harness.Reviews = test.mode
		path, err := CreateWithOptions(cfg, "tasks", "mode "+test.mode, "", CreateOptions{NoReview: test.noReview})
		if err != nil {
			t.Fatal(err)
		}
		document, err := ReadDocument(path)
		if err != nil || document.FrontMatter["review_required"] != test.want {
			t.Fatalf("mode %q review_required = %#v, %v", test.mode, document.FrontMatter["review_required"], err)
		}
	}
}

func TestNeverReviewModeReturnsQueuedReviewToImplementation(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	cfg.Harness.Reviews = config.TaskReviewsNever
	route := cfg.Orchestrator.Routes[0]
	base := filepath.Dir(cfg.Resolve(route.Source))
	task, err := Create(cfg, "tasks", "queued before policy change", "")
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(task)
	review := filepath.Join(base, "review", name)
	if err := moveDocument(task, review, "review", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	target := newFakeRecipient()
	manager := New(cfg, target, extensions.Runner{Directory: filepath.Join(root, "missing")})
	if err := manager.scanPhaseQueue(context.Background(), route, filepath.Join(base, "review"), filepath.Join(base, "reviewing"), phaseTaskReview); err != nil {
		t.Fatal(err)
	}
	document, err := ReadDocument(filepath.Join(base, "todo", name))
	if err != nil || !strings.Contains(document.Body, "Independent task review is disabled") {
		t.Fatalf("disabled review was not returned to todo: %v, %q", err, document.Body)
	}
	if target.calls != 0 {
		t.Fatalf("disabled reviewer was dispatched %d times", target.calls)
	}
}

func TestAlwaysReviewModeRedirectsDocumentNoReviewCompletion(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	route := cfg.Orchestrator.Routes[0]
	base := filepath.Dir(cfg.Resolve(route.Source))
	task, err := CreateWithOptions(cfg, "tasks", "preexisting direct task", "", CreateOptions{NoReview: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Harness.Reviews = config.TaskReviewsAlways
	name := filepath.Base(task)
	target := newFakeRecipient()
	target.beforeEmit = func() {
		working := filepath.Join(cfg.Resolve(route.Working), name)
		recordDirectCompletion(t, working, filepath.Join(base, "done", name))
	}
	manager := New(cfg, target, extensions.Runner{Directory: filepath.Join(root, "missing")})
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	if err := manager.reconcileTransitions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "review", name)); err != nil {
		t.Fatalf("always mode did not force review: %v", err)
	}
}

func TestNeverReviewModeAllowsExistingReviewRequiredTaskDirectCompletion(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	route := cfg.Orchestrator.Routes[0]
	base := filepath.Dir(cfg.Resolve(route.Source))
	task, err := Create(cfg, "tasks", "existing reviewed task", "")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Harness.Reviews = config.TaskReviewsNever
	name := filepath.Base(task)
	target := newFakeRecipient()
	target.beforeEmit = func() {
		recordDirectCompletion(t, filepath.Join(cfg.Resolve(route.Working), name), filepath.Join(base, "done", name))
	}
	manager := New(cfg, target, extensions.Runner{Directory: filepath.Join(root, "missing")})
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	if err := manager.reconcileTransitions(context.Background()); err != nil {
		t.Fatal(err)
	}
	document, err := ReadDocument(filepath.Join(base, "done", name))
	if err != nil || document.FrontMatter["review_required"] != true {
		t.Fatalf("never mode did not complete without rewriting document choice: %#v, %v", document.FrontMatter, err)
	}
}

func TestDeveloperAndReviewerPromptsUseRolePrefixes(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	cfg.Harness.DeveloperAgentPrefix = "/develop"
	cfg.Harness.ReviewerAgentPrefix = "/review"
	if err := os.WriteFile(cfg.StatePath("instructions", "agent-developer.md"), []byte("developer-only-rule"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.StatePath("instructions", "agent-reviewer.md"), []byte("reviewer-only-rule"), 0o600); err != nil {
		t.Fatal(err)
	}
	route := cfg.Orchestrator.Routes[0]
	base := filepath.Dir(cfg.Resolve(route.Source))
	task, err := Create(cfg, "tasks", "prefix phases", "")
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(task)
	target := newFakeRecipient()
	target.beforeEmit = func() {
		working := filepath.Join(cfg.Resolve(route.Working), name)
		if _, err := os.Stat(working); err == nil {
			_ = moveDocument(working, filepath.Join(base, "review", name), "review", time.Now().UTC())
		}
	}
	manager := New(cfg, target, extensions.Runner{Directory: filepath.Join(root, "missing")})
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	if len(target.prompts) != 2 || !strings.HasPrefix(target.prompts[0], "/develop ") || !strings.HasPrefix(target.prompts[1], "/review ") {
		t.Fatalf("phase prompts = %#v", target.prompts)
	}
	if !strings.Contains(target.prompts[0], "Configured task review mode: skip-trivial") {
		t.Fatalf("developer prompt omitted review mode: %q", target.prompts[0])
	}
	if !strings.Contains(target.prompts[0], "\ndeveloper-only-rule\n</workspace_owner_persistent_instructions>") || strings.Contains(target.prompts[0], "reviewer-only-rule") {
		t.Fatalf("developer prompt did not receive only its final role instructions: %q", target.prompts[0])
	}
	if !strings.Contains(target.prompts[1], "\nreviewer-only-rule\n</workspace_owner_persistent_instructions>") || strings.Contains(target.prompts[1], "developer-only-rule") {
		t.Fatalf("reviewer prompt did not receive only its final role instructions: %q", target.prompts[1])
	}
	settings := manager.harnessSettings()
	if manager.agentPrefix(phaseGoalPlanning, settings) != "/develop" || manager.agentPrefix(phaseGoalReview, settings) != "/review" {
		t.Fatalf("goal phase prefixes do not match developer/reviewer roles")
	}
}

func TestReviewedAndDirectTaskCompletion(t *testing.T) {
	for _, test := range []struct {
		name       string
		noReview   bool
		wantStatus string
	}{
		{name: "review required", wantStatus: "review"},
		{name: "direct low risk", noReview: true, wantStatus: "done"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := workspace.Init(root, false); err != nil {
				t.Fatal(err)
			}
			cfg, _ := config.Load(config.PathForRoot(root))
			route := cfg.Orchestrator.Routes[0]
			base := filepath.Dir(cfg.Resolve(route.Source))
			options := CreateOptions{NoReview: test.noReview}
			if test.noReview {
				options.Notify = true
				options.Origin = "cli/local"
				options.Outcomes = []string{"done"}
			}
			task, err := CreateWithOptions(cfg, "tasks", test.name, "", options)
			if err != nil {
				t.Fatal(err)
			}
			name := filepath.Base(task)
			fake := newFakeRecipient()
			moved := false
			fake.beforeEmit = func() {
				if moved {
					return
				}
				moved = true
				working := filepath.Join(cfg.Resolve(route.Working), name)
				if test.noReview {
					recordDirectCompletion(t, working, filepath.Join(base, "done", name))
					return
				}
				_ = moveDocument(working, filepath.Join(base, "done", name), "done", time.Now().UTC())
			}
			manager := New(cfg, fake, extensions.Runner{Directory: filepath.Join(root, "missing")})
			if err := manager.ScanOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			manager.Wait()
			if err := manager.reconcileTransitions(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(base, test.wantStatus, name)); err != nil {
				t.Fatalf("expected %s transition: %v", test.wantStatus, err)
			}
			if leases, err := manager.loadLeases(); err != nil {
				t.Fatal(err)
			} else {
				for _, lease := range leases {
					if filepath.Base(lease.File) == name && lease.Phase == phaseTaskImplementation {
						t.Fatalf("terminal implementation lease retained: %#v", lease)
					}
				}
			}
			if test.noReview {
				entries, err := os.ReadDir(manager.notificationAgentDirectory())
				if err != nil || len(entries) != 1 {
					t.Fatalf("direct completion notification agent entries = %d, %v", len(entries), err)
				}
				if err := manager.reconcileTransitions(context.Background()); err != nil {
					t.Fatal(err)
				}
				again, _ := os.ReadDir(manager.notificationAgentDirectory())
				if len(again) != 1 {
					t.Fatalf("direct completion duplicated notification: %d", len(again))
				}
			}
		})
	}
}

func TestDirectCompletionWithoutEvidenceReturnsToTodo(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	route := cfg.Orchestrator.Routes[0]
	task, err := CreateWithOptions(cfg, "tasks", "collect incomplete inventory", "", CreateOptions{NoReview: true, Notify: true, Origin: "cli/local", Outcomes: []string{"done"}})
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(task)
	fake := newFakeRecipient()
	fake.beforeEmit = func() {
		working := filepath.Join(cfg.Resolve(route.Working), name)
		_ = moveDocument(working, filepath.Join(filepath.Dir(cfg.Resolve(route.Source)), "done", name), "done", time.Now().UTC())
	}
	manager := New(cfg, fake, extensions.Runner{Directory: filepath.Join(root, "missing")})
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	if err := manager.reconcileTransitions(context.Background()); err != nil {
		t.Fatal(err)
	}
	todo := filepath.Join(cfg.Resolve(route.Source), name)
	document, err := ReadDocument(todo)
	if err != nil || !strings.Contains(document.Body, "Direct completion rejected") {
		t.Fatalf("missing-evidence completion was not returned to todo: %v, %q", err, document.Body)
	}
	if entries, err := os.ReadDir(cfg.StatePath("runtime", "outbox")); err == nil && len(entries) != 0 {
		t.Fatalf("rejected completion enqueued %d notifications", len(entries))
	}
}

func TestMalformedPolicyIsNormalizedAndDirectDoneIsReviewed(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	route := cfg.Orchestrator.Routes[0]
	task, _ := Create(cfg, "tasks", "malformed", "")
	document, _ := ReadDocument(task)
	document.FrontMatter["review_required"] = "false"
	if err := WriteDocument(task, document); err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(task)
	fake := newFakeRecipient()
	fake.beforeEmit = func() {
		working := filepath.Join(cfg.Resolve(route.Working), name)
		_ = moveDocument(working, filepath.Join(filepath.Dir(cfg.Resolve(route.Source)), "done", name), "done", time.Now().UTC())
	}
	manager := New(cfg, fake, extensions.Runner{Directory: filepath.Join(root, "missing")})
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	if err := manager.reconcileTransitions(context.Background()); err != nil {
		t.Fatal(err)
	}
	reviewPath := filepath.Join(filepath.Dir(cfg.Resolve(route.Source)), "review", name)
	document, err := ReadDocument(reviewPath)
	if err != nil || document.FrontMatter["review_required"] != true {
		t.Fatalf("unsafe policy not normalized into review: %#v, %v", document.FrontMatter, err)
	}
}

func TestNoReviewTaskManuallyQueuedForReviewIsHonored(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	route := cfg.Orchestrator.Routes[0]
	task, _ := CreateWithOptions(cfg, "tasks", "manual review", "", CreateOptions{NoReview: true})
	name := filepath.Base(task)
	fake := newFakeRecipient()
	fake.beforeEmit = func() {
		working := filepath.Join(cfg.Resolve(route.Working), name)
		review := filepath.Join(filepath.Dir(cfg.Resolve(route.Source)), "review", name)
		if _, err := os.Stat(working); err == nil {
			_ = moveDocument(working, review, "review", time.Now().UTC())
		}
	}
	manager := New(cfg, fake, extensions.Runner{Directory: filepath.Join(root, "missing")})
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	if fake.calls != 2 {
		t.Fatalf("manual review was not dispatched; calls=%d", fake.calls)
	}
}

func TestReviewQueueWaitsForImplementationLeaseReconciliation(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	route := cfg.Orchestrator.Routes[0]
	base := filepath.Dir(cfg.Resolve(route.Source))
	task, err := Create(cfg, "tasks", "review claim race", "")
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(task)
	working := filepath.Join(cfg.Resolve(route.Working), name)
	document, err := ClaimDocument(task, working, "working", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	reviewDir := filepath.Join(base, "review")
	review := filepath.Join(reviewDir, name)
	if err := moveDocument(working, review, "review", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	fake := newFakeRecipient()
	manager := New(cfg, fake, extensions.Runner{Directory: filepath.Join(root, "missing")})
	implementation := Lease{
		ID: leaseID(route.Name+":"+phaseTaskImplementation, documentID(document)), ClaimID: "implementation",
		DocumentType: "task", OwnerID: manager.ownerID, Route: route.Name, File: working,
		SessionKey: "implementation-session", ThreadID: "implementation-thread",
		State: "awaiting_transition", Phase: phaseTaskImplementation, StartedAt: time.Now().UTC(), HeartbeatAt: time.Now().UTC(),
	}
	if err := manager.saveLease(implementation); err != nil {
		t.Fatal(err)
	}

	if err := manager.scanPhaseQueue(context.Background(), route, reviewDir, filepath.Join(base, "reviewing"), phaseTaskReview); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(review); err != nil {
		t.Fatalf("review was claimed before implementation reconciliation: %v", err)
	}
	if fake.calls != 0 {
		t.Fatalf("review dispatched before implementation reconciliation: calls=%d", fake.calls)
	}

	if err := manager.reconcileTransitions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.scanPhaseQueue(context.Background(), route, reviewDir, filepath.Join(base, "reviewing"), phaseTaskReview); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	if fake.calls != 1 {
		t.Fatalf("review was not dispatched after implementation reconciliation: calls=%d", fake.calls)
	}
}

func TestImplementationReconciliationPreservesExistingReviewClaim(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	route := cfg.Orchestrator.Routes[0]
	base := filepath.Dir(cfg.Resolve(route.Source))
	task, err := Create(cfg, "tasks", "already claimed review", "")
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(task)
	reviewing := filepath.Join(base, "reviewing", name)
	document, err := ClaimDocument(task, reviewing, "reviewing", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

	manager := New(cfg, newFakeRecipient(), extensions.Runner{Directory: filepath.Join(root, "missing")})
	reviewLease := Lease{
		ID: leaseID(route.Name+":"+phaseTaskReview, documentID(document)), Route: route.Name, File: reviewing,
		State: "processing", Phase: phaseTaskReview, StartedAt: time.Now().UTC(), HeartbeatAt: time.Now().UTC(),
	}
	if err := manager.saveLease(reviewLease); err != nil {
		t.Fatal(err)
	}
	implementation := Lease{ID: "old-implementation", Route: route.Name, File: filepath.Join(base, "working", name), SessionKey: "implementation-session", ThreadID: "implementation-thread", Phase: phaseTaskImplementation}
	status, path, err := manager.reconcileTaskTransition(context.Background(), route, implementation, phaseTaskImplementation, "reviewing", reviewing)
	if err != nil {
		t.Fatal(err)
	}
	if status != "reviewing" || path != reviewing {
		t.Fatalf("existing review claim was rewritten: status=%q path=%q", status, path)
	}
	updated, err := manager.loadLease(reviewLease.ID)
	if err != nil || updated.ImplementerThread != "implementation-thread" {
		t.Fatalf("review lease implementer fence = %q, %v", updated.ImplementerThread, err)
	}
	claimed, err := ReadDocument(reviewing)
	if err != nil || claimed.FrontMatter["implementation_session"] != "implementation-session" {
		t.Fatalf("claimed review metadata = %#v, %v", claimed.FrontMatter, err)
	}
}

func TestDirectCompletionEvidenceValidation(t *testing.T) {
	valid := map[string]any{
		"verdict": "completed", "outcome": "Collected three service states.",
		"evidence":     "Local status output only.",
		"uncertainty":  "Remote health remains uncertain.",
		"completed_at": "2026-08-08T12:00:00Z",
	}
	tests := []struct {
		name    string
		updated string
		mutate  func(map[string]any)
		wantErr bool
	}{
		{name: "valid", updated: "2026-08-08T12:00:00Z"},
		{name: "missing evidence", updated: "2026-08-08T12:00:00Z", mutate: func(summary map[string]any) { delete(summary, "evidence") }, wantErr: true},
		{name: "missing uncertainty", updated: "2026-08-08T12:00:00Z", mutate: func(summary map[string]any) { delete(summary, "uncertainty") }, wantErr: true},
		{name: "mismatched timestamp", updated: "2026-08-08T12:00:01Z", wantErr: true},
		{name: "non UTC timestamp", updated: "2026-08-08T13:00:00+01:00", mutate: func(summary map[string]any) { summary["completed_at"] = "2026-08-08T13:00:00+01:00" }, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			summary := make(map[string]any, len(valid))
			for key, value := range valid {
				summary[key] = value
			}
			if test.mutate != nil {
				test.mutate(summary)
			}
			document := Document{FrontMatter: map[string]any{"updated_at": test.updated, "completion_summary": summary}}
			if err := validateDirectCompletionEvidence(document); (err != nil) != test.wantErr {
				t.Fatalf("validation error = %v, want error %v", err, test.wantErr)
			}
		})
	}
}

func TestDirectCompletionHookFiresOnceAcrossRepeatReconciliation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook fixture")
	}
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	route := cfg.Orchestrator.Routes[0]
	extension := filepath.Join(cfg.Resolve(cfg.Extensions.Directory), "counter")
	if err := os.MkdirAll(extension, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "name: counter\nhooks:\n  task.completed: [\"./hook.sh\"]\n"
	script := "#!/bin/sh\nprintf x >> completed.count\nprintf '%s\\n' '{}'\n"
	if err := os.WriteFile(filepath.Join(extension, extensions.ManifestName), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extension, "hook.sh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	task, err := CreateWithOptions(cfg, "tasks", "collect hook status", "", CreateOptions{NoReview: true, Notify: true, Origin: "cli/local", Outcomes: []string{"done"}})
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(task)
	fake := newFakeRecipient()
	moved := false
	fake.beforeEmit = func() {
		if moved {
			return
		}
		moved = true
		working := filepath.Join(cfg.Resolve(route.Working), name)
		recordDirectCompletion(t, working, filepath.Join(filepath.Dir(cfg.Resolve(route.Source)), "done", name))
	}
	manager := New(cfg, fake, extensions.Runner{Directory: cfg.Resolve(cfg.Extensions.Directory), Timeout: time.Second})
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	if err := manager.reconcileTransitions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileTransitions(context.Background()); err != nil {
		t.Fatal(err)
	}
	count, err := os.ReadFile(filepath.Join(extension, "completed.count"))
	if err != nil || string(count) != "x" {
		t.Fatalf("completion hook count = %q, %v", count, err)
	}
}

func TestDirectCompletionRetriesFailedHookWithStableEventID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook fixture")
	}
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	route := cfg.Orchestrator.Routes[0]
	extension := filepath.Join(cfg.Resolve(cfg.Extensions.Directory), "retry")
	if err := os.MkdirAll(extension, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "name: retry\nhooks:\n  task.completed: [\"./hook.sh\"]\n"
	script := "#!/bin/sh\ninput=$(cat)\nprintf '%s\\n' \"$input\" >> events\nif [ ! -f attempted ]; then touch attempted; exit 9; fi\nprintf '%s\\n' '{}'\n"
	if err := os.WriteFile(filepath.Join(extension, extensions.ManifestName), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extension, "hook.sh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	task, err := CreateWithOptions(cfg, "tasks", "collect retry status", "", CreateOptions{NoReview: true})
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(task)
	fake := newFakeRecipient()
	fake.beforeEmit = func() {
		working := filepath.Join(cfg.Resolve(route.Working), name)
		recordDirectCompletion(t, working, filepath.Join(filepath.Dir(cfg.Resolve(route.Source)), "done", name))
	}
	manager := New(cfg, fake, extensions.Runner{Directory: cfg.Resolve(cfg.Extensions.Directory), Timeout: time.Second})
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	if err := manager.reconcileTransitions(context.Background()); err == nil {
		t.Fatal("first failed hook delivery unexpectedly reconciled")
	}
	if err := manager.reconcileTransitions(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, err := os.ReadFile(filepath.Join(extension, "events"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(events)), "\n")
	if len(lines) != 2 {
		t.Fatalf("hook attempts = %d, want 2; events=%q", len(lines), events)
	}
	var first, second extensions.HookInput
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	firstID, _ := first.Payload["event_id"].(string)
	secondID, _ := second.Payload["event_id"].(string)
	if firstID == "" || firstID != secondID {
		t.Fatalf("retry event IDs = %q, %q", firstID, secondID)
	}
	if err := manager.reconcileTransitions(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(extension, "events"))
	if err != nil || string(after) != string(events) {
		t.Fatalf("completed hook was relaunched: before=%q after=%q err=%v", events, after, err)
	}
}

func recordDirectCompletion(t *testing.T, working, done string) {
	t.Helper()
	document, err := ReadDocument(working)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	document.FrontMatter["completion_summary"] = map[string]any{
		"verdict": "completed", "outcome": "Collected the requested status.",
		"evidence":     "Local status output only.",
		"uncertainty":  "Remote health remains uncertain.",
		"completed_at": now.Format(time.RFC3339),
	}
	if err := WriteDocument(working, document); err != nil {
		t.Fatal(err)
	}
	if err := moveDocument(working, done, "done", now); err != nil {
		t.Fatal(err)
	}
}
