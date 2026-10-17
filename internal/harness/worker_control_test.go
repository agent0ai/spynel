//go:build linux || darwin

package harness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/agent0ai/spynel/internal/core"
)

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
