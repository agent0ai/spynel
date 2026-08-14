package orchestrator

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/agent0ai/spynel/internal/core"
)

type ordinaryAgentResult struct {
	admitted     bool
	providerSent bool
	err          error
}

// runOrdinaryAgentTurn owns one admitted background-agent job from provider
// dispatch through the first authoritative terminal event. Harness Send is an
// admission boundary for asynchronous adapters such as Codex, not completion.
func (m *Manager) runOrdinaryAgentTurn(parent context.Context, lease Lease, description, prompt string, timeout time.Duration, providerIterations int) ordinaryAgentResult {
	jobID := 0
	if m.JobStarted != nil {
		var err error
		jobID, err = m.JobStarted(lease, description, time.Time{}, providerIterations, 0)
		if err != nil {
			return ordinaryAgentResult{err: err}
		}
	}
	result := ordinaryAgentResult{admitted: true}
	var lifecycleMu sync.Mutex
	jobFinished := false
	terminalSeen := false
	finishJob := func() {
		lifecycleMu.Lock()
		if jobFinished {
			lifecycleMu.Unlock()
			return
		}
		jobFinished = true
		lifecycleMu.Unlock()
		if jobID > 0 && m.JobFinished != nil {
			m.JobFinished(jobID)
		}
	}
	defer finishJob()

	terminal := make(chan struct{}, 1)
	emit := func(event core.Event) {
		lifecycleMu.Lock()
		if jobFinished || terminalSeen {
			lifecycleMu.Unlock()
			return
		}
		isTerminal := event.Done && !event.Continues && (event.Kind == core.EventFinal || event.Kind == core.EventError)
		if isTerminal {
			terminalSeen = true
		}
		if jobID > 0 && m.JobEvent != nil {
			m.JobEvent(jobID, event)
		}
		if jobID > 0 && m.JobExecutionUpdated != nil && event.Execution != nil {
			m.JobExecutionUpdated(jobID, *event.Execution)
		}
		if jobID > 0 && m.JobUpdated != nil {
			activity := lease
			activity.HeartbeatAt = time.Now().UTC()
			m.JobUpdated(jobID, activity)
		}
		lifecycleMu.Unlock()
		if isTerminal {
			select {
			case terminal <- struct{}{}:
			default:
			}
		}
	}
	recordError := func(err error) ordinaryAgentResult {
		select {
		case <-terminal:
			return result
		default:
		}
		emit(core.Event{Kind: core.EventError, Text: err.Error(), Done: true,
			Execution: &core.ExecutionStatus{State: "error", Detail: err.Error()}})
		result.err = err
		return result
	}

	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	_, _, err := m.Harness.Send(ctx, lease.SessionKey, prompt, emit)
	if err != nil {
		return recordError(err)
	}
	result.providerSent = true
	select {
	case <-terminal:
		return result
	default:
	}
	if !m.Harness.IsActive(lease.SessionKey) {
		// Synchronous providers emit before returning. Recheck after IsActive
		// so a concurrent terminal callback remains authoritative.
		select {
		case <-terminal:
			return result
		default:
			return recordError(errors.New("provider returned without terminal event"))
		}
	}
	select {
	case <-terminal:
		return result
	case <-ctx.Done():
		interruptTimeout := timeout / 10
		if interruptTimeout <= 0 || interruptTimeout > time.Second {
			interruptTimeout = time.Second
		}
		interruptCtx, interruptCancel := context.WithTimeout(context.Background(), interruptTimeout)
		_, _ = m.Harness.Interrupt(interruptCtx, lease.SessionKey)
		interruptCancel()
		return recordError(ctx.Err())
	}
}
