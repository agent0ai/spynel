package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	cfg, err := config.Load(config.PathForRoot(root))
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
	recovered, err := ReadDocument(working)
	if err != nil || !strings.Contains(recovered.Body, "Spynel started recovery attempt 1 for task implementation") {
		t.Fatalf("recovery progress was not journaled: body=%q err=%v", recovered.Body, err)
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

func TestJournaledReviewClaimFinishesMetadataAfterRenameOnlyCrash(t *testing.T) {
	cfg, fake, manager := workflowTestManager(t)
	task, err := Create(cfg, "tasks", "finish interrupted review rename", "")
	if err != nil {
		t.Fatal(err)
	}
	document, err := ReadDocument(task)
	if err != nil {
		t.Fatal(err)
	}
	document.FrontMatter["status"] = "review"
	document.FrontMatter["attempt"] = 1
	review := filepath.Join(filepath.Dir(task), "..", "review", filepath.Base(task))
	if err := os.MkdirAll(filepath.Dir(review), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteDocument(review, document); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(task); err != nil {
		t.Fatal(err)
	}

	id := documentID(document)
	reviewing := filepath.Join(filepath.Dir(task), "..", "reviewing", filepath.Base(task))
	started := time.Now().UTC().Add(-time.Minute)
	claimKey := leaseID("tasks:"+phaseTaskReview, id)
	lease := Lease{
		ID: claimKey, ClaimID: claimKey, DocumentType: "task", Route: "tasks", File: reviewing, SourceFile: review,
		SessionKey: phaseSessionKey("tasks", id, phaseTaskReview, 1), State: "claiming", Phase: phaseTaskReview,
		ClaimAttempt: 1, StartedAt: started, HeartbeatAt: started,
	}
	if err := manager.saveLease(lease); err != nil {
		t.Fatal(err)
	}
	// Reproduce a crash after the atomic rename but before claimDocument writes
	// the claimed status and phase-specific attempt metadata.
	if err := os.MkdirAll(filepath.Dir(reviewing), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(review, reviewing); err != nil {
		t.Fatal(err)
	}

	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	stored, err := ReadDocument(reviewing)
	if err != nil {
		t.Fatal(err)
	}
	if stored.FrontMatter["status"] != "reviewing" || stored.FrontMatter["attempt"] != 1 || stored.FrontMatter["review_attempt"] != 1 {
		t.Fatalf("recovered review metadata = %#v", stored.FrontMatter)
	}
	if fake.calls != 1 {
		t.Fatalf("review provider calls = %d, want 1", fake.calls)
	}
}

func TestLegacyJournaledReviewClaimUsesSessionAttemptAfterFirstWriteCrash(t *testing.T) {
	cfg, fake, manager := workflowTestManager(t)
	task, err := Create(cfg, "tasks", "recover a legacy interrupted review write", "")
	if err != nil {
		t.Fatal(err)
	}
	document, err := ReadDocument(task)
	if err != nil {
		t.Fatal(err)
	}
	document.FrontMatter["status"] = "reviewing"
	document.FrontMatter["attempt"] = 2 // Legacy generic claim polluted implementation attempt 1.
	document.FrontMatter["review_attempt"] = 2
	reviewing := filepath.Join(filepath.Dir(task), "..", "reviewing", filepath.Base(task))
	if err := os.MkdirAll(filepath.Dir(reviewing), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteDocument(reviewing, document); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(task); err != nil {
		t.Fatal(err)
	}

	id := documentID(document)
	started := time.Now().UTC().Add(-time.Minute)
	claimKey := leaseID("tasks:"+phaseTaskReview, id)
	if err := manager.saveLease(Lease{
		ID: claimKey, ClaimID: claimKey, DocumentType: "task", Route: "tasks", File: reviewing,
		SourceFile: filepath.Join(filepath.Dir(task), "..", "review", filepath.Base(task)),
		SessionKey: phaseSessionKey("tasks", id, phaseTaskReview, 3), State: "claiming", Phase: phaseTaskReview,
		StartedAt: started, HeartbeatAt: started,
	}); err != nil {
		t.Fatal(err)
	}

	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	stored, err := ReadDocument(reviewing)
	if err != nil {
		t.Fatal(err)
	}
	if stored.FrontMatter["status"] != "reviewing" || stored.FrontMatter["attempt"] != 1 || stored.FrontMatter["review_attempt"] != 3 {
		t.Fatalf("legacy recovered review metadata = %#v", stored.FrontMatter)
	}
	if fake.calls != 1 {
		t.Fatalf("legacy review provider calls = %d, want 1", fake.calls)
	}
}

func TestLegacyJournaledReviewClaimRemovesPollutionAfterSecondWriteCrash(t *testing.T) {
	cfg, fake, manager := workflowTestManager(t)
	task, err := Create(cfg, "tasks", "recover a legacy completed review claim", "")
	if err != nil {
		t.Fatal(err)
	}
	document, err := ReadDocument(task)
	if err != nil {
		t.Fatal(err)
	}
	document.FrontMatter["status"] = "reviewing"
	document.FrontMatter["attempt"] = 2
	document.FrontMatter["review_attempt"] = 3
	reviewing := filepath.Join(filepath.Dir(task), "..", "reviewing", filepath.Base(task))
	if err := os.MkdirAll(filepath.Dir(reviewing), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteDocument(reviewing, document); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(task); err != nil {
		t.Fatal(err)
	}

	id := documentID(document)
	started := time.Now().UTC().Add(-time.Minute)
	claimKey := leaseID("tasks:"+phaseTaskReview, id)
	if err := manager.saveLease(Lease{
		ID: claimKey, ClaimID: claimKey, DocumentType: "task", Route: "tasks", File: reviewing,
		SourceFile: filepath.Join(filepath.Dir(task), "..", "review", filepath.Base(task)),
		SessionKey: phaseSessionKey("tasks", id, phaseTaskReview, 3), State: "claiming", Phase: phaseTaskReview,
		StartedAt: started, HeartbeatAt: started,
	}); err != nil {
		t.Fatal(err)
	}

	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	stored, err := ReadDocument(reviewing)
	if err != nil {
		t.Fatal(err)
	}
	if stored.FrontMatter["attempt"] != 1 || stored.FrontMatter["review_attempt"] != 3 {
		t.Fatalf("legacy completed claim metadata = %#v", stored.FrontMatter)
	}
	if stored.FrontMatter["_spynel_attempt_repair_claim"] != claimKey {
		t.Fatalf("legacy repair marker = %#v", stored.FrontMatter["_spynel_attempt_repair_claim"])
	}
	if fake.calls != 1 {
		t.Fatalf("legacy completed claim provider calls = %d, want 1", fake.calls)
	}
}

func TestLegacyAttemptRepairReplayDoesNotDecrementTwice(t *testing.T) {
	cfg, fake, manager := workflowTestManager(t)
	task, err := Create(cfg, "tasks", "replay a persisted legacy attempt repair", "")
	if err != nil {
		t.Fatal(err)
	}
	document, err := ReadDocument(task)
	if err != nil {
		t.Fatal(err)
	}
	id := documentID(document)
	claimKey := leaseID("tasks:"+phaseTaskReview, id)
	document.FrontMatter["status"] = "reviewing"
	document.FrontMatter["attempt"] = 1
	document.FrontMatter["review_attempt"] = 3
	document.FrontMatter["_spynel_attempt_repair_claim"] = claimKey
	reviewing := filepath.Join(filepath.Dir(task), "..", "reviewing", filepath.Base(task))
	if err := os.MkdirAll(filepath.Dir(reviewing), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteDocument(reviewing, document); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(task); err != nil {
		t.Fatal(err)
	}

	started := time.Now().UTC().Add(-time.Minute)
	if err := manager.saveLease(Lease{
		ID: claimKey, ClaimID: claimKey, DocumentType: "task", Route: "tasks", File: reviewing,
		SourceFile: filepath.Join(filepath.Dir(task), "..", "review", filepath.Base(task)),
		SessionKey: phaseSessionKey("tasks", id, phaseTaskReview, 3), State: "claiming", Phase: phaseTaskReview,
		StartedAt: started, HeartbeatAt: started,
	}); err != nil {
		t.Fatal(err)
	}

	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	stored, err := ReadDocument(reviewing)
	if err != nil {
		t.Fatal(err)
	}
	if stored.FrontMatter["attempt"] != 1 || stored.FrontMatter["review_attempt"] != 3 || stored.FrontMatter["_spynel_attempt_repair_claim"] != claimKey {
		t.Fatalf("replayed legacy repair metadata = %#v", stored.FrontMatter)
	}
	if fake.calls != 1 {
		t.Fatalf("replayed legacy repair provider calls = %d, want 1", fake.calls)
	}
}

func TestPhaseClaimTargetCollisionDropsJournalWithoutDispatch(t *testing.T) {
	cfg, fake, manager := workflowTestManager(t)
	route := cfg.Orchestrator.Routes[0]
	base := filepath.Dir(cfg.Resolve(route.Source))
	source := filepath.Join(base, "review", "collision.md")
	target := filepath.Join(base, "reviewing", "collision.md")
	if err := WriteDocument(source, Document{FrontMatter: map[string]any{
		"id": "source", "status": "review", "attempt": 1,
	}, Body: "source body\n"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteDocument(target, Document{FrontMatter: map[string]any{
		"id": "target", "status": "reviewing", "attempt": 7, "review_attempt": 4,
	}, Body: "target body\n"}); err != nil {
		t.Fatal(err)
	}

	if err := manager.scanPhaseQueue(context.Background(), route, filepath.Dir(source), filepath.Dir(target), phaseTaskReview); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	if fake.calls != 0 {
		t.Fatalf("collision dispatched %d providers", fake.calls)
	}
	if leases, err := manager.loadLeases(); err != nil || len(leases) != 0 {
		t.Fatalf("collision leases = %#v, %v", leases, err)
	}
	storedSource, err := ReadDocument(source)
	if err != nil {
		t.Fatal(err)
	}
	storedTarget, err := ReadDocument(target)
	if err != nil {
		t.Fatal(err)
	}
	if documentID(storedSource) != "source" || stringField(storedSource, "status") != "review" || storedSource.Body != "source body\n" {
		t.Fatalf("collision source mutated = %#v, %q", storedSource.FrontMatter, storedSource.Body)
	}
	if documentID(storedTarget) != "target" || numberValue(storedTarget.FrontMatter["attempt"]) != 7 || numberValue(storedTarget.FrontMatter["review_attempt"]) != 4 || storedTarget.Body != "target body\n" {
		t.Fatalf("collision target mutated = %#v, %q", storedTarget.FrontMatter, storedTarget.Body)
	}
}

func TestPhaseClaimWriteFailureAfterRenameKeepsJournalForRecovery(t *testing.T) {
	cfg, fake, manager := workflowTestManager(t)
	task, err := Create(cfg, "tasks", "recover a failed post-rename metadata write", "")
	if err != nil {
		t.Fatal(err)
	}
	document, err := ReadDocument(task)
	if err != nil {
		t.Fatal(err)
	}
	document.FrontMatter["status"] = "review"
	document.FrontMatter["attempt"] = 1
	review := filepath.Join(filepath.Dir(task), "..", "review", filepath.Base(task))
	if err := os.MkdirAll(filepath.Dir(review), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteDocument(review, document); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(task); err != nil {
		t.Fatal(err)
	}

	failed := false
	manager.claimDocument = func(source, target, status, attemptField string, now time.Time) (Document, error) {
		if !failed {
			failed = true
			return claimDocumentWithWriter(source, target, status, attemptField, now, func(string, Document) error {
				return errors.New("injected metadata replacement failure")
			})
		}
		return claimDocument(source, target, status, attemptField, now)
	}
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.calls != 0 {
		t.Fatalf("provider ran after incomplete claim: %d calls", fake.calls)
	}
	leases, err := manager.loadLeases()
	if err != nil || len(leases) != 1 || leases[0].State != "claiming" || leases[0].ClaimAttempt != 1 {
		t.Fatalf("preserved claim journal = %#v, %v", leases, err)
	}
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	reviewing := filepath.Join(filepath.Dir(review), "..", "reviewing", filepath.Base(review))
	stored, err := ReadDocument(reviewing)
	if err != nil {
		t.Fatal(err)
	}
	if stored.FrontMatter["status"] != "reviewing" || stored.FrontMatter["attempt"] != 1 || stored.FrontMatter["review_attempt"] != 1 {
		t.Fatalf("recovered failed claim metadata = %#v", stored.FrontMatter)
	}
	if fake.calls != 1 {
		t.Fatalf("recovered provider calls = %d, want 1", fake.calls)
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
	resumed, err := ReadDocument(working)
	if err != nil || !strings.Contains(resumed.Body, "scheduled wake condition became due") {
		t.Fatalf("waiting wake was not journaled: body=%q err=%v", resumed.Body, err)
	}
}

func TestRuntimeProgressJournalStaysInsideProgressSection(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 34, 56, 0, time.UTC)
	document := Document{FrontMatter: map[string]any{}, Body: "# Task\n\n## Progress\n\n- Existing entry.\n\n## Notes\n\nKeep this note.\n"}
	appendProgress(&document, now, "  Runtime repaired\n  a transition. ")
	appendProgress(&document, now, "Runtime repaired a transition.")
	want := "## Progress\n\n- Existing entry.\n\n- 2026-08-08T12:34:56Z — Runtime repaired a transition.\n\n## Notes\n\nKeep this note."
	if !strings.Contains(document.Body, want) {
		t.Fatalf("progress journal escaped its section:\n%s", document.Body)
	}
	if count := strings.Count(document.Body, "Runtime repaired a transition."); count != 1 {
		t.Fatalf("idempotent progress count = %d:\n%s", count, document.Body)
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

func TestSettledGoalReviewIgnoresFutureCheckpointAcrossRestart(t *testing.T) {
	for _, trigger := range []string{"all_round_tasks_settled", "all_round_tasks_settled_or_checkpoint"} {
		t.Run(trigger, func(t *testing.T) {
			cfg, fake, first := workflowTestManager(t)
			goal := writeActiveGoalRound(t, cfg, trigger, time.Now().Add(7*24*time.Hour), "done", "done")
			goalBase := filepath.Dir(cfg.Resolve(cfg.Orchestrator.Routes[1].Source))

			// Model a restart after active-goal eligibility has been persisted but
			// before the review queue's claim phase runs.
			if err := first.advanceActiveGoals(); err != nil {
				t.Fatal(err)
			}
			queued := filepath.Join(goalBase, "review", filepath.Base(goal))
			queuedDocument, err := ReadDocument(queued)
			if err != nil {
				t.Fatalf("settled goal was not queued for review: %v", err)
			}
			if stringField(queuedDocument, "next_review_at") == "" {
				t.Fatal("queued goal lost its historical round checkpoint")
			}

			restarted := New(cfg, fake, extensions.Runner{Directory: filepath.Join(cfg.Root, "missing")})
			if err := restarted.ScanOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			restarted.Wait()
			reviewing := filepath.Join(goalBase, "reviewing", filepath.Base(goal))
			if _, err := os.Stat(reviewing); err != nil {
				t.Fatalf("future checkpoint stalled queued goal review: %v", err)
			}
			leases, err := restarted.loadLeases()
			if err != nil {
				t.Fatal(err)
			}
			reviewLeases := 0
			for _, lease := range leases {
				if lease.Phase == phaseGoalReview {
					reviewLeases++
					if !strings.HasSuffix(lease.SessionKey, ":1") {
						t.Fatalf("goal review session = %q", lease.SessionKey)
					}
				}
			}
			if reviewLeases != 1 || fake.calls != 1 {
				t.Fatalf("goal review dispatches = %d leases, %d calls", reviewLeases, fake.calls)
			}
			if err := restarted.ScanOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			restarted.Wait()
			if fake.calls != 1 {
				t.Fatalf("duplicate scan dispatched %d goal reviews", fake.calls)
			}
		})
	}
}

func TestTaskSettlementRequestsImmediateGoalReconsideration(t *testing.T) {
	cfg, fake, manager := workflowTestManager(t)
	cfg.Orchestrator.IntervalSec = 3600
	manager.Config = cfg
	goal := writeActiveGoalRound(t, cfg, "all_round_tasks_settled", time.Now().Add(7*24*time.Hour), "todo")
	document, err := ReadDocument(goal)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := manager.goalRoundTasks(document)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("round tasks = %#v, %v", tasks, err)
	}
	working := filepath.Join(filepath.Dir(filepath.Dir(tasks[0].Path)), "working", filepath.Base(tasks[0].Path))
	failed := filepath.Join(filepath.Dir(filepath.Dir(tasks[0].Path)), "failed", filepath.Base(tasks[0].Path))
	var settle sync.Once
	fake.beforeEmit = func() {
		settle.Do(func() {
			if err := moveDocument(working, failed, "failed", time.Now()); err != nil {
				t.Errorf("settle task: %v", err)
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	reviewing := filepath.Join(filepath.Dir(filepath.Dir(goal)), "reviewing", filepath.Base(goal))
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(reviewing); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("task settlement did not wake goal reconsideration before the periodic interval")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("orchestrator did not stop")
	}
	manager.Wait()
}

func TestCheckpointTriggeredGoalWaitsUntilDue(t *testing.T) {
	for _, test := range []struct {
		name       string
		taskStatus string
	}{
		{name: "unsettled", taskStatus: "waiting"},
		{name: "settled-scheduled-only", taskStatus: "done"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, _, manager := workflowTestManager(t)
			goal := writeActiveGoalRound(t, cfg, "scheduled", time.Now().Add(7*24*time.Hour), test.taskStatus)
			if err := manager.ScanOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			manager.Wait()
			if _, err := os.Stat(goal); err != nil {
				t.Fatalf("future scheduled goal reviewed early: %v", err)
			}

			document, err := ReadDocument(goal)
			if err != nil {
				t.Fatal(err)
			}
			document.FrontMatter["next_review_at"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
			if err := WriteDocument(goal, document); err != nil {
				t.Fatal(err)
			}
			if err := manager.ScanOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			manager.Wait()
			reviewing := filepath.Join(filepath.Dir(filepath.Dir(goal)), "reviewing", filepath.Base(goal))
			if _, err := os.Stat(reviewing); err != nil {
				t.Fatalf("due checkpoint did not dispatch goal review: %v", err)
			}
		})
	}
}

func TestGoalActivationValidatesConfiguredCheckpoint(t *testing.T) {
	for _, test := range []struct {
		name, trigger string
		checkpoint    any
		remove        bool
	}{
		{name: "scheduled-missing", trigger: "scheduled", remove: true},
		{name: "scheduled-invalid", trigger: "scheduled", checkpoint: "next week"},
		{name: "settled-or-checkpoint-invalid", trigger: "all_round_tasks_settled_or_checkpoint", checkpoint: "next week"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, _, manager := workflowTestManager(t)
			goal := writeActiveGoalRound(t, cfg, test.trigger, time.Now().Add(time.Hour), "waiting")
			document, err := ReadDocument(goal)
			if err != nil {
				t.Fatal(err)
			}
			if test.remove {
				delete(document.FrontMatter, "next_review_at")
			} else {
				document.FrontMatter["next_review_at"] = test.checkpoint
			}
			if err := manager.validateGoalActivation(document); err == nil {
				t.Fatal("invalid checkpoint was accepted for activation")
			}
		})
	}
}

func TestGoalPlanningCheckpointRequiresReason(t *testing.T) {
	cfg, _, manager := workflowTestManager(t)
	goal := writeActiveGoalRound(t, cfg, "scheduled", time.Now().Add(time.Hour), "waiting")
	document, err := ReadDocument(goal)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.validateGoalPlanningTransition(document); err == nil || !strings.Contains(err.Error(), "checkpoint_reason") {
		t.Fatalf("missing checkpoint rationale error = %v", err)
	}
	document.FrontMatter["checkpoint_reason"] = "Recheck a bounded asynchronous rollout after ten minutes."
	if err := manager.validateGoalPlanningTransition(document); err != nil {
		t.Fatalf("reasoned checkpoint rejected: %v", err)
	}
	if err := WriteDocument(goal, document); err != nil {
		t.Fatal(err)
	}
	waits, err := manager.ScheduledCheckpoints(time.Now())
	if err != nil || len(waits) != 1 || waits[0].Reason == "" || waits[0].ID != documentID(document) {
		t.Fatalf("scheduled checkpoint status = %#v, %v", waits, err)
	}
}

func TestReviewQueuesIgnoreEarlierPhaseDates(t *testing.T) {
	cfg, fake, manager := workflowTestManager(t)
	future := time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	for index, route := range cfg.Orchestrator.Routes[:2] {
		base := filepath.Dir(cfg.Resolve(route.Source))
		path := filepath.Join(base, "review", route.Name+"-dated.md")
		document := Document{FrontMatter: map[string]any{
			"id": route.Name + "-dated", "title": "dated review", "status": "review",
			"created_at": time.Now().UTC().Format(time.RFC3339), "updated_at": time.Now().UTC().Format(time.RFC3339),
			"not_before": future, "next_dispatch_at": future, "next_review_at": future,
		}, Body: "# Dated review\n"}
		if err := WriteDocument(path, document); err != nil {
			t.Fatal(err)
		}
		phase := phaseTaskReview
		if index == 1 {
			phase = phaseGoalReview
		}
		if err := manager.scanPhaseQueue(context.Background(), route, filepath.Dir(path), filepath.Join(base, "reviewing"), phase); err != nil {
			t.Fatal(err)
		}
	}
	manager.Wait()
	if fake.calls != 2 {
		t.Fatalf("phase-eligible dated reviews dispatched %d times", fake.calls)
	}
}

func writeActiveGoalRound(t *testing.T, cfg config.Config, trigger string, checkpoint time.Time, taskStatuses ...string) string {
	t.Helper()
	goal, err := Create(cfg, "goals", "exercise goal review eligibility", "")
	if err != nil {
		t.Fatal(err)
	}
	document, err := ReadDocument(goal)
	if err != nil {
		t.Fatal(err)
	}
	goalID := documentID(document)
	document.FrontMatter["round"] = 1
	document.FrontMatter["status"] = "active"
	document.FrontMatter["review_trigger"] = trigger
	document.FrontMatter["next_review_at"] = checkpoint.UTC().Format(time.RFC3339)
	ids := make([]any, 0, len(taskStatuses))
	taskBase := filepath.Dir(cfg.Resolve(cfg.Orchestrator.Routes[0].Source))
	for index, status := range taskStatuses {
		task, createErr := CreateWithOptions(cfg, "tasks", "round task", "", CreateOptions{GoalID: goalID, GoalRound: 1})
		if createErr != nil {
			t.Fatal(createErr)
		}
		taskDocument, readErr := ReadDocument(task)
		if readErr != nil {
			t.Fatal(readErr)
		}
		ids = append(ids, documentID(taskDocument))
		target := filepath.Join(taskBase, status, strings.TrimSuffix(filepath.Base(task), ".md")+string(rune('a'+index))+".md")
		if err := moveDocument(task, target, status, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	document.FrontMatter["round_task_ids"] = ids
	if err := WriteDocument(goal, document); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(filepath.Dir(cfg.Resolve(cfg.Orchestrator.Routes[1].Source)), "active", filepath.Base(goal))
	if err := os.Rename(goal, active); err != nil {
		t.Fatal(err)
	}
	return active
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
