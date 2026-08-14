package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/instance"
)

const (
	defaultManualCleanupDays = 7
	maxCleanupDays           = 36500
	liveTUILeaseDuration     = instance.OwnerlessCleanupGrace
)

var errCleanupAlreadyRunning = errors.New("cleanup is already running")
var errCleanupLeaseReestablishing = errors.New("live TUI protection is re-establishing after primary startup; retry shortly")

type cleanupResult struct {
	RemovedConversations int
	RemovedJobArchives   int
	RemovedJobBytes      int64
	ArchivedTasks        int
	RemovedObsoleteState int
	Protected            int
	Failed               int
}

func (r cleanupResult) String() string {
	return fmt.Sprintf("Cleanup complete: %d conversations removed, %d job archives removed (%d bytes), %d terminal tasks archived, %d obsolete runtime items removed, %d live/protected items skipped, %d failures.", r.RemovedConversations, r.RemovedJobArchives, r.RemovedJobBytes, r.ArchivedTasks, r.RemovedObsoleteState, r.Protected, r.Failed)
}

func (s *Service) cleanupCommand(message core.Message, remainder string, emit core.Emit) error {
	days := defaultManualCleanupDays
	parts := strings.Fields(remainder)
	if len(parts) > 1 {
		return s.localReply(message, "Usage: /cleanup [days] — days must be a whole number from 1 to 36500.", emit)
	}
	if len(parts) == 1 {
		value, err := strconv.Atoi(parts[0])
		if err != nil || value < 1 || value > maxCleanupDays {
			return s.localReply(message, "Usage: /cleanup [days] — days must be a whole number from 1 to 36500.", emit)
		}
		days = value
	}
	result, err := s.runCleanup(days, message.Channel, message.Conversation, time.Now().UTC())
	if errors.Is(err, errCleanupAlreadyRunning) {
		return s.localReply(message, "Cleanup is already running; no second cleanup was started.", emit)
	}
	if errors.Is(err, errCleanupLeaseReestablishing) {
		return s.localReply(message, "Cleanup is temporarily unavailable while live conversations reconnect; retry shortly.", emit)
	}
	if err != nil {
		return s.localReply(message, "Cleanup could not start: "+err.Error(), emit)
	}
	return s.localReply(message, result.String(), emit)
}

func (s *Service) runAutomaticCleanup(_ context.Context, days int) (string, error) {
	result, err := s.runCleanup(days, "", "", time.Now().UTC())
	if err != nil {
		return "", err
	}
	return result.String(), nil
}

func (s *Service) runCleanup(days int, liveChannel, liveConversation string, now time.Time) (cleanupResult, error) {
	if days < 1 || days > maxCleanupDays {
		return cleanupResult{}, fmt.Errorf("retention days must be from 1 to %d", maxCleanupDays)
	}
	now = now.UTC()
	s.instanceMu.RLock()
	cleanupNotBefore := s.cleanupNotBefore
	primary := s.primaryInstance
	s.instanceMu.RUnlock()
	if primary != "" && now.Before(cleanupNotBefore) {
		return cleanupResult{}, errCleanupLeaseReestablishing
	}
	lock, acquired, err := tryCleanupLock(s.Config.StatePath("runtime", "cleanup.lock"))
	if err != nil {
		return cleanupResult{}, err
	}
	if !acquired {
		return cleanupResult{}, errCleanupAlreadyRunning
	}
	defer releaseCleanupLock(lock)

	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	protected := map[string]bool{}
	if liveChannel != "" && liveConversation != "" {
		protected[liveChannel+"\x00"+liveConversation] = true
	} else if latest, found, err := s.History.Latest("tui"); err == nil && found {
		// The primary does not own a transport-level list of idle TUI clients.
		// Protecting the newest durable TUI conversation keeps automatic cleanup
		// from removing the conversation selected by primary startup.
		protected[latest.Channel+"\x00"+latest.Conversation] = true
	}
	for _, job := range s.Runtime.Jobs() {
		if job.Channel != "" && job.Conversation != "" && job.Channel != "orchestrator" {
			protected[job.Channel+"\x00"+job.Conversation] = true
		}
	}
	s.liveTUIMu.Lock()
	for conversation := range s.liveTUIConversationsLocked(now) {
		protected["tui\x00"+conversation] = true
	}
	if s.cleanupHistoryStep != nil {
		s.cleanupHistoryStep("protected")
	}
	historyResult := s.History.RemoveOlderThan(cutoff, protected)
	if s.cleanupHistoryStep != nil {
		s.cleanupHistoryStep("removed")
	}
	s.liveTUIMu.Unlock()
	result := cleanupResult{RemovedConversations: historyResult.Removed, Protected: historyResult.Protected, Failed: historyResult.Failed}
	removedJobs, removedJobBytes, jobProtected, jobFailed := s.Runtime.CleanupArchivedJobs(cutoff)
	result.RemovedJobArchives = removedJobs
	result.RemovedJobBytes = removedJobBytes
	result.Protected += jobProtected
	result.Failed += jobFailed
	archived, taskProtected, failed := s.Orchestrator.ArchiveTerminalTasks(cutoff)
	result.ArchivedTasks = archived
	result.Protected += taskProtected
	result.Failed += failed
	removedObsolete, obsoleteFailed := removeObsoleteNotificationState(s.Config.StatePath())
	result.RemovedObsoleteState = removedObsolete
	result.Failed += obsoleteFailed
	return result, nil
}

func removeObsoleteNotificationState(stateDirectory string) (removed, failed int) {
	for _, name := range []string{"notification-agents", "notification-agent-locks"} {
		path := filepath.Join(stateDirectory, "runtime", name)
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			failed++
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			failed++
			continue
		}
		removed++
	}
	return removed, failed
}

// RegisterLiveTUI records a renewable owner-side lease for one conversation
// displayed by an authenticated local TUI instance. A single instance may
// temporarily retain multiple conversations so a screen switch cannot expose
// the previous conversation to cleanup while the client is in transition.
func (s *Service) RegisterLiveTUI(instanceID, conversation string, now time.Time) error {
	if err := validateLiveTUIIdentity(instanceID, conversation); err != nil {
		return err
	}
	now = now.UTC()
	s.liveTUIMu.Lock()
	defer s.liveTUIMu.Unlock()
	s.registerLiveTUILocked(instanceID, conversation, now)
	return nil
}

func validateLiveTUIIdentity(instanceID, conversation string) error {
	instanceID = strings.TrimSpace(instanceID)
	conversation = strings.TrimSpace(conversation)
	if instanceID == "" || conversation == "" || len(instanceID) > 128 || len(conversation) > 256 || strings.ContainsAny(instanceID+conversation, "\x00\r\n") {
		return errors.New("invalid live TUI identity")
	}
	return nil
}

// registerLiveTUILocked records a previously validated identity. The caller
// must hold liveTUIMu so compound admission operations can share cleanup's
// history-deletion boundary.
func (s *Service) registerLiveTUILocked(instanceID, conversation string, now time.Time) {
	instanceID = strings.TrimSpace(instanceID)
	conversation = strings.TrimSpace(conversation)
	conversations := s.liveTUI[instanceID]
	if conversations == nil {
		conversations = map[string]time.Time{}
		s.liveTUI[instanceID] = conversations
	}
	for name, expires := range conversations {
		if !expires.After(now) {
			delete(conversations, name)
		}
	}
	conversations[conversation] = now.Add(liveTUILeaseDuration)
}

// UnregisterLiveTUI releases all conversation leases owned by a cleanly
// exiting TUI. Crashed clients are removed by lease expiry instead.
func (s *Service) UnregisterLiveTUI(instanceID string) {
	s.liveTUIMu.Lock()
	delete(s.liveTUI, strings.TrimSpace(instanceID))
	s.liveTUIMu.Unlock()
}

func (s *Service) liveTUIConversations(now time.Time) map[string]bool {
	s.liveTUIMu.Lock()
	defer s.liveTUIMu.Unlock()
	return s.liveTUIConversationsLocked(now)
}

// liveTUIConversationsLocked returns and prunes the current lease set. The
// caller must hold liveTUIMu. Cleanup deliberately retains that mutex through
// history deletion so registration and retention have one admission order.
func (s *Service) liveTUIConversationsLocked(now time.Time) map[string]bool {
	protected := map[string]bool{}
	for instanceID, conversations := range s.liveTUI {
		for conversation, expires := range conversations {
			if expires.After(now) {
				protected[conversation] = true
			} else {
				delete(conversations, conversation)
			}
		}
		if len(conversations) == 0 {
			delete(s.liveTUI, instanceID)
		}
	}
	return protected
}
