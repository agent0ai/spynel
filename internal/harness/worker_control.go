package harness

import (
	"context"
	"errors"
	"time"
)

// control is called inside the request's election fence; only later queued
// deliveries acquire that fence again, around their entire admission.
func (h *workerHost) control(request WorkerRequest) WorkerReply {
	h.mu.Lock()
	execution := h.executions[request.Sequence]
	valid := execution != nil && !execution.Done && !execution.Cancelled && execution.Key == request.Key && execution.Workflow != nil && request.Workflow != nil
	if valid {
		claim := *execution.Workflow
		claim.OwnerID = request.Workflow.OwnerID // Managers change on attachment; the claim does not.
		valid = claim == *request.Workflow
	}
	h.mu.Unlock()
	if !valid || h.workflow == nil || request.Control == nil || len(request.Control.ID) < 8 || len(request.Control.ID) > 128 || request.Prompt == "" {
		return WorkerReply{Error: "worker control requires the exact retained workflow execution"}
	}
	apply := func(action string) bool {
		reservation, err := h.workflow(h.supervisor.HarnessConfig().Cwd, request.Key, *request.Workflow, action)
		if err != nil {
			return false
		}
		if action == "reserve-control" {
			h.mu.Lock()
			defer h.mu.Unlock()
			execution.Reservation = &reservation
			if h.saveAdmissionLocked(execution) != nil {
				h.failJournalLocked()
				return false
			}
		}
		return true
	}
	if !apply("validate") {
		return WorkerReply{Error: "worker control workflow ownership changed"}
	}
	// Bound the acknowledgement wait without cancelling the retained provider turn.
	ctx, cancel := context.WithTimeout(h.supervisor.ctx, 5*time.Second)
	defer cancel()
	result, err := h.supervisor.SendControl(ctx, request.Key, ControlRequest{
		ID: request.Control.ID, Prompt: request.Prompt, ContinuationPrompt: request.Control.ContinuationPrompt,
		Validate: func() bool { return apply("validate") }, PrepareContinuation: func() bool { return apply("continue") },
		ReserveProviderTurn: func() bool { return apply("reserve-control") },
		RunContinuation: func(action func()) error {
			h.operations.Lock()
			defer h.operations.Unlock()
			ran, err := h.election.RunForOwner(request.InstanceID, request.TermToken, func() error {
				h.mu.Lock()
				blocked := h.failure != nil || execution.Cancelled
				h.mu.Unlock()
				if blocked {
					return errors.New("worker continuation is no longer admitted")
				}
				action()
				return nil
			})
			if err == nil && !ran {
				err = errors.New("worker continuation controller changed")
			}
			return err
		},
	})
	if err != nil {
		return WorkerReply{Error: err.Error()}
	}
	return WorkerReply{Control: &result}
}

// Persist intent before provider interruption, including synchronous terminal
// callbacks. A rejected interrupt restores only this serialized attempt.
func (h *workerHost) interrupt(key string) WorkerReply {
	h.mu.Lock()
	var marked []*workerExecution
	for _, execution := range h.executions {
		if execution.Key == key && !execution.Done && !execution.Cancelled {
			execution.Cancelled = true
			marked = append(marked, execution)
			if h.saveAdmissionLocked(execution) != nil {
				h.failJournalLocked()
				h.mu.Unlock()
				return WorkerReply{Error: "persist worker cancellation failed"}
			}
		}
	}
	h.mu.Unlock()
	interrupted, err := h.supervisor.Interrupt(context.Background(), key)
	if err != nil || !interrupted {
		h.mu.Lock()
		for _, execution := range marked {
			execution.Cancelled = false
			if h.saveAdmissionLocked(execution) != nil {
				h.failJournalLocked()
				err = errors.Join(err, errors.New("restore worker cancellation failed"))
			}
		}
		h.mu.Unlock()
	}
	if err != nil {
		return WorkerReply{Error: err.Error()}
	}
	return WorkerReply{Interrupted: interrupted}
}
