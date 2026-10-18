package harness

import (
	"encoding/json"
	"errors"

	"github.com/agent0ai/spynel/internal/fsx"
)

// configure runs under the worker operation and exact-election fences. This
// commits runtime settings and any supplied canonical save before dispatch resumes.
func (h *workerHost) configure(request WorkerRequest) WorkerReply {
	if request.Config == nil {
		return WorkerReply{Error: "worker configuration is required"}
	}
	previous := h.supervisor.HarnessConfig()
	next := *request.Config
	if next.Cwd != previous.Cwd || next.Version != previous.Version {
		return WorkerReply{Error: "worker workspace and version cannot change"}
	}
	next.Stderr = previous.Stderr
	if !ValidReasoningEffort(next.Effort) {
		return WorkerReply{Error: "invalid worker reasoning effort"}
	}
	var commit, restore func() error
	var unavailable error
	if request.Settings != nil {
		if h.settings == nil {
			return WorkerReply{Error: "canonical worker settings are unavailable"}
		}
		plan, err := h.settings(previous.Cwd, previous, next, *request.Settings)
		if err != nil || plan.Commit == nil || plan.Restore == nil {
			return WorkerReply{Error: "canonical worker settings are invalid or changed"}
		}
		commit, restore, unavailable = plan.Commit, plan.Restore, plan.Unavailable
	}
	digest := ConfigDigest(next)
	publish := func() error {
		var err error
		if commit != nil {
			err = commit()
		}
		if err == nil {
			err = h.publishConfiguration(digest)
		}
		if err != nil && restore != nil {
			if rollback := restore(); rollback != nil {
				h.mu.Lock()
				h.failure = errors.New("canonical worker settings could not be restored")
				h.stopOnce.Do(func() { close(h.stop) })
				h.mu.Unlock()
				err = errors.Join(err, rollback)
			}
		}
		return err
	}
	available, _ := h.supervisor.Available()
	if digest == h.info.ConfigDigest && available {
		if err := publish(); err != nil {
			return WorkerReply{Error: err.Error()}
		}
		return WorkerReply{ConfigDigest: digest}
	}
	if request.ExpectedConfig != h.info.ConfigDigest {
		return WorkerReply{Error: "worker configuration changed; inspect before applying settings"}
	}
	inferenceOnly := next
	inferenceOnly.Model, inferenceOnly.Effort, inferenceOnly.LegacyEffort, inferenceOnly.ServiceMode = previous.Model, previous.Effort, previous.LegacyEffort, previous.ServiceMode
	var err error
	if ConfigDigest(inferenceOnly) == h.info.ConfigDigest && available {
		err = h.supervisor.CommitInference(InferenceSelection{
			Model: next.Model, Effort: next.Effort, LegacyEffort: next.LegacyEffort, ServiceMode: next.ServiceMode,
		}, publish)
	} else {
		h.mu.Lock()
		retained := len(h.executions) > 0
		h.mu.Unlock()
		if retained {
			return WorkerReply{Error: "consume and retire worker executions before replacing their harness"}
		}
		if unavailable != nil {
			err = h.supervisor.CommitUnavailable(next, unavailable, publish)
		} else if err = h.supervisor.Reconfigure(next); err == nil {
			if err = publish(); err != nil {
				if rollback := h.supervisor.Reconfigure(previous); rollback != nil {
					h.mu.Lock()
					h.failure = errors.New("worker configuration could not be restored")
					h.stopOnce.Do(func() { close(h.stop) })
					h.mu.Unlock()
					err = errors.Join(err, rollback)
				}
			}
		}
	}
	if err != nil {
		return WorkerReply{Error: err.Error()}
	}
	return WorkerReply{ConfigDigest: h.info.ConfigDigest}
}

func (h *workerHost) publishConfiguration(digest [32]byte) error {
	current, err := readWorkerRecord(h.recordPath)
	if err != nil || current.Info.ID != h.info.ID || current.Token != h.token || current.Info.ConfigDigest != h.info.ConfigDigest {
		return errors.New("worker configuration record unavailable or changed")
	}
	current.Info.ConfigDigest = digest
	data, err := json.Marshal(current)
	if err != nil {
		return err
	}
	if err := fsx.AtomicWriteFile(h.recordPath, data, 0600); err != nil {
		return errors.New("worker configuration record could not be saved")
	}
	h.info.ConfigDigest = digest
	return nil
}
