package history

import (
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// AdmitConversation validates and syncs the complete source batch in one
// receipted record. Its caller must hold the exact cross-process owner fence.
func (s *Store) AdmitConversation(channel, conversation, executionID, receipt string, sources []string, recovery bool) error {
	return s.admitConversation(channel, conversation, executionID, receipt, sources, recovery, time.Time{}, time.Time{}, nil)
}

// AdmitRecoveryConversation reserves and admits the exact eligible batch in one
// synced receipt; no controller-side reservation may precede this operation.
func (s *Store) AdmitRecoveryConversation(channel, conversation, executionID, receipt string, sources []string, activation, cutoff time.Time, validate func([]Entry) error) error {
	if activation.IsZero() || cutoff.IsZero() || validate == nil {
		return errors.New("conversation recovery requires its activation and recent window")
	}
	return s.admitConversation(channel, conversation, executionID, receipt, sources, true, activation, cutoff, validate)
}

func validConversationIdentity(id string) bool {
	return id != "" && len(id) <= 128 && utf8.ValidString(id) && strings.IndexFunc(id, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

func validateConversationSources(executionID string, sources []string) error {
	if !validConversationIdentity(executionID) || len(sources) == 0 || len(sources) > 1024 || !slices.IsSorted(sources) {
		return errors.New("invalid conversation admission sources")
	}
	for i, id := range sources {
		if !validConversationIdentity(id) || i > 0 && id == sources[i-1] {
			return errors.New("invalid conversation admission sources")
		}
	}
	return nil
}

func (s *Store) admitConversation(channel, conversation, executionID, receipt string, sources []string, recovery bool, activation, cutoff time.Time, validate func([]Entry) error) error {
	if err := validateConversationSources(executionID, sources); err != nil {
		return err
	}
	entry := Entry{Role: "correlation", ExecutionID: executionID, Admission: "new", AdmittedSources: sources, Recovery: recovery}
	if !activation.IsZero() {
		entry.RetriggerOf = sources
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.appendOnceLocked(channel, conversation, receipt, entry, func() error {
		snapshot, err := s.recoveryWindowLocked(channel, conversation, time.Time{}, 0, len(sources), sources)
		if err != nil {
			return err
		}
		if len(snapshot.Entries) != len(sources) {
			return errors.New("conversation admission source is absent from history")
		}
		if !activation.IsZero() {
			for _, source := range snapshot.Entries {
				if source.AcceptedAt.IsZero() || source.AcceptedAt.Before(activation) || source.At.Before(cutoff) || snapshot.Covered[source.SourceMessageID] != "" {
					return errors.New("conversation recovery source is no longer eligible")
				}
			}
			return validate(snapshot.Entries)
		}
		for _, id := range sources {
			if recovery {
				if snapshot.Covered[id] != "prior_reservation" || snapshot.recoveryAdmitted[id] {
					return errors.New("conversation recovery source is not reserved or is already covered")
				}
			} else if snapshot.Covered[id] != "" || snapshot.Admitted[id] {
				return errors.New("conversation source is already admitted or covered")
			}
		}
		return nil
	})
	return err
}
