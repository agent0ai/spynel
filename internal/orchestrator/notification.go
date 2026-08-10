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
	"unicode/utf8"

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
	ID            string    `json:"id"`
	Origin        string    `json:"origin"`
	Message       string    `json:"message"`
	State         string    `json:"state"`
	Attempts      int       `json:"attempts"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	NextAttemptAt time.Time `json:"next_attempt_at,omitempty"`
	DeliveredAt   time.Time `json:"delivered_at,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
}

type Outbox struct {
	Directory string
	Deliver   func(context.Context, Origin, string, string) error
	Now       func() time.Time
	mu        sync.Mutex
}

func (o *Outbox) now() time.Time {
	if o.Now != nil {
		return o.Now().UTC()
	}
	return time.Now().UTC()
}
func (o *Outbox) path(id string) string { return filepath.Join(o.Directory, id+".json") }

func NotificationOutboxID(eventKey, outcome string) string {
	hash := sha256.Sum256([]byte(eventKey + "\x00" + outcome))
	return hex.EncodeToString(hash[:16])
}

// NormalizeNotificationText removes terminal protocol traffic before a
// notification can enter any durable or visible boundary. A PTY returns
// terminal query replies on the same input stream used by an stdin-based
// notification action, so treating that stream as authored prose is unsafe.
func NormalizeNotificationText(value string) (string, error) {
	var normalized strings.Builder
	normalized.Grow(len(value))
	for index := 0; index < len(value); {
		character, size := utf8.DecodeRuneInString(value[index:])
		if character == utf8.RuneError && size == 1 {
			if value[index] >= 0x80 && value[index] <= 0x9f {
				index = skipC1NotificationControl(value, index, 1)
				continue
			}
			return "", errors.New("notification message must be valid UTF-8")
		}
		switch {
		case character == 0x1b:
			index = skipESCNotificationControl(value, index+size)
		case character >= 0x80 && character <= 0x9f:
			index = skipC1NotificationControl(value, index, size)
		case character < 0x20 || character == 0x7f:
			if character == '\n' || character == '\t' {
				normalized.WriteString(value[index : index+size])
			}
			index += size
		default:
			normalized.WriteString(value[index : index+size])
			index += size
		}
	}
	text := strings.TrimSpace(normalized.String())
	if text == "" {
		return "", errors.New("notification message is empty after removing terminal controls")
	}
	return text, nil
}

func skipESCNotificationControl(value string, index int) int {
	if index >= len(value) {
		return len(value)
	}
	character, size := utf8.DecodeRuneInString(value[index:])
	if character == utf8.RuneError && size == 1 {
		return index
	}
	switch character {
	case '[':
		return skipCSINotificationControl(value, index+size)
	case ']':
		return skipStringNotificationControl(value, index+size, true)
	case 'P', 'X', '^', '_':
		return skipStringNotificationControl(value, index+size, false)
	case '\\':
		return index + size
	}
	if character >= 0x20 && character <= 0x2f {
		index += size
		for index < len(value) {
			character, size = utf8.DecodeRuneInString(value[index:])
			if character >= 0x20 && character <= 0x2f {
				index += size
				continue
			}
			if character >= 0x30 && character <= 0x7e {
				return index + size
			}
			return index
		}
		return len(value)
	}
	if character >= 0x30 && character <= 0x7e {
		return index + size
	}
	return index
}

func skipC1NotificationControl(value string, index, size int) int {
	character, _ := utf8.DecodeRuneInString(value[index:])
	if size == 1 {
		character = rune(value[index])
	}
	next := index + size
	switch character {
	case 0x90, 0x98, 0x9e, 0x9f:
		return skipStringNotificationControl(value, next, false)
	case 0x9b:
		return skipCSINotificationControl(value, next)
	case 0x9d:
		return skipStringNotificationControl(value, next, true)
	default:
		return next
	}
}

func skipCSINotificationControl(value string, index int) int {
	intermediates := false
	for index < len(value) {
		character, size := utf8.DecodeRuneInString(value[index:])
		if character == utf8.RuneError && size == 1 {
			return index
		}
		switch {
		case character >= 0x30 && character <= 0x3f && !intermediates:
			index += size
		case character >= 0x20 && character <= 0x2f:
			intermediates = true
			index += size
		case character >= 0x40 && character <= 0x7e:
			return index + size
		default:
			return index
		}
	}
	return len(value)
}

func skipStringNotificationControl(value string, index int, bellTerminates bool) int {
	for index < len(value) {
		if value[index] == 0x1b && index+1 < len(value) && value[index+1] == '\\' {
			return index + 2
		}
		character, size := utf8.DecodeRuneInString(value[index:])
		if character == utf8.RuneError && size == 1 {
			if value[index] == 0x9c {
				return index + 1
			}
			index++
			continue
		}
		if character == 0x9c || (bellTerminates && character == 0x07) {
			return index + size
		}
		index += size
	}
	return len(value)
}

func (o *Outbox) Enqueue(eventKey, outcome, origin, message string) (OutboxEntry, error) {
	if _, err := ParseOrigin(origin); err != nil {
		return OutboxEntry{}, err
	}
	message, err := NormalizeNotificationText(message)
	if err != nil {
		return OutboxEntry{}, err
	}
	id := NotificationOutboxID(eventKey, outcome)
	o.mu.Lock()
	defer o.mu.Unlock()
	if data, err := os.ReadFile(o.path(id)); err == nil {
		var existing OutboxEntry
		if json.Unmarshal(data, &existing) == nil {
			normalized, normalizeErr := NormalizeNotificationText(existing.Message)
			if normalizeErr != nil {
				return OutboxEntry{}, normalizeErr
			}
			if normalized != existing.Message {
				existing.Message = normalized
				existing.UpdatedAt = o.now()
				if err := o.write(existing); err != nil {
					return OutboxEntry{}, err
				}
			}
			return existing, nil
		}
	}
	now := o.now()
	entry := OutboxEntry{ID: id, Origin: origin, Message: message, State: "pending", CreatedAt: now, UpdatedAt: now, NextAttemptAt: now}
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
		normalized, normalizeErr := NormalizeNotificationText(entry.Message)
		if normalizeErr != nil {
			entry.State = "cancelled"
			entry.Attempts++
			entry.UpdatedAt = o.now()
			entry.LastError = normalizeErr.Error()
			if writeErr := o.write(entry); writeErr != nil {
				problems = append(problems, writeErr)
			}
			problems = append(problems, normalizeErr)
			continue
		}
		entry.Message = normalized
		origin, parseErr := ParseOrigin(entry.Origin)
		deliveryErr := parseErr
		if deliveryErr == nil {
			deliveryErr = o.Deliver(ctx, origin, entry.ID, entry.Message)
		}
		entry.Attempts++
		entry.UpdatedAt = o.now()
		if deliveryErr == nil {
			entry.State = "delivered"
			entry.DeliveredAt = entry.UpdatedAt
			entry.LastError = ""
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
