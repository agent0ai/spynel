package app

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/agent0ai/spynel/internal/harness"
)

type jobWorkerReceipt struct {
	Worker     string
	Sequence   uint64
	Cursor     uint64
	Iterations int
	Digest     [32]byte
}

func (r jobWorkerReceipt) valid() bool {
	return len(r.Worker) >= 32 && len(r.Worker) <= 128 && strings.IndexFunc(r.Worker, func(c rune) bool {
		return !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-' || c == '_')
	}) < 0 && r.Sequence > 0 && r.Cursor > 0
}

// RecordJobDelivery durably records one ordered worker event before its cursor
// can commit. The private prefix survives reopening the same workflow archive.
func (r *Runtime) RecordJobDelivery(id int, delivery harness.WorkerDelivery) error {
	r.mu.Lock()
	job, ok := r.jobs[id]
	archive := r.archive
	r.mu.Unlock()
	if !ok || archive == nil {
		return errors.New("worker job archive unavailable")
	}
	return archive.workerEvent(job.StableID, r.Now().UTC(), delivery)
}

func (a *JobArchive) workerEvent(stableID string, at time.Time, delivery harness.WorkerDelivery) error {
	data, err := json.Marshal(delivery.Event)
	if err != nil {
		return err
	}
	next := jobWorkerReceipt{Worker: delivery.WorkerID, Sequence: delivery.Sequence, Cursor: delivery.Cursor, Digest: sha256.Sum256(data)}
	if !next.valid() {
		return errors.New("invalid job worker receipt")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	record := a.active[stableID]
	if record == nil {
		return errors.New("worker job archive is not owned")
	}
	next.Iterations = record.ProviderIterations
	previous := record.workerReceipt
	if previous != nil {
		if previous.Worker != next.Worker || previous.Sequence != next.Sequence {
			if next.Cursor != 1 || next.Iterations <= previous.Iterations {
				return errors.New("worker job execution changed without a new reserved turn")
			}
			previous = nil // A later durable reservation starts a new event prefix in the same job.
		}
		// The authenticated worker journal is immutable; a page may replay any
		// earlier prefix after its commit reply was lost.
		if previous != nil && next.Cursor <= previous.Cursor {
			if next.Cursor == previous.Cursor && next.Digest != previous.Digest {
				return errors.New("worker job event changed")
			}
			return a.writeLocked(record)
		}
	}
	expected := uint64(1)
	if previous != nil {
		expected = previous.Cursor + 1
	}
	if next.Cursor != expected {
		return errors.New("worker job event gap")
	}
	oldReceipt := record.workerReceipt
	output, sequence, truncated, written := record.Output.String(), record.Sequence, record.Truncated, record.LastWriteAt
	record.workerReceipt = &next
	if err := a.eventLocked(record, at, delivery.Event, true); err != nil {
		record.workerReceipt, record.Sequence, record.Truncated, record.LastWriteAt = oldReceipt, sequence, truncated, written
		record.Output = strings.Builder{}
		record.Output.WriteString(output)
		return err
	}
	return nil
}
