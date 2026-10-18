package orchestrator

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/extensions"
	"github.com/agent0ai/spynel/internal/harness"
)

func workerWorkflowFixture(t *testing.T, route, phase string) (string, string, harness.WorkerWorkflow, *Manager) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	work := harness.WorkerWorkflow{WorkID: "synthetic", Route: route, Phase: phase, ClaimAttempt: 3, OwnerID: "synthetic-owner"}
	work.LeaseID = leaseID(route+":"+phase, work.WorkID)
	work.ClaimID = work.LeaseID
	work.File = filepath.Join(root, ".spynel", route, phaseClaimedStatus(phase), "synthetic.md")
	key := phaseSessionKey(route, work.WorkID, phase, work.ClaimAttempt)
	if err := os.MkdirAll(filepath.Dir(work.File), 0700); err != nil {
		t.Fatal(err)
	}
	document := Document{FrontMatter: map[string]any{
		"id": work.WorkID, "status": phaseClaimedStatus(phase), "attempt": 2,
		"provider_iterations": 4, "custom": "preserved",
	}, Body: "# Synthetic claimed work\n"}
	document.FrontMatter[phaseAttemptField(phase)] = work.ClaimAttempt
	if err := WriteDocument(work.File, document); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Root = root
	manager := New(cfg, nil, extensions.Runner{})
	if err := manager.saveLease(Lease{
		ID: work.LeaseID, ClaimID: work.ClaimID, OwnerID: work.OwnerID, File: work.File,
		Route: route, Phase: phase, ClaimAttempt: work.ClaimAttempt, SessionKey: key, State: "processing",
	}); err != nil {
		t.Fatal(err)
	}
	return root, key, work, manager
}

func TestReserveWorkerTurnExactClaim(t *testing.T) {
	for _, scenario := range []struct{ route, phase string }{
		{"tasks", phaseTaskImplementation}, {"tasks", phaseTaskReview},
		{"goals", phaseGoalPlanning}, {"goals", phaseGoalReview},
	} {
		t.Run(scenario.phase, func(t *testing.T) {
			root, key, work, _ := workerWorkflowFixture(t, scenario.route, scenario.phase)
			result, err := ApplyWorkerWorkflow(root, key, work, "reserve")
			if err != nil || result.ProviderIterations != 5 || result.FirstAssignedAt.IsZero() || result.FirstAssignedAt.After(time.Now()) {
				t.Fatalf("reservation = %+v, %v", result, err)
			}
			document, err := ReadDocument(work.File)
			if err != nil || numberValue(document.FrontMatter["provider_iterations"]) != 5 || numberValue(document.FrontMatter[phaseAttemptField(scenario.phase)]) != 3 || document.FrontMatter["custom"] != "preserved" || document.Body != "# Synthetic claimed work\n" {
				t.Fatalf("reservation damaged durable work: %+v, %v", document, err)
			}
			wantAttempts := 0
			if scenario.route == "tasks" {
				wantAttempts = numberValue(document.FrontMatter["attempt"])
			}
			if result.ImplementationAttempts != wantAttempts {
				t.Fatal("reservation confused implementation and review attempts")
			}
		})
	}
}

func TestWorkerControlActionsValidateAndReserveExactClaim(t *testing.T) {
	root, key, work, manager := workerWorkflowFixture(t, "tasks", phaseTaskImplementation)
	lease, _ := manager.loadLease(work.LeaseID)
	lease.State, lease.LastError = "awaiting_transition", "previous diagnostic"
	if err := manager.saveLease(lease); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"validate", "continue", "reserve-control"} {
		result, err := ApplyWorkerWorkflow(root, key, work, action)
		want := 4
		if action == "reserve-control" {
			want++
		}
		if err != nil || result.ProviderIterations != want {
			t.Fatal(action, result, err)
		}
	}
	lease, _ = manager.loadLease(work.LeaseID)
	if lease.State != "processing" || lease.LastError != "" {
		t.Fatal("continuation did not prepare the exact durable execution")
	}
	lease.State = "cancelled"
	if err := manager.saveLease(lease); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"reserve", "validate", "continue", "reserve-control", "unknown"} {
		if _, err := ApplyWorkerWorkflow(root, key, work, action); err == nil {
			t.Fatal("cancelled claim admitted", action)
		}
	}
	if err := manager.recoverStale(t.Context()); err != nil {
		t.Fatal(err)
	}
	document, err := ReadDocument(work.File)
	if err != nil || numberValue(document.FrontMatter["provider_iterations"]) != 5 {
		t.Fatal("refused control changed provider reservations", err)
	}
}

func TestWorkerWorkflowCancellationIntentFencesAdmission(t *testing.T) {
	root, key, work, manager := workerWorkflowFixture(t, "tasks", phaseTaskImplementation)
	if _, err := ApplyWorkerWorkflow(root, key, work, "cancel"); err != nil {
		t.Fatal(err)
	}
	lease, err := manager.loadLease(work.LeaseID)
	if err != nil || !lease.CancellationRequested || lease.State != "processing" {
		t.Fatal("intent did not preserve the admitted execution state", err)
	}
	for _, action := range []string{"reserve", "validate", "continue", "reserve-control"} {
		if _, err := ApplyWorkerWorkflow(root, key, work, action); err == nil {
			t.Fatal("paused workflow admitted work", action)
		}
	}
	changed := work
	changed.OwnerID += "-changed"
	if _, err := ApplyWorkerWorkflow(root, key, changed, "restore-cancellation"); err == nil {
		t.Fatal("changed owner cleared cancellation intent")
	}
	if _, err := ApplyWorkerWorkflow(root, key, work, "restore-cancellation"); err != nil {
		t.Fatal(err)
	}
	lease, _ = manager.loadLease(work.LeaseID)
	if lease.CancellationRequested || lease.State != "processing" {
		t.Fatal("clean refusal did not restore only its intent")
	}
	if _, err := ApplyWorkerWorkflow(root, key, work, "validate"); err != nil {
		t.Fatal("restored execution remained fenced", err)
	}
	if _, err := ApplyWorkerWorkflow(root, key, work, "cancel"); err != nil {
		t.Fatal(err)
	}
	if err := manager.recoverStale(t.Context()); err != nil {
		t.Fatal(err)
	}
	lease, _ = manager.loadLease(work.LeaseID)
	document, err := ReadDocument(work.File)
	if !lease.CancellationRequested || lease.State != "cancelled" || err != nil || numberValue(document.FrontMatter["provider_iterations"]) != 4 {
		t.Fatal("recovery redispatched a cancellation whose worker is gone", err)
	}
}

func TestReserveWorkerTurnRejectsChangedOrUnsafeClaim(t *testing.T) {
	for _, scenario := range []string{"wrong-key", "wrong-route", "wrong-path", "wrong-claim", "changed-owner", "changed-work-id", "changed-attempt", "terminal-lease", "changed-document", "moved-document", "corrupt-lease", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			root, key, work, manager := workerWorkflowFixture(t, "tasks", phaseTaskImplementation)
			original := work.File
			before, err := os.ReadFile(original)
			if err != nil {
				t.Fatal(err)
			}
			lease, err := manager.loadLease(work.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "wrong-key":
				key += "changed"
			case "wrong-route":
				work.Route = "unknown"
			case "wrong-path":
				work.File = filepath.Join(root, "outside.md")
			case "wrong-claim":
				work.ClaimID += "changed"
			case "changed-owner":
				lease.OwnerID += "changed"
			case "changed-work-id":
				lease.WorkID = "changed"
			case "changed-attempt":
				lease.ClaimAttempt++
			case "terminal-lease":
				lease.State = "awaiting_transition"
			case "changed-document":
				document, _ := ReadDocument(original)
				document.FrontMatter["attempt"] = 9
				if err := WriteDocument(original, document); err != nil {
					t.Fatal(err)
				}
				before, _ = os.ReadFile(original)
			case "moved-document":
				if err := os.Rename(original, original+".moved"); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(original, original+".real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(original+".real", original); err != nil {
					t.Skip("symlink unavailable")
				}
			}
			if err := manager.saveLease(lease); err != nil {
				t.Fatal(err)
			}
			if scenario == "corrupt-lease" {
				if err := os.WriteFile(manager.leasePath(work.LeaseID), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if result, err := ApplyWorkerWorkflow(root, key, work, "reserve"); err == nil || result.ProviderIterations != 0 {
				t.Fatalf("changed claim reserved work: %+v, %v", result, err)
			}
			if scenario == "moved-document" {
				original += ".moved"
			}
			after, err := os.ReadFile(original)
			if err != nil || string(after) != string(before) {
				t.Fatal("rejected claim changed durable work")
			}
		})
	}
}
