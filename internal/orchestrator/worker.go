package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/fsx"
	"github.com/agent0ai/spynel/internal/harness"
)

// attachWorkerWorkflows runs under the election fence and scan lock, using a
// snapshot fetched before either lock. No RPC may run inside this callback.
func (m *Manager) attachWorkerWorkflows(ctx context.Context, executions []harness.WorkerExecution) error {
	for _, execution := range executions {
		work := execution.Workflow
		if work == nil || m.isInflight(work.LeaseID) {
			continue
		}
		path, leasePath, err := workerWorkflowPaths(m.Config.Root, execution.Key, *work)
		if err != nil {
			return err
		}
		for _, candidate := range []string{path, leasePath} {
			resolved, err := filepath.EvalSymlinks(candidate)
			if os.IsNotExist(err) && candidate == path {
				continue
			}
			if err != nil || resolved != candidate {
				return errors.New("retained worker workflow path unavailable or unsafe")
			}
			info, err := os.Stat(candidate)
			if err != nil || !info.Mode().IsRegular() {
				return errors.New("retained worker workflow path is not regular")
			}
		}
		lease, err := m.loadLease(work.LeaseID)
		if err != nil {
			return errors.New("retained worker workflow lease unavailable; refusing replacement")
		}
		if lease.State == "hook_cancelled" || lease.State == "claiming" || lease.WorkID != "" && lease.WorkID != work.WorkID || lease.ID != work.LeaseID || lease.ClaimID != work.ClaimID || lease.File != work.File ||
			lease.SessionKey != execution.Key || lease.Route != work.Route || lease.Phase != work.Phase || lease.ClaimAttempt != work.ClaimAttempt {
			return errors.New("retained worker workflow claim changed; refusing replacement")
		}
		route, ok := routeByName(work.Route)
		if !ok {
			return errors.New("retained worker workflow route unavailable")
		}
		if document, err := ReadDocument(lease.File); err == nil {
			if documentID(document) != work.WorkID || numberValue(document.FrontMatter[phaseAttemptField(work.Phase)]) != work.ClaimAttempt {
				return errors.New("retained worker workflow document changed")
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		lease.WorkID, lease.OwnerID, lease.HeartbeatAt = work.WorkID, m.ownerID, time.Now().UTC()
		if !execution.Done {
			lease.State = "processing"
		}
		if execution.Cancelled {
			lease.State = "cancelled"
		}
		if err := m.saveLease(lease); err != nil {
			return err
		}
		m.dispatchExecution(ctx, route, lease, false, &execution)
	}
	return nil
}

func (m *Manager) runWorkerWorkflow(ctx context.Context, route workflowRoute, lease Lease, prompt string, execution *harness.WorkerExecution, jobID int, consume func(core.Event, *harness.WorkerDelivery) error) (retired bool) {
	if jobID <= 0 || m.JobDelivery == nil {
		m.recordDispatchError(ctx, lease, errors.New("durable worker job admission and delivery are required"))
		return
	}
	if execution == nil {
		document, err := ReadDocument(lease.File)
		if err != nil {
			m.recordDispatchError(ctx, lease, err)
			return
		}
		work := &harness.WorkerWorkflow{LeaseID: lease.ID, ClaimID: lease.ClaimID, OwnerID: lease.OwnerID,
			WorkID: documentID(document), File: lease.File, Route: route.Name, Phase: lease.Phase, ClaimAttempt: lease.ClaimAttempt}
		// Sequence allocation and Send share this local fence across workflows.
		// A failed reply is reconciled by the next scan, never automatically resent.
		m.workerAdmissionMu.Lock()
		reply, err := m.Worker.Call(ctx, harness.WorkerRequest{Action: "inspect"})
		if err == nil {
			reply, err = m.Worker.Call(ctx, harness.WorkerRequest{Action: "send", Sequence: reply.NextSequence, Key: lease.SessionKey, Prompt: prompt, Workflow: work})
		}
		m.workerAdmissionMu.Unlock()
		if err != nil || len(reply.Executions) != 1 {
			if err == nil {
				err = errors.New("worker dispatch result unavailable")
			}
			m.recordDispatchError(ctx, lease, err)
			return
		}
		execution = &reply.Executions[0]
	}
	err := m.Worker.RunOwned(ctx, func() error {
		if execution.Reservation != nil && m.JobTimingUpdated != nil {
			m.JobTimingUpdated(jobID, execution.Reservation.FirstAssignedAt, execution.Reservation.ProviderIterations)
		}
		_, err := m.updateLease(lease, func(current *Lease) {
			current.ThreadID, current.HeartbeatAt = execution.ThreadID, time.Now().UTC()
			if current.State == "recovering" {
				current.State = "processing"
				current.RecoveryCount++
			}
		})
		return err
	})
	if err != nil {
		return
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, err := m.Worker.Consume(ctx, execution.Sequence, func(delivery harness.WorkerDelivery) error {
			return consume(delivery.Event, &delivery)
		})
		if err != nil {
			if ctx.Err() == nil {
				m.recordDispatchError(ctx, lease, err)
			}
			return
		}
		if current.Reservation != nil && (execution.Reservation == nil || *current.Reservation != *execution.Reservation) {
			if err := m.Worker.RunOwned(ctx, func() error {
				if m.JobTimingUpdated != nil {
					m.JobTimingUpdated(jobID, current.Reservation.FirstAssignedAt, current.Reservation.ProviderIterations)
				}
				return nil
			}); err != nil {
				return
			}
			execution.Reservation = current.Reservation
		}
		if current.Done && current.CommittedCursor == current.LastCursor {
			if current.Cancelled {
				if err := m.Worker.RunOwned(ctx, func() error {
					_, err := m.updateLease(lease, func(saved *Lease) { saved.State = "cancelled" })
					return err
				}); err != nil && !os.IsNotExist(err) {
					return
				}
			}
			if current.Error != "" && !current.Cancelled {
				err := m.Worker.RunOwned(ctx, func() error {
					saved, err := m.updateLease(lease, func(currentLease *Lease) {
						currentLease.State, currentLease.LastError = "error", current.Error
					})
					if err == nil && m.JobUpdated != nil {
						m.JobUpdated(jobID, saved)
					}
					return err
				})
				if err != nil && !os.IsNotExist(err) {
					return
				}
			}
			m.workerAdmissionMu.Lock()
			_, err := m.Worker.Call(ctx, harness.WorkerRequest{Action: "ack", Sequence: current.Sequence, After: current.LastCursor})
			m.workerAdmissionMu.Unlock()
			if err != nil {
				m.log("retire worker workflow: " + err.Error())
				return
			}
			if current.Error != "" || current.Cancelled || !m.leaseExists(lease.ID) {
				m.finishRuntimeJob(lease.ID)
			}
			return true
		}
		select {
		case <-ctx.Done():
			return // Detach; the independent worker retains execution ownership.
		case <-ticker.C:
		}
	}
}

func (m *Manager) recordDispatchError(ctx context.Context, lease Lease, err error) {
	if m.Worker == nil {
		m.recordError(lease, err)
		return
	}
	_ = m.Worker.RunOwned(ctx, func() error {
		m.recordError(lease, err)
		if id := m.runtimeJob(lease.ID); id > 0 && m.JobExecutionUpdated != nil {
			m.JobExecutionUpdated(id, core.ExecutionStatus{State: "degraded", Detail: "Worker delivery requires reconciliation"})
		}
		return nil
	})
}

// ApplyWorkerWorkflow runs under the worker's election fence and shares the
// document lock for validation, continuation preparation and reservations.
func ApplyWorkerWorkflow(root, key string, work harness.WorkerWorkflow, action string) (harness.WorkerReservation, error) {
	var result harness.WorkerReservation
	if action != "reserve" && action != "validate" && action != "continue" && action != "reserve-control" {
		return result, errors.New("invalid worker workflow action")
	}
	path, leasePath, err := workerWorkflowPaths(root, key, work)
	if err != nil {
		return result, err
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
	if lease.ID != work.LeaseID || lease.WorkID != "" && lease.WorkID != work.WorkID || lease.ClaimID != work.ClaimID || lease.OwnerID != work.OwnerID ||
		lease.SessionKey != key || lease.File != path || lease.Route != work.Route || lease.Phase != work.Phase ||
		lease.ClaimAttempt != work.ClaimAttempt || lease.State != "processing" && lease.State != "recovering" && (action == "reserve" || lease.State != "awaiting_transition") {
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
	if action == "continue" {
		lease.State, lease.LastError = "processing", ""
		data, err := json.Marshal(lease)
		if err != nil {
			return result, err
		}
		if err := fsx.AtomicWriteFile(leasePath, append(data, '\n'), 0600); err != nil {
			return result, err
		}
	}
	result.FirstAssignedAt, _ = frontMatterTime(document.FrontMatter["first_assigned_at"])
	result.ProviderIterations = numberValue(document.FrontMatter["provider_iterations"])
	if action == "reserve" || action == "reserve-control" {
		result.FirstAssignedAt, result.ProviderIterations, err = reserveProviderTurnLocked(path, time.Now().UTC(), document)
	}
	if work.Route == "tasks" {
		result.ImplementationAttempts = numberValue(document.FrontMatter["attempt"])
	}
	return result, err
}

func workerWorkflowPaths(root, key string, work harness.WorkerWorkflow) (string, string, error) {
	validPhase := work.Route == "tasks" && (work.Phase == phaseTaskImplementation || work.Phase == phaseTaskReview) ||
		work.Route == "goals" && (work.Phase == phaseGoalPlanning || work.Phase == phaseGoalReview)
	if !validPhase || work.WorkID == "" || work.OwnerID == "" || work.ClaimAttempt < 1 ||
		work.LeaseID != leaseID(work.Route+":"+work.Phase, work.WorkID) || work.ClaimID != work.LeaseID ||
		key != phaseSessionKey(work.Route, work.WorkID, work.Phase, work.ClaimAttempt) {
		return "", "", errors.New("invalid worker workflow identity")
	}
	path := filepath.Join(root, ".spynel", work.Route, phaseClaimedStatus(work.Phase), filepath.Base(work.File))
	leasePath := filepath.Join(root, ".spynel", "runtime", "leases", work.LeaseID+".json")
	if work.File != path {
		return "", "", errors.New("worker workflow path does not match its claimed phase")
	}
	return path, leasePath, nil
}
