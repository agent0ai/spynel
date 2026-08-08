package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agent0ai/spynel/internal/fsx"
)

type Origin struct{ Channel, Conversation string }

func ParseOrigin(value string) (Origin, error) {
	channel, conversation, ok := strings.Cut(strings.TrimSpace(value), "/")
	if !ok || conversation == "" {
		return Origin{}, errors.New("origin must be channel/conversation")
	}
	switch channel {
	case "telegram", "whatsapp", "tui", "cli":
	default:
		return Origin{}, fmt.Errorf("unsupported origin channel %q", channel)
	}
	if strings.ContainsAny(conversation, "\r\n\x00") {
		return Origin{}, errors.New("origin conversation contains invalid characters")
	}
	return Origin{Channel: channel, Conversation: conversation}, nil
}

type NotificationPolicy struct {
	Enabled  bool
	Origin   Origin
	Outcomes map[string]bool
}

func NotificationFromDocument(document Document) (NotificationPolicy, error) {
	raw, exists := document.FrontMatter["notify"]
	if !exists {
		return NotificationPolicy{}, nil
	}
	values, ok := raw.(map[string]any)
	if !ok {
		return NotificationPolicy{}, errors.New("notify must be a mapping")
	}
	policy := NotificationPolicy{Outcomes: map[string]bool{}}
	if enabled, ok := values["enabled"].(bool); ok {
		policy.Enabled = enabled
	} else {
		return policy, errors.New("notify.enabled must be a boolean")
	}
	if !policy.Enabled {
		return policy, nil
	}
	originText, ok := values["origin"].(string)
	if !ok {
		return policy, errors.New("notify.origin is required when enabled")
	}
	origin, err := ParseOrigin(originText)
	if err != nil {
		return policy, err
	}
	policy.Origin = origin
	rawOutcomes, exists := values["on"]
	if !exists {
		rawOutcomes = []any{"done"}
	}
	switch list := rawOutcomes.(type) {
	case []any:
		for _, item := range list {
			value, ok := item.(string)
			if !ok {
				return policy, errors.New("notify.on values must be strings")
			}
			policy.Outcomes[value] = true
		}
	case []string:
		for _, value := range list {
			policy.Outcomes[value] = true
		}
	default:
		return policy, errors.New("notify.on must be a list")
	}
	for outcome := range policy.Outcomes {
		if outcome != "done" && outcome != "failed" && outcome != "waiting" && outcome != "cancelled" {
			return policy, fmt.Errorf("notify.on contains unsupported outcome %q", outcome)
		}
	}
	return policy, nil
}

type OutboxEntry struct {
	ID               string    `json:"id"`
	Origin           string    `json:"origin"`
	Message          string    `json:"message"`
	TaskID           string    `json:"task_id,omitempty"`
	Outcome          string    `json:"outcome"`
	State            string    `json:"state"`
	Attempts         int       `json:"attempts"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	NextAttemptAt    time.Time `json:"next_attempt_at,omitempty"`
	DeliveredAt      time.Time `json:"delivered_at,omitempty"`
	LastError        string    `json:"last_error,omitempty"`
	ActionRequestID  string    `json:"action_request_id,omitempty"`
	Kind             string    `json:"kind,omitempty"`
	NativeMessageIDs []string  `json:"native_message_ids,omitempty"`
}

type Outbox struct {
	Directory   string
	Deliver     func(context.Context, Origin, string, string) ([]string, error)
	Now         func() time.Time
	OnDelivered func(OutboxEntry) error
	mu          sync.Mutex
}

func (o *Outbox) now() time.Time {
	if o.Now != nil {
		return o.Now().UTC()
	}
	return time.Now().UTC()
}
func (o *Outbox) path(id string) string { return filepath.Join(o.Directory, id+".json") }

func (o *Outbox) Enqueue(taskID, outcome, origin, message string) (OutboxEntry, error) {
	return o.enqueueLinked(taskID, outcome, origin, message, "", "")
}

func (o *Outbox) EnqueueLinked(taskID, outcome, origin, message, requestID, kind string) (OutboxEntry, error) {
	if requestID == "" || (kind != "action_request" && kind != "reminder") {
		return OutboxEntry{}, errors.New("invalid linked outbox record")
	}
	return o.enqueueLinked(taskID, outcome, origin, message, requestID, kind)
}

func (o *Outbox) enqueueLinked(taskID, outcome, origin, message, requestID, kind string) (OutboxEntry, error) {
	if _, err := ParseOrigin(origin); err != nil {
		return OutboxEntry{}, err
	}
	hash := sha256.Sum256([]byte(taskID + "\x00" + outcome))
	id := hex.EncodeToString(hash[:16])
	o.mu.Lock()
	defer o.mu.Unlock()
	if data, err := os.ReadFile(o.path(id)); err == nil {
		var existing OutboxEntry
		if json.Unmarshal(data, &existing) == nil {
			if requestID != "" && existing.ActionRequestID == "" && existing.State == "pending" {
				existing.ActionRequestID = requestID
				existing.Kind = kind
				if err := o.write(existing); err != nil {
					return OutboxEntry{}, err
				}
			}
			return existing, nil
		}
	}
	now := o.now()
	entry := OutboxEntry{ID: id, Origin: origin, Message: message, TaskID: taskID, Outcome: outcome, State: "pending", CreatedAt: now, UpdatedAt: now, NextAttemptAt: now, ActionRequestID: requestID, Kind: kind}
	return entry, o.write(entry)
}

func (o *Outbox) write(entry OutboxEntry) error {
	if err := os.MkdirAll(o.Directory, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	return fsx.AtomicWriteFile(o.path(entry.ID), append(data, '\n'), 0o600)
}

func (o *Outbox) linkAction(entryID, requestID, kind string) (OutboxEntry, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var entry OutboxEntry
	data, err := os.ReadFile(o.path(entryID))
	if err != nil {
		return entry, err
	}
	if err := json.Unmarshal(data, &entry); err != nil {
		return entry, err
	}
	entry.ActionRequestID = requestID
	entry.Kind = kind
	return entry, o.write(entry)
}

func (o *Outbox) cancelPendingActionReminders(requestID string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	entries, err := os.ReadDir(o.Directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, item := range entries {
		if item.IsDir() || !strings.HasSuffix(item.Name(), ".json") {
			continue
		}
		var entry OutboxEntry
		data, readErr := os.ReadFile(filepath.Join(o.Directory, item.Name()))
		if readErr != nil || json.Unmarshal(data, &entry) != nil || entry.ActionRequestID != requestID || entry.Kind != "reminder" || entry.State != "pending" {
			continue
		}
		entry.State = "cancelled"
		entry.UpdatedAt = o.now()
		if err := o.write(entry); err != nil {
			return err
		}
	}
	return nil
}

func (o *Outbox) Process(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.Deliver == nil {
		return nil
	}
	entries, err := os.ReadDir(o.Directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var problems []error
	for _, item := range entries {
		if item.IsDir() || !strings.HasSuffix(item.Name(), ".json") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(o.Directory, item.Name()))
		if readErr != nil {
			problems = append(problems, readErr)
			continue
		}
		var entry OutboxEntry
		if json.Unmarshal(data, &entry) != nil || entry.State == "delivered" || entry.State == "cancelled" || entry.NextAttemptAt.After(o.now()) {
			continue
		}
		origin, parseErr := ParseOrigin(entry.Origin)
		deliveryErr := parseErr
		var nativeMessageIDs []string
		if deliveryErr == nil {
			nativeMessageIDs, deliveryErr = o.Deliver(ctx, origin, entry.ID, entry.Message)
		}
		entry.Attempts++
		entry.UpdatedAt = o.now()
		if deliveryErr == nil {
			entry.State = "delivered"
			entry.DeliveredAt = entry.UpdatedAt
			entry.NativeMessageIDs = append([]string(nil), nativeMessageIDs...)
			entry.LastError = ""
			if o.OnDelivered != nil {
				deliveryErr = o.OnDelivered(entry)
				if deliveryErr != nil {
					entry.State = "pending"
					entry.DeliveredAt = time.Time{}
					entry.LastError = deliveryErr.Error()
					entry.NextAttemptAt = entry.UpdatedAt.Add(time.Second)
					problems = append(problems, deliveryErr)
				}
			}
		} else {
			entry.State = "pending"
			entry.LastError = deliveryErr.Error()
			delay := time.Second * time.Duration(1<<min(entry.Attempts-1, 8))
			entry.NextAttemptAt = entry.UpdatedAt.Add(delay)
			problems = append(problems, deliveryErr)
		}
		if writeErr := o.write(entry); writeErr != nil {
			problems = append(problems, writeErr)
		}
	}
	return errors.Join(problems...)
}
