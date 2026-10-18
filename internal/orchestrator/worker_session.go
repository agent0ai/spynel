package orchestrator

import (
	"context"
	"errors"

	"github.com/agent0ai/spynel/internal/harness"
)

// ConversationSession is advisory; provider admission still fences completion.
// Call it outside ownership callbacks because a bound worker requires RPC.
func (m *Manager) ConversationSession(ctx context.Context, key string) (harness.WorkerSession, error) {
	if m.Worker == nil {
		return harness.WorkerSession{Active: m.Harness.IsActive(key), ThreadID: m.Harness.ThreadID(key)}, nil
	}
	reply, err := m.Worker.Call(ctx, harness.WorkerRequest{Action: "session", Key: key})
	if err != nil {
		return harness.WorkerSession{}, err
	}
	if reply.Session == nil {
		return harness.WorkerSession{}, errors.New("worker session state unavailable")
	}
	if m.Harness.IsActive(key) {
		return harness.WorkerSession{}, errors.New("conversation has a controller-owned execution")
	}
	return *reply.Session, nil
}

// ResetConversation confirms the owning reset before clearing history. Bound
// admissions/retirement cannot interleave, and the clear remains term-fenced.
func (m *Manager) ResetConversation(ctx context.Context, key string, clear func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.Worker == nil {
		if err := m.Harness.ResetSession(key); err != nil {
			return err
		}
		return clear()
	}
	m.workerAdmissionMu.Lock()
	defer m.workerAdmissionMu.Unlock()
	if m.Harness.IsActive(key) {
		return errors.New("conversation has a controller-owned execution")
	}
	reply, err := m.Worker.Call(ctx, harness.WorkerRequest{Action: "reset", Key: key})
	if err != nil {
		return err
	}
	if reply.Session == nil || reply.Session.Active || reply.Session.ThreadID != "" {
		return errors.New("worker session reset was not confirmed")
	}
	return m.Worker.RunOwned(ctx, clear)
}
