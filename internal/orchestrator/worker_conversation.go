package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
)

// ErrConversationPending means provider admission or delivery must reconcile;
// callers must not turn it into a fabricated terminal history result.
var ErrConversationPending = errors.New("conversation remains pending reconciliation")

// AdmitConversation requires already persisted sources. Application admission
// runs under exact ownership; worker RPCs never run inside that callback.
func (m *Manager) AdmitConversation(ctx context.Context, conversation harness.WorkerConversation, prompt, message string) (harness.WorkerExecution, error) {
	m.workerAdmissionMu.Lock()
	defer m.workerAdmissionMu.Unlock()
	return m.admitConversation(ctx, conversation, prompt, message, false, nil)
}

// Converse consumes an ordinary request through the same admission, receipt and
// retirement boundary as scheduled attachment. Request cancellation detaches.
func (m *Manager) Converse(ctx context.Context, conversation harness.WorkerConversation, prompt, message string, followupOnly bool, deliver func(context.Context, int, harness.WorkerDelivery) error) error {
	if deliver == nil {
		return errors.New("conversation request delivery unavailable")
	}
	m.workerAdmissionMu.Lock()
	request := &conversationRequest{deliver: deliver, done: make(chan error, 1)}
	var sequence uint64
	execution, err := m.admitConversation(ctx, conversation, prompt, message, followupOnly, func(execution harness.WorkerExecution) error {
		sequence = execution.Sequence
		return m.registerConversationRequest(sequence, conversation.SourceMessageID, request)
	})
	if err != nil {
		m.workerAdmissionMu.Unlock()
		m.removeConversationRequest(sequence, conversation.SourceMessageID, request)
		return err
	}
	m.startWorkerConversationConsumer(ctx, execution)
	m.workerAdmissionMu.Unlock()
	defer m.removeConversationRequest(sequence, conversation.SourceMessageID, request)
	select {
	case err := <-request.done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", ErrConversationPending, ctx.Err())
	}
}

// The caller holds the shared snapshot/admission/retirement lock.
func (m *Manager) admitConversation(ctx context.Context, conversation harness.WorkerConversation, prompt, message string, followupOnly bool, prepare func(harness.WorkerExecution) error) (result harness.WorkerExecution, resultErr error) {
	pending := m.runtimeJob("worker-conversation:"+conversation.ExecutionID) > 0
	defer func() {
		if pending {
			m.requestScan()
			if resultErr != nil {
				resultErr = fmt.Errorf("%w: %w", ErrConversationPending, resultErr)
			}
		}
	}()
	if m.Worker == nil || m.ConversationStarted == nil || m.ConversationAttached == nil || m.ConversationDelivery == nil || m.JobFinished == nil || conversation.Validate() != nil || prompt == "" {
		return harness.WorkerExecution{}, errors.New("worker conversation admission unavailable or invalid")
	}
	conversation.Sources = slices.Clone(conversation.Sources)
	reply, err := m.Worker.Call(ctx, harness.WorkerRequest{Action: "inspect"})
	if err != nil {
		return harness.WorkerExecution{}, err
	}
	key := "chat:" + conversation.Channel + ":" + conversation.Conversation
	execution := harness.WorkerExecution{Key: key, Sequence: reply.NextSequence, Conversation: &conversation}
	action := "send"
	for _, retained := range reply.Executions {
		if retained.Key == key {
			if retained.Conversation == nil || retained.Done || retained.Cancelled || !retained.SourcesReserved || retained.Error != "" || conversation.Recovery {
				return harness.WorkerExecution{}, fmt.Errorf("%w: retained conversation requires reconciliation", ErrConversationPending)
			}
			for _, source := range conversation.Sources {
				if slices.Contains(retained.Conversation.Sources, source) {
					return harness.WorkerExecution{}, errors.New("conversation source already retained")
				}
			}
			for _, batch := range retained.Followups {
				if batch.Conversation.SourceMessageID == conversation.SourceMessageID {
					return harness.WorkerExecution{}, errors.New("conversation follow-up requires reconciliation, not another admission")
				}
			}
			conversation.ExecutionID = retained.Conversation.ExecutionID
			execution, action = retained, "followup"
		} else if retained.Conversation != nil && retained.Conversation.ExecutionID == conversation.ExecutionID {
			return harness.WorkerExecution{}, errors.New("conversation execution belongs to another session")
		}
	}
	if followupOnly && action != "followup" {
		return harness.WorkerExecution{}, &harness.SendNotAdmittedError{Err: errors.New("there is no active execution for this conversation")}
	}
	jobKey := "worker-conversation:" + conversation.ExecutionID
	if action == "send" && m.runtimeJob(jobKey) > 0 {
		return harness.WorkerExecution{}, errors.New("conversation admission requires reconciliation")
	}
	var jobID int
	err = m.Worker.RunOwned(ctx, func() error {
		var err error
		jobID, err = m.ConversationStarted(conversation)
		if err == nil && jobID <= 0 {
			err = errors.New("conversation admission returned no job")
		}
		if err == nil {
			m.setRuntimeJob(jobKey, jobID)
			pending = true
		}
		return err
	})
	if err != nil {
		return execution, err
	}
	if prepare != nil {
		if err := prepare(execution); err != nil {
			return execution, err
		}
	}
	reply, err = m.Worker.Call(ctx, harness.WorkerRequest{Action: action, Sequence: execution.Sequence, Key: key, Prompt: prompt, Message: message, Conversation: &conversation})
	validReply := len(reply.Executions) == 1 && reply.Executions[0].Sequence == execution.Sequence &&
		reply.Executions[0].Key == key && reply.Executions[0].Conversation != nil && reply.Executions[0].Conversation.ExecutionID == conversation.ExecutionID
	if action == "followup" && validReply {
		for _, batch := range reply.Executions[0].Followups {
			if batch.Conversation.SourceMessageID == conversation.SourceMessageID && (!batch.Reserved || batch.Restored) {
				// This exact batch was refused/restored; the original job stays live.
				pending = false
				return reply.Executions[0], &harness.SendNotAdmittedError{Err: errors.New("conversation follow-up was not admitted")}
			}
		}
	}
	if err == nil && !validReply {
		err = errors.New("conversation admission result unavailable")
	}
	if err == nil {
		execution = reply.Executions[0]
		if !execution.SourcesReserved {
			err = errors.New("conversation sources were not reserved")
		}
	}
	if err != nil {
		// A lost reply is unknown, never proof that Send failed or may be repeated.
		_ = m.Worker.RunOwned(ctx, func() error {
			if m.JobExecutionUpdated != nil {
				m.JobExecutionUpdated(jobID, core.ExecutionStatus{State: "degraded", Detail: "Conversation admission requires reconciliation"})
			}
			return nil
		})
	}
	return execution, err
}

// Each execution has one journal consumer. Live HTTP/TUI requests attach only
// their callback; recorded request identities choose the response owner.
type conversationRequest struct {
	mu       sync.Mutex
	deliver  func(context.Context, int, harness.WorkerDelivery) error
	done     chan error
	closed   bool
	terminal bool
}

func (m *Manager) registerConversationRequest(sequence uint64, source string, request *conversationRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conversationRequests == nil {
		m.conversationRequests = make(map[uint64]map[string]*conversationRequest)
	}
	requests := m.conversationRequests[sequence]
	if requests == nil {
		requests = make(map[string]*conversationRequest)
		m.conversationRequests[sequence] = requests
	}
	if requests[source] != nil || len(requests) >= 64 {
		return errors.New("conversation request already attached or at capacity")
	}
	requests[source] = request
	return nil
}

func (m *Manager) removeConversationRequest(sequence uint64, source string, request *conversationRequest) {
	m.mu.Lock()
	if requests := m.conversationRequests[sequence]; requests[source] == request {
		delete(requests, source)
		if len(requests) == 0 {
			delete(m.conversationRequests, sequence)
		}
	}
	m.mu.Unlock()
	request.mu.Lock()
	request.closed = true
	request.mu.Unlock()
}

func (m *Manager) deliverConversationRequest(ctx context.Context, jobID int, delivery harness.WorkerDelivery) error {
	if delivery.Conversation.Validate() != nil {
		return errors.New("invalid conversation request delivery")
	}
	m.mu.Lock()
	request := m.conversationRequests[delivery.Sequence][delivery.Conversation.SourceMessageID]
	m.mu.Unlock()
	if request != nil {
		request.mu.Lock()
		defer request.mu.Unlock()
		if !request.closed {
			if err := request.deliver(ctx, jobID, delivery); err != nil {
				return err
			}
			if delivery.Event.Done && !delivery.Event.Continues {
				if delivery.RequestOnly || delivery.Event.Kind == core.EventStatus {
					request.closed = true
					request.done <- nil
				} else {
					request.terminal = true // Keep the result if retirement acknowledgement is lost.
				}
			}
			return nil
		}
	}
	return m.ConversationDelivery(ctx, jobID, delivery)
}

func (m *Manager) finishConversationRequests(sequence uint64, err error) {
	m.mu.Lock()
	requests := m.conversationRequests[sequence]
	delete(m.conversationRequests, sequence)
	m.mu.Unlock()
	for _, request := range requests {
		request.mu.Lock()
		if !request.closed {
			result := err
			if request.terminal {
				result = nil
			} else if result == nil {
				result = errors.New("request response requires history reconciliation")
			}
			if result != nil {
				result = fmt.Errorf("%w: %w", ErrConversationPending, result)
			}
			request.closed = true
			request.done <- result
		}
		request.mu.Unlock()
	}
}

// The caller holds the snapshot/election fence. Proven absent admissions end
// only their provisional job; journaled refusals settle through archive delivery.
func (m *Manager) reconcileConversationAdmissions(executions []harness.WorkerExecution) {
	retained := map[string]harness.WorkerExecution{}
	for _, execution := range executions {
		if execution.Conversation != nil {
			retained["worker-conversation:"+execution.Conversation.ExecutionID] = execution
		}
	}
	m.mu.Lock()
	absent := map[string]int{}
	for key, id := range m.runtimeJobs {
		if !strings.HasPrefix(key, "worker-conversation:") {
			continue
		}
		if _, exists := retained[key]; !exists {
			absent[key] = id
		}
	}
	m.mu.Unlock()
	for key, id := range absent {
		if m.JobExecutionUpdated != nil {
			m.JobExecutionUpdated(id, core.ExecutionStatus{State: "error", Detail: "Conversation admission did not dispatch"})
		}
		m.finishRuntimeJob(key)
	}
}
