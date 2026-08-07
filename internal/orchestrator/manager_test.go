package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/extensions"
	"github.com/agent0ai/spynel/internal/workspace"
)

type fakeHarness struct {
	mu         sync.Mutex
	calls      int
	keys       []string
	threads    map[string]string
	active     map[string]bool
	beforeEmit func()
}

func newFakeRecipient() *fakeHarness {
	return &fakeHarness{threads: map[string]string{}, active: map[string]bool{}}
}

func (f *fakeHarness) Start(context.Context) error { return nil }
func (f *fakeHarness) Close() error                { return nil }
func (f *fakeHarness) ResetSession(key string) error {
	delete(f.threads, key)
	return nil
}
func (f *fakeHarness) ThreadID(key string) string { return f.threads[key] }
func (f *fakeHarness) IsActive(key string) bool   { return f.active[key] }
func (f *fakeHarness) Interrupt(_ context.Context, key string) (bool, error) {
	if !f.active[key] {
		return false, nil
	}
	f.active[key] = false
	return true, nil
}
func (f *fakeHarness) Send(_ context.Context, key, _ string, emit core.Emit) (string, bool, error) {
	f.mu.Lock()
	f.calls++
	f.keys = append(f.keys, key)
	thread := f.threads[key]
	if thread == "" {
		thread = "thread-" + key
		f.threads[key] = thread
	}
	f.mu.Unlock()
	if f.beforeEmit != nil {
		f.beforeEmit()
	}
	emit(core.Event{Kind: core.EventFinal, Text: "done", ThreadID: thread, Done: true})
	return thread, false, nil
}

func TestImplementationDoneMoveIsReconciledIntoIndependentReview(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	task, err := Create(cfg, "tasks", "move and release", "")
	if err != nil {
		t.Fatal(err)
	}
	working := filepath.Join(cfg.Resolve(cfg.Orchestrator.Routes[0].Working), filepath.Base(task))
	done := filepath.Join(filepath.Dir(cfg.Resolve(cfg.Orchestrator.Routes[0].Source)), "done", filepath.Base(task))
	fake := newFakeRecipient()
	var manager *Manager
	var moved sync.Once
	fake.beforeEmit = func() {
		moved.Do(func() {
			if err := os.Rename(working, done); err != nil {
				t.Errorf("move completed task: %v", err)
			}
			if err := manager.recoverStale(context.Background()); err != nil {
				t.Errorf("recover after completed move: %v", err)
			}
		})
	}
	manager = New(cfg, fake, extensions.Runner{Directory: filepath.Join(root, "missing")})
	started := 0
	finished := 0
	manager.JobStarted = func(sessionKey, description string) int {
		if sessionKey == "" || description == "" {
			t.Errorf("empty orchestrator job metadata: %q %q", sessionKey, description)
		}
		started++
		return 7
	}
	manager.JobFinished = func(id int) {
		if id != 7 {
			t.Errorf("finished job ID = %d, want 7", id)
		}
		finished++
	}
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	leases, err := manager.loadLeases()
	if err != nil || len(leases) != 1 || leases[0].Phase != phaseTaskReview {
		t.Fatalf("task did not enter review phase: %#v, %v", leases, err)
	}
	reviewing := filepath.Join(filepath.Dir(cfg.Resolve(cfg.Orchestrator.Routes[0].Source)), "reviewing", filepath.Base(task))
	if _, err := os.Stat(reviewing); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(done); !os.IsNotExist(err) {
		t.Fatalf("unreviewed task remained done: %v", err)
	}
	if started != 2 || finished != 2 {
		t.Fatalf("job lifecycle callbacks = %d started, %d finished", started, finished)
	}
}

func TestClaimLeasePreventsDuplicatesAndStaleLeaseRecovers(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(root, "spynel.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	task, err := Create(cfg, "tasks", "verify lease behavior", "")
	if err != nil {
		t.Fatal(err)
	}
	fake := newFakeRecipient()
	manager := New(cfg, fake, extensions.Runner{Directory: filepath.Join(root, "missing")})
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	working := filepath.Join(cfg.Resolve(cfg.Orchestrator.Routes[0].Working), filepath.Base(task))
	if _, err := os.Stat(working); err != nil {
		t.Fatalf("task was not claimed into working: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("dispatch calls = %d, want 1", fake.calls)
	}
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	if fake.calls != 1 {
		t.Fatalf("leased task was duplicated: %d calls", fake.calls)
	}
	leases, err := manager.loadLeases()
	if err != nil || len(leases) != 1 {
		t.Fatalf("leases = %#v, %v", leases, err)
	}
	lease := leases[0]
	lease.HeartbeatAt = time.Now().Add(-time.Hour)
	if err := manager.saveLease(lease); err != nil {
		t.Fatal(err)
	}
	fake.active[lease.SessionKey] = true
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	if fake.calls != 1 {
		t.Fatalf("active harness session received duplicate stale recovery: %d calls", fake.calls)
	}
	fake.active[lease.SessionKey] = false
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	if fake.calls != 2 {
		t.Fatalf("stale task was not recovered: %d calls", fake.calls)
	}
	recovered, err := manager.loadLease(lease.ID)
	if err != nil || recovered.RecoveryCount != 1 || recovered.State != "awaiting_transition" {
		t.Fatalf("recovered lease = %#v, %v", recovered, err)
	}
}

func TestFutureReviewTimeDefersDispatch(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	goal, err := Create(cfg, "goals", "review this later", "")
	if err != nil {
		t.Fatal(err)
	}
	document, err := ReadDocument(goal)
	if err != nil {
		t.Fatal(err)
	}
	document.FrontMatter["next_review_at"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if err := WriteDocument(goal, document); err != nil {
		t.Fatal(err)
	}
	fake := newFakeRecipient()
	manager := New(cfg, fake, extensions.Runner{Directory: filepath.Join(root, "missing")})
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	if fake.calls != 0 {
		t.Fatalf("future goal dispatched %d times", fake.calls)
	}
	if _, err := os.Stat(goal); err != nil {
		t.Fatalf("deferred goal moved unexpectedly: %v", err)
	}
}

func TestReviewTransitionAcceptRejectAndSelfReviewGuard(t *testing.T) {
	for _, test := range []struct {
		name, target, want string
		sameThread         bool
	}{{"accept", "done", "done", false}, {"reject", "todo", "todo", false}, {"self-review", "done", "todo", true}} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := workspace.Init(root, false); err != nil {
				t.Fatal(err)
			}
			cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
			route := cfg.Orchestrator.Routes[0]
			reviewDir := filepath.Join(filepath.Dir(cfg.Resolve(route.Source)), "review")
			path := filepath.Join(filepath.Dir(reviewDir), "reviewing", "review.md")
			doc := Document{FrontMatter: map[string]any{"id": "review-id", "title": "review", "status": "reviewing", "created_at": time.Now().UTC().Format(time.RFC3339), "updated_at": time.Now().UTC().Format(time.RFC3339), "notify": map[string]any{"enabled": false}}, Body: "# review\n"}
			if err := WriteDocument(path, doc); err != nil {
				t.Fatal(err)
			}
			thread := "review-thread"
			implementer := "implementation-thread"
			if test.sameThread {
				implementer = thread
			}
			manager := New(cfg, newFakeRecipient(), extensions.Runner{Directory: filepath.Join(root, "missing")})
			lease := Lease{ID: "lease", Route: "tasks", File: path, SessionKey: "review-session", Phase: phaseTaskReview, ThreadID: thread, ImplementerThread: implementer, StartedAt: time.Now(), HeartbeatAt: time.Now()}
			if err := manager.saveLease(lease); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(filepath.Dir(cfg.Resolve(route.Source)), test.target, filepath.Base(path))
			if err := os.Rename(path, target); err != nil {
				t.Fatal(err)
			}
			if err := manager.reconcileTransitions(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(filepath.Dir(cfg.Resolve(route.Source)), test.want, filepath.Base(path))
			if _, err := os.Stat(want); err != nil {
				t.Fatalf("wanted %s: %v", want, err)
			}
			if manager.leaseExists(lease.ID) {
				t.Fatal("terminal review retained lease")
			}
		})
	}
}

func TestReviewDispatchUsesFreshAttemptSessionAcrossRestart(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, "spynel.yaml"))
	route := cfg.Orchestrator.Routes[0]
	path := filepath.Join(filepath.Dir(cfg.Resolve(route.Source)), "review", "retry.md")
	if err := WriteDocument(path, Document{FrontMatter: map[string]any{"id": "retry", "title": "retry", "status": "review", "created_at": time.Now().UTC().Format(time.RFC3339), "updated_at": time.Now().UTC().Format(time.RFC3339)}, Body: "# retry\n"}); err != nil {
		t.Fatal(err)
	}
	first := New(cfg, newFakeRecipient(), extensions.Runner{Directory: filepath.Join(root, "missing")})
	if err := first.scanPhaseQueue(context.Background(), route, filepath.Dir(path), filepath.Join(filepath.Dir(filepath.Dir(path)), "reviewing"), phaseTaskReview); err != nil {
		t.Fatal(err)
	}
	first.Wait()
	leases, _ := first.loadLeases()
	if len(leases) != 1 || !strings.HasSuffix(leases[0].SessionKey, ":1") {
		t.Fatalf("first review lease = %#v", leases)
	}
	restarted := New(cfg, newFakeRecipient(), extensions.Runner{Directory: filepath.Join(root, "missing")})
	if err := restarted.scanPhaseQueue(context.Background(), route, filepath.Dir(path), filepath.Join(filepath.Dir(filepath.Dir(path)), "reviewing"), phaseTaskReview); err != nil {
		t.Fatal(err)
	}
	restarted.Wait()
	leases, _ = restarted.loadLeases()
	if len(leases) != 1 {
		t.Fatalf("restart duplicated review: %#v", leases)
	}
}
