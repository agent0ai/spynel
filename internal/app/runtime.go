package app

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/frdel/spynel/internal/core"
)

const (
	logPageEntries      = 20
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
	if len(entries) == 0 {
		return "# Log\n\nNo runtime log entries."
	}
	totalPages := (len(entries) + logPageEntries - 1) / logPageEntries
	if page > totalPages {
		return fmt.Sprintf("# Log\n\nPage %d does not exist. The log has %d entries across %d pages.", page, len(entries), totalPages)
	}
	end := len(entries) - (page-1)*logPageEntries
	start := max(0, end-logPageEntries)
	lines := []string{"# Log", "", fmt.Sprintf("%d entries. Page %d of %d, showing %d.", len(entries), page, totalPages, end-start), ""}
	for _, entry := range entries[start:end] {
		text := strings.ReplaceAll(entry.Text, "\n", " ")
		lines = append(lines, fmt.Sprintf("- `%s` %s", entry.At.Local().Format("15:04:05"), text))
	}
	if totalPages > 1 {
		lines = append(lines, "", "Use `/log page <number>` to move through the log; page 1 is newest.")
	}
	return strings.Join(lines, "\n")
}

func formatLogSearch(entries []LogEntry, query string) string {
	queryLower := strings.ToLower(query)
	matches := make([]LogEntry, 0)
	for _, entry := range entries {
		if strings.Contains(strings.ToLower(entry.Text), queryLower) {
			matches = append(matches, entry)
		}
	}
	if len(matches) == 0 {
		return fmt.Sprintf("# Log search\n\nNo runtime log entries contain %q.", query)
	}
	start := max(0, len(matches)-maxLogSearchEntries)
	lines := []string{"# Log search", "", fmt.Sprintf("%d matches for %q. Showing %d most recent.", len(matches), query, len(matches)-start), ""}
	for _, entry := range matches[start:] {
		text := strings.ReplaceAll(entry.Text, "\n", " ")
		lines = append(lines, fmt.Sprintf("- `%s` %s", entry.At.Local().Format("15:04:05"), text))
	}
	return strings.Join(lines, "\n")
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
