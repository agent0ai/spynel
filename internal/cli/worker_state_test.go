//go:build linux || darwin

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
	"github.com/agent0ai/spynel/internal/instance"
	"github.com/agent0ai/spynel/internal/shortid"
)

func TestWorkerConversationOwningState(t *testing.T) {
	completion := filepath.Join(t.TempDir(), "complete")
	t.Setenv("SPYNEL_TEST_WORKER_COMPLETIONS", completion)
	f := newWorkerAgentFixture(t)
	c := harness.WorkerConversation{Channel: "tui", Conversation: "owning-state", SourceMessageID: "alpha", ExecutionID: "synthetic-state-execution", Sources: []string{"alpha"}}
	if _, err := f.service.History.Append(c.Channel, c.Conversation, history.Entry{Role: "user", Content: "synthetic request", SourceMessageID: c.SourceMessageID, AcceptedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	execution, err := f.service.Orchestrator.AdmitConversation(t.Context(), c, "synthetic request", "synthetic request")
	if err != nil {
		t.Fatal(err)
	}
	identity := f.client.Info()
	t.Logf("state worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
	job, ok := f.service.Runtime.JobForSession(execution.Key)
	if !ok {
		t.Fatal("admitted job absent")
	}
	message := core.Message{Channel: c.Channel, Conversation: c.Conversation}
	check := func(active bool) {
		t.Helper()
		status, err := f.service.Status(message)
		if err != nil || status.HarnessState != "connected" || status.TurnActive != active {
			t.Errorf("status did not inspect the owning worker: connected=%s active=%v error=%v", status.HarnessState, status.TurnActive, err)
		}
		if strings.Contains(f.service.HarnessScreen(false).Status, "unavailable") || strings.Contains(f.service.WelcomeScreen().Subtitle, "harness is unavailable") {
			t.Error("configuration presentation read the inactive controller harness")
		}
		command := message
		command.Text = fmt.Sprintf("/job info %d", job.Number)
		var text string
		if err := f.service.Handle(t.Context(), command, func(e core.Event) { text += e.Text }); err != nil || !strings.Contains(text, "- Execution: `"+shortid.Display(execution.ThreadID)+"`") {
			t.Errorf("job details omitted the owning worker thread: %v", err)
		}
	}
	check(true)
	notificationID, err := f.service.Notify(t.Context(), c.Channel+"/"+c.Conversation, "synthetic active notification")
	if err != nil {
		t.Fatal(err)
	}
	checkNotification := func(id string) {
		t.Helper()
		data, err := os.ReadFile(f.service.History.Path(c.Channel, c.Conversation))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			var entry history.Entry
			if json.Unmarshal([]byte(line), &entry) == nil && entry.EventID == id {
				if entry.Role != "notification_pending" {
					t.Error("active worker notification bypassed pending interleave")
				}
				return
			}
		}
		t.Fatal("notification was not durably delivered")
	}
	checkNotification(notificationID)
	queued, err := f.service.Orchestrator.Outbox.Enqueue("synthetic-scheduled-notification", "manual", c.Channel+"/"+c.Conversation, "synthetic scheduled notification")
	if err != nil {
		t.Fatal(err)
	}
	scanCtx, scanCancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer scanCancel()
	if err := f.service.Orchestrator.ScanOnce(scanCtx); err != nil {
		t.Fatal("scheduled notification inspection held election ownership", err)
	}
	checkNotification(queued.ID)
	scanCancel()
	f.stop(t)
	if err := f.owner.Release(f.term); err != nil {
		t.Fatal(err)
	}
	old := f.client
	f.open(t)
	defer old.Close()
	if f.client.Info().Process != identity.Process {
		t.Fatal("state inspection replaced the native worker")
	}
	if _, err := old.Call(t.Context(), harness.WorkerRequest{Action: "session", Key: execution.Key}); err == nil {
		t.Fatal("obsolete controller inspected the worker session")
	}
	// Restore the same archive without starting the consumer, retaining terminal delivery.
	id, err := f.service.Runtime.TryBeginJobWithDetails(execution.Key, c.Channel, c.Conversation, "synthetic request", app.JobDetails{Kind: "conversation", WorkID: c.ExecutionID})
	if err != nil {
		t.Fatal(err)
	}
	restored, _ := f.service.Runtime.Job(id)
	if restored.Number != job.Number || restored.Generation != job.Generation {
		t.Fatal("inspection changed public job identity")
	}
	check(true)
	if err := os.WriteFile(completion+".1", nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitWorkerAgent(t, func() bool { return f.inspect(t)[0].Done })
	check(false)
	command := message
	command.Text = fmt.Sprintf("/job kill %d", job.Number)
	var text string
	if err := f.service.Handle(t.Context(), command, func(e core.Event) { text += e.Text }); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.service.Runtime.Job(id); !ok || !strings.Contains(text, "delivery") {
		t.Error("inactive worker kill discarded pending terminal delivery")
	}
	f.start(t)
	waitWorkerAgent(t, func() bool { return len(f.inspect(t)) == 0 && len(f.service.Runtime.Jobs()) == 0 })
	entries, _, err := f.service.History.Entries(c.Channel, c.Conversation)
	terminals := 0
	for _, entry := range entries {
		if entry.ExecutionID == c.ExecutionID && len(entry.Covers) > 0 {
			terminals++
		}
	}
	methods, readErr := os.ReadFile(f.log)
	if err != nil || readErr != nil || terminals != 1 || strings.Count(string(methods), "turn/start\n") != 1 {
		t.Fatal("inspection changed delivery or provider admission", err, readErr)
	}
}

func TestWorkerConversationStateUnavailable(t *testing.T) {
	f := newWorkerAgentFixture(t)
	message := core.Message{Channel: "tui", Conversation: "unknown-state"}
	if _, err := f.service.History.Append(message.Channel, message.Conversation, history.Entry{Role: "user", Content: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	f.client.Close()
	restore := dropWorkerReply(t, f.client.Info().Socket, "session", false, false)
	defer restore()
	if _, err := f.service.Status(message); err == nil {
		t.Error("unknown worker session was reported as idle")
	}
	if _, err := f.service.Notify(t.Context(), message.Channel+"/"+message.Conversation, "synthetic unknown notification"); err != nil {
		t.Fatal(err)
	}
	entries, _, err := f.service.History.Entries(message.Channel, message.Conversation)
	if err != nil || len(entries) != 1 {
		t.Error("unknown worker state committed a delivered notification", err)
	}
}

func TestWorkerServiceStartAndDetach(t *testing.T) {
	f := newWorkerAgentFixture(t)
	identity := f.client.Info()
	t.Logf("lifetime worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
	path, err := harness.ProcessOwnershipPath(f.cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	f.service.Harness.(*harness.Supervisor).SetProcessStartGuard(path, nil)
	if err := f.service.Start(t.Context()); err != nil {
		t.Error("bound service started a competing controller harness", err)
	}
	if local, ok := f.service.Harness.(harness.Availability); ok {
		if ready, _ := local.Available(); ready {
			t.Fatal("controller acquired the worker's provider ownership")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	f.cancel, f.done = cancel, make(chan error, 1)
	go func() { f.done <- runPrimaryOrchestrator(ctx, f.service) }()
	deadline := time.Now().Add(2 * time.Second)
	for f.service.Orchestrator.WorkStatus().NextHeartbeatAt.IsZero() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if f.service.Orchestrator.WorkStatus().NextHeartbeatAt.IsZero() {
		t.Error("primary readiness waited on the inactive controller harness")
	}
	f.stop(t)
	if _, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "inspect"}); err != nil || f.client.Info().Process != identity.Process {
		t.Fatal("service close stopped its independent worker", err)
	}
}

func TestWorkerNotificationTakeoverAfterInspection(t *testing.T) {
	f := newWorkerAgentFixture(t)
	message := core.Message{Channel: "tui", Conversation: "takeover-notice"}
	if _, err := f.service.History.Append(message.Channel, message.Conversation, history.Entry{Role: "user", Content: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	socket, target := f.client.Info().Socket, f.client.Info().Socket+".target"
	f.client.Close()
	if err := os.Rename(socket, target); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		_ = os.Rename(target, socket)
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", target)
	}}
	type successor struct {
		owner *instance.Election
		term  string
		err   error
	}
	replaced := make(chan successor, 1)
	proxy := &httputil.ReverseProxy{Director: func(r *http.Request) { r.URL.Scheme, r.URL.Host = "http", "worker" }, Transport: transport}
	proxy.ModifyResponse = func(_ *http.Response) error {
		result := successor{}
		result.err = f.owner.Release(f.term)
		if result.err == nil {
			result.owner, result.err = instance.New(f.cfg.StatePath())
		}
		if result.err == nil {
			result.term, result.err = result.owner.NewToken()
		}
		if result.err == nil {
			_, acquired, err := result.owner.TryAcquire("127.0.0.1:10002", result.term)
			result.err = err
			if !acquired && err == nil {
				result.err = fmt.Errorf("synthetic successor did not acquire ownership")
			}
		}
		replaced <- result
		return result.err
	}
	server := &http.Server{Handler: proxy}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	var once sync.Once
	restore := func() {
		once.Do(func() {
			_ = server.Close()
			<-done
			transport.CloseIdleConnections()
			_ = os.Remove(socket)
			if err := os.Rename(target, socket); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(restore)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := f.service.Notify(ctx, message.Channel+"/"+message.Conversation, "synthetic stale notification"); err != nil {
		t.Fatal(err)
	}
	var replacement successor
	select {
	case replacement = <-replaced:
	case <-ctx.Done():
		t.Fatal("notification did not inspect its worker")
	}
	if replacement.err != nil {
		t.Fatal(replacement.err)
	}
	f.owner, f.term = replacement.owner, replacement.term
	restore()
	f.client.Close()
	f.client, err = harness.OpenWorker(t.Context(), f.harnessConfig, f.owner.ID(), f.term)
	if err != nil {
		t.Fatal(err)
	}
	entries, _, err := f.service.History.Entries(message.Channel, message.Conversation)
	if err != nil || len(entries) != 1 {
		t.Fatal("obsolete controller committed a notification after inspection", err)
	}
	t.Logf("notification worker pid=%d start=%s generation=%s", f.client.Info().PID, f.client.Info().Process.StartTime, f.client.Info().Process.Generation)
}
