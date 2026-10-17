package orchestrator

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/agent0ai/spynel/internal/harness"
)

// ReserveWorkerTurn runs inside the worker's exact-election admission fence;
// it must never reacquire election ownership or call the worker client.
func ReserveWorkerTurn(root, key string, work harness.WorkerWorkflow) (harness.WorkerReservation, error) {
	var result harness.WorkerReservation
	validPhase := work.Route == "tasks" && (work.Phase == phaseTaskImplementation || work.Phase == phaseTaskReview) ||
		work.Route == "goals" && (work.Phase == phaseGoalPlanning || work.Phase == phaseGoalReview)
	if !validPhase || work.WorkID == "" || work.OwnerID == "" || work.ClaimAttempt < 1 ||
		work.LeaseID != leaseID(work.Route+":"+work.Phase, work.WorkID) || work.ClaimID != work.LeaseID ||
		key != phaseSessionKey(work.Route, work.WorkID, work.Phase, work.ClaimAttempt) {
		return result, errors.New("invalid worker workflow identity")
	}
	path := filepath.Join(root, ".spynel", work.Route, phaseClaimedStatus(work.Phase), filepath.Base(work.File))
	leasePath := filepath.Join(root, ".spynel", "runtime", "leases", work.LeaseID+".json")
	if work.File != path {
		return result, errors.New("worker workflow path does not match its claimed phase")
	}
	for _, candidate := range []string{path, leasePath} {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil || resolved != candidate {
			return result, errors.New("worker workflow document or lease unavailable")
		}
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() {
			return result, errors.New("worker workflow document or lease is not regular")
		}
	}
	lock, err := lockProviderTurn(path)
	if err != nil {
		return result, err
	}
	defer unlockProviderTurn(lock)
	file, err := os.Open(leasePath)
	if err != nil {
		return result, err
	}
	data, err := io.ReadAll(io.LimitReader(file, 64*1024+1))
	closeErr := file.Close()
	var lease Lease
	if err != nil || closeErr != nil || len(data) > 64*1024 || json.Unmarshal(data, &lease) != nil {
		return result, errors.New("worker workflow lease is unreadable")
	}
	if lease.ID != work.LeaseID || lease.ClaimID != work.ClaimID || lease.OwnerID != work.OwnerID ||
		lease.SessionKey != key || lease.File != path || lease.Route != work.Route || lease.Phase != work.Phase ||
		lease.ClaimAttempt != work.ClaimAttempt || lease.State != "processing" && lease.State != "recovering" {
		return result, errors.New("worker workflow claim changed before admission")
	}
	document, err := ReadDocument(path)
	if err != nil {
		return result, err
	}
	if documentID(document) != work.WorkID || stringField(document, "status") != phaseClaimedStatus(work.Phase) ||
		numberValue(document.FrontMatter[phaseAttemptField(work.Phase)]) != work.ClaimAttempt {
		return result, errors.New("worker workflow document changed before admission")
	}
	result.FirstAssignedAt, result.ProviderIterations, err = reserveProviderTurnLocked(path, time.Now().UTC(), document)
	if work.Route == "tasks" {
		result.ImplementationAttempts = numberValue(document.FrontMatter["attempt"])
	}
	return result, err
}
