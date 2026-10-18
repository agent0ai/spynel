package history

import (
	"errors"
	"time"
)

// SetConversationCancellation syncs a recovery pause before provider control.
// A restoration can remove only this intent, never an earlier cancellation.
// The caller serializes interruption attempts and fences each write by ownership.
func (s *Store) SetConversationCancellation(channel, conversation, executionID, intent string, sources []string, restore bool) error {
	if err := validateConversationSources(executionID, sources); err != nil {
		return err
	}
	entry := Entry{Role: "correlation", ExecutionID: executionID, CancellationID: intent, Covers: sources, Outcome: "cancellation_requested"}
	receipt := intent
	if restore {
		entry.Outcome = "cancellation_restored"
		receipt += ":restore"
	}
	if !validConversationIdentity(intent) {
		return errors.New("conversation cancellation requires an intent identity")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.appendOnceLocked(channel, conversation, receipt, entry, func() error {
		snapshot, err := s.recoveryWindowLocked(channel, conversation, time.Time{}, 0, len(sources), sources)
		if err != nil {
			return err
		}
		if len(snapshot.Entries) != len(sources) {
			return errors.New("conversation cancellation source is absent from history")
		}
		for _, id := range sources {
			if snapshot.admittedExecution[id] != executionID {
				return errors.New("conversation cancellation admission changed or is unavailable")
			}
		}
		return nil
	})
	return err
}
