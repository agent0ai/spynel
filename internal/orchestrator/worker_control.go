package orchestrator

import (
	"context"
	"errors"

	"github.com/agent0ai/spynel/internal/harness"
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
			reply, err := m.Worker.Call(ctx, harness.WorkerRequest{Action: "interrupt", Key: key})
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
