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

	"github.com/frdel/spynel/internal/shortid"
)

const (
	historyReadChunk     = 64 * 1024
	maxHistoryEntryBytes = 4 * 1024 * 1024
)

type Entry struct {
	At      time.Time `json:"at"`
	Role    string    `json:"role"`
	Sender  string    `json:"sender,omitempty"`
	Content string    `json:"content"`
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
			content := []rune(entry.Content)
			if len(content) > characterLimit {
				entry.Content = "…" + string(content[len(content)-characterLimit:])
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
	return reversed, nil
}

func formatEntry(entry Entry) string {
	label := entry.Role
	if entry.Sender != "" {
		label += " (" + entry.Sender + ")"
	}
	return fmt.Sprintf("[%s] %s: %s", entry.At.Format(time.RFC3339), label, entry.Content)
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
	if err := os.MkdirAll(filepath.Join(s.root, "tui"), 0o700); err != nil {
		s.mu.Unlock()
		return "", "", err
	}
	s.mu.Unlock()
	tuiDirectory := filepath.Join(s.root, "tui")
	output, err := os.CreateTemp(tuiDirectory, ".resume-*")
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
		destination := s.Path("tui", conversation)
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
	return entries, nil
}
