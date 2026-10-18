package orchestrator

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
)

// AdmitConversation requires already persisted sources. Application admission
// runs under exact ownership; worker RPCs never run inside that callback.
func (m *Manager) AdmitConversation(ctx context.Context, conversation harness.WorkerConversation, prompt, message string) (harness.WorkerExecution, error) {
	if m.Worker == nil || m.ConversationStarted == nil || m.ConversationAttached == nil || m.ConversationDelivery == nil || m.JobFinished == nil || conversation.Validate() != nil || prompt == "" {
		return harness.WorkerExecution{}, errors.New("worker conversation admission unavailable or invalid")
	}
	conversation.Sources = slices.Clone(conversation.Sources)
	m.workerAdmissionMu.Lock()
	defer m.workerAdmissionMu.Unlock()
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
