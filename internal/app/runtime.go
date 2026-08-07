package app

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/agent0ai/spynel/internal/core"
)

const (
	logPageEntries      = 20
	maxLogPageRange     = 5
	maxLogSearchEntries = 20
	maxLogEntries       = 4096
	maxLogEntryRunes    = 4096
	maxJobDescription   = 80
)

type LogEntry struct {
	At   time.Time
	Text string
}

type Job struct {
	ID           int
	SessionKey   string
	Channel      string
	Conversation string
	Description  string
	StartedAt    time.Time
}

// Runtime owns process-local operational logs and interruptible harness
// jobs. Its update channel keeps only the newest counts for the TUI.
type Runtime struct {
	mu        sync.Mutex
	logs      []LogEntry
	logStart  int
	jobs      map[int]Job
	bySession map[string]int
	nextJobID int
	updates   chan core.RuntimeStatus

	writerMu sync.Mutex
	partial  []byte
}

func NewRuntime() *Runtime {
	return &Runtime{
		jobs:      map[int]Job{},
		bySession: map[string]int{},
		updates:   make(chan core.RuntimeStatus, 1),
	}
}

func (r *Runtime) Log(message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	if runes := []rune(message); len(runes) > maxLogEntryRunes {
		message = "…" + string(runes[len(runes)-maxLogEntryRunes+1:])
	}
	r.mu.Lock()
	entry := LogEntry{At: time.Now().UTC(), Text: message}
	if len(r.logs) < maxLogEntries {
		r.logs = append(r.logs, entry)
	} else {
		r.logs[r.logStart] = entry
		r.logStart = (r.logStart + 1) % maxLogEntries
	}
	r.publishLocked()
	r.mu.Unlock()
}

// Write captures line-oriented stderr without allowing it to collide with an
// alternate-screen TUI. It implements io.Writer for subprocesses and channels.
func (r *Runtime) Write(data []byte) (int, error) {
	r.writerMu.Lock()
	defer r.writerMu.Unlock()
	length := len(data)
	r.partial = append(r.partial, data...)
	for {
		index := bytes.IndexByte(r.partial, '\n')
		if index < 0 {
			break
		}
		r.Log(strings.TrimSuffix(string(r.partial[:index]), "\r"))
		r.partial = r.partial[index+1:]
	}
	if len(r.partial) > 4096 {
		r.Log(string(r.partial))
		r.partial = nil
	}
	return length, nil
}

func (r *Runtime) BeginJob(sessionKey, channel, conversation, description string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.bySession[sessionKey]; ok {
		return id
	}
	r.nextJobID++
	description = strings.Join(strings.Fields(description), " ")
	if runes := []rune(description); len(runes) > maxJobDescription {
		description = string(runes[:maxJobDescription-1]) + "…"
	}
	job := Job{
		ID: r.nextJobID, SessionKey: sessionKey, Channel: channel,
		Conversation: conversation, Description: description, StartedAt: time.Now().UTC(),
	}
	r.jobs[job.ID] = job
	r.bySession[sessionKey] = job.ID
	r.publishLocked()
	return job.ID
}

func (r *Runtime) EndJob(id int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	job, ok := r.jobs[id]
	if !ok {
		return
	}
	delete(r.jobs, id)
	delete(r.bySession, job.SessionKey)
	r.publishLocked()
}

func (r *Runtime) JobForSession(sessionKey string) (Job, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.bySession[sessionKey]
	if !ok {
		return Job{}, false
	}
	job, ok := r.jobs[id]
	return job, ok
}

func (r *Runtime) Job(id int) (Job, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	job, ok := r.jobs[id]
	return job, ok
}

func (r *Runtime) Jobs() []Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	jobs := make([]Job, 0, len(r.jobs))
	for _, job := range r.jobs {
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	return jobs
}

func (r *Runtime) Logs() []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.logStart == 0 {
		return append([]LogEntry(nil), r.logs...)
	}
	logs := make([]LogEntry, 0, len(r.logs))
	logs = append(logs, r.logs[r.logStart:]...)
	logs = append(logs, r.logs[:r.logStart]...)
	return logs
}

// ClearLogs removes completed and partially captured runtime output and
// publishes the new count to status consumers.
func (r *Runtime) ClearLogs() int {
	r.writerMu.Lock()
	defer r.writerMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	count := len(r.logs)
	r.logs = nil
	r.logStart = 0
	r.partial = nil
	r.publishLocked()
	return count
}

func (r *Runtime) Status() core.RuntimeStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return core.RuntimeStatus{Logs: len(r.logs), Jobs: len(r.jobs)}
}

func (r *Runtime) Updates() <-chan core.RuntimeStatus { return r.updates }

func (r *Runtime) publishLocked() {
	status := core.RuntimeStatus{Logs: len(r.logs), Jobs: len(r.jobs)}
	select {
	case <-r.updates:
	default:
	}
	r.updates <- status
}

func formatLogs(entries []LogEntry) string {
	return formatLogPage(entries, 1)
}

func formatLogPage(entries []LogEntry, page int) string {
	return formatLogPages(entries, page, page)
}

func formatLogPages(entries []LogEntry, firstPage, lastPage int) string {
	if len(entries) == 0 {
		return "# Log\n\nNo runtime log entries."
	}
	totalPages := (len(entries) + logPageEntries - 1) / logPageEntries
	if firstPage > totalPages {
		return fmt.Sprintf("# Log\n\nPage %d does not exist. The log has %d entries across %d pages.", firstPage, len(entries), totalPages)
	}
	lastPage = min(lastPage, totalPages)
	shown := 0
	for page := firstPage; page <= lastPage; page++ {
		end := len(entries) - (page-1)*logPageEntries
		shown += end - max(0, end-logPageEntries)
	}
	coverage := fmt.Sprintf("Page %d", firstPage)
	if firstPage != lastPage {
		coverage = fmt.Sprintf("Pages %d-%d", firstPage, lastPage)
	}
	lines := []string{"# Log", "", fmt.Sprintf("%d entries. %s of %d, showing %d.", len(entries), coverage, totalPages, shown), ""}
	for page := firstPage; page <= lastPage; page++ {
		end := len(entries) - (page-1)*logPageEntries
		start := max(0, end-logPageEntries)
		for _, entry := range entries[start:end] {
			lines = append(lines, formatLogEntry(entry)...)
		}
	}
	if totalPages > 1 {
		lines = append(lines, "", fmt.Sprintf("Use `/log page <number>` or `/log page <start>-<end>` (up to %d pages); page 1 is newest.", maxLogPageRange))
	}
	return strings.Join(lines, "\n")
}

func formatLogSearch(entries []LogEntry, query string) string {
	queryLower := strings.ToLower(query)
	matches := make([]LogEntry, 0)
	for _, entry := range entries {
		if strings.Contains(strings.ToLower(sanitizeLogText(entry.Text)), queryLower) {
			matches = append(matches, entry)
		}
	}
	if len(matches) == 0 {
		return fmt.Sprintf("# Log search\n\nNo runtime log entries contain %q.", query)
	}
	start := max(0, len(matches)-maxLogSearchEntries)
	lines := []string{"# Log search", "", fmt.Sprintf("%d matches for %q. Showing %d most recent.", len(matches), query, len(matches)-start), ""}
	for _, entry := range matches[start:] {
		lines = append(lines, formatLogEntry(entry)...)
	}
	return strings.Join(lines, "\n")
}

// formatLogEntry is the shared user-facing boundary for captured output. The
// source entry stays untouched while every channel receives safe plain text.
func formatLogEntry(entry LogEntry) []string {
	textLines := strings.Split(sanitizeLogText(entry.Text), "\n")
	lines := []string{fmt.Sprintf("- `%s` %s", entry.At.Local().Format("15:04:05"), textLines[0])}
	for _, line := range textLines[1:] {
		lines = append(lines, "  "+line)
	}
	return lines
}

// sanitizeLogText strips ECMA-48 terminal commands and unsafe residual
// controls while retaining Unicode and meaningful line boundaries.
func sanitizeLogText(text string) string {
	runes := decodeLogRunes(text)
	var clean strings.Builder
	for index := 0; index < len(runes); {
		current := runes[index]
		switch current {
		case '\x1b':
			index = consumeEscape(runes, index)
		case '\u009b': // CSI
			index = consumeCSI(runes, index+1)
		case '\u009d': // OSC
			index = consumeControlString(runes, index+1, true)
		case '\u0090', '\u0098', '\u009e', '\u009f': // DCS, SOS, PM, APC
			index = consumeControlString(runes, index+1, false)
		case '\n':
			clean.WriteRune('\n')
			index++
		case '\r':
			clean.WriteRune('\n')
			index++
			if index < len(runes) && runes[index] == '\n' {
				index++
			}
		case '\t':
			clean.WriteString("    ")
			index++
		default:
			if current >= 0x20 && current != 0x7f && !(current >= 0x80 && current <= 0x9f) {
				clean.WriteRune(current)
			}
			index++
		}
	}
	return clean.String()
}

// decodeLogRunes preserves valid UTF-8 while recovering legacy single-byte
// C1 controls so the terminal parser can remove them instead of rendering a
// replacement rune followed by the command parameters.
func decodeLogRunes(text string) []rune {
	runes := make([]rune, 0, utf8.RuneCountInString(text))
	for len(text) > 0 {
		decoded, size := utf8.DecodeRuneInString(text)
		if decoded == utf8.RuneError && size == 1 {
			raw := text[0]
			if raw >= 0x80 && raw <= 0x9f {
				decoded = rune(raw)
			}
		}
		runes = append(runes, decoded)
		text = text[size:]
	}
	return runes
}

func consumeEscape(runes []rune, index int) int {
	index++
	if index >= len(runes) {
		return index
	}
	switch runes[index] {
	case '[':
		return consumeCSI(runes, index+1)
	case ']':
		return consumeControlString(runes, index+1, true)
	case 'P', 'X', '^', '_':
		return consumeControlString(runes, index+1, false)
	}
	for index < len(runes) && runes[index] >= 0x20 && runes[index] <= 0x2f {
		index++
	}
	if index < len(runes) && runes[index] >= 0x30 && runes[index] <= 0x7e {
		index++
	}
	return index
}

func consumeCSI(runes []rune, index int) int {
	for index < len(runes) {
		current := runes[index]
		if current == '\r' || current == '\n' {
			return index
		}
		index++
		if current == '\x18' || current == '\x1a' { // CAN or SUB cancels the sequence.
			break
		}
		if current >= 0x40 && current <= 0x7e {
			break
		}
	}
	return index
}

func consumeControlString(runes []rune, index int, bellTerminated bool) int {
	for index < len(runes) {
		switch runes[index] {
		case '\u009c':
			return index + 1
		case '\a':
			if bellTerminated {
				return index + 1
			}
		case '\x18', '\x1a': // CAN or SUB cancels the string.
			return index + 1
		case '\x1b':
			if index+1 < len(runes) && runes[index+1] == '\\' {
				return index + 2
			}
		}
		index++
	}
	return index
}

func formatJobs(jobs []Job) string {
	if len(jobs) == 0 {
		return "# Jobs\n\nNo agent jobs are running."
	}
	lines := []string{"# Jobs", ""}
	now := time.Now()
	for _, job := range jobs {
		location := job.Channel + "/" + job.Conversation
		lines = append(lines, fmt.Sprintf("- **Job %d** · %s · %s · %s", job.ID, location, shortDuration(now.Sub(job.StartedAt)), job.Description))
	}
	lines = append(lines, "", "Use `/job kill <number>` to stop a job.")
	return strings.Join(lines, "\n")
}

func shortDuration(duration time.Duration) string {
	duration = duration.Round(time.Second)
	if duration < time.Second {
		return "0s"
	}
	return duration.String()
}
