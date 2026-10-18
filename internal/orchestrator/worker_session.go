package orchestrator

import (
	"context"
	"errors"

	"github.com/agent0ai/spynel/internal/harness"
)

// Session is advisory; provider admission still fences completion.
// Call it outside ownership callbacks because a bound worker requires RPC.
func (m *Manager) Session(ctx context.Context, key string) (harness.WorkerSession, error) {
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

// Availability inspects the execution owner, including an unavailable adapter.
// Like Session, it cannot be called inside an ownership callback.
func (m *Manager) Availability(ctx context.Context) (bool, string, error) {
	if m.Worker == nil {
		if target, ok := m.Harness.(harness.Availability); ok {
			ready, detail := target.Available()
			return ready, detail, nil
		}
		return true, "", nil
	}
	reply, err := m.Worker.Call(ctx, harness.WorkerRequest{Action: "inspect"})
	if err != nil || reply.Availability == nil {
		return false, "", errors.New("worker availability unavailable")
	}
	return reply.Availability.Available, reply.Availability.Detail, nil
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
	if reply.Session == nil || reply.Session.Active || reply.Session.Retained || reply.Session.ThreadID != "" {
		return errors.New("worker session reset was not confirmed")
	}
	return m.Worker.RunOwned(ctx, clear)
}
