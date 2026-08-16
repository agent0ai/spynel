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
	if !strings.Contains(prompt, "final-response payload") || !strings.Contains(prompt, "tasks") || !strings.Contains(prompt, "command /trigger orchestrator") || !strings.Contains(prompt, "agents do the work") || !strings.Contains(prompt, "spynel notify --recent-authorized") {
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
	for _, obsolete := range []string{"This audit is read-only", "primary runtime\u2014not this agent", "notify.origin", "HEARTBEAT_ACTION_COMMAND", "exactly one audit outcome", "Absence of an audit action"} {
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
