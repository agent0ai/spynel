//go:build linux || darwin

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/updater"
)

func TestWorkerUpdatePreservesAdmissionAndDelivery(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(root, "releases", "1.0.0")
	if err := os.MkdirAll(bundle, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(bundle, "spynel")
	if err := os.WriteFile(executable, data, 0700); err != nil {
		t.Fatal(err)
	}
	marker, _ := json.Marshal(map[string]string{"version": "1.0.0", "os": runtime.GOOS, "arch": runtime.GOARCH})
	if err := os.WriteFile(filepath.Join(bundle, ".bundle.json"), marker, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("releases", "1.0.0"), filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPYNEL_TEST_WORKER_INSTALL", root)
	t.Setenv("SPYNEL_TEST_WORKER_EXECUTABLE", executable)
	fixture := newWorkerFixture(t, "acp", "acp-interrupt")
	manager := &updater.Manager{InstallRoot: root, CurrentVersion: "1.0.0"}
	unregister, err := manager.RegisterApplicationClient()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unregister)

	// Unknown GitHub metadata used to refuse even this idle native worker.
	if err := manager.CheckRestartableFor(0); err != nil {
		t.Fatal(err)
	}
	guard, err := manager.BeginWorkerUpdate(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	send := WorkerRequest{Action: "send", Sequence: 1, Key: "synthetic/update", Prompt: "hold until interrupted"}
	if reply, _ := fixture.call(send); reply.Error == "" {
		t.Fatal("pause admitted a new turn")
	}
	blocked, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if unlock, err := updater.LockWorkerLaunch(blocked); !errors.Is(err, context.DeadlineExceeded) {
		if unlock != nil {
			unlock()
		}
		t.Fatal("update did not fence a competing worker launch", err)
	}
	launched := make(chan error, 1)
	go func() {
		unlock, err := updater.LockWorkerLaunch(t.Context())
		if unlock != nil {
			unlock()
		}
		launched <- err
	}()
	if _, err := manager.BeginWorkerUpdate(t.Context(), 0); err == nil {
		t.Fatal("concurrent update acquired ownership")
	}
	guard.Close() // Failed installation keeps the original worker usable.
	select {
	case err := <-launched:
		if err != nil {
			t.Fatal("waiting application failed after update released its lock", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiting application did not resume")
	}
	// A compatible upgrade retains the worker's original version. Aborting a
	// later update must compare the installation with its coordinator, not it.
	marker, _ = json.Marshal(map[string]string{"version": "1.1.0", "os": runtime.GOOS, "arch": runtime.GOARCH})
	if err := os.WriteFile(filepath.Join(bundle, ".bundle.json"), marker, 0600); err != nil {
		t.Fatal(err)
	}
	manager.CurrentVersion = "1.1.0"
	unregister, err = manager.RegisterApplicationClient()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unregister)
	guard, err = manager.BeginWorkerUpdate(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	guard.Close()
	fixture.mustCall(t, send)
	if _, err := manager.BeginWorkerUpdate(t.Context(), 0); err == nil {
		t.Fatal("update paused active work")
	}
	compatible, err := manager.BeginWorkerUpdate(t.Context(), WorkerProtocol)
	if err != nil {
		t.Fatal("compatible update blocked active work", err)
	}
	if err := compatible.Finish(t.Context()); err != nil {
		t.Fatal(err)
	}
	compatible.Close()
	if err := updater.VerifyProcess(fixture.info.Process); err != nil {
		t.Fatal("compatible update stopped worker", err)
	}
	fixture.mustCall(t, WorkerRequest{Action: "interrupt", Key: send.Key})
	var after uint64
	deadline := time.Now().Add(5 * time.Second)
	for {
		page := fixture.mustCall(t, WorkerRequest{Action: "events", Sequence: 1})
		done := false
		for _, event := range page.Events {
			after = event.Cursor
			done = done || event.Event.Done && !event.Event.Continues
		}
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("provider did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := manager.BeginWorkerUpdate(t.Context(), 0); err == nil {
		t.Fatal("update discarded pending terminal delivery")
	}
	fixture.mustCall(t, WorkerRequest{Action: "commit", Sequence: 1, After: after})
	if _, err := manager.BeginWorkerUpdate(t.Context(), 0); err == nil {
		t.Fatal("update skipped execution retirement")
	}
	fixture.mustCall(t, WorkerRequest{Action: "ack", Sequence: 1, After: after})

	// The installation endpoint retains authentication and exact generation.
	request, _ := json.Marshal(updater.WorkerUpdateRequest{Generation: "wrong", Action: "stop"})
	req, _ := http.NewRequest(http.MethodPost, "http://worker/update", bytes.NewReader(request))
	req.Header.Set("Authorization", "Bearer "+fixture.token)
	response, err := fixture.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatal("changed generation accepted")
	}
	guard, err = manager.BeginWorkerUpdate(t.Context(), WorkerProtocol+1)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	if err := guard.Finish(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(updater.VerifyProcess(fixture.info.Process), syscall.ESRCH) {
		t.Fatal("idle replacement did not confirm native death")
	}
}
