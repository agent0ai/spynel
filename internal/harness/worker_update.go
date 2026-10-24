package harness

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/agent0ai/spynel/internal/updater"
)

// Installation updates use their native registered owner, not a workspace
// election term: one installation may serve several independently elected owners.
func (h *workerHost) serveUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+h.token)) != 1 {
		http.Error(w, "update authorization denied", http.StatusUnauthorized)
		return
	}
	var request updater.WorkerUpdateRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || request.Generation != h.info.Process.Generation {
		http.Error(w, "invalid update identity", http.StatusBadRequest)
		return
	}
	if err := h.operations.LockContext(r.Context()); err != nil {
		http.Error(w, "update expired", http.StatusRequestTimeout)
		return
	}
	defer h.operations.Unlock()
	if request.Action != "check" {
		registered, err := updater.ProcessRecord(request.Owner.PID)
		if err != nil || registered.Installation == "" || registered.Installation != h.info.Process.Installation || registered.Generation != request.Owner.Generation || updater.VerifyProcess(request.Owner) != nil {
			http.Error(w, "update owner unavailable", http.StatusForbidden)
			return
		}
	}
	if r.Context().Err() != nil {
		http.Error(w, "update expired", http.StatusRequestTimeout)
		return
	}
	if err := h.applyUpdate(request); err != nil {
		http.Error(w, "worker is busy", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *workerHost) updatePaused() bool {
	if h.updateOwner.PID != 0 && !updater.WorkerUpdateActive(h.info.Process.Installation) && updater.WorkerInstallationCurrent(h.updateOwner) && updater.VerifyProcess(h.updateOwner) == nil {
		h.updateOwner = updater.ProcessRegistration{}
	}
	return h.updateOwner.PID != 0
}

func (h *workerHost) applyUpdate(request updater.WorkerUpdateRequest) error {
	paused := h.updatePaused()
	sameOwner := paused && h.updateOwner.PID == request.Owner.PID && h.updateOwner.Generation == request.Owner.Generation
	if request.Action == "resume" {
		if sameOwner && updater.WorkerInstallationCurrent(h.updateOwner) {
			h.updateOwner = updater.ProcessRegistration{}
		}
		return nil
	}
	if h.stopping {
		return errors.New("worker is stopping")
	}
	if request.Action == "recover" {
		if request.Protocol != h.info.Protocol || !updater.WorkerUpdateActive(h.info.Process.Installation) {
			return errors.New("incompatible update recovery")
		}
		h.updateOwner = updater.ProcessRegistration{}
		return nil
	}
	h.mu.Lock()
	retained := len(h.executions) != 0 || h.failure != nil
	h.mu.Unlock()
	h.supervisor.operationMu.Lock()
	h.supervisor.mu.RLock()
	busy := len(h.supervisor.active) != 0 || h.supervisor.closing != nil
	h.supervisor.mu.RUnlock()
	h.supervisor.operationMu.Unlock()
	if retained || busy {
		return errors.New("active work or pending delivery")
	}
	switch request.Action {
	case "check":
	case "pause":
		if !updater.WorkerUpdateActive(h.info.Process.Installation) {
			return errors.New("update has no installation lock")
		}
		h.updateOwner = request.Owner
	case "stop":
		if !sameOwner {
			return errors.New("worker was not paused by this update")
		}
		h.stopping = true
		h.stopOnce.Do(func() { close(h.stop) })
	default:
		return errors.New("invalid update action")
	}
	return nil
}
