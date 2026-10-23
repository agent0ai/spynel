//go:build linux || darwin

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/updater"
)

func TestWorkerStartupWaitsForPendingProcessOwnership(t *testing.T) {
	for _, protocol := range []int{WorkerProtocol, WorkerProtocol - 1} {
		t.Run(strconv.Itoa(protocol), func(t *testing.T) {
			testWorkerPendingProcessOwnership(t, protocol)
		})
	}
}

func testWorkerPendingProcessOwnership(t *testing.T, protocol int) {
	fixture := newWorkerFixture(t, "codex", "codex-worker")
	path, err := ProcessOwnershipPath(fixture.config.Cwd)
	if err != nil {
		t.Fatal(err)
	}
	recordPath, err := workerRecordPath(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	var record workerRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	record.Info.Protocol = protocol
	// Model container restart: the private rendezvous survives but /tmp is cleared.
	// Pending guardian ownership must still delay replacement.
	previous, err := os.MkdirTemp("", "spynel-worker-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(previous) })
	if err := os.RemoveAll(previous); err != nil {
		t.Fatal(err)
	}
	record.Info.Socket = filepath.Join(previous, "control.sock")
	data, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	fixture.mustCall(t, WorkerRequest{Action: "shutdown"})
	waitWorkerExit(t, fixture.info)
	for _, invalid := range [][]byte{
		bytes.Replace(data, []byte(`"Protocol":`+strconv.Itoa(protocol)), []byte(`"Protocol":0`), 1),
		append([]byte(`{"FutureSchema":true,`), data[1:]...),
	} {
		if err := os.WriteFile(recordPath, invalid, 0600); err != nil {
			t.Fatal(err)
		}
		if client, err := OpenWorker(t.Context(), fixture.config, fixture.owner.ID(), fixture.term); err == nil {
			client.Close()
			t.Fatal("malformed dead worker record authorized replacement")
		}
		if current, err := os.ReadFile(recordPath); err != nil || !bytes.Equal(current, invalid) {
			t.Fatal("malformed dead worker record was changed", err)
		}
	}
	// Preserve a verified dead identity while a modeled cleanup owner still
	// holds the same OS lock. No PID-only or missing-record recovery is authorized.
	if err := os.WriteFile(recordPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := lockProcessOwnership(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	type result struct {
		client *WorkerClient
		err    error
	}
	opened := make(chan result, 1)
	go func() {
		client, err := OpenWorker(t.Context(), fixture.config, fixture.owner.ID(), fixture.term)
		opened <- result{client, err}
	}()
	select {
	case r := <-opened:
		if r.client != nil {
			r.client.Close()
		}
		t.Fatal("worker did not wait for confirmed-dead ownership cleanup", r.err)
	case <-time.After(time.Second):
	}
	// Waiting must release the election mutation lock between acquisition attempts.
	started := time.Now()
	if _, owned, err := fixture.owner.Renew(fixture.term); err != nil || !owned || time.Since(started) > 500*time.Millisecond {
		t.Fatal("pending harness ownership blocked the election fence", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	r := <-opened
	if r.err != nil {
		t.Fatal(r.err)
	}
	client := r.client
	defer client.Close()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := client.Call(ctx, WorkerRequest{Action: "shutdown"}); err != nil {
			t.Error("replacement shutdown", err)
			return
		}
		waitWorkerExit(t, client.Info())
		if _, err := os.Stat(filepath.Dir(client.Info().Socket)); !os.IsNotExist(err) {
			t.Error("replacement retained its endpoint or journals")
		}
	})
	t.Logf("pending-ownership worker pid=%d start=%s generation=%s", client.Info().PID, client.Info().Process.StartTime, client.Info().Process.Generation)
	state := workerClientCall(t, client, WorkerRequest{Action: "inspect"})
	if state.Availability == nil || !state.Availability.Available || len(state.Executions) != 0 || state.NextSequence != 1 {
		t.Fatal("pending ownership recovery invented provider admission or lost readiness")
	}
}

func TestWorkerUnavailableStartupRetainsOwnershipAndRecovers(t *testing.T) {
	for _, scenario := range []struct{ name, mode string }{
		{"codex", "codex-worker"}, {"claude-code", "claude-interrupt"}, {"pi", "pi-interrupt"}, {"acp", "acp-interrupt"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var installed string
			fixture := newWorkerFixture(t, scenario.name, scenario.mode, func(cfg *HarnessConfig) {
				installed = cfg.Command
				cfg.Command = filepath.Join(cfg.Cwd, "not-yet-installed")
			})
			client, err := OpenWorker(t.Context(), fixture.config, fixture.owner.ID(), fixture.term)
			if err != nil {
				t.Fatal("unavailable adapter prevented authenticated discovery", err)
			}
			defer client.Close()
			identity := client.Info()
			t.Logf("unavailable worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
			inspection := workerClientCall(t, client, WorkerRequest{Action: "inspect"})
			if inspection.Availability == nil || inspection.Availability.Available || inspection.Availability.Detail == "" ||
				inspection.NextSequence != 1 || len(inspection.Executions) != 0 || inspection.ConfigDigest != ConfigDigest(fixture.config) {
				t.Fatal("startup failure invented readiness, admission or a different configuration")
			}
			lockPath, err := ProcessOwnershipPath(fixture.config.Cwd)
			if err != nil {
				t.Fatal(err)
			}
			if lock, err := lockProcessOwnership(lockPath); !errors.Is(err, errProcessOwnershipBusy) {
				if lock != nil {
					_ = lock.Close()
				}
				t.Fatal("unavailable worker did not retain exclusive harness ownership", err)
			}
			if _, err := os.Stat(fixture.log); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unavailable startup ran a provider", err)
			}
			fixture.replaceOwner(t)
			if _, err := client.Call(t.Context(), WorkerRequest{Action: "inspect"}); err == nil {
				t.Fatal("unavailable adapter allowed an obsolete controller")
			}
			replacement, err := OpenWorker(t.Context(), fixture.config, fixture.owner.ID(), fixture.term)
			if err != nil {
				t.Fatal("successor could not adopt the unavailable worker", err)
			}
			defer replacement.Close()
			if replacement.Info().Process != identity.Process || replacement.Info().ID != identity.ID {
				t.Fatal("unavailable adoption replaced the worker")
			}
			if err := os.Symlink(installed, fixture.config.Command); err != nil {
				t.Fatal(err)
			}
			workerClientCall(t, replacement, WorkerRequest{Action: "configure", Config: &fixture.config, ExpectedConfig: identity.ConfigDigest})
			inspection = workerClientCall(t, replacement, WorkerRequest{Action: "inspect"})
			if inspection.Availability == nil || !inspection.Availability.Available || inspection.NextSequence != 1 || len(inspection.Executions) != 0 {
				t.Fatal("explicit configuration retry did not restore availability without admission")
			}
			if err := updater.VerifyProcess(identity.Process); err != nil {
				t.Fatal("recovery lost the original native worker", err)
			}
			workerClientCall(t, replacement, WorkerRequest{Action: "send", Sequence: 1, Key: "synthetic/startup", Prompt: "synthetic recovered turn"})
			workerClientCall(t, replacement, WorkerRequest{Action: "interrupt", Key: "synthetic/startup"})
			deadline := time.Now().Add(5 * time.Second)
			for !workerClientCall(t, replacement, WorkerRequest{Action: "inspect"}).Executions[0].Done {
				if time.Now().After(deadline) {
					t.Fatal("recovered provider did not complete interruption")
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}
