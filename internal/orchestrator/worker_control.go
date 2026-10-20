package orchestrator

import (
	"context"
	"errors"

	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/shortid"
)

// SendControl routes to the execution owner. Controller-local callbacks are
// never serialized: the worker validates and reserves the exact durable claim.
func (m *Manager) SendControl(ctx context.Context, key string, request harness.ControlRequest) (harness.ControlResult, error) {
	if m.Worker == nil {
		return harness.ControlResult{}, errors.New("workflow worker control is unavailable")
	}
	lease, ok := m.LeaseForSession(key)
	if !ok || !m.ControlStillValid(lease, lease.WorkID) {
		return harness.ControlResult{}, errors.New("workflow control ownership changed")
	}
	snapshot, err := m.Worker.Call(ctx, harness.WorkerRequest{Action: "inspect"})
	if err != nil {
		return harness.ControlResult{}, err
	}
	for _, execution := range snapshot.Executions {
		if execution.Key != key || execution.Workflow == nil {
			continue
		}
		work := &harness.WorkerWorkflow{LeaseID: lease.ID, ClaimID: lease.ClaimID, OwnerID: lease.OwnerID,
			WorkID: lease.WorkID, File: lease.File, Route: lease.Route, Phase: lease.Phase, ClaimAttempt: lease.ClaimAttempt}
		reply, err := m.Worker.Call(ctx, harness.WorkerRequest{Action: "control", Sequence: execution.Sequence, Key: key, Workflow: work,
			Prompt: request.Prompt, Control: &harness.WorkerControl{ID: request.ID, ContinuationPrompt: request.ContinuationPrompt}})
		if err == nil && reply.Control == nil {
			err = errors.New("worker control result unavailable")
		}
		if err != nil {
			return harness.ControlResult{}, err
		}
		return *reply.Control, nil
	}
	return harness.ControlResult{}, errors.New("workflow execution is no longer retained")
}

func (m *Manager) Interrupt(ctx context.Context, key string) (bool, error) {
	if m.Worker == nil {
		return false, errors.New("workflow worker control is unavailable")
	}
	m.workerAdmissionMu.Lock()
	defer m.workerAdmissionMu.Unlock()
	snapshot, err := m.Worker.Call(ctx, harness.WorkerRequest{Action: "inspect"})
	if err != nil {
		return false, err
	}
	for _, execution := range snapshot.Executions {
		if execution.Key == key {
			if m.Harness.IsActive(key) {
				return false, errors.New("session has conflicting execution owners")
			}
			var alreadyRequested bool
			if execution.Workflow != nil {
				alreadyRequested, err = m.workerCancellation(ctx, *execution.Workflow, "cancel")
				if err != nil {
					return false, err
				}
			}
			var intent string
			if execution.Conversation != nil && execution.SourcesReserved && !execution.Done {
				if m.ConversationCancellation == nil {
					return false, errors.New("durable conversation cancellation unavailable")
				}
				intent, err = shortid.New()
				if err == nil {
					err = m.Worker.RunOwned(ctx, func() error { return m.ConversationCancellation(*execution.Conversation, intent, false) })
				}
				if err != nil {
					return false, err
				}
			}
			reply, err := m.Worker.Call(ctx, harness.WorkerRequest{Action: "interrupt", Key: key})
			if intent != "" && !execution.Cancelled && err == nil && !reply.Interrupted && ctx.Err() == nil {
				err = m.Worker.RunOwned(ctx, func() error { return m.ConversationCancellation(*execution.Conversation, intent, true) })
			}
			if execution.Workflow != nil && !alreadyRequested && !execution.Cancelled && err == nil && !reply.Interrupted && ctx.Err() == nil {
				_, err = m.workerCancellation(ctx, *execution.Workflow, "restore-cancellation")
			}
			return reply.Interrupted, err
		}
	}
	var stopped bool
	err = m.Worker.RunOwned(ctx, func() error {
		var err error
		stopped, err = m.Harness.Interrupt(ctx, key)
		return err
	})
	return stopped, err
}

// Persist before the RPC: worker death must not erase an uncertain interruption.
func (m *Manager) workerCancellation(ctx context.Context, work harness.WorkerWorkflow, action string) (alreadyRequested bool, err error) {
	work.OwnerID = m.ownerID
	err = m.Worker.RunOwned(ctx, func() error {
		m.scanMu.Lock()
		defer m.scanMu.Unlock()
		lease, err := m.loadLease(work.LeaseID)
		if err != nil {
			return err
		}
		alreadyRequested = lease.CancellationRequested
		_, err = ApplyWorkerWorkflow(m.Config.Root, lease.SessionKey, work, action)
		return err
	})
	return alreadyRequested, err
}
