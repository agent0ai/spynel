//go:build linux || darwin

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/instance"
	"github.com/agent0ai/spynel/internal/updater"
)

type workerFixture struct {
	info    WorkerInfo
	token   string
	owner   *instance.Election
	term    string
	client  *http.Client
	command *exec.Cmd
	log     string
	state   string
	config  HarnessConfig
}

func newWorkerFixture(t *testing.T, name, mode string) *workerFixture {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	provider, root, log := portableHarnessFixture(t, mode)
	state := filepath.Join(root, ".spynel")
	owner, err := instance.New(state)
	if err != nil {
		t.Fatal(err)
	}
	term, _ := owner.NewToken()
	if _, acquired, err := owner.TryAcquire("127.0.0.1:10001", term); err != nil || !acquired {
		t.Fatalf("acquire worker test owner: %v", err)
	}
	token, _ := owner.NewToken()
	bootstrap := WorkerBootstrap{Protocol: WorkerProtocol, Token: token, InstanceID: owner.ID(), TermToken: term, Harness: HarnessConfig{
		Name: name, Command: provider, Cwd: root, ApprovalPolicy: "plan", Sandbox: "read-only", SessionsFile: filepath.Join(root, "sessions.json"),
	}}
	data, err := json.Marshal(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "__worker")
	command.Stdin = bytes.NewReader(data)
	command.Stderr = os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	fixture := &workerFixture{token: token, owner: owner, term: term, command: command, log: log, state: state, config: bootstrap.Harness}
	fixture.client = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", fixture.info.Socket)
	}}, Timeout: 5 * time.Second}
	t.Cleanup(func() {
		_, _ = fixture.call(WorkerRequest{Action: "shutdown"})
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("worker cleanup: %v", err)
			}
		case <-time.After(10 * time.Second):
			_ = command.Process.Kill()
			<-done
			t.Error("worker did not join its providers")
		}
		fixture.client.CloseIdleConnections()
		_ = fixture.owner.Release(fixture.term)
		if _, err := os.Stat(filepath.Dir(fixture.info.Socket)); !os.IsNotExist(err) {
			t.Error("worker left its private endpoint or journals")
		}
	})
	ready := make(chan error, 1)
	go func() { ready <- json.NewDecoder(stdout).Decode(&fixture.info) }()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("worker readiness: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not become ready")
	}
	if fixture.info.Protocol != WorkerProtocol || fixture.info.PID != command.Process.Pid || fixture.info.ID == "" {
		t.Fatal("worker readiness identity is invalid")
	}
	if info, err := os.Lstat(fixture.info.Socket); err != nil || info.Mode().Perm() != 0600 || info.Mode()&os.ModeSocket == 0 {
		t.Fatal("worker endpoint is not a private socket")
	}
	return fixture
}

func (f *workerFixture) call(request WorkerRequest) (WorkerReply, int) {
	request.WorkerID, request.InstanceID, request.TermToken = f.info.ID, f.owner.ID(), f.term
	return f.rawCall(request, f.token)
}

func (f *workerFixture) rawCall(request WorkerRequest, token string) (WorkerReply, int) {
	data, _ := json.Marshal(request)
	req, _ := http.NewRequest(http.MethodPost, "http://worker/worker", bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := f.client.Do(req)
	if err != nil {
		return WorkerReply{Error: err.Error()}, 0
	}
	defer response.Body.Close()
	var reply WorkerReply
	if response.StatusCode == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(&reply); err != nil {
			reply.Error = err.Error()
		}
	}
	return reply, response.StatusCode
}

func (f *workerFixture) mustCall(t *testing.T, request WorkerRequest) WorkerReply {
	t.Helper()
	reply, status := f.call(request)
	if status != http.StatusOK || reply.Error != "" {
		t.Fatalf("worker %s: status %d error %q", request.Action, status, reply.Error)
	}
	return reply
}

func (f *workerFixture) replaceOwner(t *testing.T) {
	t.Helper()
	if err := f.owner.Release(f.term); err != nil {
		t.Fatal(err)
	}
	owner, err := instance.New(f.state)
	if err != nil {
		t.Fatal(err)
	}
	term, _ := owner.NewToken()
	if _, acquired, err := owner.TryAcquire("127.0.0.1:10002", term); err != nil || !acquired {
		t.Fatalf("replacement owner: %v", err)
	}
	f.owner, f.term = owner, term
	f.client.CloseIdleConnections()
}

func TestWorkerRetainsFullAdapterAcrossControllerTerms(t *testing.T) {
	for _, scenario := range []struct{ name, mode string }{
		{"codex", "codex-worker"}, {"claude-code", "claude-interrupt"}, {"pi", "pi-interrupt"}, {"acp", "acp-interrupt"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newWorkerFixture(t, scenario.name, scenario.mode)
			identity, err := updater.ProcessRecord(fixture.info.PID)
			if err != nil || !identity.HarnessWorker || identity.StartTime == "" {
				t.Fatalf("worker has no native start identity: %v", err)
			}
			send := WorkerRequest{Action: "send", Sequence: 1, Key: "synthetic/execution", Prompt: "synthetic bounded worker test"}
			first := fixture.mustCall(t, send).Executions[0]
			if first.Error != "" || first.Done {
				t.Fatalf("provider did not remain active: %#v", first)
			}
			fixture.mustCall(t, send)
			var after uint64
			var oldRequest WorkerRequest
			for restart := 0; restart < 3; restart++ {
				oldRequest = WorkerRequest{WorkerID: fixture.info.ID, InstanceID: fixture.owner.ID(), TermToken: fixture.term, Action: "interrupt", Key: send.Key}
				fixture.replaceOwner(t)
				if _, status := fixture.rawCall(oldRequest, fixture.token); status != http.StatusForbidden {
					t.Fatalf("obsolete controller could interrupt worker: %d", status)
				}
				current := fixture.mustCall(t, WorkerRequest{Action: "inspect"})
				if current.NextSequence != 2 || len(current.Executions) != 1 || current.Executions[0].Done || current.Executions[0].ThreadID != first.ThreadID {
					t.Fatalf("attachment dispatched or lost a live turn: %#v", current)
				}
				time.Sleep(50 * time.Millisecond)
				page := fixture.mustCall(t, WorkerRequest{Action: "events", Sequence: 1, After: after})
				for _, event := range page.Events {
					if event.Cursor != after+1 {
						t.Fatal("worker replay skipped or duplicated a cursor")
					}
					after = event.Cursor
				}
			}
			currentIdentity, err := updater.ProcessRecord(fixture.info.PID)
			if err != nil || currentIdentity.StartTime != identity.StartTime || currentIdentity.Generation != identity.Generation || currentIdentity.Executable != identity.Executable {
				t.Fatal("attachment replaced the actual worker")
			}
			if err := syscall.Kill(fixture.info.PID, 0); err != nil {
				t.Fatal("worker process exited during attachment")
			}
			wrong := WorkerRequest{WorkerID: fixture.info.ID + "wrong", InstanceID: fixture.owner.ID(), TermToken: fixture.term, Action: "shutdown"}
			if _, status := fixture.rawCall(wrong, fixture.token); status != http.StatusConflict {
				t.Fatal("wrong worker identity was admitted")
			}
			wrong.WorkerID = fixture.info.ID
			if _, status := fixture.rawCall(wrong, "wrong credential"); status != http.StatusUnauthorized {
				t.Fatal("wrong endpoint credential was admitted")
			}
			if reply, _ := fixture.call(WorkerRequest{Action: "ack", Sequence: 1, After: after}); reply.Error == "" {
				t.Fatal("active execution was prematurely retired")
			}
			if !fixture.mustCall(t, WorkerRequest{Action: "interrupt", Key: send.Key}).Interrupted {
				t.Fatal("new controller could not cancel retained adapter")
			}
			deadline := time.Now().Add(5 * time.Second)
			var terminal bool
			for !terminal {
				page := fixture.mustCall(t, WorkerRequest{Action: "events", Sequence: 1, After: after})
				for _, event := range page.Events {
					if event.Cursor != after+1 {
						t.Fatal("terminal replay order changed")
					}
					after = event.Cursor
					terminal = terminal || event.Event.Done && !event.Event.Continues
				}
				if time.Now().After(deadline) {
					t.Fatal("provider cancellation did not terminate its original stream")
				}
				time.Sleep(10 * time.Millisecond)
			}
			fixture.mustCall(t, WorkerRequest{Action: "ack", Sequence: 1, After: after})
			fixture.mustCall(t, WorkerRequest{Action: "ack", Sequence: 1, After: after})
			if reply, _ := fixture.call(send); reply.Error == "" {
				t.Fatal("retired admission could dispatch a duplicate turn")
			}
			if scenario.name == "codex" {
				starts, effects := 0, 0
				for _, record := range readFixtureRecords(t, fixture.log) {
					if record.Method == "turn/start" {
						starts++
					}
					if record.Kind == "effect" {
						effects++
						if err := syscall.Kill(record.PID, 0); err != nil {
							t.Fatal("original provider process did not survive")
						}
					}
				}
				if starts != 1 || effects != 1 || after < 5 {
					t.Fatalf("provider work was duplicated or made no progress: starts=%d effects=%d events=%d", starts, effects, after)
				}
			}
		})
	}
}

func TestWorkerReplaysCompletionAfterDetachedDelivery(t *testing.T) {
	fixture := newWorkerFixture(t, "codex", "codex-lifecycle")
	fixture.mustCall(t, WorkerRequest{Action: "send", Sequence: 1, Key: "synthetic/completion", Prompt: "synthetic terminal delivery"})
	if err := fixture.owner.Release(fixture.term); err != nil {
		t.Fatal(err)
	}
	fixture.client.CloseIdleConnections()
	time.Sleep(150 * time.Millisecond)
	fixture.replaceOwner(t)
	page := fixture.mustCall(t, WorkerRequest{Action: "events", Sequence: 1})
	if !page.Executions[0].Done || len(page.Events) < 3 || !page.Events[len(page.Events)-1].Event.Done {
		t.Fatalf("detached terminal result was lost: %#v", page)
	}
	last := page.Events[len(page.Events)-1]
	if last.Event.Text != "hello world" {
		t.Fatal("worker did not retain the full adapter terminal result")
	}
	if replay := fixture.mustCall(t, WorkerRequest{Action: "events", Sequence: 1, After: last.Cursor}); len(replay.Events) != 0 {
		t.Fatal("reconnection replayed already consumed events")
	}
	fixture.mustCall(t, WorkerRequest{Action: "ack", Sequence: 1, After: last.Cursor})
}

func TestWorkerRetainsQueuedFollowUpAndNativeEmitterHandoff(t *testing.T) {
	for _, scenario := range []struct{ name, mode string }{{"acp", "acp-interrupt"}, {"codex", "codex-worker"}} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newWorkerFixture(t, scenario.name, scenario.mode)
			fixture.mustCall(t, WorkerRequest{Action: "send", Sequence: 1, Key: "synthetic/queue", Prompt: "first"})
			second := WorkerRequest{Action: "send", Sequence: 2, Key: "synthetic/queue", Prompt: "second", Message: "second"}
			result := fixture.mustCall(t, second)
			if !result.Executions[0].Steered {
				t.Fatal("worker lost the supervisor follow-up admission")
			}
			fixture.replaceOwner(t)
			fixture.mustCall(t, second)
			changed := second
			changed.Prompt = "changed retry"
			if reply, _ := fixture.call(changed); reply.Error == "" {
				t.Fatal("worker accepted a different payload for an existing admission")
			}
			fixture.mustCall(t, WorkerRequest{Action: "interrupt", Key: second.Key})
			deadline := time.Now().Add(5 * time.Second)
			for _, sequence := range []uint64{1, 2} {
				var after uint64
				for {
					page := fixture.mustCall(t, WorkerRequest{Action: "events", Sequence: sequence, After: after})
					for _, event := range page.Events {
						if event.Cursor != after+1 {
							t.Fatal("follow-up replay order changed")
						}
						after = event.Cursor
					}
					if page.Executions[0].Done {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("queued cancellation or emitter release was lost")
					}
					time.Sleep(10 * time.Millisecond)
				}
				fixture.mustCall(t, WorkerRequest{Action: "ack", Sequence: sequence, After: after})
			}
			starts, steers := 0, 0
			for _, record := range readFixtureRecords(t, fixture.log) {
				if record.Method == "turn/start" || record.Method == "session/prompt" {
					starts++
				}
				if record.Method == "turn/steer" {
					steers++
				}
			}
			if starts != 1 || scenario.name == "codex" && steers != 1 {
				t.Fatalf("attachment/retry duplicated follow-up work: starts=%d steers=%d", starts, steers)
			}
		})
	}
}

func TestWorkerJournalPagesPreserveWholeOrderedRecords(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "journal-")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	execution := &workerExecution{WorkerExecution: WorkerExecution{Sequence: 1}, file: file}
	host := &workerHost{stop: make(chan struct{}), executions: map[uint64]*workerExecution{1: execution}}
	for count := 0; count < 3; count++ {
		host.capture(execution, core.Event{Kind: core.EventDelta, Text: strings.Repeat("界", workerPageBytes/3)})
	}
	host.capture(execution, core.Event{Kind: core.EventFinal, Text: "synthetic terminal", Done: true})
	var cursor uint64
	for cursor < 4 {
		page := host.events(WorkerRequest{Sequence: 1, After: cursor})
		if page.Error != "" || len(page.Events) == 0 {
			t.Fatal("bounded replay discarded a complete valid record")
		}
		if cursor < 3 && (len(page.Events) != 1 || len(page.Events[0].Event.Text) != workerPageBytes/3*3) {
			t.Fatal("bounded replay split or truncated a large UTF-8 record")
		}
		for _, event := range page.Events {
			if event.Cursor != cursor+1 {
				t.Fatal("paged replay skipped an event")
			}
			cursor = event.Cursor
		}
	}
	if after := host.events(WorkerRequest{Sequence: 1, After: cursor}); len(after.Events) != 0 || after.Error != "" {
		t.Fatal("fully consumed replay did not stay empty")
	}
}

func TestWorkerJournalFailsClosedOnStorageFailureAndCapacity(t *testing.T) {
	for _, failure := range []string{"storage", "capacity"} {
		t.Run(failure, func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "journal-")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			execution := &workerExecution{file: file}
			host := &workerHost{stop: make(chan struct{})}
			if failure == "storage" {
				_ = file.Close()
			} else {
				host.bytes = workerJournalBytes
			}
			host.capture(execution, core.Event{Kind: core.EventFinal, Text: "synthetic", Done: true})
			if host.failure == nil || len(execution.offsets) != 0 || execution.Done {
				t.Fatal("unrecorded event was acknowledged as delivered")
			}
			select {
			case <-host.stop:
			default:
				t.Fatal("failed journaling did not stop further provider effects")
			}
		})
	}
}

func TestWorkerStopsOnUnreadableOrCorruptReplay(t *testing.T) {
	for _, failure := range []string{"unreadable", "corrupt"} {
		t.Run(failure, func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "journal-")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			execution := &workerExecution{file: file}
			host := &workerHost{stop: make(chan struct{}), executions: map[uint64]*workerExecution{1: execution}}
			host.capture(execution, core.Event{Kind: core.EventDelta, Text: "synthetic"})
			if failure == "unreadable" {
				_ = file.Close()
			} else if _, err := file.WriteAt([]byte("!"), 0); err != nil {
				t.Fatal(err)
			}
			page := host.events(WorkerRequest{Sequence: 1})
			if page.Error == "" || len(page.Events) > 0 || host.failure == nil {
				t.Fatal("unrecoverable replay was presented as a successful event page")
			}
			select {
			case <-host.stop:
			default:
				t.Fatal("worker could keep making effects with unreadable replay")
			}
		})
	}
}

func TestWorkerRejectsOversizedAndTrailingRequests(t *testing.T) {
	fixture := newWorkerFixture(t, "codex", "codex-interrupt")
	for _, body := range []string{strings.Repeat(" ", workerRequestBytes+1), "{} {}", `{"unknown":true}`} {
		req, _ := http.NewRequest(http.MethodPost, "http://worker/worker", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+fixture.token)
		response, err := fixture.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("unsafe worker body returned %d", response.StatusCode)
		}
	}
}
