//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/instance"
	"github.com/agent0ai/spynel/internal/orchestrator"
	"github.com/agent0ai/spynel/internal/updater"
	"github.com/agent0ai/spynel/internal/workspace"
)

type workerAgentFixture struct {
	service       *app.Service
	client        *harness.WorkerClient
	owner         *instance.Election
	term          string
	cfg           config.Config
	harnessConfig harness.HarnessConfig
	log           string
	cancel        context.CancelFunc
	done          chan error
}

func newWorkerAgentFixture(t *testing.T) *workerAgentFixture {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Orchestrator.IntervalSec, cfg.Orchestrator.SemanticHeartbeatMinutes = 60, 1
	f := &workerAgentFixture{cfg: cfg, log: filepath.Join(root, "synthetic-methods")}
	t.Setenv("SPYNEL_TEST_WORKER_PROVIDER", "1")
	t.Setenv("SPYNEL_TEST_WORKER_EVIDENCE", f.log)
	executable, _ := os.Executable()
	f.harnessConfig = harness.HarnessConfig{Name: "codex", Command: executable, Cwd: root, Sandbox: "read-only", ApprovalPolicy: "never", SessionsFile: filepath.Join(root, "sessions.json")}
	f.open(t)
	t.Cleanup(func() {
		f.stop(t)
		_, err := f.client.Call(context.Background(), harness.WorkerRequest{Action: "shutdown"})
		if err != nil {
			t.Error(err)
		}
		f.client.Close()
		_ = f.owner.Release(f.term)
		waitWorkerAgent(t, func() bool { _, err := os.Stat(filepath.Dir(f.client.Info().Socket)); return os.IsNotExist(err) })
	})
	return f
}

func (f *workerAgentFixture) open(t *testing.T) {
	t.Helper()
	var err error
	f.owner, err = instance.New(f.cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	f.term, err = f.owner.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.owner.TryAcquire("127.0.0.1:10001", f.term); err != nil || !ok {
		t.Fatal("owner acquisition", err)
	}
	f.client, err = harness.OpenWorker(t.Context(), f.harnessConfig, f.owner.ID(), f.term)
	if err != nil {
		t.Fatal(err)
	}
	f.service = app.New(f.cfg, harness.NewSupervisor(harness.NewBuiltinRegistry(), f.harnessConfig))
	f.service.Orchestrator.Worker = f.client
	f.service.Orchestrator.SetPrimaryOwned(true)
}

func (f *workerAgentFixture) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	f.cancel, f.done = cancel, make(chan error, 1)
	manager := f.service.Orchestrator
	go func() { f.done <- manager.Run(ctx) }()
}

func (f *workerAgentFixture) stop(t *testing.T) {
	t.Helper()
	if f.cancel != nil {
		f.cancel()
		select {
		case <-f.done:
		case <-time.After(5 * time.Second):
			t.Fatal("manager did not detach")
		}
		f.cancel = nil
	}
	f.service.Orchestrator.Wait()
	if err := f.service.Close(); err != nil {
		t.Error(err)
	}
}

func waitWorkerAgent(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !predicate() {
		if time.Now().After(deadline) {
			t.Fatal("background worker assertion timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *workerAgentFixture) inspect(t *testing.T) []harness.WorkerExecution {
	t.Helper()
	reply, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "inspect"})
	if err != nil {
		t.Fatal(err)
	}
	return reply.Executions
}

func (f *workerAgentFixture) trigger(t *testing.T, want bool) {
	t.Helper()
	var started bool
	var err error
	waitWorkerAgent(t, func() bool {
		started, err = f.service.Orchestrator.TriggerSemanticHeartbeat(t.Context())
		return err == nil || !strings.Contains(err.Error(), "scheduler is unavailable")
	})
	if err != nil || started != want {
		t.Fatalf("manual heartbeat = %v, %v; want %v", started, err, want)
	}
}

func TestWorkerAgentManagerRestoresHeartbeatJobAndFixedDelay(t *testing.T) {
	f := newWorkerAgentFixture(t)
	var delivery harness.WorkerDelivery
	reached := make(chan struct{})
	var once sync.Once
	f.service.Orchestrator.JobDelivery = func(id int, event harness.WorkerDelivery) error {
		if err := f.service.Runtime.RecordJobDelivery(id, event); err != nil {
			return err
		}
		once.Do(func() { delivery = event; close(reached) })
		return errors.New("synthetic lost controller after archive sync")
	}
	f.start(t)
	f.trigger(t, true)
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("ordinary agent failed actual admission")
	}
	first := f.inspect(t)[0]
	if first.Agent == nil || len(first.Agent.ID) < 8 || first.Agent.Deadline.Sub(first.Agent.StartedAt) != 5*time.Minute {
		t.Fatal("ordinary agent descriptor/deadline invalid")
	}
	jobs := f.service.Runtime.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d", len(jobs))
	}
	job := jobs[0]
	if job.Kind != "heartbeat" || job.WorkID != first.Agent.ID || job.ProviderIterations != 1 {
		t.Fatal("background job identity/accounting changed")
	}
	f.trigger(t, false)
	f.stop(t)
	identity := f.client.Info()
	obsolete := f.client
	defer obsolete.Close()
	if err := f.owner.Release(f.term); err != nil {
		t.Fatal(err)
	}
	f.open(t)
	if f.client.Info().PID != identity.PID || f.client.Info().Process.StartTime != identity.Process.StartTime || f.client.Info().Process.Generation != identity.Process.Generation || updater.VerifyProcess(identity.Process) != nil {
		t.Fatal("replacement changed worker identity")
	}
	t.Logf("retained ordinary worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
	if _, err := obsolete.Call(t.Context(), harness.WorkerRequest{Action: "interrupt", Key: first.Key}); err == nil {
		t.Fatal("obsolete controller retained control")
	}
	admit := f.service.Orchestrator.JobStarted
	f.service.Orchestrator.JobStarted = func(orchestrator.Lease, string, time.Time, int, int) (int, error) {
		return 0, errors.New("synthetic job admission failure")
	}
	if err := f.service.Orchestrator.ScanOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.service.Orchestrator.Wait()
	if f.inspect(t)[0].CommittedCursor != 0 {
		t.Fatal("failed admission consumed events")
	}
	f.service.Orchestrator.JobStarted = admit
	f.start(t)
	waitWorkerAgent(t, func() bool {
		current := f.inspect(t)
		return len(current) == 1 && current[0].CommittedCursor >= delivery.Cursor
	})
	current := f.inspect(t)[0]
	if *current.Agent != *first.Agent {
		t.Fatal("attachment reset ordinary agent lifetime")
	}
	jobs = f.service.Runtime.Jobs()
	if len(jobs) != 1 || jobs[0].Number != job.Number || jobs[0].StableID != job.StableID || jobs[0].Generation != job.Generation {
		t.Fatal("attachment allocated another job")
	}
	f.trigger(t, false)
	for i := 0; i < 3; i++ {
		if err := f.service.Orchestrator.ScanOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	releasedAt := time.Now()
	if _, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "interrupt", Key: first.Key}); err != nil {
		t.Fatal(err)
	}
	waitWorkerAgent(t, func() bool {
		return len(f.inspect(t)) == 0 && len(f.service.Runtime.Jobs()) == 0 && f.service.Orchestrator.WorkStatus().HeartbeatState == "scheduled"
	})
	next := f.service.Orchestrator.WorkStatus().NextHeartbeatAt
	if next.Before(releasedAt.Add(time.Minute)) || next.After(time.Now().Add(time.Minute)) {
		t.Fatal("fixed delay was not measured from terminal release")
	}
	data, err := os.ReadFile(f.log)
	if err != nil || strings.Count(string(data), "turn/start\n") != 1 {
		t.Fatal("attachment sent another provider turn", err)
	}
	_, output, err := f.service.Runtime.ArchivedJob(job.Number)
	if err != nil || delivery.Event.Text != "" && strings.Count(output, delivery.Event.Text) != 1 {
		t.Fatal("archive replay duplicated output", err)
	}
}

// Drop exactly the Send reply at a private Unix proxy. The accepted case
// forwards first; the rejected case proves no provider admission happened.
func dropWorkerAgentSendReply(t *testing.T, socket string, accepted bool) func() {
	t.Helper()
	target := socket + ".target"
	if err := os.Rename(socket, target); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", target)
	}}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		var request harness.WorkerRequest
		if json.Unmarshal(body, &request) != nil {
			return
		}
		if request.Action == "send" && !accepted {
			connection, _, _ := w.(http.Hijacker).Hijack()
			_ = connection.Close()
			return
		}
		upstream, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://worker/worker", bytes.NewReader(body))
		upstream.Header = r.Header.Clone()
		response, err := transport.RoundTrip(upstream)
		if err != nil {
			http.Error(w, "synthetic proxy failure", 502)
			return
		}
		defer response.Body.Close()
		if request.Action == "send" {
			_, _ = io.Copy(io.Discard, response.Body)
			connection, _, _ := w.(http.Hijacker).Hijack()
			_ = connection.Close()
			return
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	})}
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
	return restore
}

func TestWorkerAgentAmbiguousSendReconcilesSameJob(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		name := "not-admitted"
		if accepted {
			name = "admitted-reply-lost"
		}
		t.Run(name, func(t *testing.T) {
			f := newWorkerAgentFixture(t)
			f.client.Close()
			restore := dropWorkerAgentSendReply(t, f.client.Info().Socket, accepted)
			f.start(t)
			f.trigger(t, true)
			waitWorkerAgent(t, func() bool {
				jobs := f.service.Runtime.Jobs()
				return len(jobs) == 1 && jobs[0].Execution == app.JobDegraded
			})
			job := f.service.Runtime.Jobs()[0]
			restore()
			f.client.Close()
			if err := f.service.Orchestrator.ScanOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			if accepted {
				waitWorkerAgent(t, func() bool { current := f.inspect(t); return len(current) == 1 && current[0].CommittedCursor > 0 })
				jobs := f.service.Runtime.Jobs()
				if len(jobs) != 1 || jobs[0].StableID != job.StableID {
					t.Fatal("ambiguous send duplicated job")
				}
				f.trigger(t, false)
				if _, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "interrupt", Key: f.inspect(t)[0].Key}); err != nil {
					t.Fatal(err)
				}
			}
			waitWorkerAgent(t, func() bool { return len(f.service.Runtime.Jobs()) == 0 && len(f.inspect(t)) == 0 })
			data, _ := os.ReadFile(f.log)
			want := 0
			if accepted {
				want = 1
			}
			if strings.Count(string(data), "turn/start\n") != want {
				t.Fatal("ambiguous send repeated provider admission")
			}
		})
	}
}

func TestWorkerAgentNotificationAttachmentKeepsIndependentIdentity(t *testing.T) {
	f := newWorkerAgentFixture(t)
	file := f.cfg.StatePath("tasks", "done", "synthetic.md")
	if err := orchestrator.WriteDocument(file, orchestrator.Document{FrontMatter: map[string]any{"id": "synthetic-task", "status": "done"}}); err != nil {
		t.Fatal(err)
	}
	id, err := core.NewSourceMessageID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	agent := &harness.WorkerAgent{ID: id, Kind: "notification", File: file, StartedAt: now, Deadline: now.Add(time.Minute)}
	if _, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "send", Sequence: 1, Key: "orchestrator:notification:synthetic", Prompt: "synthetic notification", Agent: agent}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := f.service.Orchestrator.ScanOnce(ctx); err != nil {
		t.Fatal(err)
	}
	waitWorkerAgent(t, func() bool { return len(f.service.Runtime.Jobs()) == 1 })
	job := f.service.Runtime.Jobs()[0]
	if job.Kind != "notification" || job.WorkID != id || job.ProviderIterations != 1 {
		t.Fatal("notification inherited task identity")
	}
	if _, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "interrupt", Key: f.inspect(t)[0].Key}); err != nil {
		t.Fatal(err)
	}
	waitWorkerAgent(t, func() bool { return len(f.inspect(t)) == 0 && len(f.service.Runtime.Jobs()) == 0 })
}
