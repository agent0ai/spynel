package orchestrator

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
)

func (m *Manager) claimWorkerAgent(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	key = "worker-agent:" + key
	if m.inflight[key] {
		return false
	}
	m.inflight[key] = true
	return true
}

// Called under the scan/election fence with an authenticated snapshot. Only
// the consumer is restored; notification decisions and payloads stay opaque.
func (m *Manager) attachWorkerAgents(ctx context.Context, executions []harness.WorkerExecution) {
	retained := map[string]bool{}
	for _, execution := range executions {
		if execution.Agent != nil {
			retained["worker-agent:"+execution.Agent.ID] = true
		}
	}
	// Admission and retirement share the snapshot lock. An absent provisional
	// admission was never retained; a lost Send reply must not leak its job.
	m.mu.Lock()
	absent := map[string]int{}
	for key, id := range m.runtimeJobs {
		if strings.HasPrefix(key, "worker-agent:") && !retained[key] {
			absent[key] = id
		}
	}
	m.mu.Unlock()
	for key, id := range absent {
		if m.JobExecutionUpdated != nil {
			m.JobExecutionUpdated(id, core.ExecutionStatus{State: "error", Detail: "Worker admission was not retained"})
		}
		m.finishRuntimeJob(key)
	}
	for _, execution := range executions {
		if execution.Agent == nil || !m.claimWorkerAgent(execution.Key) {
			continue
		}
		m.jobs.Add(1)
		go func() {
			defer m.jobs.Done()
			defer m.setInflight("worker-agent:"+execution.Key, false)
			if result := m.consumeWorkerAgent(ctx, execution, 0); result.err != nil && ctx.Err() == nil {
				m.log("retained background agent delivery: " + result.err.Error())
			}
		}()
	}
}

func workerAgentLease(execution harness.WorkerExecution) (Lease, string) {
	agent := execution.Agent
	lease := Lease{ID: agent.ID, WorkID: agent.ID, DocumentType: agent.Kind, File: agent.File,
		SessionKey: execution.Key, State: "working", StartedAt: agent.StartedAt, HeartbeatAt: time.Now().UTC(), ClaimAttempt: 1}
	description := "task notification agent"
	lease.Route, lease.Phase = "notifications", "notification"
	if agent.Kind == "heartbeat" {
		description = "semantic workflow heartbeat"
		lease.Route, lease.Phase = "semantic-heartbeat", "semantic_heartbeat"
	}
	return lease, description
}

func (m *Manager) admitWorkerAgent(ctx context.Context, execution harness.WorkerExecution) (int, error) {
	if m.JobStarted == nil || m.JobDelivery == nil {
		return 0, errors.New("durable background job admission and delivery are required")
	}
	lease, description := workerAgentLease(execution)
	key := "worker-agent:" + execution.Agent.ID
	var jobID int
	err := m.Worker.RunOwned(ctx, func() error {
		if jobID = m.runtimeJob(key); jobID > 0 {
			return nil
		}
		var err error
		jobID, err = m.JobStarted(lease, description, execution.Agent.StartedAt, 1, 0)
		if err == nil {
			m.setRuntimeJob(key, jobID)
		}
		return err
	})
	if err == nil && jobID <= 0 {
		err = errors.New("background job admission returned no job")
	}
	return jobID, err
}

func (m *Manager) runWorkerAgent(ctx context.Context, source Lease, prompt string, timeout time.Duration) ordinaryAgentResult {
	if !m.claimWorkerAgent(source.SessionKey) {
		return ordinaryAgentResult{err: errors.New("background agent already attached")}
	}
	defer m.setInflight("worker-agent:"+source.SessionKey, false)
	// Share sequence allocation with workflow admission and scan snapshots.
	m.workerAdmissionMu.Lock()
	execution, jobID, err := m.prepareWorkerAgent(ctx, source, prompt, timeout)
	m.workerAdmissionMu.Unlock()
	if err != nil {
		if jobID > 0 {
			m.degradeWorkerAgent(ctx, jobID)
		}
		return ordinaryAgentResult{admitted: jobID > 0, err: err}
	}
	return m.consumeWorkerAgent(ctx, execution, jobID)
}

// The caller holds only workerAdmissionMu, never the election or scan lock.
func (m *Manager) prepareWorkerAgent(ctx context.Context, source Lease, prompt string, timeout time.Duration) (harness.WorkerExecution, int, error) {
	reply, err := m.Worker.Call(ctx, harness.WorkerRequest{Action: "inspect"})
	if err != nil {
		return harness.WorkerExecution{}, 0, err
	}
	for _, execution := range reply.Executions {
		if execution.Key == source.SessionKey {
			if execution.Agent == nil || execution.Agent.Kind != source.DocumentType {
				return execution, 0, errors.New("background session belongs to another retained admission")
			}
			return execution, 0, nil
		}
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	now := time.Now().UTC()
	id, err := core.NewSourceMessageID()
	if err != nil {
		return harness.WorkerExecution{}, 0, err
	}
	execution := harness.WorkerExecution{Key: source.SessionKey, Sequence: reply.NextSequence,
		Agent: &harness.WorkerAgent{ID: id, Kind: source.DocumentType, File: source.File, StartedAt: now, Deadline: now.Add(timeout)}}
	jobID, err := m.admitWorkerAgent(ctx, execution)
	if err != nil {
		return execution, 0, err
	}
	reply, err = m.Worker.Call(ctx, harness.WorkerRequest{Action: "send", Sequence: execution.Sequence, Key: execution.Key, Prompt: prompt, Agent: execution.Agent})
	if err == nil && len(reply.Executions) != 1 {
		err = errors.New("background worker admission result unavailable")
	}
	if err != nil {
		return execution, jobID, err // The next scheduled scan reconciles an ambiguous Send.
	}
	return reply.Executions[0], jobID, nil
}

func (m *Manager) degradeWorkerAgent(ctx context.Context, jobID int) {
	_ = m.Worker.RunOwned(ctx, func() error {
		if m.JobExecutionUpdated != nil {
			m.JobExecutionUpdated(jobID, core.ExecutionStatus{State: "degraded", Detail: "Worker delivery requires reconciliation"})
		}
		return nil
	})
}

func (m *Manager) consumeWorkerAgent(ctx context.Context, execution harness.WorkerExecution, jobID int) (result ordinaryAgentResult) {
	result.providerSent = true
	if jobID == 0 {
		var err error
		jobID, err = m.admitWorkerAgent(ctx, execution)
		if err != nil {
			result.err = err
			return result
		}
	}
	result.admitted = true
	lease, _ := workerAgentLease(execution)
	defer func() {
		// Controller loss detaches only; no invented terminal or cancellation.
		if result.err != nil && ctx.Err() == nil && m.runtimeJob("worker-agent:"+execution.Agent.ID) > 0 {
			m.degradeWorkerAgent(ctx, jobID)
		}
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, err := m.Worker.Consume(ctx, execution.Sequence, func(delivery harness.WorkerDelivery) error {
			if err := m.JobDelivery(jobID, delivery); err != nil {
				return err
			}
			if m.JobExecutionUpdated != nil && delivery.Event.Execution != nil {
				m.JobExecutionUpdated(jobID, *delivery.Event.Execution)
			}
			if m.JobUpdated != nil {
				lease.HeartbeatAt = time.Now().UTC()
				m.JobUpdated(jobID, lease)
			}
			return nil
		})
		if err != nil {
			result.err = err
			return result
		}
		if current.Done && current.CommittedCursor == current.LastCursor {
			m.workerAdmissionMu.Lock()
			// Finish the durable terminal before retiring its worker descriptor;
			// a lost retirement reply can safely reopen and replay this same job.
			err := m.Worker.RunOwned(ctx, func() error {
				if current.Error != "" && m.JobExecutionUpdated != nil {
					m.JobExecutionUpdated(jobID, core.ExecutionStatus{State: "error", Detail: current.Error})
				}
				m.finishRuntimeJob("worker-agent:" + execution.Agent.ID)
				return nil
			})
			if err == nil {
				_, err = m.Worker.Call(ctx, harness.WorkerRequest{Action: "ack", Sequence: current.Sequence, After: current.LastCursor})
			}
			m.workerAdmissionMu.Unlock()
			if err == nil && current.Error != "" {
				err = errors.New(current.Error)
			}
			result.err = err
			return result
		}
		select {
		case <-ctx.Done():
			result.err = ctx.Err()
			return result
		case <-ticker.C:
		}
	}
}

func (m *Manager) ordinaryAgentActive(ctx context.Context, key string) bool {
	if m.Worker == nil {
		return m.Harness.IsActive(key)
	}
	reply, err := m.Worker.Call(ctx, harness.WorkerRequest{Action: "inspect"})
	if err != nil {
		return true // Unknown delivery keeps the existing non-overlap fence.
	}
	for _, execution := range reply.Executions {
		if execution.Key == key {
			return true // Terminal delivery remains owned until durable retirement.
		}
	}
	return false
}
