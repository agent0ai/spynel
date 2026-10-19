//go:build linux || darwin

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/updater"
)

func TestWorkerCrashReplayPreservesTerminalOutcome(t *testing.T) {
	for _, kind := range []string{core.EventFinal, core.EventError} {
		for _, late := range []bool{false, true} {
			name := kind + "/on-time"
			if late {
				name = kind + "/late"
			}
			t.Run(name, func(t *testing.T) {
				root := t.TempDir()
				previous, err := os.MkdirTemp(root, "spynel-worker-")
				if err != nil {
					t.Fatal(err)
				}
				current := filepath.Join(root, "current")
				if err := os.Mkdir(current, 0700); err != nil {
					t.Fatal(err)
				}
				cfg := HarnessConfig{Cwd: root}
				executable, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				// File-level crash modeling; the CLI test kills a real worker.
				native := updater.ProcessRegistration{PID: 1 << 30, StartTime: "absent-fixture", Generation: strings.Repeat("a", 32), Executable: executable, HarnessWorker: true}
				if !errors.Is(updater.VerifyProcess(native), syscall.ESRCH) {
					t.Fatal("fixture PID is not confirmed absent")
				}
				prior := workerRecord{Token: strings.Repeat("b", 32), Info: WorkerInfo{Protocol: WorkerProtocol, ID: strings.Repeat("c", 32), PID: native.PID, Socket: filepath.Join(previous, "control.sock"), EnvironmentID: "environment", WorkspaceID: "workspace", ConfigDigest: ConfigDigest(cfg), Process: native}}
				file, err := os.OpenFile(filepath.Join(previous, "1.jsonl"), os.O_CREATE|os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				now := time.Now().UTC()
				execution := &workerExecution{WorkerExecution: WorkerExecution{ReceiptWorkerID: prior.Info.ID, Sequence: 1, Key: "orchestrator:semantic-heartbeat", Error: "uncertain admission", Agent: &WorkerAgent{ID: "synthetic-replay-agent", Kind: "heartbeat", StartedAt: now, Deadline: now.Add(time.Second)}}, file: file}
				original := &workerHost{executions: map[uint64]*workerExecution{1: execution}, stop: make(chan struct{})}
				if err := original.saveAdmissionLocked(execution); err != nil {
					t.Fatal(err)
				}
				if late {
					time.Sleep(time.Until(execution.Agent.Deadline) + time.Millisecond)
				}
				original.captureLocked(execution, core.Event{Kind: kind, Text: "synthetic terminal", Done: true}, false, "")
				want := ""
				if late {
					want = context.DeadlineExceeded.Error()
				}
				if original.failure != nil || !execution.Done || execution.Error != want {
					t.Fatalf("terminal outcome = %q, want %q; failure %v", execution.Error, want, original.failure)
				}
				// Crash before another admission checkpoint. Replay both cases
				// after their deadline, using only the original terminal record.
				time.Sleep(time.Until(execution.Agent.Deadline) + time.Millisecond)
				host := &workerHost{directory: current, recordPath: filepath.Join(root, "worker.json"), next: 1, executions: map[uint64]*workerExecution{}, info: WorkerInfo{EnvironmentID: "environment", WorkspaceID: "workspace"}}
				record, err := json.Marshal(prior)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(host.recordPath, record, 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := host.recoverPrevious(cfg); err != nil {
					t.Fatal(err)
				}
				replay := host.executions[1]
				if replay == nil {
					t.Fatal("terminal lost")
				}
				defer replay.file.Close()
				if !replay.Done || replay.Error != want || replay.LastCursor != 1 || replay.CommittedCursor != 0 || replay.ReceiptWorkerID != prior.Info.ID {
					t.Fatalf("terminal result changed during replay: %+v", replay.WorkerExecution)
				}
			})
		}
	}
}

func TestWorkerCrashJournalValidation(t *testing.T) {
	for _, scenario := range []string{"terminal", "unfinished", "atomic-scratch", "unsafe-scratch", "symlink-scratch", "unknown-scratch", "truncated", "future-cursor", "rewound-cursor", "invalid-utf8", "unsafe-file", "symlink", "missing-directory", "changed-config", "unverified-process"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			previous, err := os.MkdirTemp(root, "spynel-worker-")
			if err != nil {
				t.Fatal(err)
			}
			current := filepath.Join(root, "current")
			if err := os.Mkdir(current, 0700); err != nil {
				t.Fatal(err)
			}
			cfg := HarnessConfig{Cwd: root}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			// This read-only model covers file validation; the CLI regression uses
			// actual worker/guardian death and the ordinary recovery consumer.
			native := updater.ProcessRegistration{PID: 1 << 30, StartTime: "absent-fixture", Generation: strings.Repeat("a", 32), Executable: executable, HarnessWorker: true}
			if !errors.Is(updater.VerifyProcess(native), syscall.ESRCH) {
				t.Fatal("fixture PID is not confirmed absent")
			}
			prior := workerRecord{Token: strings.Repeat("b", 32), Info: WorkerInfo{Protocol: WorkerProtocol, ID: strings.Repeat("c", 32), PID: native.PID, Socket: filepath.Join(previous, "control.sock"), EnvironmentID: "environment", WorkspaceID: "workspace", ConfigDigest: ConfigDigest(cfg), Process: native}}
			host := &workerHost{directory: current, recordPath: filepath.Join(root, "worker.json"), next: 1, executions: map[uint64]*workerExecution{}, info: WorkerInfo{EnvironmentID: "environment", WorkspaceID: "workspace"}}
			write := func(path string, data []byte) {
				t.Helper()
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			checkpoint := struct {
				Execution WorkerExecution
				Digest    [32]byte
			}{Execution: WorkerExecution{ReceiptWorkerID: prior.Info.ID, Sequence: 1, Key: "synthetic/replay", LastCursor: 1}}
			if scenario == "rewound-cursor" {
				checkpoint.Execution.CommittedCursor = 1
			}
			admission, _ := json.Marshal(checkpoint)
			write(filepath.Join(previous, "1.jsonl.admission"), admission)
			event := WorkerEvent{Cursor: 1, Event: core.Event{Kind: core.EventFinal, Text: "synthetic terminal", Done: true}}
			if scenario == "unfinished" {
				event.Event.Kind, event.Event.Done = core.EventStatus, false
			}
			data, _ := json.Marshal(event)
			data = append(data, '\n')
			if scenario == "truncated" {
				data = data[:len(data)-1]
			}
			if scenario == "invalid-utf8" {
				data = bytes.Replace(data, []byte("synthetic terminal"), []byte{0xff}, 1)
			}
			journal := filepath.Join(previous, "1.jsonl")
			write(journal, data)
			switch scenario {
			case "atomic-scratch", "unsafe-scratch", "symlink-scratch", "unknown-scratch":
				// AtomicWriteFile can die before renaming this uncommitted sibling.
				scratch := filepath.Join(previous, ".spynel-write-1234567890")
				if scenario == "unknown-scratch" {
					scratch += "-unknown"
				}
				write(scratch, []byte{0xff, '{'})
				if scenario == "unsafe-scratch" {
					if err := os.Chmod(scratch, 0644); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "symlink-scratch" {
					if err := os.Remove(scratch); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(journal, scratch); err != nil {
						t.Fatal(err)
					}
				}
			case "future-cursor":
				write(journal+".cursor", []byte("2\n"))
			case "rewound-cursor":
				write(journal+".cursor", []byte("0\n"))
			case "unsafe-file":
				if err := os.Chmod(journal, 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(root, "unrelated")
				write(target, data)
				if err := os.Remove(journal); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, journal); err != nil {
					t.Fatal(err)
				}
			case "missing-directory":
				if err := os.RemoveAll(previous); err != nil {
					t.Fatal(err)
				}
			case "changed-config":
				prior.Info.ConfigDigest[0]++
			case "unverified-process":
				prior.Info.PID, prior.Info.Process.PID = os.Getpid(), os.Getpid()
			}
			record, _ := json.Marshal(prior)
			write(host.recordPath, record)
			directory, err := host.recoverPrevious(cfg)
			for _, execution := range host.executions {
				if execution.file != nil {
					defer execution.file.Close()
				}
			}
			if scenario != "terminal" && scenario != "unfinished" && scenario != "atomic-scratch" {
				if err == nil {
					t.Fatal("invalid or unverified journal accepted")
				}
				if strings.Contains(err.Error(), root) {
					t.Fatal("private journal path exposed")
				}
				return
			}
			if err != nil || directory != previous || host.next != 2 {
				t.Fatal("journal recovery failed", err)
			}
			if scenario == "unfinished" {
				if len(host.executions) != 0 || host.incomplete != 1 {
					t.Fatal("unfinished execution was represented as completed")
				}
				copied, err := os.ReadFile(filepath.Join(current, "1.jsonl"))
				if err != nil || string(copied) != string(data) {
					t.Fatal("unfinished evidence lost", err)
				}
			} else {
				execution := host.executions[1]
				if execution == nil || !execution.Done || execution.ReceiptWorkerID != prior.Info.ID || execution.CommittedCursor != 0 || execution.LastCursor != 1 {
					t.Fatal("terminal or receipt identity lost")
				}
			}
			if _, err := os.Stat(filepath.Join(previous, "1.jsonl")); err != nil {
				t.Fatal("predecessor changed before publication", err)
			}
			if scenario == "atomic-scratch" {
				if _, err := os.Stat(filepath.Join(current, ".spynel-write-1234567890")); !os.IsNotExist(err) {
					t.Fatal("uncommitted scratch copied into successor", err)
				}
			}
		})
	}
}
