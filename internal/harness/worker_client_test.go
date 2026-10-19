//go:build linux || darwin

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/fsx"
	"github.com/agent0ai/spynel/internal/history"
	"github.com/agent0ai/spynel/internal/instance"
	"github.com/agent0ai/spynel/internal/updater"
)

type workerTransportHook func(*http.Request) (*http.Response, error)

func TestWorkerWithLongTemporaryDirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), strings.Repeat("temporary", 16))
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", directory)
	fixture := newWorkerFixture(t, "codex", "codex-worker")
	client, err := OpenWorker(t.Context(), fixture.config, fixture.owner.ID(), fixture.term)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if len(client.Info().Socket) > 103 {
		t.Fatal("worker socket exceeds the supported Unix path budget")
	}
	workerClientCall(t, client, WorkerRequest{Action: "inspect"})
}

func (hook workerTransportHook) RoundTrip(request *http.Request) (*http.Response, error) {
	return hook(request)
}

func TestWorkerConsumerDurableReceiptAndOwnership(t *testing.T) {
	for _, scenario := range []struct{ name, mode string }{
		{"codex", "codex-worker"}, {"claude-code", "claude-interrupt"}, {"pi", "pi-interrupt"}, {"acp", "acp-interrupt"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newWorkerFixture(t, scenario.name, scenario.mode)
			client, err := OpenWorker(t.Context(), fixture.config, fixture.owner.ID(), fixture.term)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			workerClientCall(t, client, WorkerRequest{Action: "send", Sequence: 1, Key: "synthetic/receipt", Prompt: "synthetic receipt"})
			workerClientCall(t, client, WorkerRequest{Action: "interrupt", Key: "synthetic/receipt"})
			deadline := time.Now().Add(5 * time.Second)
			for !workerClientCall(t, client, WorkerRequest{Action: "inspect"}).Executions[0].Done {
				if time.Now().After(deadline) {
					t.Fatal("fixture did not finish")
				}
				time.Sleep(time.Millisecond)
			}
			root := t.TempDir()
			store := history.New(root)
			appended := 0
			sink := func(delivery WorkerDelivery) error {
				inserted, err := store.AppendOnce("cli", "owned", delivery.ReceiptID(), history.Entry{
					Role: "assistant", Content: delivery.Event.Text, Terminal: delivery.Event.Done,
				})
				if inserted {
					appended++
				}
				return err
			}
			failed := errors.New("synthetic failure after durable effect before prefix commit")
			if state, err := client.Consume(t.Context(), 1, func(delivery WorkerDelivery) error {
				if err := sink(delivery); err != nil {
					return err
				}
				return failed
			}); !errors.Is(err, failed) || state.CommittedCursor != 0 || appended != 1 {
				t.Fatalf("failed effect/commit boundary: state=%+v added=%d err=%v", state, appended, err)
			}
			fixture.replaceOwner(t)
			calls := 0
			if _, err := client.Consume(t.Context(), 1, func(WorkerDelivery) error { calls++; return nil }); err == nil || calls != 0 {
				t.Fatal("obsolete consumer applied effects")
			}
			replacement, err := OpenWorker(t.Context(), fixture.config, fixture.owner.ID(), fixture.term)
			if err != nil {
				t.Fatal(err)
			}
			defer replacement.Close()
			store = history.New(root)
			state, err := replacement.Consume(t.Context(), 1, sink)
			if err != nil || state.CommittedCursor != state.LastCursor || appended != int(state.LastCursor) {
				t.Fatalf("replacement replay repeated/lost effect: state=%+v added=%d err=%v", state, appended, err)
			}
			if _, err := replacement.Consume(t.Context(), 1, func(WorkerDelivery) error { calls++; return nil }); err != nil || calls != 0 {
				t.Fatal("committed prefix was applied again")
			}
			workerClientCall(t, replacement, WorkerRequest{Action: "ack", Sequence: 1, After: state.CommittedCursor})
			if _, err := replacement.Consume(t.Context(), 1, sink); err == nil {
				t.Fatal("retired delivery was silently consumed")
			}

		})
	}
}
func TestWorkerConsumerFencesTakeoverAfterReadAndLostCommitReply(t *testing.T) {
	for _, takeover := range []bool{true, false} {
		name := "lost-commit-reply"
		if takeover {
			name = "takeover-after-read"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newWorkerFixture(t, "codex", "codex-lifecycle")
			client, err := OpenWorker(t.Context(), fixture.config, fixture.owner.ID(), fixture.term)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			workerClientCall(t, client, WorkerRequest{Action: "send", Sequence: 1, Key: "synthetic/race", Prompt: "synthetic race"})
			deadline := time.Now().Add(5 * time.Second)
			for !workerClientCall(t, client, WorkerRequest{Action: "inspect"}).Executions[0].Done {
				if time.Now().After(deadline) {
					t.Fatal("fixture did not finish")
				}
				time.Sleep(time.Millisecond)
			}
			transport := client.client.Transport
			client.client.Transport = workerTransportHook(func(request *http.Request) (*http.Response, error) {
				body, _ := request.GetBody()
				var operation WorkerRequest
				_ = json.NewDecoder(body).Decode(&operation)
				_ = body.Close()
				response, err := transport.RoundTrip(request)
				if err != nil {
					return response, err
				}
				if takeover && operation.Action == "events" {
					fixture.replaceOwner(t)
				}
				if !takeover && operation.Action == "commit" {
					_, _ = io.Copy(io.Discard, response.Body)
					_ = response.Body.Close()
					return nil, errors.New("synthetic lost commit reply")
				}
				return response, nil
			})
			calls := 0
			if _, err := client.Consume(t.Context(), 1, func(WorkerDelivery) error { calls++; return nil }); err == nil {
				t.Fatal("injected failure was hidden")
			}
			client.client.Transport = transport
			if takeover {
				if calls != 0 {
					t.Fatal("consumer applied effects after takeover won")
				}
			} else {
				if calls == 0 {
					t.Fatal("consumer did not reach the commit boundary")
				}
				before := calls
				state, err := client.Consume(t.Context(), 1, func(WorkerDelivery) error { calls++; return nil })
				if err != nil || calls != before || state.CommittedCursor != state.LastCursor {
					t.Fatal("lost commit reply caused effects to repeat")
				}
			}
		})
	}
}

func workerClientCall(t *testing.T, client *WorkerClient, request WorkerRequest) WorkerReply {
	t.Helper()
	reply, err := client.Call(t.Context(), request)
	if err != nil {
		t.Fatalf("worker %s failed: %v", request.Action, err)
	}
	return reply
}

func TestWorkerDiscoveryAdoptsAllAdaptersWithoutAdmission(t *testing.T) {
	for _, scenario := range []struct{ name, mode string }{
		{"codex", "codex-worker"}, {"claude-code", "claude-interrupt"}, {"pi", "pi-interrupt"}, {"acp", "acp-interrupt"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newWorkerFixture(t, scenario.name, scenario.mode, func(cfg *HarnessConfig) { cfg.Version = "synthetic-initial-version" })
			before, err := OpenWorker(t.Context(), fixture.config, fixture.owner.ID(), fixture.term)
			if err != nil {
				t.Fatal(err)
			}
			defer before.Close()
			workerClientCall(t, before, WorkerRequest{Action: "send", Sequence: 1, Key: "synthetic/discovery", Prompt: "synthetic discovery"})
			fixture.replaceOwner(t)
			successor := fixture.config
			successor.Version = "synthetic-next-version"
			changed := successor
			changed.Model = "different-selection"
			if client, err := OpenWorker(t.Context(), changed, fixture.owner.ID(), fixture.term); err == nil {
				client.Close()
				t.Fatal("version compatibility admitted changed runtime settings")
			}
			after, err := OpenWorker(t.Context(), successor, fixture.owner.ID(), fixture.term)
			if err != nil {
				t.Fatal(err)
			}
			defer after.Close()
			if before.Info() != after.Info() {
				t.Fatal("discovery changed actual worker identity")
			}
			t.Logf("retained native worker pid=%d start=%s generation=%s", after.Info().PID, after.Info().Process.StartTime, after.Info().Process.Generation)
			if _, err := before.Call(t.Context(), WorkerRequest{Action: "interrupt", Key: "synthetic/discovery"}); err == nil {
				t.Fatal("obsolete client could cancel retained work")
			}
			reply := workerClientCall(t, after, WorkerRequest{Action: "inspect"})
			if reply.NextSequence != 2 || len(reply.Executions) != 1 || reply.Executions[0].Done || reply.Config == nil || reply.Config.Version != fixture.config.Version {
				t.Fatal("discovery admitted or lost existing work")
			}
			if !workerClientCall(t, after, WorkerRequest{Action: "interrupt", Key: "synthetic/discovery"}).Interrupted {
				t.Fatal("new controller could not cancel original execution")
			}
		})
	}
}

func TestWorkerDiscoveryFailsClosedOnUncertainIdentity(t *testing.T) {
	fixture := newWorkerFixture(t, "codex", "codex-worker")
	fixture.mustCall(t, WorkerRequest{Action: "send", Sequence: 1, Key: "synthetic/identity", Prompt: "synthetic retained turn"})
	lockPath, _ := ProcessOwnershipPath(fixture.config.Cwd)
	path, _ := workerRecordPath(lockPath)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record workerRecord
	if err := json.Unmarshal(original, &record); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*workerRecord)
	}{
		{"protocol", func(r *workerRecord) { r.Info.Protocol-- }},
		{"worker identity", func(r *workerRecord) { r.Info.ID += "changed" }},
		{"credential", func(r *workerRecord) { r.Token += "changed" }},
		{"endpoint unavailable", func(r *workerRecord) { r.Info.Socket += ".unavailable" }},
		{"generation", func(r *workerRecord) { r.Info.Process.Generation = "00000000000000000000000000000000" }},
		{"start", func(r *workerRecord) { r.Info.Process.StartTime += "changed" }},
		{"executable", func(r *workerRecord) { r.Info.Process.Executable += ".wrong" }},
		{"role", func(r *workerRecord) { r.Info.Process.HarnessWorker = false }},
		{"pid", func(r *workerRecord) { r.Info.Process.PID = 1 }},
		{"environment", func(r *workerRecord) { r.Info.EnvironmentID = "wrong" }},
		{"workspace", func(r *workerRecord) { r.Info.WorkspaceID = "wrong" }},
		{"configuration", func(r *workerRecord) { r.Info.ConfigDigest[0]++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := record
			test.mutate(&changed)
			data, _ := json.Marshal(changed)
			if err := fsx.AtomicWriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			defer fsx.AtomicWriteFile(path, original, 0600)
			if client, err := OpenWorker(t.Context(), fixture.config, fixture.owner.ID(), fixture.term); err == nil {
				client.Close()
				t.Fatal("uncertain worker identity accepted")
			}
			if test.name == "protocol" {
				if err := publishWorkerRecord(path, record); err == nil {
					t.Fatal("live incompatible worker record was replaced")
				}
				if current, err := os.ReadFile(path); err != nil || string(current) != string(data) {
					t.Fatal("protocol refusal changed the original record", err)
				}
			}
			if err := updater.VerifyProcess(record.Info.Process); err != nil {
				t.Fatal("failed discovery disturbed the original worker")
			}
		})
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(configDir, "spynel", "processes", strconv.Itoa(record.Info.PID)+".json")
	registration, err := os.ReadFile(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(registry); err != nil {
		t.Fatal(err)
	}
	_, missingErr := OpenWorker(t.Context(), fixture.config, fixture.owner.ID(), fixture.term)
	if err := fsx.AtomicWriteFile(registry, registration, 0600); err != nil {
		t.Fatal(err)
	}
	if missingErr == nil || missingErr.Error() != "worker native identity unavailable" {
		t.Fatal("missing live registration was treated as a replacement opportunity")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenWorker(t.Context(), fixture.config, fixture.owner.ID(), fixture.term); err == nil {
		t.Fatal("public worker credentials accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fixture.info.Socket, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenWorker(t.Context(), fixture.config, fixture.owner.ID(), fixture.term); err == nil {
		t.Fatal("public worker endpoint accepted")
	}
	if err := os.Chmod(fixture.info.Socket, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path+".saved", path); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenWorker(t.Context(), fixture.config, fixture.owner.ID(), fixture.term); err == nil {
		t.Fatal("symlinked worker credentials accepted")
	}
	_ = os.Remove(path)
	if err := os.Rename(path+".saved", path); err != nil {
		t.Fatal(err)
	}
	if reply := fixture.mustCall(t, WorkerRequest{Action: "inspect"}); reply.NextSequence != 2 || len(reply.Executions) != 1 || reply.Executions[0].Done {
		t.Fatal("identity failures replaced, duplicated or stopped the active execution")
	}
}

func TestWorkerLaunchSurvivesActualControllerExit(t *testing.T) {
	for _, abrupt := range []bool{false, true} {
		name := "graceful"
		if abrupt {
			name = "abrupt"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			provider, root, log := portableHarnessFixture(t, "codex-worker")
			cfg := HarnessConfig{Name: "codex", Command: provider, Cwd: root, ApprovalPolicy: "plan", Sandbox: "read-only", SessionsFile: filepath.Join(root, "sessions.json")}
			controller := exec.Command(os.Args[0], "__worker-controller")
			input, err := controller.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := controller.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			controller.Stderr = os.Stderr
			if err := controller.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = input.Close(); _ = controller.Process.Kill(); _ = controller.Wait() })
			if err := json.NewEncoder(input).Encode(cfg); err != nil {
				t.Fatal(err)
			}
			var original WorkerInfo
			ready := make(chan error, 1)
			go func() { ready <- json.NewDecoder(output).Decode(&original) }()
			select {
			case err := <-ready:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("controller fixture did not launch worker")
			}
			state := filepath.Join(root, ".spynel")
			owner, err := instance.New(state)
			if err != nil {
				t.Fatal(err)
			}
			prior, err := owner.Current()
			if err != nil {
				t.Fatal(err)
			}
			term, _ := owner.NewToken()
			var client *WorkerClient
			t.Cleanup(func() {
				if client != nil {
					_, _ = client.Call(context.Background(), WorkerRequest{Action: "shutdown"})
					client.Close()
				}
				_ = owner.Release(term)
			})
			if abrupt {
				if err := controller.Process.Kill(); err != nil {
					t.Fatal(err)
				}
			} else {
				_ = input.Close()
			}
			exitErr := controller.Wait()
			if abrupt != (exitErr != nil) {
				t.Fatalf("unexpected controller exit: %v", exitErr)
			}
			deadline := time.Now().Add(40 * time.Second)
			for {
				_, acquired, err := owner.TryAcquire("127.0.0.1:10002", term)
				if err != nil {
					t.Fatal(err)
				}
				if acquired {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("controller ownership did not expire naturally")
				}
				time.Sleep(100 * time.Millisecond)
			}
			client, err = OpenWorker(t.Context(), cfg, owner.ID(), term)
			if err != nil {
				t.Fatal(err)
			}
			if client.Info() != original {
				t.Fatal("controller exit replaced the actual worker")
			}
			obsolete := newWorkerClient(client.record, prior.InstanceID, prior.Token, owner)
			defer obsolete.Close()
			if _, err := obsolete.Call(t.Context(), WorkerRequest{Action: "interrupt", Key: "synthetic/controller"}); err == nil {
				t.Fatal("old controller could interrupt after takeover")
			}
			inspect := workerClientCall(t, client, WorkerRequest{Action: "inspect"})
			if inspect.NextSequence != 2 || len(inspect.Executions) != 1 || inspect.Executions[0].Done || inspect.Executions[0].CommittedCursor != 1 {
				t.Fatal("controller restart duplicated or lost execution")
			}
			store := history.New(filepath.Join(root, "consumer-history"))
			entries, _, err := store.RecentEntries("cli", "owned", 10, 100000)
			if err != nil || len(entries) != 2 || entries[0].ReceiptID != (WorkerDelivery{WorkerID: original.ID, Sequence: 1, WorkerEvent: WorkerEvent{Cursor: 1}}).ReceiptID() || entries[1].ReceiptID != (WorkerDelivery{WorkerID: original.ID, Sequence: 1, WorkerEvent: WorkerEvent{Cursor: 2}}).ReceiptID() {
				t.Fatal("controller lost its committed or not-yet-committed history effect")
			}
			after := inspect.Executions[0].CommittedCursor
			deadline = time.Now().Add(5 * time.Second)
			for {
				page := workerClientCall(t, client, WorkerRequest{Action: "events", Sequence: 1, After: after})
				for _, event := range page.Events {
					if event.Cursor != after+1 {
						t.Fatal("missed event replay was not ordered")
					}
					after = event.Cursor
				}
				if len(page.Events) == 0 && after >= 2 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("could not catch up with retained worker progress")
				}
				time.Sleep(time.Millisecond)
			}
			time.Sleep(80 * time.Millisecond)
			page := workerClientCall(t, client, WorkerRequest{Action: "events", Sequence: 1, After: after})
			if len(page.Events) == 0 || page.Events[0].Cursor != after+1 {
				t.Fatal("original execution stopped progressing or repeated events")
			}
			starts, effects := 0, 0
			for _, record := range readFixtureRecords(t, log) {
				if record.Method == "turn/start" {
					starts++
				}
				if record.Kind == "effect" {
					effects++
					if err := syscall.Kill(record.PID, 0); err != nil {
						t.Fatal("original provider did not survive controller exit")
					}
				}
			}
			if starts != 1 || effects != 1 {
				t.Fatalf("attachment created provider work: starts=%d effects=%d", starts, effects)
			}
			if !workerClientCall(t, client, WorkerRequest{Action: "interrupt", Key: "synthetic/controller"}).Interrupted {
				t.Fatal("new controller could not cancel original execution")
			}
			deadline = time.Now().Add(5 * time.Second)
			terminal := false
			applied := uint64(1)
			for {
				state, err := client.Consume(t.Context(), 1, func(delivery WorkerDelivery) error {
					if delivery.Cursor != applied+1 {
						t.Fatal("terminal replay repeated or lost events")
					}
					applied = delivery.Cursor
					terminal = terminal || delivery.Event.Done && !delivery.Event.Continues
					if delivery.Cursor == 2 || delivery.Event.Done {
						_, err := store.AppendOnce("cli", "owned", delivery.ReceiptID(), history.Entry{Role: "assistant", Content: delivery.Event.Text, Terminal: delivery.Event.Done})
						return err
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				after = state.CommittedCursor
				if terminal && state.Done && after == state.LastCursor {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("original execution did not settle cancellation")
				}
				time.Sleep(10 * time.Millisecond)
			}
			entries, _, err = store.RecentEntries("cli", "owned", 10, 100000)
			if err != nil || len(entries) != 3 || !entries[2].Terminal {
				t.Fatal("controller replacement repeated a history effect or lost terminal history")
			}
			workerClientCall(t, client, WorkerRequest{Action: "ack", Sequence: 1, After: after})
			client.Close()
			reattached, err := OpenWorker(t.Context(), cfg, owner.ID(), term)
			if err != nil {
				t.Fatal(err)
			}
			client = reattached
			if state := workerClientCall(t, client, WorkerRequest{Action: "inspect"}); state.NextSequence != 2 || len(state.Executions) != 0 {
				t.Fatal("reconnect revived cancelled execution")
			}
			workerClientCall(t, client, WorkerRequest{Action: "shutdown"})
			waitWorkerExit(t, original)
			if _, err := os.Lstat(filepath.Dir(original.Socket)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("worker did not remove its private endpoint/journals")
			}
			t.Logf("actual controller exit retained worker pid=%d start=%s generation=%s; one provider admission/effect, committed and uncommitted history receipts, ordered progress and replacement cancellation", original.PID, original.Process.StartTime, original.Process.Generation)
		})
	}
}

func waitWorkerExit(t *testing.T, info WorkerInfo) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !errors.Is(updater.VerifyProcess(info.Process), syscall.ESRCH) {
		if time.Now().After(deadline) {
			t.Fatal("worker did not stop its owned process")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWorkerConcurrentLaunchAndConfirmedDeadReplacement(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	provider, root, _ := portableHarnessFixture(t, "codex-lifecycle")
	owner, err := instance.New(filepath.Join(root, ".spynel"))
	if err != nil {
		t.Fatal(err)
	}
	term, _ := owner.NewToken()
	if _, acquired, err := owner.TryAcquire("127.0.0.1:10001", term); err != nil || !acquired {
		t.Fatal("test owner did not acquire election")
	}
	t.Cleanup(func() { _ = owner.Release(term) })
	cfg := HarnessConfig{Name: "codex", Command: provider, Cwd: root, ApprovalPolicy: "plan", SessionsFile: filepath.Join(root, "sessions.json")}
	var clients [2]*WorkerClient
	var failures [2]error
	var group sync.WaitGroup
	for index := range clients {
		group.Go(func() { clients[index], failures[index] = OpenWorker(t.Context(), cfg, owner.ID(), term) })
	}
	group.Wait()
	var client *WorkerClient
	for index, current := range clients {
		if failures[index] != nil {
			continue
		}
		defer current.Close()
		if client != nil && client.Info() != current.Info() {
			t.Fatal("concurrent launch created two workers")
		}
		client = current
	}
	if client == nil {
		t.Fatal("neither concurrent opener reached the worker")
	}
	t.Cleanup(func() { _, _ = client.Call(context.Background(), WorkerRequest{Action: "shutdown"}); client.Close() })
	if state := workerClientCall(t, client, WorkerRequest{Action: "inspect"}); state.NextSequence != 1 || len(state.Executions) != 0 {
		t.Fatal("discovery dispatched provider work")
	}
	prior := client.record
	if err := updater.VerifyProcess(prior.Info.Process); err != nil {
		t.Fatal(err)
	}
	process, err := os.FindProcess(prior.Info.PID)
	if err != nil {
		t.Fatal(err)
	}
	defer process.Release()
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitWorkerExit(t, prior.Info)
	lock, _ := ProcessOwnershipPath(root)
	path, _ := workerRecordPath(lock)
	replacement, err := OpenWorker(t.Context(), cfg, owner.ID(), term)
	if err != nil {
		t.Fatal(err)
	}
	client = replacement
	if client.Info().ID == prior.Info.ID || client.Info().Process.Generation == prior.Info.Process.Generation {
		t.Fatal("confirmed dead worker was not replaced")
	}
	if state := workerClientCall(t, client, WorkerRequest{Action: "inspect"}); state.NextSequence != 1 || len(state.Executions) != 0 {
		t.Fatal("dead worker recovery implicitly replayed provider work")
	}
	workerClientCall(t, client, WorkerRequest{Action: "shutdown"})
	waitWorkerExit(t, client.Info())
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("explicit shutdown retained worker credentials")
	}
}

func TestWorkerConfigurationSurvivesNativeControllerReplacement(t *testing.T) {
	alias := filepath.Join(t.TempDir(), "temporary alias λ")
	if err := os.Symlink(t.TempDir(), alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", alias)
	for _, scenario := range []struct{ name, mode string }{{"codex", "codex-worker"}, {"claude-code", "claude-interrupt"}, {"pi", "pi-interrupt"}, {"acp", "acp-interrupt"}} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newWorkerFixture(t, scenario.name, scenario.mode)
			client, err := OpenWorker(t.Context(), fixture.config, fixture.owner.ID(), fixture.term)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			send := WorkerRequest{Action: "send", Sequence: 1, Key: "synthetic/settings", Prompt: "synthetic retained work"}
			first := workerClientCall(t, client, send).Executions[0]
			if first.Done || first.Error != "" {
				t.Fatalf("no retained provider turn: %+v", first)
			}
			next := fixture.config
			next.Model = "model-a"
			if scenario.name != "acp" {
				next.Effort = "turbo"
			}
			request := WorkerRequest{Action: "configure", Config: &next, ExpectedConfig: client.Info().ConfigDigest}
			lockPath, _ := ProcessOwnershipPath(fixture.config.Cwd)
			recordPath, _ := workerRecordPath(lockPath)
			originalRecord, err := os.ReadFile(recordPath)
			if err != nil {
				t.Fatal(err)
			}
			reply := workerClientCall(t, client, request)
			if reply.ConfigDigest != ConfigDigest(next) {
				t.Fatal("configuration reply differs from requested runtime")
			}
			if workerClientCall(t, client, WorkerRequest{Action: "inspect"}).Executions[0].Done {
				t.Fatal("configuration ended the active turn")
			}
			if stale, err := OpenWorker(t.Context(), fixture.config, fixture.owner.ID(), fixture.term); err == nil {
				stale.Close()
				t.Fatal("discovery accepted stale settings")
			}
			committedRecord, err := os.ReadFile(recordPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := fsx.AtomicWriteFile(recordPath, originalRecord, 0600); err != nil {
				t.Fatal(err)
			}
			stale, staleErr := OpenWorker(t.Context(), fixture.config, fixture.owner.ID(), fixture.term)
			if stale != nil {
				stale.Close()
			}
			if err := fsx.AtomicWriteFile(recordPath, committedRecord, 0600); err != nil {
				t.Fatal(err)
			}
			if staleErr == nil {
				t.Fatal("discovery trusted a record snapshot after authenticated settings changed")
			}
			// An exact retry after a lost reply has no new admission or provider start.
			workerClientCall(t, client, request)
			fixture.replaceOwner(t)
			rejected := next
			rejected.Model = "obsolete"
			if _, err := client.Call(t.Context(), WorkerRequest{Action: "configure", Config: &rejected, ExpectedConfig: reply.ConfigDigest}); err == nil {
				t.Fatal("obsolete controller changed worker settings")
			}
			successor, err := OpenWorker(t.Context(), next, fixture.owner.ID(), fixture.term)
			if err != nil {
				t.Fatal(err)
			}
			defer successor.Close()
			oldInfo, newInfo := client.Info(), successor.Info()
			oldInfo.ConfigDigest = newInfo.ConfigDigest
			if oldInfo != newInfo {
				t.Fatal("settings commit replaced the actual worker")
			}
			inspection := workerClientCall(t, successor, WorkerRequest{Action: "inspect"})
			if inspection.ConfigDigest != reply.ConfigDigest || inspection.NextSequence != 2 || len(inspection.Executions) != 1 || inspection.Executions[0].ThreadID != first.ThreadID || inspection.Executions[0].Done {
				t.Fatal("settings attachment lost work or dispatched again")
			}
			changed := next
			changed.Sandbox = "workspace-write"
			if _, err := successor.Call(t.Context(), WorkerRequest{Action: "configure", Config: &changed, ExpectedConfig: reply.ConfigDigest}); err == nil {
				t.Fatal("structural settings replaced a retained provider")
			}
			for sequence := uint64(1); sequence <= 2; sequence++ {
				if !workerClientCall(t, successor, WorkerRequest{Action: "interrupt", Key: send.Key}).Interrupted {
					t.Fatal("successor could not interrupt")
				}
				deadline := time.Now().Add(5 * time.Second)
				for {
					current := workerClientCall(t, successor, WorkerRequest{Action: "inspect"}).Executions[0]
					if current.Done {
						workerClientCall(t, successor, WorkerRequest{Action: "commit", Sequence: sequence, After: current.LastCursor})
						workerClientCall(t, successor, WorkerRequest{Action: "ack", Sequence: sequence, After: current.LastCursor})
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("provider did not release")
					}
					time.Sleep(time.Millisecond)
				}
				if sequence == 1 {
					send.Sequence = 2
					if current := workerClientCall(t, successor, send).Executions[0]; current.Error != "" || current.Done {
						t.Fatalf("next dispatch: %+v", current)
					}
				}
			}
			delivered := false
			for _, record := range readFixtureRecords(t, fixture.log) {
				var params map[string]any
				if len(record.Params) > 0 {
					if err := json.Unmarshal(record.Params, &params); err != nil {
						t.Fatal(err)
					}
				}
				switch scenario.name {
				case "codex":
					if record.Method == "turn/start" && params["model"] == next.Model && params["effort"] == next.Effort {
						delivered = true
					}
				case "claude-code", "pi":
					if record.Kind == "invocation" && containsArgument(record.Args, next.Model) && containsArgument(record.Args, next.Effort) {
						delivered = true
					}
				case "acp":
					if record.Method == "session/set_config_option" && params["value"] == next.Model {
						delivered = true
					}
				}
			}
			if !delivered {
				t.Fatal("committed settings did not reach the next native provider dispatch")
			}
			workerClientCall(t, successor, WorkerRequest{Action: "configure", Config: &changed, ExpectedConfig: reply.ConfigDigest})
			reconfigured, err := OpenWorker(t.Context(), changed, fixture.owner.ID(), fixture.term)
			if err != nil {
				t.Fatal(err)
			}
			defer reconfigured.Close()
			if reconfigured.Info().Process != newInfo.Process {
				t.Fatal("idle harness replacement changed worker identity")
			}
			t.Logf("retained native worker pid=%d start=%s generation=%s", newInfo.PID, newInfo.Process.StartTime, newInfo.Process.Generation)
		})
	}
}
