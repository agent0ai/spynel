//go:build linux || darwin

package harness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/updater"
)

func TestWorkerBlockedProviderInputReleasesOwnership(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release-input")
	t.Setenv("SPYNEL_TEST_CODEX_PAUSE_INPUT", release)
	f := newWorkerFixture(t, "codex", "codex-worker")
	t.Cleanup(func() {
		if err := os.WriteFile(release, nil, 0600); err != nil {
			t.Error(err)
		}
	})
	work := &WorkerWorkflow{WorkID: "synthetic", OwnerID: "synthetic-owner", ClaimAttempt: 1}
	send := WorkerRequest{Action: "send", Sequence: 1, Key: "synthetic/blocked-input", Prompt: "original", Workflow: work}
	first := f.mustCall(t, send).Executions[0]
	// The provider keeps publishing useful progress but stops consuming stdin.
	deadline := time.Now().Add(3 * time.Second)
	for {
		paused := false
		for _, record := range readFixtureRecords(t, f.log) {
			paused = paused || record.Kind == "input-paused"
		}
		if paused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("provider did not pause its input")
		}
		time.Sleep(10 * time.Millisecond)
	}
	control := WorkerRequest{Action: "control", Sequence: 1, Key: send.Key, Prompt: strings.Repeat("x", 192*1024), Workflow: work, Control: &WorkerControl{ID: "synthetic-blocked-input"}}
	f.client.Timeout = 8 * time.Second
	started := time.Now()
	reply, status := f.call(control)
	if status != http.StatusOK || reply.Error == "" || time.Since(started) > 7*time.Second {
		t.Errorf("provider input did not return a bounded error: status=%d error=%q elapsed=%s", status, reply.Error, time.Since(started))
	}
	f.client.Timeout = time.Second
	inspection, status := f.call(WorkerRequest{Action: "inspect"})
	if status != http.StatusOK || inspection.Error != "" {
		t.Fatalf("blocked provider input retained the worker/election fence: status=%d error=%q", status, inspection.Error)
	}
	retained := inspection.Executions[0]
	if retained.Done || retained.Cancelled || retained.ThreadID != first.ThreadID || retained.LastCursor <= first.LastCursor {
		t.Fatal("blocked input replaced or ended the ongoing provider execution")
	}
	if _, renewed, err := f.owner.Renew(f.term); err != nil || !renewed {
		t.Fatal("blocked provider input prevented renewal:", renewed, err)
	}
	f.replaceOwner(t)
	if err := updater.VerifyProcess(f.info.Process); err != nil {
		t.Fatal("native worker identity changed:", err)
	}
	if duplicate := f.mustCall(t, control); duplicate.Control == nil || !duplicate.Control.Duplicate {
		t.Fatal("ambiguous partial delivery lost control deduplication")
	}
	if reply, status := f.call(WorkerRequest{Action: "interrupt", Key: send.Key}); status != http.StatusOK || reply.Error == "" || reply.Interrupted {
		t.Fatalf("closed provider input invented successful interruption: status=%d reply=%+v", status, reply)
	}
	if f.mustCall(t, WorkerRequest{Action: "inspect"}).Executions[0].Cancelled {
		t.Fatal("rejected interruption left accepted cancellation intent")
	}
	starts, effects := 0, 0
	for _, record := range readFixtureRecords(t, f.log) {
		if record.Method == "turn/start" {
			starts++
		}
		if record.Kind == "effect" {
			effects++
		}
		if record.Method == "turn/steer" {
			t.Fatal("provider parsed an incomplete blocked control frame")
		}
	}
	if starts != 1 || effects != 1 {
		t.Fatalf("blocked input duplicated work: starts=%d effects=%d", starts, effects)
	}
	t.Logf("blocked stdin released ownership without replacing worker PID=%d start=%s generation=%s; same execution, progress, renewal, takeover and ambiguous retry deduplication passed", f.info.PID, f.info.Process.StartTime, f.info.Process.Generation)
}

func TestWorkerControlTimeoutPreservesExecutionAndReleasesOwnership(t *testing.T) {
	t.Setenv("SPYNEL_TEST_CODEX_STALL_CONTROL", "1")
	f := newWorkerFixture(t, "codex", "codex-worker")
	work := &WorkerWorkflow{WorkID: "synthetic", OwnerID: "synthetic-owner", ClaimAttempt: 1}
	send := WorkerRequest{Action: "send", Sequence: 1, Key: "synthetic/control", Prompt: "original", Workflow: work}
	first := f.mustCall(t, send).Executions[0]
	control := WorkerRequest{Action: "control", Sequence: 1, Key: send.Key, Prompt: "synthetic guidance", Workflow: work, Control: &WorkerControl{ID: "synthetic-stalled-control"}}
	f.client.Timeout = 8 * time.Second
	started := time.Now()
	reply, status := f.call(control)
	if status != http.StatusOK || reply.Error != context.DeadlineExceeded.Error() || time.Since(started) > 7*time.Second {
		t.Fatalf("control acknowledgement was not bounded: status=%d reply=%+v elapsed=%s", status, reply, time.Since(started))
	}
	if _, renewed, err := f.owner.Renew(f.term); err != nil || !renewed {
		t.Fatal("control timeout retained the election fence:", renewed, err)
	}
	f.client.Timeout = time.Second
	retained := f.mustCall(t, WorkerRequest{Action: "inspect"}).Executions[0]
	if retained.Done || retained.Cancelled || retained.ThreadID != first.ThreadID || retained.Sequence != first.Sequence {
		t.Fatal("control timeout ended or replaced the retained execution")
	}
	f.replaceOwner(t)
	if err := updater.VerifyProcess(f.info.Process); err != nil {
		t.Fatal("native worker identity changed:", err)
	}
	if duplicate := f.mustCall(t, control); duplicate.Control == nil || !duplicate.Control.Duplicate {
		t.Fatal("ambiguous control retry was not deduplicated")
	}
	starts, steers, effects := 0, 0, 0
	for _, record := range readFixtureRecords(t, f.log) {
		if record.Method == "turn/start" {
			starts++
		}
		if record.Method == "turn/steer" {
			steers++
		}
		if record.Kind == "effect" {
			effects++
		}
	}
	if starts != 1 || steers != 1 || effects != 1 {
		t.Fatalf("timeout/retry duplicated provider work: starts=%d steers=%d effects=%d", starts, steers, effects)
	}
	if !f.mustCall(t, WorkerRequest{Action: "interrupt", Key: send.Key}).Interrupted {
		t.Fatal("successor could not interrupt after unacknowledged control")
	}
	t.Logf("permanently unacknowledged steer retained worker PID=%d start=%s generation=%s; renewal, takeover, deduplication and successor interruption passed", f.info.PID, f.info.Process.StartTime, f.info.Process.Generation)
}

func TestWorkerInterruptTimeoutRetainsIntentAndReleasesOwnership(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release-interrupt")
	t.Setenv("SPYNEL_TEST_CODEX_DELAY_INTERRUPT", release)
	f := newWorkerFixture(t, "codex", "codex-worker")
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
	send := WorkerRequest{Action: "send", Sequence: 1, Key: "synthetic/interrupt-timeout", Prompt: "original"}
	first := f.mustCall(t, send).Executions[0]
	interrupt := WorkerRequest{Action: "interrupt", Key: send.Key, WorkerID: f.info.ID, InstanceID: f.owner.ID(), TermToken: f.term}
	f.client.Timeout = 8 * time.Second
	started := time.Now()
	reply, status := f.call(interrupt)
	if status != http.StatusOK || reply.Error != context.DeadlineExceeded.Error() || reply.Interrupted || time.Since(started) > 7*time.Second {
		t.Fatalf("interrupt acknowledgement was not bounded and honest: status=%d reply=%+v elapsed=%s", status, reply, time.Since(started))
	}
	f.client.Timeout = time.Second
	retained := f.mustCall(t, WorkerRequest{Action: "inspect"}).Executions[0]
	if retained.Done || !retained.Cancelled || retained.ThreadID != first.ThreadID || retained.LastCursor <= first.LastCursor {
		t.Fatal("uncertain interruption lost its intent or ongoing execution")
	}
	if _, renewed, err := f.owner.Renew(f.term); err != nil || !renewed {
		t.Fatal("interrupt timeout retained the election fence:", renewed, err)
	}
	f.replaceOwner(t)
	if err := updater.VerifyProcess(f.info.Process); err != nil {
		t.Fatal("interrupt timeout replaced the native worker:", err)
	}
	if _, status := f.rawCall(interrupt, f.token); status != http.StatusForbidden {
		t.Fatal("obsolete controller could interrupt")
	}
	if duplicate := f.mustCall(t, send).Executions[0]; !duplicate.Cancelled || duplicate.Sequence != first.Sequence || duplicate.Done {
		t.Fatal("replacement lost the uncertain cancellation fence")
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		retained = f.mustCall(t, WorkerRequest{Action: "inspect"}).Executions[0]
		if retained.Done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("late provider terminal did not settle")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !retained.Cancelled || retained.Sequence != first.Sequence || retained.ThreadID != first.ThreadID {
		t.Fatal("late terminal lost cancellation intent")
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
	if starts != 1 || interrupts != 1 {
		t.Fatalf("timeout/attachment repeated provider work: starts=%d interrupts=%d", starts, interrupts)
	}
	t.Logf("interrupt timeout preserved intent and late terminal with worker pid=%d start=%s generation=%s; renewal, takeover and obsolete-controller refusal passed", f.info.PID, f.info.Process.StartTime, f.info.Process.Generation)
}

func TestWorkerControlsRetainAdapterAndCancellation(t *testing.T) {
	for _, scenario := range []struct{ name, mode string }{
		{"codex", "codex-worker"}, {"claude-code", "claude-interrupt"}, {"pi", "pi-interrupt"}, {"acp", "acp-interrupt"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newWorkerFixture(t, scenario.name, scenario.mode)
			work := &WorkerWorkflow{WorkID: "synthetic", OwnerID: "synthetic-owner", ClaimAttempt: 1}
			send := WorkerRequest{Action: "send", Sequence: 1, Key: "synthetic/control", Prompt: "original", Workflow: work}
			f.mustCall(t, send)
			request := WorkerRequest{Action: "control", Sequence: 1, Key: send.Key, Workflow: work, Prompt: "synthetic guidance", Control: &WorkerControl{ID: "synthetic-control"}}
			first := f.mustCall(t, request)
			if first.Control == nil || first.Control.Duplicate {
				t.Fatal("control was not admitted")
			}
			if duplicate := f.mustCall(t, request); duplicate.Control == nil || !duplicate.Control.Duplicate {
				t.Fatal("control retry was not deduplicated")
			}
			changed := *work
			changed.ClaimAttempt++
			request.Workflow = &changed
			if reply, _ := f.call(request); reply.Error == "" {
				t.Fatal("changed control claim was admitted")
			}
			request.Workflow = work
			request.WorkerID, request.InstanceID, request.TermToken = f.info.ID, f.owner.ID(), f.term
			f.replaceOwner(t)
			if _, status := f.rawCall(request, f.token); status != http.StatusForbidden {
				t.Fatal("obsolete control reached the provider")
			}
			if !f.mustCall(t, WorkerRequest{Action: "interrupt", Key: send.Key}).Interrupted {
				t.Fatal("successor could not cancel")
			}
			f.replaceOwner(t)
			if retained := f.mustCall(t, WorkerRequest{Action: "inspect"}).Executions[0]; !retained.Cancelled {
				t.Fatal("replacement lost accepted cancellation")
			}
			if reply, _ := f.call(request); reply.Error == "" {
				t.Fatal("cancelled execution accepted control")
			}
		})
	}
}

func TestWorkerCancellationPersistenceAndRejectedInterrupt(t *testing.T) {
	for _, brokenStorage := range []bool{false, true} {
		t.Run(fmt.Sprint(brokenStorage), func(t *testing.T) {
			root := t.TempDir()
			target := &supervisorHarness{name: "synthetic", active: map[string]bool{}, emits: map[string]core.Emit{}, interruptErr: errors.New("synthetic refused interrupt")}
			supervisor := NewSupervisor(NewRegistry(), HarnessConfig{Cwd: root})
			supervisor.current, supervisor.ctx = target, context.Background()
			host := &workerHost{supervisor: supervisor, directory: root, next: 1, stop: make(chan struct{}), executions: map[uint64]*workerExecution{}}
			if reply := host.send(WorkerRequest{Sequence: 1, Key: "synthetic", Prompt: "original"}); reply.Error != "" {
				t.Fatal(reply.Error)
			}
			defer host.executions[1].file.Close()
			if brokenStorage {
				path := filepath.Join(root, "1.jsonl.admission")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if reply := host.interrupt("synthetic"); reply.Error == "" || reply.Interrupted {
				t.Fatal("failed cancellation reported success")
			}
			if brokenStorage != (host.failure != nil) || !target.IsActive("synthetic") || !brokenStorage && host.executions[1].Cancelled {
				t.Fatal("failed cancellation lost its storage fence or changed the provider")
			}
		})
	}
}
