package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/extensions"
	"github.com/agent0ai/spynel/internal/workspace"
)

func workflowTestManager(t *testing.T) (config.Config, *fakeHarness, *Manager) {
	t.Helper()
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(root, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	fake := newFakeRecipient()
	manager := New(cfg, fake, extensions.Runner{Directory: filepath.Join(root, "missing")})
	return cfg, fake, manager
}

func TestOrphanedClaimedTaskReceivesRecoveryLease(t *testing.T) {
	cfg, fake, manager := workflowTestManager(t)
	task, err := Create(cfg, "tasks", "recover an interrupted claim", "")
	if err != nil {
		t.Fatal(err)
	}
	working := filepath.Join(filepath.Dir(cfg.Resolve(cfg.Orchestrator.Routes[0].Source)), "working", filepath.Base(task))
	if _, err := ClaimDocument(task, working, "working", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	leases, err := manager.loadLeases()
	if err != nil || len(leases) != 1 {
		t.Fatalf("recovery leases = %#v, %v", leases, err)
	}
	if leases[0].Phase != phaseTaskImplementation || leases[0].RecoveryCount == 0 || fake.calls != 1 {
		t.Fatalf("orphan recovery = lease %#v, calls %d", leases[0], fake.calls)
	}
}

func TestJournaledClaimIsFinishedAfterRestart(t *testing.T) {
	cfg, fake, manager := workflowTestManager(t)
	task, err := Create(cfg, "tasks", "finish interrupted rename", "")
	if err != nil {
		t.Fatal(err)
	}
	document, err := ReadDocument(task)
	if err != nil {
		t.Fatal(err)
	}
	id := documentID(document)
	target := filepath.Join(filepath.Dir(cfg.Resolve(cfg.Orchestrator.Routes[0].Source)), "working", filepath.Base(task))
	now := time.Now().UTC()
	claimKey := leaseID("tasks:"+phaseTaskImplementation, id)
	lease := Lease{
		ID: claimKey, ClaimID: claimKey, DocumentType: "task", Route: "tasks", File: target, SourceFile: task,
		SessionKey: phaseSessionKey("tasks", id, phaseTaskImplementation, 1), State: "claiming", Phase: phaseTaskImplementation,
		StartedAt: now, HeartbeatAt: now,
	}
	if err := manager.saveLease(lease); err != nil {
		t.Fatal(err)
	}
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("journaled claim target = %v", err)
	}
	if _, err := os.Stat(task); !os.IsNotExist(err) {
		t.Fatalf("journaled source still exists: %v", err)
	}
	recovered, err := manager.loadLease(claimKey)
	if err != nil || recovered.State != "awaiting_transition" || recovered.SourceFile != "" || fake.calls != 1 {
		t.Fatalf("resumed journaled claim = %#v, calls %d, error %v", recovered, fake.calls, err)
	}
}

func TestFreshLeaseFromPreviousManagerRecoversWithoutFullStaleDelay(t *testing.T) {
	cfg, fake, first := workflowTestManager(t)
	if _, err := Create(cfg, "tasks", "recover after process restart", ""); err != nil {
		t.Fatal(err)
	}
	if err := first.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	first.Wait()
	leases, err := first.loadLeases()
	if err != nil || len(leases) != 1 || leases[0].OwnerID != first.ownerID {
		t.Fatalf("first lease = %#v, %v", leases, err)
	}
	second := New(cfg, fake, extensions.Runner{Directory: filepath.Join(cfg.Root, "missing")})
	if second.ownerID == first.ownerID {
		t.Fatal("manager ownership IDs were reused")
	}
	if err := second.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	second.Wait()
	if fake.calls != 2 {
		t.Fatalf("fresh foreign lease was not recovered after restart: %d calls", fake.calls)
	}
	recovered, err := second.loadLeases()
	if err != nil || len(recovered) != 1 || recovered[0].OwnerID != second.ownerID || recovered[0].RecoveryCount != 1 {
		t.Fatalf("recovered ownership = %#v, %v", recovered, err)
	}
}

func TestWaitingTaskWithWakeTimeReturnsToImplementationQueue(t *testing.T) {
	cfg, _, manager := workflowTestManager(t)
	task, err := Create(cfg, "tasks", "resume after dependency", "")
	if err != nil {
		t.Fatal(err)
	}
	document, err := ReadDocument(task)
	if err != nil {
		t.Fatal(err)
	}
	document.FrontMatter["wake_at"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	if err := WriteDocument(task, document); err != nil {
		t.Fatal(err)
	}
	waiting := filepath.Join(filepath.Dir(task), "..", "waiting", filepath.Base(task))
	if err := moveDocument(task, waiting, "waiting", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	working := filepath.Join(filepath.Dir(filepath.Dir(task)), "working", filepath.Base(task))
	if _, err := os.Stat(working); err != nil {
		t.Fatalf("due waiting task was not claimed: %v", err)
	}
}

func TestGoalRoundSettlesIntoFreshGoalReviewAndContinuesToPlanning(t *testing.T) {
	cfg, _, manager := workflowTestManager(t)
	goal, err := Create(cfg, "goals", "keep the release healthy", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	goalBase := filepath.Dir(cfg.Resolve(cfg.Orchestrator.Routes[1].Source))
	planning := filepath.Join(goalBase, "planning", filepath.Base(goal))
	goalDocument, err := ReadDocument(planning)
	if err != nil {
		t.Fatal(err)
	}
	goalID := documentID(goalDocument)
	goalDocument.FrontMatter["round"] = 1
	goalDocument.FrontMatter["status"] = "active"
	if err := WriteDocument(planning, goalDocument); err != nil {
		t.Fatal(err)
	}
	task, err := CreateWithOptions(cfg, "tasks", "verify release", "", CreateOptions{GoalID: goalID, GoalRound: 1})
	if err != nil {
		t.Fatal(err)
	}
	taskDocument, err := ReadDocument(task)
	if err != nil {
		t.Fatal(err)
	}
	goalDocument.FrontMatter["round_task_ids"] = []any{documentID(taskDocument)}
	if err := WriteDocument(planning, goalDocument); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(goalBase, "active", filepath.Base(goal))
	if err := os.Rename(planning, active); err != nil {
		t.Fatal(err)
	}
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	taskBase := filepath.Dir(cfg.Resolve(cfg.Orchestrator.Routes[0].Source))
	workingTask := filepath.Join(taskBase, "working", filepath.Base(task))
	reviewTask := filepath.Join(taskBase, "review", filepath.Base(task))
	if err := moveDocument(workingTask, reviewTask, "review", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	reviewingTask := filepath.Join(taskBase, "reviewing", filepath.Base(task))
	doneTask := filepath.Join(taskBase, "done", filepath.Base(task))
	if err := moveDocument(reviewingTask, doneTask, "done", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	reviewingGoal := filepath.Join(goalBase, "reviewing", filepath.Base(goal))
	if _, err := os.Stat(reviewingGoal); err != nil {
		t.Fatalf("settled goal was not claimed for review: %v", err)
	}
	leasing, err := manager.loadLeases()
	if err != nil {
		t.Fatal(err)
	}
	var reviewLease Lease
	for _, lease := range leasing {
		if lease.Phase == phaseGoalReview {
			reviewLease = lease
		}
	}
	if reviewLease.ID == "" || !strings.Contains(reviewLease.SessionKey, phaseGoalReview) {
		t.Fatalf("goal review lease = %#v", reviewLease)
	}
	goalDocument, err = ReadDocument(reviewingGoal)
	if err != nil {
		t.Fatal(err)
	}
	goalDocument.FrontMatter["last_review"] = map[string]any{
		"round": 1, "verdict": "continue", "criteria_satisfied": false,
		"reviewed_at": time.Now().UTC().Format(time.RFC3339),
	}
	if err := WriteDocument(reviewingGoal, goalDocument); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(reviewingGoal, planning); err != nil {
		t.Fatal(err)
	}
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	leasing, _ = manager.loadLeases()
	var planningLease Lease
	for _, lease := range leasing {
		if lease.Phase == phaseGoalPlanning {
			planningLease = lease
		}
	}
	if planningLease.ID == "" || planningLease.SessionKey == reviewLease.SessionKey {
		t.Fatalf("continuation did not receive a fresh planning lease: review=%#v planning=%#v", reviewLease, planningLease)
	}
}

func TestGoalDoneRequiresReviewProof(t *testing.T) {
	cfg, _, manager := workflowTestManager(t)
	goalRoute := cfg.Orchestrator.Routes[1]
	base := filepath.Dir(cfg.Resolve(goalRoute.Source))
	path := filepath.Join(base, "reviewing", "proof.md")
	now := time.Now().UTC()
	document := Document{FrontMatter: map[string]any{
		"id": "goal-proof", "title": "proof", "status": "reviewing", "round": 1,
		"created_at": now.Format(time.RFC3339), "updated_at": now.Format(time.RFC3339),
		"review_trigger":   "all_round_tasks_settled",
		"success_criteria": []any{map[string]any{"id": "bar", "condition": "verified", "evidence_required": "test output"}},
	}, Body: "# proof\n"}
	if err := WriteDocument(path, document); err != nil {
		t.Fatal(err)
	}
	lease := Lease{ID: "goal-review", Route: "goals", File: path, Phase: phaseGoalReview, SessionKey: "review-session", State: "processing", StartedAt: now, HeartbeatAt: now}
	if err := manager.saveLease(lease); err != nil {
		t.Fatal(err)
	}
	done := filepath.Join(base, "done", filepath.Base(path))
	if err := moveDocument(path, done, "done", now); err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileTransitions(context.Background()); err != nil {
		t.Fatal(err)
	}
	queued := filepath.Join(base, "review", filepath.Base(path))
	if _, err := os.Stat(queued); err != nil {
		t.Fatalf("unproven goal completion was not rejected: %v", err)
	}
}
