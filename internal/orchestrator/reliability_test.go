package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/extensions"
	"github.com/agent0ai/spynel/internal/workspace"
)

func reliabilityManager(t *testing.T) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Extensions.Enabled = false
	return New(cfg, newFakeRecipient(), extensions.Runner{}), root
}

func TestTransitionCandidatesAreIsolated(t *testing.T) {
	for _, scenario := range []string{"malformed-and-valid", "wrong-id", "wrong-status", "ambiguous", "missing"} {
		t.Run(scenario, func(t *testing.T) {
			m, _ := reliabilityManager(t)
			file, err := Create(m.Config, "tasks", "transition", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := m.ScanOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			m.Wait()
			leases, err := m.loadLeases()
			if err != nil || len(leases) != 1 {
				t.Fatalf("leases: %+v, %v", leases, err)
			}
			lease := leases[0]
			doc, err := ReadDocument(lease.File)
			if err != nil {
				t.Fatal(err)
			}
			candidate := m.Config.StatePath("tasks", "review", filepath.Base(file))
			doc.FrontMatter["status"] = "review"
			if scenario == "wrong-id" {
				doc.FrontMatter["id"] = "unrelated"
			}
			if scenario == "wrong-status" {
				doc.FrontMatter["status"] = "done"
			}
			if scenario != "missing" {
				if err := WriteDocument(candidate, doc); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Remove(lease.File); err != nil {
				t.Fatal(err)
			}
			if scenario == "malformed-and-valid" {
				if err := os.WriteFile(file, []byte("---\nstatus:"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "ambiguous" {
				doc.FrontMatter["status"] = "waiting"
				if err := WriteDocument(m.Config.StatePath("tasks", "waiting", filepath.Base(file)), doc); err != nil {
					t.Fatal(err)
				}
			}
			var logs []string
			m.Log = func(line string) { logs = append(logs, line) }
			if err := m.reconcileTransitions(t.Context()); err != nil {
				t.Fatalf("isolated candidate stopped reconciliation: %v", err)
			}
			valid := scenario == "malformed-and-valid"
			if m.leaseExists(lease.ID) == valid {
				t.Fatal("valid transition was not retired, or unsafe lease was discarded")
			}
			if len(logs) == 0 {
				t.Fatal("unsafe candidate was not diagnosed")
			}
			if _, err := Create(m.Config, "tasks", "unrelated work", ""); err != nil {
				t.Fatal(err)
			}
			// Unrelated implementation must still dispatch through the ordinary scan.
			m.Config.Harness.Reviews = config.TaskReviewsAlways
			if err := m.ScanOnce(t.Context()); err != nil {
				t.Fatalf("unrelated scan blocked: %v", err)
			}
			m.Wait()
			fake := m.Harness.(*fakeHarness)
			found := false
			for _, prompt := range fake.prompts {
				found = found || strings.Contains(prompt, "unrelated-work")
			}
			if !found {
				t.Fatal("unrelated work did not dispatch")
			}
			if scenario != "missing" && !valid {
				if _, err := os.Stat(candidate); err != nil {
					t.Fatal("candidate was deleted", err)
				}
			}
		})
	}
}

func TestWaitForIdleSettlesWithoutDispatchingReview(t *testing.T) {
	m, _ := reliabilityManager(t)
	file, err := Create(m.Config, "tasks", "one shot", "")
	if err != nil {
		t.Fatal(err)
	}
	fake := m.Harness.(*fakeHarness)
	fake.beforeEmit = func() {
		name := filepath.Base(file)
		if err := moveDocument(m.Config.StatePath("tasks", "working", name), m.Config.StatePath("tasks", "review", name), "review", time.Now().UTC()); err != nil {
			t.Error(err)
		}
	}
	var finished atomic.Int32
	m.JobStarted = func(Lease, string, time.Time, int, int) (int, error) { return 1, nil }
	m.JobFinished = func(int) { finished.Add(1) }
	if err := m.ScanOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := m.WaitForIdle(ctx); err != nil {
		t.Fatal(err)
	}
	leases, err := m.loadLeases()
	if err != nil || len(leases) != 0 || finished.Load() != 1 || fake.calls != 1 {
		t.Fatalf("settlement: leases=%+v error=%v finished=%d sends=%d", leases, err, finished.Load(), fake.calls)
	}
	if err := m.WaitForIdle(ctx); err != nil || finished.Load() != 1 {
		t.Fatal("bookkeeping repeated", err)
	}
}

func TestWaitForIdleAwaitsNotificationsAndHonorsCancellation(t *testing.T) {
	for _, cancelWait := range []bool{false, true} {
		t.Run(map[bool]string{false: "terminal", true: "cancelled"}[cancelWait], func(t *testing.T) {
			m, _ := reliabilityManager(t)
			file, err := CreateWithOptions(m.Config, "tasks", "one-shot notification", "", CreateOptions{Notify: true, Origin: "cli/synthetic", Outcomes: []string{"failed"}})
			if err != nil {
				t.Fatal(err)
			}
			m.NotificationContext = func(Origin) (string, error) { return "synthetic context", nil }
			admitted, release := make(chan struct{}), make(chan struct{})
			h := &leaseLifecycleHarness{fakeHarness: newFakeRecipient()}
			h.send = func(ctx context.Context, key, _ string, emit core.Emit) (string, bool, error) {
				if strings.HasPrefix(key, "orchestrator:notification:") {
					close(admitted)
					select {
					case <-release:
						emit(core.Event{Kind: core.EventFinal, Done: true})
						return "notification", false, nil
					case <-ctx.Done():
						return "", false, ctx.Err()
					}
				}
				name := filepath.Base(file)
				if err := moveDocument(m.Config.StatePath("tasks", "working", name), m.Config.StatePath("tasks", "failed", name), "failed", time.Now().UTC()); err != nil {
					t.Error(err)
				}
				emit(core.Event{Kind: core.EventFinal, Done: true})
				return "implementation", false, nil
			}
			m.Harness = h
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			if err := m.ScanOnce(ctx); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- m.WaitForIdle(ctx) }()
			select {
			case <-admitted:
			case err := <-done:
				t.Fatalf("returned before notification admission: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			select {
			case err := <-done:
				t.Fatalf("returned before notification completed: %v", err)
			default:
			}
			if cancelWait {
				cancel()
			} else {
				close(release)
			}
			select {
			case err := <-done:
				if cancelWait != (err != nil) {
					t.Fatal("unexpected wait result", err)
				}
			case <-time.After(time.Second):
				t.Fatal("idle wait did not return")
			}
			m.Wait()
		})
	}
}

func TestMalformedClaimsDoNotBlockRecovery(t *testing.T) {
	for _, mode := range []string{"orphan", "stale", "interrupted", "identity"} {
		t.Run(mode, func(t *testing.T) {
			m, _ := reliabilityManager(t)
			path := m.Config.StatePath("tasks", "working", "broken.md")
			state, source := "processing", ""
			if mode == "interrupted" {
				source = m.Config.StatePath("tasks", "todo", "broken.md")
				state = "claiming"
			}
			badPath := path
			if source != "" {
				badPath = source
			}
			bad := []byte("---\nstatus:")
			if mode == "identity" {
				bad = []byte("---\nid: another-document\nstatus: working\n---\nUntouched\n")
			}
			if err := os.WriteFile(badPath, bad, 0600); err != nil {
				t.Fatal(err)
			}
			lease := Lease{ID: "broken-lease", WorkID: "broken", Route: "tasks", File: path, SourceFile: source, State: state, Phase: phaseTaskImplementation, ClaimAttempt: 1, OwnerID: "previous-owner", SessionKey: "broken-session", StartedAt: time.Now().Add(-time.Hour)}
			if mode != "orphan" {
				if err := m.saveLease(lease); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Create(m.Config, "tasks", "unrelated recovery", ""); err != nil {
				t.Fatal(err)
			}
			if err := m.ScanOnce(t.Context()); err != nil {
				t.Fatal("invalid claim blocked scan", err)
			}
			m.Wait()
			if m.Harness.(*fakeHarness).calls != 1 {
				t.Fatal("invalid claim dispatched or unrelated work stalled")
			}
			after, err := os.ReadFile(badPath)
			if err != nil || string(after) != string(bad) {
				t.Fatal("invalid document was changed", err)
			}
			if mode != "orphan" {
				current, err := m.loadLease(lease.ID)
				if err != nil || current.OwnerID != lease.OwnerID {
					t.Fatal("invalid claim was discarded or adopted", err)
				}
			}
		})
	}
}

func TestWaitForIdleDoesNotDispatchNextGoalPhase(t *testing.T) {
	m, _ := reliabilityManager(t)
	path := m.Config.StatePath("goals", "planning", "settled-review.md")
	if err := WriteDocument(path, Document{FrontMatter: map[string]any{"id": "goal", "status": "planning"}}); err != nil {
		t.Fatal(err)
	}
	lease := Lease{ID: "goal-review", WorkID: "goal", Route: "goals", File: m.Config.StatePath("goals", "reviewing", filepath.Base(path)), State: "awaiting_transition", Phase: phaseGoalReview, SessionKey: "goal-review"}
	if err := m.saveLease(lease); err != nil {
		t.Fatal(err)
	}
	if err := m.WaitForIdle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if m.leaseExists(lease.ID) || m.Harness.(*fakeHarness).calls != 0 {
		t.Fatal("one-shot settlement dispatched goal planning")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("next phase document lost", err)
	}
}

func TestNotificationDeadlineUsesReloadedSnapshotAndKeepsAdmittedContext(t *testing.T) {
	m, path := notificationTestManager(t, config.TaskNotificationsDecide, nil)
	store := config.NewStore(m.Config)
	var calls atomic.Int32
	h := &notificationActionHarness{}
	h.action = func(ctx context.Context, _ string) error {
		before, ok := ctx.Deadline()
		want := 2 * time.Minute
		if calls.Add(1) == 2 {
			want = 10 * time.Minute
		}
		if !ok || time.Until(before) > want || time.Until(before) < want-time.Second {
			t.Errorf("dispatch deadline %v, want %v", before, want)
		}
		updated, err := store.Update(func(next *config.Config) error {
			_, err := config.SetSetting(next, "orchestrator.notification_timeout_seconds", "600")
			return err
		})
		if err != nil {
			t.Error(err)
			return err
		}
		m.ApplyRuntimeConfig(updated)
		after, _ := ctx.Deadline()
		if !after.Equal(before) {
			t.Error("settings changed admitted deadline")
		}
		return nil
	}
	m.Harness = h
	for attempt := 1; attempt <= 2; attempt++ {
		m.runTaskNotificationAgent(t.Context(), Lease{ID: "deadline", Route: "tasks", Phase: phaseTaskImplementation, ClaimAttempt: attempt}, "done", path)
	}
	if calls.Load() != 2 {
		t.Fatal("notification dispatches", calls.Load())
	}
}
