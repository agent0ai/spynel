package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/extensions"
	"github.com/agent0ai/spynel/internal/workspace"
)

type heartbeatHarness struct {
	mu                 sync.Mutex
	prompt             string
	final              string
	active             atomic.Bool
	asyncRelease       chan struct{}
	ignoreCancellation bool
	err                error
}

func (*heartbeatHarness) Start(context.Context) error { return nil }
func (*heartbeatHarness) Close() error                { return nil }
func (h *heartbeatHarness) Interrupt(context.Context, string) (bool, error) {
	return h.active.Load(), nil
}
func (*heartbeatHarness) ResetSession(string) error { return nil }
func (*heartbeatHarness) ThreadID(string) string    { return "heartbeat-thread" }
func (h *heartbeatHarness) IsActive(string) bool    { return h.active.Load() }

func (h *heartbeatHarness) Send(ctx context.Context, _ string, prompt string, emit core.Emit) (string, bool, error) {
	h.mu.Lock()
	h.prompt = prompt
	h.mu.Unlock()
	if h.err != nil {
		return "", false, h.err
	}
	h.active.Store(true)
	emit(core.Event{Kind: core.EventStatus, Text: "Codex turn started"})
	finish := func() {
		h.active.Store(false)
		text := h.final
		if text == "" {
			text = "Completed workflow inspection."
		}
		emit(core.Event{Kind: core.EventFinal, Text: text, FinalText: &text, Done: true})
	}
	if h.asyncRelease != nil {
		go func() {
			select {
			case <-h.asyncRelease:
				finish()
			case <-ctx.Done():
				if h.ignoreCancellation {
					<-h.asyncRelease
					finish()
				}
			}
		}()
		return "heartbeat-thread", false, nil
	}
	finish()
	return "heartbeat-thread", false, nil
}

func newHeartbeatManager(t *testing.T, target *heartbeatHarness) *Manager {
	t.Helper()
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	manager := New(cfg, target, extensions.Runner{Directory: filepath.Join(root, "missing")})
	manager.SetPrimaryOwned(true)
	return manager
}

func TestSemanticHeartbeatIgnoresProviderProseWithoutFrameworkState(t *testing.T) {
	target := &heartbeatHarness{final: "Codex turn started, then completed with arbitrary prose."}
	manager := newHeartbeatManager(t, target)
	manager.runSemanticHeartbeatOnce(context.Background())

	target.mu.Lock()
	prompt := target.prompt
	target.mu.Unlock()
	if !strings.Contains(prompt, "final-response payload") || !strings.Contains(prompt, "tasks") || !strings.Contains(prompt, "command /trigger orchestrator") || !strings.Contains(prompt, "agents do the work") || !strings.Contains(prompt, "spynel notify --recent-authorized") || !strings.Contains(prompt, "durable pending notification deliveries") || !strings.Contains(prompt, "exact originating channel") || !strings.Contains(prompt, "explicitly authorized fallback") || !strings.Contains(prompt, "never infer a recipient") || !strings.Contains(prompt, "expired after 48 hours") || !strings.Contains(prompt, "do not retry, reactivate, or alter them") {
		t.Fatalf("heartbeat prompt omitted worker/CLI guidance: %q", prompt)
	}
	if strings.Contains(prompt, "HEARTBEAT_ACTION_COMMAND") || strings.Contains(prompt, "spynel.semantic-heartbeat/v1") || strings.Contains(prompt, "Finish with only one JSON") {
		t.Fatalf("heartbeat prompt retained a response protocol: %q", prompt)
	}
	if strings.Contains(prompt, "--origin 'tui/") || strings.Contains(prompt, "Change only the example `Hello there`") {
		t.Fatalf("heartbeat guidance was incorrectly bound to a task notification origin: %q", prompt)
	}
	for _, name := range []string{"semantic-heartbeat.json", "semantic-heartbeat-health.json", "semantic-heartbeat-actions", "semantic-heartbeat-alert-scan.json", "semantic-waiting-fallbacks"} {
		if _, err := os.Stat(manager.Config.StatePath("runtime", name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("framework created retired heartbeat state %q: %v", name, err)
		}
	}
}

func TestSemanticHeartbeatSanitizesRetiredWorkspaceOverride(t *testing.T) {
	target := &heartbeatHarness{}
	manager := newHeartbeatManager(t, target)
	legacy := `# Custom heartbeat

Keep this workspace-specific inspection rule.

This audit is read-only: do not edit or move workflow documents and do not start jobs. The primary runtime\u2014not this agent\u2014owns due reminder selection and progress journaling.

Propose notifications only to the affected workflow's authorized ` + "`notify.origin`" + `; never switch channels.

Record findings with {{HEARTBEAT_ACTION_COMMAND}} and finish with exactly one audit outcome. Absence of an audit action is failure.
`
	path := manager.Config.StatePath("prompts", "heartbeat.md")
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	prompt, err := manager.semanticHeartbeatPrompt("heartbeat-test", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "Keep this workspace-specific inspection rule.") {
		t.Fatalf("heartbeat sanitizer removed unrelated customization: %q", prompt)
	}
	for _, obsolete := range []string{"This audit is read-only", "primary runtime\u2014not this agent", "only to the affected workflow's authorized", "HEARTBEAT_ACTION_COMMAND", "exactly one audit outcome", "Absence of an audit action"} {
		if strings.Contains(prompt, obsolete) {
			t.Fatalf("heartbeat prompt retained obsolete rule %q: %q", obsolete, prompt)
		}
	}
	if !strings.Contains(prompt, "agents do the work") || !strings.Contains(prompt, "spynel notify --recent-authorized") || !strings.Contains(prompt, "append the successful send") {
		t.Fatalf("heartbeat prompt omitted current worker boundary: %q", prompt)
	}
}

func TestSemanticHeartbeatAdmissionFailureDoesNotReachProvider(t *testing.T) {
	target := &heartbeatHarness{}
	manager := newHeartbeatManager(t, target)
	manager.JobStarted = func(Lease, string, time.Time, int, int) (int, error) {
		return 0, errors.New("durable job counter unavailable")
	}

	manager.runSemanticHeartbeatOnce(context.Background())
	target.mu.Lock()
	prompt := target.prompt
	target.mu.Unlock()
	if prompt != "" || target.active.Load() {
		t.Fatalf("failed job admission reached provider: prompt=%q active=%t", prompt, target.active.Load())
	}
	if manager.heartbeatProviderActive.Load() {
		t.Fatal("failed job admission retained the heartbeat provider fence")
	}
}

func TestSemanticHeartbeatWaitsForAsynchronousTerminal(t *testing.T) {
	target := &heartbeatHarness{asyncRelease: make(chan struct{})}
	manager := newHeartbeatManager(t, target)
	done := make(chan struct{})
	go func() {
		manager.runSemanticHeartbeatOnce(context.Background())
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("heartbeat treated asynchronous admission as completion")
	case <-time.After(20 * time.Millisecond):
	}
	close(target.asyncRelease)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("heartbeat did not finish after provider terminal event")
	}
}

func TestSemanticHeartbeatJobWaitsForDelayedProviderTerminalAndCapturesCompleteStream(t *testing.T) {
	target := &heartbeatHarness{asyncRelease: make(chan struct{})}
	manager := newHeartbeatManager(t, target)
	recorder := newOrdinaryJobRecorder(manager)
	done := make(chan struct{})
	go func() {
		manager.runSemanticHeartbeatOnce(context.Background())
		close(done)
	}()
	deadline := time.After(time.Second)
	for !target.active.Load() {
		select {
		case <-deadline:
			t.Fatal("heartbeat provider was not admitted")
		case <-time.After(time.Millisecond):
		}
	}
	_, finished, events := recorder.snapshot()
	if finished != 0 || len(events) != 1 || events[0].Text != "Codex turn started" {
		t.Fatalf("heartbeat job ended at asynchronous admission: finished=%d events=%#v", finished, events)
	}
	target.mu.Lock()
	prompt := target.prompt
	target.mu.Unlock()
	if !strings.Contains(prompt, "command /trigger orchestrator") || !strings.Contains(prompt, "Markdown task and goal documents") {
		t.Fatalf("heartbeat agent did not receive its full role prompt: %q", prompt)
	}
	close(target.asyncRelease)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("heartbeat did not finish after provider terminal")
	}
	started, finished, events := recorder.snapshot()
	if started != 1 || finished != 1 || len(events) != 2 || events[0].Text != "Codex turn started" || events[1].Kind != core.EventFinal {
		t.Fatalf("heartbeat archived lifecycle = started=%d finished=%d events=%#v", started, finished, events)
	}
}

func TestSemanticHeartbeatTimeoutRetainsNonOverlapFenceUntilRelease(t *testing.T) {
	target := &heartbeatHarness{asyncRelease: make(chan struct{}), ignoreCancellation: true}
	manager := newHeartbeatManager(t, target)
	manager.heartbeatTimeout = 20 * time.Millisecond
	manager.runSemanticHeartbeatOnce(context.Background())
	if !manager.semanticHeartbeatProviderInFlight() {
		t.Fatal("timed-out provider lost its non-overlap fence")
	}
	term := manager.beginSemanticHeartbeatTerm()
	if manager.runSemanticHeartbeatOnceForTerm(context.Background(), term) {
		t.Fatal("overlapping heartbeat provider was admitted")
	}
	manager.endSemanticHeartbeatTerm(term)
	close(target.asyncRelease)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, ok := manager.waitForSemanticHeartbeatProviderRelease(ctx); !ok {
		t.Fatal("provider fence did not release")
	}
}

func TestSemanticHeartbeatPromptUsesHeartbeatPrefixAndInstructions(t *testing.T) {
	target := &heartbeatHarness{}
	manager := newHeartbeatManager(t, target)
	settings := manager.harnessSettings()
	settings.HeartbeatAgentPrefix = "/audit"
	manager.ApplyHarnessConfig(settings)
	if err := os.WriteFile(manager.Config.StatePath("instructions", "agent-heartbeat.md"), []byte("heartbeat-only-rule"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager.runSemanticHeartbeatOnce(context.Background())
	target.mu.Lock()
	prompt := target.prompt
	target.mu.Unlock()
	if !strings.HasPrefix(prompt, "/audit ") || !strings.Contains(prompt, "heartbeat-only-rule") {
		t.Fatalf("heartbeat prefix/instructions missing: %q", prompt)
	}
}

func TestScheduledHeartbeatSelectsCustomPromptAndRepairsStaleInspectionRule(t *testing.T) {
	target := &heartbeatHarness{asyncRelease: make(chan struct{})}
	manager := newHeartbeatManager(t, target)
	const legacy = "Keep the custom workspace inspection rule.\n\nInspect only bounded, relevant evidence and body-free state for notification triage. Never read question/message bodies, recipient or native message identifiers, secrets, full conversation histories, unrelated repositories, or unbounded logs."
	if err := os.WriteFile(manager.Config.StatePath("prompts", "heartbeat.md"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	ticks := make(chan time.Time, 1)
	manager.heartbeatTicks = ticks
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); manager.runSemanticHeartbeat(ctx) }()
	defer func() { cancel(); <-done; manager.jobs.Wait() }()
	ticks <- time.Now()
	deadline := time.After(5 * time.Second)
	var prompt string
	for prompt == "" {
		target.mu.Lock()
		prompt = target.prompt
		target.mu.Unlock()
		select {
		case <-deadline:
			t.Fatal("scheduled heartbeat did not reach provider")
		default:
		}
		if prompt == "" {
			time.Sleep(time.Millisecond)
		}
	}
	for _, obsolete := range []string{"Never read question/message bodies", "body-free state for notification triage"} {
		if strings.Contains(prompt, obsolete) {
			t.Errorf("scheduled prompt retains %q", obsolete)
		}
	}
	for _, want := range []string{
		"Keep the custom workspace inspection rule.", "Inspect only bounded, relevant evidence and bounded original-thread evidence for notification triage.",
		"privately", "first prerequisite question", "freshness", "existing authorization", "independent work",
		"failed or absent attempt", "relevant answer", "unrelated later message", "exact enabled notify.origin",
		"explicitly owner-established same-operator route", "queue acceptance from send and receipt",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("scheduled custom prompt omitted %q", want)
		}
	}
	data, err := os.ReadFile(manager.Config.StatePath("prompts", "heartbeat.md"))
	if err != nil || string(data) != legacy {
		t.Fatal("rendering rewrote the workspace override")
	}
	close(target.asyncRelease)
}

func TestSemanticHeartbeatBlockedAdmissionWaitsForRelease(t *testing.T) {
	for _, manual := range []bool{false, true} {
		for _, change := range []string{"unchanged", "interval", "disabled"} {
			name := "scheduled/" + change
			if manual {
				name = "manual/" + change
			}
			t.Run(name, func(t *testing.T) {
				target := &heartbeatHarness{asyncRelease: make(chan struct{}), ignoreCancellation: true}
				manager := newHeartbeatManager(t, target)
				recorder := newOrdinaryJobRecorder(manager)
				ticks := make(chan time.Time, 1)
				manager.heartbeatTicks = ticks
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan struct{})
				go func() { defer close(done); manager.runSemanticHeartbeat(ctx) }()
				var release sync.Once
				defer func() {
					release.Do(func() { close(target.asyncRelease) })
					cancel()
					<-done
					manager.jobs.Wait()
				}()
				wait := func(predicate func() bool) {
					t.Helper()
					deadline := time.Now().Add(3 * time.Second)
					for !predicate() {
						if time.Now().After(deadline) {
							t.Fatal("heartbeat scheduler did not progress")
						}
						time.Sleep(time.Millisecond)
					}
				}
				wait(func() bool { return !manager.WorkStatus().NextHeartbeatAt.IsZero() })
				// Another retained provider becomes active after the timer is armed.
				providerDone := make(chan struct{})
				manager.heartbeatCommit.Lock()
				term := manager.heartbeatTerm
				manager.heartbeatCommit.Unlock()
				go func() { defer close(providerDone); manager.runSemanticHeartbeatOnceForTerm(ctx, term) }()
				defer func() {
					release.Do(func() { close(target.asyncRelease) })
					<-providerDone
				}()
				wait(func() bool { return target.active.Load() })
				if manual {
					if started, err := manager.TriggerSemanticHeartbeat(ctx); err != nil || started {
						t.Fatalf("overlapping manual admission = %v, %v", started, err)
					}
				} else {
					ticks <- time.Now()
				}
				// WorkStatus also hides the deadline while a provider is active.
				wait(func() bool {
					manager.heartbeatStatusMu.RLock()
					defer manager.heartbeatStatusMu.RUnlock()
					return manager.heartbeatNext.IsZero()
				})
				cfg := manager.runtimeSnapshot()
				if change == "interval" {
					cfg.Orchestrator.SemanticHeartbeatMinutes = 2
				} else if change == "disabled" {
					cfg.Orchestrator.SemanticHeartbeatMinutes = 0
				}
				if change != "unchanged" {
					manager.ApplyRuntimeConfig(cfg)
					wait(func() bool {
						return manager.heartbeatAppliedGeneration.Load() == manager.heartbeatConfigGeneration.Load()
					})
				}
				releasedAt := time.Now()
				release.Do(func() { close(target.asyncRelease) })
				<-providerDone
				if change == "disabled" {
					cancel()
					<-done
					if status := manager.WorkStatus(); status.HeartbeatState != "disabled" || !status.NextHeartbeatAt.IsZero() {
						t.Fatal("release rearmed a disabled heartbeat")
					}
				} else {
					wait(func() bool { return manager.WorkStatus().NextHeartbeatAt.After(releasedAt) })
					delay := time.Duration(cfg.Orchestrator.SemanticHeartbeatMinutes) * time.Minute
					next := manager.WorkStatus().NextHeartbeatAt
					if next.Before(releasedAt.Add(delay)) || next.After(time.Now().Add(delay)) {
						t.Fatalf("release did not preserve the configured fixed delay: released=%s next=%s now=%s delay=%s", releasedAt.Format(time.RFC3339Nano), next.Format(time.RFC3339Nano), time.Now().Format(time.RFC3339Nano), delay)
					}
				}
				if started, finished, _ := recorder.snapshot(); started != 1 || finished != 1 {
					t.Fatalf("blocked admission duplicated work: started=%d finished=%d", started, finished)
				}
			})
		}
	}
}
