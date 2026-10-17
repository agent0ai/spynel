package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
)

func TestWorkerJobDeliveryPersistsAcrossRuntimeReplacement(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "jobs")
	details := JobDetails{DurableFile: "synthetic.md", WorkID: "synthetic", Kind: "task", Phase: "task_implementation", PhaseAttempt: 2, ProviderIterations: 5}
	open := func() (*Runtime, int, Job) {
		t.Helper()
		r := NewRuntime()
		r.ConfigureJobArchive(directory)
		id, err := r.TryBeginJobWithDetails("synthetic", "orchestrator", "markdown", "synthetic", details)
		if err != nil {
			t.Fatal(err)
		}
		job, _ := r.Job(id)
		return r, id, job
	}
	r, id, first := open()
	delivery := harness.WorkerDelivery{WorkerID: strings.Repeat("a", 32), Sequence: 1, WorkerEvent: harness.WorkerEvent{Cursor: 1, Event: core.Event{Kind: core.EventDelta, Text: "synthetic first"}}}
	if err := r.RecordJobDelivery(id, delivery); err != nil {
		t.Fatal(err)
	}
	delivery.Cursor, delivery.Event.Text = 2, "synthetic second"
	if err := r.RecordJobDelivery(id, delivery); err != nil {
		t.Fatal(err)
	}
	r.Close()
	r, id, second := open()
	defer r.Close()
	if first.StableID != second.StableID || first.Number != second.Number || first.Generation != second.Generation {
		t.Fatal("attachment allocated another public job")
	}
	delivery.Cursor, delivery.Event.Text = 1, "synthetic first"
	if err := r.RecordJobDelivery(id, delivery); err != nil {
		t.Fatal(err)
	}
	delivery.Cursor, delivery.Event.Text = 2, "synthetic second"
	if err := r.RecordJobDelivery(id, delivery); err != nil {
		t.Fatal(err)
	}
	changed := delivery
	changed.Event.Text = "changed"
	if r.RecordJobDelivery(id, changed) == nil {
		t.Fatal("changed event was confirmed")
	}
	changed = delivery
	changed.Cursor = 4
	if r.RecordJobDelivery(id, changed) == nil {
		t.Fatal("event gap was confirmed")
	}
	changed = delivery
	changed.WorkerID, changed.Cursor = strings.Repeat("b", 32), 1
	if r.RecordJobDelivery(id, changed) == nil {
		t.Fatal("execution changed without a new reservation")
	}
	item, output, err := r.ArchivedJob(second.Number)
	if err != nil || strings.Count(output, "synthetic first") != 1 || strings.Count(output, "synthetic second") != 1 {
		t.Fatalf("duplicate event after replay: %q, %v", output, err)
	}
	public, _ := json.Marshal(item)
	if strings.Contains(string(public), delivery.WorkerID) || strings.Contains(output, "worker_delivery") {
		t.Fatal("receipt leaked through inspection")
	}
	// A genuine later provider reservation can reopen the same durable job.
	r.UpdateJobDurableTiming(id, details.FirstAssignedAt, 6)
	if err := r.RecordJobDelivery(id, changed); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerJobDeliveryStorageFailureDoesNotAdvancePrefix(t *testing.T) {
	r := NewRuntime()
	defer r.Close()
	directory := filepath.Join(t.TempDir(), "jobs")
	r.ConfigureJobArchive(directory)
	id, err := r.TryBeginJobWithDetails("synthetic", "cli", "synthetic", "synthetic", JobDetails{ProviderIterations: 1})
	if err != nil {
		t.Fatal(err)
	}
	job, _ := r.Job(id)
	path := filepath.Join(directory, job.StableID+".md")
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	delivery := harness.WorkerDelivery{WorkerID: strings.Repeat("a", 32), Sequence: 1, WorkerEvent: harness.WorkerEvent{Cursor: 1, Event: core.Event{Kind: core.EventDelta, Text: "one durable event"}}}
	if r.RecordJobDelivery(id, delivery) == nil {
		t.Fatal("storage failure was consumed")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".saved", path); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordJobDelivery(id, delivery); err != nil {
		t.Fatal(err)
	}
	_, output, err := r.ArchivedJob(job.Number)
	if err != nil || strings.Count(output, "one durable event") != 1 {
		t.Fatalf("failed write duplicated output: %q %v", output, err)
	}
	r.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "worker_delivery: ", "worker_delivery: invalid-", 1))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readJobArchive(path, false); err == nil {
		t.Fatal("corrupt receipt silently discarded")
	}
}
