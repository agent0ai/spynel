//go:build linux || darwin

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/instance"
	"github.com/agent0ai/spynel/internal/updater"
)

// Signal complete decoding, so cancellation does not depend on handler timing.
type workerAdmissionBody struct {
	*bytes.Reader
	read chan struct{}
	once sync.Once
}

func (b *workerAdmissionBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		b.once.Do(func() { close(b.read) })
	}
	return n, err
}

func TestWorkerCancelledRequestDoesNotAcquireOwnershipOrApply(t *testing.T) {
	for _, action := range []string{"send", "followup", "control", "interrupt", "shutdown", "inspect"} {
		for _, contended := range []bool{false, true} {
			t.Run(action+map[bool]string{false: "/already-cancelled", true: "/contended-deadline"}[contended], func(t *testing.T) {
				target := &supervisorHarness{name: "codex", followUp: FollowUpSteer, active: map[string]bool{}, emits: map[string]core.Emit{}}
				registry := NewRegistry()
				registry.Register("codex", func(HarnessConfig) (Harness, error) { return target, nil })
				supervisor := NewSupervisor(registry, HarnessConfig{Name: "codex"})
				if err := supervisor.Start(t.Context()); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = supervisor.Close() })
				if _, _, err := supervisor.Send(t.Context(), "original", "original", nil); err != nil {
					t.Fatal(err)
				}
				owner, err := instance.New(filepath.Join(t.TempDir(), ".spynel"))
				if err != nil {
					t.Fatal(err)
				}
				term, err := owner.NewToken()
				if err != nil {
					t.Fatal(err)
				}
				if _, ok, err := owner.TryAcquire("127.0.0.1:10001", term); err != nil || !ok {
					t.Fatal("test ownership unavailable", err)
				}
				t.Cleanup(func() { _ = owner.Release(term) })
				before := time.Now().Add(-time.Minute)
				host := &workerHost{supervisor: supervisor, election: owner, info: WorkerInfo{ID: "synthetic-worker"}, token: "synthetic-token", directory: t.TempDir(), stop: make(chan struct{}), lastSeen: before, next: 1, executions: map[uint64]*workerExecution{}}
				t.Cleanup(func() {
					for _, execution := range host.executions {
						_ = execution.file.Close()
					}
				})
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
				defer cancel()
				release := func() {}
				if contended {
					host.operations.Lock()
					release = sync.OnceFunc(host.operations.Unlock)
					defer release()
				} else {
					cancel()
				}
				request := WorkerRequest{WorkerID: host.info.ID, InstanceID: owner.ID(), TermToken: term, Action: action, Key: "original", Sequence: 1, Prompt: "expired", Control: &WorkerControl{ID: "expired-control"}}
				data, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				body := &workerAdmissionBody{Reader: bytes.NewReader(data), read: make(chan struct{})}
				req := httptest.NewRequest(http.MethodPost, "/worker", body).WithContext(ctx)
				req.Header.Set("Authorization", "Bearer "+host.token)
				response := httptest.NewRecorder()
				finished := make(chan struct{})
				go func() { host.serve(response, req); close(finished) }()
				<-body.read
				select {
				case <-finished:
				case <-time.After(time.Second):
					t.Error("worker request ignored cancellation before operation admission")
					release()
					<-finished
				}
				release()
				if response.Code != http.StatusRequestTimeout || !host.lastSeen.Equal(before) || host.next != 1 || len(host.executions) != 0 || !target.IsActive("original") {
					t.Errorf("expired request applied effects: status=%d freshness_changed=%t next=%d retained=%d active=%t", response.Code, !host.lastSeen.Equal(before), host.next, len(host.executions), target.IsActive("original"))
				}
				select {
				case <-host.stop:
					t.Error("expired shutdown stopped the worker")
				default:
				}
			})
		}
	}
}

func TestWorkerNativeExpiredRequestsPreserveAdmittedExecution(t *testing.T) {
	t.Setenv("SPYNEL_TEST_CODEX_STALL_CONTROL", "1")
	f := newWorkerFixture(t, "codex", "codex-worker")
	t.Logf("owned fixture worker pid=%d start=%s generation=%s", f.info.PID, f.info.Process.StartTime, f.info.Process.Generation)
	f.client.Timeout = 8 * time.Second
	work := &WorkerWorkflow{WorkID: "synthetic", OwnerID: "synthetic-owner", ClaimAttempt: 1}
	first := f.mustCall(t, WorkerRequest{Action: "send", Sequence: 1, Key: "synthetic/request-admission", Prompt: "original", Workflow: work}).Executions[0]
	control := WorkerRequest{Action: "control", Sequence: 1, Key: first.Key, Prompt: "guidance", Workflow: work, Control: &WorkerControl{ID: "admitted-holding-control"}}
	holder := make(chan WorkerReply, 1)
	go func() { reply, _ := f.call(control); holder <- reply }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		seen := false
		for _, record := range readFixtureRecords(t, f.log) {
			seen = seen || record.Method == "turn/steer"
		}
		if seen {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native control never acquired worker admission")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, request := range []WorkerRequest{
		{Action: "send", Sequence: 2, Key: first.Key, Prompt: "expired", Workflow: work},
		{Action: "interrupt", Key: first.Key},
	} {
		request.WorkerID, request.InstanceID, request.TermToken = f.info.ID, f.owner.ID(), f.term
		data, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://worker/worker", bytes.NewReader(data))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+f.token)
		response, err := f.client.Do(req)
		cancel()
		if response != nil {
			_ = response.Body.Close()
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("contended %s did not expire: %v", request.Action, err)
		}
	}
	select {
	case reply := <-holder:
		if reply.Error != context.DeadlineExceeded.Error() {
			t.Fatal("admitted control lost its server-owned lifetime", reply.Error)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("admitted control did not release worker admission")
	}
	// The bounded control has released the gate; expired handlers must not run
	// after it, even though their caller already observed an honest timeout.
	for range 3 {
		reply := f.mustCall(t, WorkerRequest{Action: "inspect"})
		if reply.NextSequence != 2 || len(reply.Executions) != 1 || reply.Executions[0].Done || reply.Executions[0].Cancelled || reply.Executions[0].ThreadID != first.ThreadID || reply.Executions[0].LastCursor <= first.LastCursor {
			t.Fatal("expired worker requests changed the retained turn or admission sequence")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, renewed, err := f.owner.Renew(f.term); err != nil || !renewed {
		t.Fatal("worker request cancellation lost renewal", err)
	}
	f.replaceOwner(t)
	if err := updater.VerifyProcess(f.info.Process); err != nil {
		t.Fatal("worker identity changed", err)
	}
	if duplicate := f.mustCall(t, control); duplicate.Control == nil || !duplicate.Control.Duplicate {
		t.Fatal("already-admitted ambiguous control lost deduplication")
	}
	starts, interrupts := 0, 0
	for _, record := range readFixtureRecords(t, f.log) {
		if record.Method == "turn/start" {
			starts++
		}
		if record.Method == "turn/interrupt" {
			interrupts++
		}
	}
	if starts != 1 || interrupts != 0 {
		t.Fatalf("expired requests reached provider: starts=%d interrupts=%d", starts, interrupts)
	}
	if !f.mustCall(t, WorkerRequest{Action: "interrupt", Key: first.Key}).Interrupted {
		t.Fatal("successor could not explicitly interrupt the original turn")
	}
	t.Logf("native worker pid=%d start=%s generation=%s retained original execution through expired HTTP waiters, server-owned control timeout, renewal, replacement and explicit interruption", f.info.PID, f.info.Process.StartTime, f.info.Process.Generation)
}

func TestWorkerRequestExpiresWhileWaitingForElection(t *testing.T) {
	owner, err := instance.New(filepath.Join(t.TempDir(), ".spynel"))
	if err != nil {
		t.Fatal(err)
	}
	term, err := owner.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := owner.TryAcquire("127.0.0.1:10001", term); err != nil || !ok {
		t.Fatal("test ownership unavailable", err)
	}
	t.Cleanup(func() { _ = owner.Release(term) })
	entered, unlock, released := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(released)
		_, _ = owner.RunForOwner(owner.ID(), term, func() error { close(entered); <-unlock; return nil })
	}()
	<-entered
	release := sync.OnceFunc(func() { close(unlock); <-released })
	defer release()
	before := time.Now().Add(-time.Minute)
	host := &workerHost{election: owner, info: WorkerInfo{ID: "synthetic-worker"}, token: "synthetic-token", stop: make(chan struct{}), lastSeen: before}
	data, err := json.Marshal(WorkerRequest{WorkerID: host.info.ID, InstanceID: owner.ID(), TermToken: term, Action: "shutdown"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/worker", bytes.NewReader(data)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+host.token)
	response := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() { host.serve(response, req); close(finished) }()
	// Taking the operation gate proves the handler passed that cancellation
	// check and is now behind the separately held election mutation lock.
	deadline := time.Now().Add(time.Second)
	for {
		probeCtx, probeCancel := context.WithTimeout(t.Context(), time.Millisecond)
		err := host.operations.LockContext(probeCtx)
		probeCancel()
		if err != nil {
			break
		}
		host.operations.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("request did not reach election acquisition")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	release()
	<-finished
	if response.Code != http.StatusServiceUnavailable || !host.lastSeen.Equal(before) {
		t.Fatal("request applied after cancellation during election acquisition")
	}
	select {
	case <-host.stop:
		t.Fatal("cancelled election waiter stopped the worker")
	default:
	}
}
