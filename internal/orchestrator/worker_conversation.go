package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

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
	return m.admitConversation(ctx, conversation, prompt, message)
}

// Converse consumes an ordinary request through the same admission, receipt and
// retirement boundary as scheduled attachment. Request cancellation detaches.
func (m *Manager) Converse(ctx context.Context, conversation harness.WorkerConversation, prompt, message string, deliver func(context.Context, int, harness.WorkerDelivery) error) error {
	if deliver == nil {
		return errors.New("conversation request delivery unavailable")
	}
	m.workerAdmissionMu.Lock()
	execution, err := m.admitConversation(ctx, conversation, prompt, message)
	if err != nil {
		m.workerAdmissionMu.Unlock()
		return err
	}
	key := "worker-conversation:" + strconv.FormatUint(execution.Sequence, 10)
	m.setInflight(key, true)
	m.jobs.Add(1)
	m.workerAdmissionMu.Unlock()
	defer m.jobs.Done()
	defer m.setInflight(key, false)
	if err := m.consumeWorkerConversation(ctx, execution, deliver); err != nil {
		return fmt.Errorf("%w: %w", ErrConversationPending, err)
	}
	return nil
}

// The caller holds the shared snapshot/admission/retirement lock.
func (m *Manager) admitConversation(ctx context.Context, conversation harness.WorkerConversation, prompt, message string) (result harness.WorkerExecution, resultErr error) {
	defer func() {
		if resultErr != nil && m.runtimeJob("worker-conversation:"+conversation.ExecutionID) > 0 {
			resultErr = fmt.Errorf("%w: %w", ErrConversationPending, resultErr)
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
	for _, retained := range reply.Executions {
		if retained.Key == key || retained.Conversation != nil && retained.Conversation.ExecutionID == conversation.ExecutionID {
			return harness.WorkerExecution{}, errors.New("conversation admission already retained; consume and retire before another dispatch")
		}
	}
	jobKey := "worker-conversation:" + conversation.ExecutionID
	if m.runtimeJob(jobKey) > 0 {
		return harness.WorkerExecution{}, errors.New("conversation admission requires reconciliation")
	}
	execution := harness.WorkerExecution{Key: key, Sequence: reply.NextSequence, Conversation: &conversation}
	var jobID int
	err = m.Worker.RunOwned(ctx, func() error {
		var err error
		jobID, err = m.ConversationStarted(conversation)
		if err == nil && jobID <= 0 {
			err = errors.New("conversation admission returned no job")
		}
		if err == nil {
			m.setRuntimeJob(jobKey, jobID)
		}
		return err
	})
	if err != nil {
		return execution, err
	}
	reply, err = m.Worker.Call(ctx, harness.WorkerRequest{Action: "send", Sequence: execution.Sequence, Key: key, Prompt: prompt, Message: message, Conversation: &conversation})
	if err == nil && (len(reply.Executions) != 1 || reply.Executions[0].Sequence != execution.Sequence) {
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
	m.requestScan()
	return execution, err
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
