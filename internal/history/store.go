package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agent0ai/spynel/internal/shortid"
)

const (
	historyReadChunk     = 64 * 1024
	maxHistoryEntryBytes = 4 * 1024 * 1024
)

type Entry struct {
	At         time.Time `json:"at"`
	Role       string    `json:"role"`
	Sender     string    `json:"sender,omitempty"`
	ReplyTo    string    `json:"reply_to,omitempty"`
	Content    string    `json:"content"`
	EventID    string    `json:"event_id,omitempty"`
	AfterChars int       `json:"after_chars,omitempty"`
	Terminal   bool      `json:"terminal,omitempty"`
	compact    bool
}

type Conversation struct {
	Channel      string
	Conversation string
	Path         string
	UpdatedAt    time.Time
	LastRole     string
	Preview      string
}

type Store struct {
	root string
	mu   sync.Mutex
}

var unsafePath = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

func New(root string) *Store {
	return &Store{root: root}
}

func (s *Store) Path(channel, conversation string) string {
	return filepath.Join(s.root, clean(channel), clean(conversation)+".jsonl")
}

func clean(value string) string {
	value = strings.Trim(unsafePath.ReplaceAllString(value, "_"), "._")
	if value == "" {
		return "default"
	}
	if len(value) > 120 {
		value = value[:120]
	}
	return value
}

func (s *Store) Append(channel, conversation string, entry Entry) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.Path(channel, conversation)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if entry.At.IsZero() {
		entry.At = time.Now().UTC()
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return "", err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		return "", err
	}
	return path, file.Sync()
}

func (s *Store) DeliveryState(channel, conversation, eventID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	file, err := os.Open(s.Path(channel, conversation))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), maxHistoryEntryBytes)
	state := ""
	for scanner.Scan() {
		var entry Entry
		if json.Unmarshal(scanner.Bytes(), &entry) == nil && entry.EventID == eventID {
			switch entry.Role {
			case "notification_sending":
				state = "sending"
			case "notification_failed":
				state = "failed"
			case "assistant":
				if entry.Sender == "Spy" {
					state = "sent"
				}
			}
		}
	}
	return state, scanner.Err()
}

func (s *Store) Recent(channel, conversation string, characterLimit int) (string, string, error) {
	return s.RecentBounded(channel, conversation, -1, characterLimit)
}

// RecentBounded reads JSONL from the end of the file and stops as soon as the
// requested message and character window is satisfied. A zero message limit
// disables history context; a negative value leaves only the character bound.
func (s *Store) RecentBounded(channel, conversation string, messageLimit, characterLimit int) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.Path(channel, conversation)
	if characterLimit <= 0 || messageLimit == 0 {
		return "", path, nil
	}
	entries, err := readRecentEntries(path, messageLimit, characterLimit)
	if err != nil {
		return "", path, err
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, formatEntry(entry))
	}
	result := strings.Join(lines, "\n")
	runes := []rune(result)
	if len(runes) > characterLimit {
		result = string(runes[len(runes)-characterLimit:])
	}
	return result, path, nil
}

// RecentEntries returns a bounded structured tail for TUI display without
// loading the complete append-only conversation.
func (s *Store) RecentEntries(channel, conversation string, messageLimit, characterLimit int) ([]Entry, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.Path(channel, conversation)
	if messageLimit == 0 || characterLimit <= 0 {
		return nil, path, nil
	}
	entries, err := readRecentEntries(path, messageLimit, characterLimit)
	return entries, path, err
}

func readRecentEntries(path string, messageLimit, characterLimit int) ([]Entry, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	position := info.Size()
	var carry []byte
	var reversed []Entry
	used := 0
	stopped := false
	consume := func(line []byte) {
		if stopped || len(bytes.TrimSpace(line)) == 0 {
			return
		}
		var entry Entry
		if json.Unmarshal(line, &entry) != nil {
			return
		}
		formatted := formatEntry(entry)
		characters := len([]rune(formatted))
		if len(reversed) == 0 && characters > characterLimit {
			var ok bool
			entry, ok = boundNewestEntry(entry, characterLimit)
			if !ok {
				stopped = true
				return
			}
			formatted = formatEntry(entry)
			characters = len([]rune(formatted))
		}
		if len(reversed) > 0 {
			characters++
		}
		if messageLimit > 0 && len(reversed) >= messageLimit {
			stopped = true
			return
		}
		if used > 0 && used+characters > characterLimit {
			stopped = true
			return
		}
		reversed = append(reversed, entry)
		used += characters
		if (messageLimit > 0 && len(reversed) >= messageLimit) || used >= characterLimit {
			stopped = true
		}
	}
	for position > 0 && !stopped {
		readSize := int64(historyReadChunk)
		if position < readSize {
			readSize = position
		}
		position -= readSize
		chunk := make([]byte, readSize)
		if _, err := file.ReadAt(chunk, position); err != nil {
			return nil, err
		}
		block := make([]byte, 0, len(chunk)+len(carry))
		block = append(block, chunk...)
		block = append(block, carry...)
		parts := bytes.Split(block, []byte{'\n'})
		carry = append(carry[:0], parts[0]...)
		if len(carry) > maxHistoryEntryBytes {
			return nil, fmt.Errorf("history entry exceeds %d bytes", maxHistoryEntryBytes)
		}
		for index := len(parts) - 1; index >= 1 && !stopped; index-- {
			consume(parts[index])
		}
	}
	if position == 0 && !stopped {
		consume(carry)
	}
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	return resolveNotificationOrder(reversed), nil
}

func formatEntry(entry Entry) string {
	reply := ""
	if entry.ReplyTo != "" {
		reply = "[reply_to: " + entry.ReplyTo + "]"
	}
	if entry.compact {
		if entry.Content == "" {
			return reply
		}
		return reply + " " + entry.Content
	}
	label := entry.Role
	if entry.Sender != "" {
		label += " (" + entry.Sender + ")"
	}
	if reply != "" {
		reply += " "
	}
	return fmt.Sprintf("[%s] %s: %s%s", entry.At.Format(time.RFC3339), label, reply, entry.Content)
}

func boundNewestEntry(entry Entry, limit int) (Entry, bool) {
	if limit <= 0 {
		return Entry{}, false
	}
	if entry.ReplyTo == "" {
		content := []rune(entry.Content)
		if len(content) > limit {
			entry.Content = "…" + string(content[len(content)-limit:])
		}
		return entry, true
	}
	for _, compact := range []bool{false, true} {
		candidate, ok := boundReplyEntry(entry, limit, compact)
		if ok {
			return candidate, true
		}
	}
	// A bound too small for a complete labeled native ID is safer as an empty
	// history window than a misleading tail-sliced identifier.
	return Entry{}, false
}

func boundReplyEntry(entry Entry, limit int, compact bool) (Entry, bool) {
	id, preview := splitReply(entry.ReplyTo)
	if id == "" {
		return Entry{}, false
	}
	candidate := entry
	candidate.compact = compact
	candidate.ReplyTo = id
	candidate.Content = ""
	base := len([]rune(formatEntry(candidate)))
	previewRunes := []rune(preview)
	contentRunes := []rune(entry.Content)
	minimum := base
	if len(previewRunes) > 0 {
		minimum += 2 // space and visible truncation marker
	}
	if len(contentRunes) > 0 {
		minimum += 2 // separator and visible truncation marker
	}
	if minimum > limit {
		return Entry{}, false
	}
	remaining := limit - base
	if len(previewRunes) > 0 {
		budget := remaining - 1
		if len(contentRunes) > 0 {
			budget -= 2
		}
		preview = truncateHead(previewRunes, budget)
		candidate.ReplyTo = id + " " + preview
		remaining -= 1 + len([]rune(preview))
	}
	if len(contentRunes) > 0 {
		budget := remaining - 1
		candidate.Content = truncateTail(contentRunes, budget)
	}
	return candidate, len([]rune(formatEntry(candidate))) <= limit
}

func splitReply(value string) (string, string) {
	parts := strings.SplitN(strings.TrimSpace(value), " ", 2)
	if len(parts) == 0 {
		return "", ""
	}
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], strings.TrimSpace(parts[1])
}

func truncateHead(value []rune, budget int) string {
	if budget <= 0 {
		return ""
	}
	if len(value) <= budget {
		return string(value)
	}
	if budget == 1 {
		return "…"
	}
	return string(value[:budget-1]) + "…"
}

func truncateTail(value []rune, budget int) string {
	if budget <= 0 {
		return ""
	}
	if len(value) <= budget {
		return string(value)
	}
	if budget == 1 {
		return "…"
	}
	return "…" + string(value[len(value)-budget+1:])
}

// Entries returns the complete structured history for one channel
// conversation, in append order.
func (s *Store) Entries(channel, conversation string) ([]Entry, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.Path(channel, conversation)
	entries, err := readEntries(path)
	return entries, path, err
}

func (s *Store) HasEntries(channel, conversation string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := os.Stat(s.Path(channel, conversation))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Size() > 0, nil
}

// List discovers conversations from disk and reads only one bounded tail entry
// per file. No transcript bodies are retained merely to populate a picker.
func (s *Store) List(limit int) ([]Conversation, error) {
	channels, err := os.ReadDir(s.root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var conversations []Conversation
	for _, channelEntry := range channels {
		if !channelEntry.IsDir() {
			continue
		}
		channelName := channelEntry.Name()
		files, err := os.ReadDir(filepath.Join(s.root, channelName))
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			if file.IsDir() || filepath.Ext(file.Name()) != ".jsonl" {
				continue
			}
			path := filepath.Join(s.root, channelName, file.Name())
			info, err := file.Info()
			if err != nil {
				return nil, err
			}
			conversation := Conversation{Channel: channelName, Conversation: strings.TrimSuffix(file.Name(), ".jsonl"), Path: path, UpdatedAt: info.ModTime()}
			entries, err := readRecentEntries(path, 1, 4096)
			if err != nil {
				return nil, err
			}
			if len(entries) > 0 {
				last := entries[len(entries)-1]
				conversation.LastRole = last.Role
				conversation.Preview = strings.ReplaceAll(strings.TrimSpace(last.Content), "\n", " ")
				if runes := []rune(conversation.Preview); len(runes) > 120 {
					conversation.Preview = string(runes[:120]) + "…"
				}
				if !last.At.IsZero() {
					conversation.UpdatedAt = last.At
				}
			}
			conversations = append(conversations, conversation)
			if limit > 0 && len(conversations) >= limit*2 {
				conversations = newestConversations(conversations, limit)
			}
		}
	}
	return newestConversations(conversations, limit), nil
}

// Latest returns the most recently updated conversation for one channel
// without retaining transcript bodies. Startup uses it to resume the last TUI
// conversation only when this process becomes the first workspace owner.
func (s *Store) Latest(channel string) (Conversation, bool, error) {
	directory := filepath.Join(s.root, clean(channel))
	files, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return Conversation{}, false, nil
	}
	if err != nil {
		return Conversation{}, false, err
	}
	var latest Conversation
	found := false
	for _, file := range files {
		if file.IsDir() || filepath.Ext(file.Name()) != ".jsonl" {
			continue
		}
		path := filepath.Join(directory, file.Name())
		info, err := file.Info()
		if err != nil {
			return Conversation{}, false, err
		}
		candidate := Conversation{
			Channel: channel, Conversation: strings.TrimSuffix(file.Name(), ".jsonl"),
			Path: path, UpdatedAt: info.ModTime(),
		}
		entries, err := readRecentEntries(path, 1, 4096)
		if err != nil {
			return Conversation{}, false, err
		}
		if len(entries) > 0 && !entries[0].At.IsZero() {
			candidate.UpdatedAt = entries[0].At
		}
		if !found || candidate.UpdatedAt.After(latest.UpdatedAt) {
			latest = candidate
			found = true
		}
	}
	return latest, found, nil
}

func newestConversations(conversations []Conversation, limit int) []Conversation {
	sort.Slice(conversations, func(i, j int) bool { return conversations[i].UpdatedAt.After(conversations[j].UpdatedAt) })
	if limit <= 0 || len(conversations) <= limit {
		return conversations
	}
	return append([]Conversation(nil), conversations[:limit]...)
}

// Branch streams an existing transcript to a new TUI conversation. The source
// transport history remains untouched and future messages append independently.
func (s *Store) Branch(sourceChannel, sourceConversation string) (string, string, error) {
	return s.BranchTo(sourceChannel, sourceConversation, "tui")
}

// BranchTo streams an existing transcript to a new conversation owned by the
// requested target channel. It captures the source's current size, so later
// source messages never leak into the independent branch.
func (s *Store) BranchTo(sourceChannel, sourceConversation, targetChannel string) (string, string, error) {
	s.mu.Lock()
	source := s.Path(sourceChannel, sourceConversation)
	input, err := os.Open(source)
	if err != nil {
		s.mu.Unlock()
		return "", "", err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		s.mu.Unlock()
		return "", "", err
	}
	targetDirectory := filepath.Join(s.root, clean(targetChannel))
	if err := os.MkdirAll(targetDirectory, 0o700); err != nil {
		s.mu.Unlock()
		return "", "", err
	}
	s.mu.Unlock()
	output, err := os.CreateTemp(targetDirectory, ".resume-*")
	if err != nil {
		return "", "", err
	}
	temporary := output.Name()
	defer os.Remove(temporary)
	if err := output.Chmod(0o600); err != nil {
		_ = output.Close()
		return "", "", err
	}
	_, copyErr := io.CopyN(output, input, info.Size())
	if copyErr == nil {
		copyErr = output.Sync()
	}
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		if copyErr != nil {
			return "", "", copyErr
		}
		return "", "", closeErr
	}
	for attempts := 0; attempts < 100; attempts++ {
		id, err := shortid.New()
		if err != nil {
			return "", "", err
		}
		conversation := "resume-" + id
		destination := s.Path(targetChannel, conversation)
		if err := os.Link(temporary, destination); err == nil {
			return conversation, destination, nil
		} else if !os.IsExist(err) {
			return "", "", err
		}
	}
	return "", "", errors.New("cannot allocate a unique resumed conversation")
}

// Clear atomically removes one channel conversation's durable history.
func (s *Store) Clear(channel, conversation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.Path(channel, conversation))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func readEntries(path string) ([]Entry, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var entries []Entry
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var entry Entry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return resolveNotificationOrder(entries), nil
}

func resolveNotificationOrder(entries []Entry) []Entry {
	type pending struct {
		entry Entry
		index int
		after int
		ack   bool
	}
	acks := map[string]int{}
	var notices []pending
	for _, entry := range entries {
		if entry.Role == "notification_ack" && entry.EventID != "" {
			acks[entry.EventID] = entry.AfterChars
		}
	}
	for index, entry := range entries {
		if entry.Role == "notification_pending" {
			after, ok := acks[entry.EventID]
			notices = append(notices, pending{entry: entry, index: index, after: after, ack: ok})
		}
	}
	if len(notices) == 0 {
		var filtered []Entry
		for _, entry := range entries {
			if entry.Role != "notification_ack" && entry.Role != "notification_sending" && entry.Role != "notification_failed" {
				filtered = append(filtered, entry)
			}
		}
		return filtered
	}
	targets := map[int][]pending{}
	fallback := map[int][]pending{}
	for _, notice := range notices {
		target := -1
		if notice.ack {
			for i := notice.index + 1; i < len(entries); i++ {
				if entries[i].Role == "assistant" && entries[i].Sender != "Spy" {
					target = i
					break
				}
				if entries[i].Terminal && entries[i].Role == "error" {
					break
				}
			}
		}
		if target >= 0 {
			targets[target] = append(targets[target], notice)
			continue
		}
		for i := notice.index + 1; i < len(entries); i++ {
			if entries[i].Terminal {
				target = i
				break
			}
		}
		if target < 0 {
			target = len(entries) - 1
		}
		fallback[target] = append(fallback[target], notice)
	}
	var output []Entry
	for index, entry := range entries {
		if entry.Role == "notification_pending" || entry.Role == "notification_ack" || entry.Role == "notification_sending" || entry.Role == "notification_failed" {
			for _, notice := range fallback[index] {
				output = append(output, Entry{At: notice.entry.At, Role: "assistant", Sender: "Spy", Content: notice.entry.Content, EventID: notice.entry.EventID})
			}
			continue
		}
		group := targets[index]
		if len(group) > 0 {
			sort.SliceStable(group, func(i, j int) bool { return group[i].after < group[j].after })
			runes := []rune(entry.Content)
			start := 0
			for _, notice := range group {
				at := min(max(notice.after, start), len(runes))
				if at > start {
					segment := entry
					segment.Content = string(runes[start:at])
					output = append(output, segment)
				}
				output = append(output, Entry{At: notice.entry.At, Role: "assistant", Sender: "Spy", Content: notice.entry.Content, EventID: notice.entry.EventID})
				start = at
			}
			if start < len(runes) {
				entry.Content = string(runes[start:])
				output = append(output, entry)
			}
		} else {
			output = append(output, entry)
		}
		for _, notice := range fallback[index] {
			output = append(output, Entry{At: notice.entry.At, Role: "assistant", Sender: "Spy", Content: notice.entry.Content, EventID: notice.entry.EventID})
		}
	}
	return output
}
