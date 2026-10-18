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
	validID := func(id string) bool {
		return id != "" && len(id) <= 128 && utf8.ValidString(id) && strings.IndexFunc(id, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
	}
	if !validID(executionID) || len(sources) == 0 || len(sources) > 1024 || !slices.IsSorted(sources) {
		return errors.New("invalid conversation admission sources")
	}
	for i, id := range sources {
		if !validID(id) || i > 0 && id == sources[i-1] {
			return errors.New("invalid conversation admission sources")
		}
	}
	entry := Entry{Role: "correlation", ExecutionID: executionID, Admission: "new", AdmittedSources: sources, Recovery: recovery}
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
