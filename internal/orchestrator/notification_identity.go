package orchestrator

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type contactActivity struct {
	Principal string    `json:"principal"`
	Origin    string    `json:"origin"`
	LastSeen  time.Time `json:"last_seen"`
}

// RecordContactActivity records activity only when the exact origin belongs
// to an explicitly configured principal. Unbound traffic is intentionally not
// usable for cross-channel routing.
func (m *Manager) RecordContactActivity(origin string, at time.Time) error {
	principal, ok := m.boundPrincipal(origin)
	if !ok {
		return nil
	}
	if at.IsZero() {
		at = m.notificationNow()
	}
	record := contactActivity{Principal: principal, Origin: origin, LastSeen: at.UTC()}
	path := m.contactActivityPath(principal, origin)
	m.notificationActivityMu.Lock()
	defer m.notificationActivityMu.Unlock()
	var current contactActivity
	if err := readPrivateJSON(path, &current, 16<<10); err == nil && current.LastSeen.After(record.LastSeen) {
		return nil
	}
	return writePrivateJSON(path, record)
}

func (m *Manager) boundPrincipal(origin string) (string, bool) {
	for _, binding := range m.Config.Notifications.ContactBindings {
		for _, contact := range binding.Contacts {
			if strings.TrimSpace(contact) == origin {
				return strings.TrimSpace(binding.Principal), true
			}
		}
	}
	return "", false
}

func (m *Manager) contactActivityPath(principal, origin string) string {
	sum := sha256.Sum256([]byte(principal + "\x00" + origin))
	return m.Config.StatePath("runtime", "notification-activity", hex.EncodeToString(sum[:16])+".json")
}

// reminderOrigin selects the most recently active authorized remote contact
// for the origin's explicitly bound principal. With no binding or eligible
// remote contact it fails closed to the original authorized conversation.
func (m *Manager) reminderOrigin(originText string) (Origin, string, error) {
	origin, err := ParseOrigin(originText)
	if err != nil {
		return Origin{}, "", err
	}
	if m.AuthorizeNotificationOrigin == nil {
		return Origin{}, "", errors.New("notification authorization is unavailable")
	}
	if err := m.AuthorizeNotificationOrigin(origin); err != nil {
		return Origin{}, "", err
	}
	principal, bound := m.boundPrincipal(originText)
	if !bound {
		return origin, "cross-channel escalation requires an explicit trusted identity binding; reminder retained on origin", nil
	}
	type candidate struct {
		origin Origin
		seen   time.Time
	}
	var candidates []candidate
	directory := m.Config.StatePath("runtime", "notification-activity")
	entries, readErr := os.ReadDir(directory)
	if readErr != nil && !os.IsNotExist(readErr) {
		return Origin{}, "", readErr
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var activity contactActivity
		if readPrivateJSON(filepath.Join(directory, entry.Name()), &activity, 16<<10) != nil || activity.Principal != principal {
			continue
		}
		candidateOrigin, parseErr := ParseOrigin(activity.Origin)
		if parseErr != nil || (candidateOrigin.Channel != "telegram" && candidateOrigin.Channel != "whatsapp") || m.AuthorizeNotificationOrigin(candidateOrigin) != nil {
			continue
		}
		candidates = append(candidates, candidate{origin: candidateOrigin, seen: activity.LastSeen})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].seen.Equal(candidates[j].seen) {
			left := candidates[i].origin.Channel + "/" + candidates[i].origin.Conversation
			right := candidates[j].origin.Channel + "/" + candidates[j].origin.Conversation
			return left < right
		}
		return candidates[i].seen.After(candidates[j].seen)
	})
	if len(candidates) == 0 {
		return origin, "no authorized active remote contact is available; reminder retained on origin", nil
	}
	return candidates[0].origin, "", nil
}
