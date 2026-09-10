package history

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/agent0ai/spynel/internal/core"
)

const EventReplayBytes = 4 << 20
const EventReplayRecords = 256

var ErrEventCursor = errors.New("invalid event cursor")
var ErrEventExpired = errors.New("event cursor expired; resynchronize conversation history")
var ErrEventLag = errors.New("event replay exceeds retained delivery window; resynchronize conversation history")

// ConversationEvent is a committed history projection, never a harness payload
// or a delivery acknowledgement. Deltas remain on the request response stream.
type ConversationEvent struct {
	ID             string    `json:"id"`
	Cursor         string    `json:"cursor"`
	At             time.Time `json:"at"`
	Kind           string    `json:"kind"`
	Text           string    `json:"text,omitempty"`
	RequestID      string    `json:"request_id,omitempty"`
	NotificationID string    `json:"notification_id,omitempty"`
	FinalText      *string   `json:"final_text,omitempty"`
	Done           bool      `json:"done,omitempty"`
	Continues      bool      `json:"continues,omitempty"`
}

// Events reads one bounded committed suffix. Empty cursor starts at the current
// tail. The cursor binds the workspace/conversation, first record, and exact
// preceding record; clear, replacement, corruption and excessive lag fail closed.
func (s *Store) Events(channel, conversation, cursor string) ([]ConversationEvent, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.eventsLocked(channel, conversation, cursor)
}

// EventSnapshot pairs a bounded visible history tail with its exact resume
// boundary under the same append lock. Private correlation ledgers stay private.
func (s *Store) EventSnapshot(channel, conversation string) ([]ConversationEvent, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, cursor, err := s.eventsLocked(channel, conversation, "")
	if err != nil {
		return nil, "", err
	}
	entries, err := readRecentEntries(s.Path(channel, conversation), 100, 2*1024*1024)
	if err != nil {
		return nil, "", err
	}
	var events []ConversationEvent
	for _, entry := range entries {
		entry.FinalText = nil // Snapshot bounds apply to the visible aggregate tail.
		if event, ok := conversationEvent(entry, ""); ok {
			events = append(events, event)
		}
	}
	return events, cursor, nil
}

func (s *Store) eventsLocked(channel, conversation, cursor string) ([]ConversationEvent, string, error) {
	path := s.Path(channel, conversation)
	info, err := os.Lstat(path)
	if cursor == "" && (os.IsNotExist(err) || err == nil && info.Size() == 0) {
		id, idErr := core.NewSourceMessageID()
		if idErr != nil {
			return nil, "", idErr
		}
		if _, err = s.appendLocked(channel, conversation, Entry{Role: "correlation", Outcome: "event_stream_start", ExecutionID: id}); err != nil {
			return nil, "", err
		}
		info, err = os.Lstat(path)
	}
	if os.IsNotExist(err) {
		return nil, "", ErrEventExpired
	}
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() {
		return nil, "", ErrEventExpired
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	first, err := bufio.NewReader(io.LimitReader(file, maxHistoryEntryBytes)).ReadBytes('\n')
	if err != nil {
		return nil, "", ErrEventExpired
	}
	scope := sha256.Sum256(append([]byte(path+"\x00"), first...))
	prefix := "v1." + hex.EncodeToString(scope[:]) + "."
	offset := info.Size()
	if cursor != "" {
		parts := strings.Split(cursor, ".")
		if len(cursor) > 256 || len(parts) != 5 || parts[0] != "v1" {
			return nil, "", ErrEventCursor
		}
		if !strings.HasPrefix(cursor, prefix) {
			return nil, "", ErrEventExpired
		}
		var parseErr error
		offset, parseErr = strconv.ParseInt(parts[2], 10, 64)
		length, lengthErr := strconv.ParseInt(parts[3], 10, 64)
		if parseErr != nil || lengthErr != nil || offset < 1 || length < 1 || length > maxHistoryEntryBytes || length > offset {
			return nil, "", ErrEventCursor
		}
		if offset > info.Size() {
			return nil, "", ErrEventExpired
		}
		anchor := make([]byte, length)
		if _, err = file.ReadAt(anchor, offset-length); err != nil {
			return nil, "", ErrEventExpired
		}
		if eventCursor(prefix, offset, anchor) != cursor {
			return nil, "", ErrEventExpired
		}
	} else {
		// Read only the bounded final record, even for a large retained history.
		tail := make([]byte, min(info.Size(), int64(maxHistoryEntryBytes)))
		if _, err = file.ReadAt(tail, info.Size()-int64(len(tail))); err != nil {
			return nil, "", err
		}
		if len(tail) == 0 || tail[len(tail)-1] != '\n' {
			return nil, "", ErrEventExpired
		}
		start := bytes.LastIndexByte(tail[:len(tail)-1], '\n') + 1
		if start == 0 && int64(len(tail)) < info.Size() {
			return nil, "", ErrEventLag
		}
		return nil, eventCursor(prefix, offset, tail[start:]), nil
	}
	if info.Size()-offset > EventReplayBytes {
		return nil, "", ErrEventLag
	}
	reader := bufio.NewReader(io.NewSectionReader(file, offset, info.Size()-offset))
	var events []ConversationEvent
	for records := 0; offset < info.Size(); records++ {
		if records == EventReplayRecords {
			return nil, "", ErrEventLag
		}
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil {
			return nil, "", ErrEventExpired
		}
		var entry Entry
		if json.Unmarshal(line, &entry) != nil {
			return nil, "", ErrEventExpired
		}
		offset += int64(len(line))
		cursor = eventCursor(prefix, offset, line)
		if event, ok := conversationEvent(entry, cursor); ok {
			events = append(events, event)
		}
	}
	return events, cursor, nil
}

func conversationEvent(entry Entry, cursor string) (ConversationEvent, bool) {
	kind := entry.Role
	if entry.EventID != "" && (kind == "assistant" || kind == "notification_pending") {
		kind = "notification"
	}
	if kind != "user" && kind != "assistant" && kind != "error" && kind != "notification" {
		return ConversationEvent{}, false
	}
	return ConversationEvent{ID: cursor, Cursor: cursor, At: entry.At, Kind: kind, Text: entry.Content,
		RequestID: entry.SourceMessageID, NotificationID: entry.EventID, FinalText: entry.FinalText,
		Done: entry.Terminal, Continues: entry.Continues}, true
}

func eventCursor(prefix string, offset int64, line []byte) string {
	digest := sha256.Sum256(line)
	return fmt.Sprintf("%s%d.%d.%x", prefix, offset, len(line), digest)
}
