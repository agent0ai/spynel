package app

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/orchestrator"
	"github.com/agent0ai/spynel/internal/shortid"
)

const (
	maxJobProgressEntries = 3
	maxJobProgressRunes   = 320
	maxJobProgressTotal   = 900
	maxJobMetadataRunes   = 160
	maxJobDocumentBytes   = 2 << 20
)

func (s *Service) jobInfoCommand(message core.Message, id int, emit core.Emit) error {
	job, ok := s.Runtime.Job(id)
	if !ok {
		return s.localReply(message, fmt.Sprintf("Job %d is not running; it may be unknown or already finished. Use /jobs to list running jobs.", id), emit)
	}
	if err := s.authorizeJobAccess(message, job); err != nil {
		return s.localReply(message, fmt.Sprintf("Job %d is not available to this conversation.", id), emit)
	}

	lease, hasLease := s.Orchestrator.LeaseForSession(job.SessionKey)
	text := s.formatJobInfo(job, lease, hasLease)

	// Durable reads and harness completion can race. Never present a stale job
	// as active after it has left the process-local registry.
	current, stillRunning := s.Runtime.Job(id)
	if !stillRunning || current.SessionKey != job.SessionKey {
		return s.localReply(message, fmt.Sprintf("Job %d finished while its details were being read. Use /jobs to list running jobs.", id), emit)
	}
	return s.localReply(message, text, emit)
}

func (s *Service) accessibleJobs(message core.Message) []Job {
	jobs := s.Runtime.Jobs()
	visible := jobs[:0]
	for _, job := range jobs {
		if s.authorizeJobAccess(message, job) == nil {
			visible = append(visible, job)
		}
	}
	return visible
}

func (s *Service) formatJobInfo(job Job, lease orchestrator.Lease, hasLease bool) string {
	now := time.Now().UTC()
	kind := job.Kind
	if kind == "" {
		kind = "conversation"
	}
	route := job.Route
	if route == "" {
		route = job.Channel
	}

	lines := []string{
		fmt.Sprintf("# Job %d", job.ID), "",
		"- Kind: " + safeJobText(kind, maxJobMetadataRunes),
		"- Route: " + safeJobText(route, maxJobMetadataRunes),
		"- Execution status: " + safeJobText(formatExecutionStatus(job), maxJobMetadataRunes),
		"- Health: " + safeJobText(formatJobHealth(job), maxJobMetadataRunes),
		"- Current execution age: " + shortDuration(now.Sub(job.StartedAt)),
		"- Started: " + job.StartedAt.UTC().Format(time.RFC3339),
	}
	if job.Durable {
		first := job.FirstAssignedAt
		if first.IsZero() || first.After(now) {
			first = job.StartedAt
		}
		lines = append(lines,
			"- Durable lifetime: "+shortDuration(now.Sub(first)),
			fmt.Sprintf("- Provider steps (▶): %d", max(1, job.ProviderIterations)),
		)
		if job.ImplementationAttempts > 0 {
			lines = append(lines, fmt.Sprintf("- Implementation attempts (↻): %d", job.ImplementationAttempts))
		}
	} else {
		lines = append(lines, "- Provider steps (▶): 1 (live conversation)")
	}
	if !job.LastActivityAt.IsZero() {
		age := now.Sub(job.LastActivityAt)
		if age < 0 {
			age = 0
		}
		lines = append(lines, "- Last activity: "+shortDuration(age)+" ago · "+job.LastActivityAt.UTC().Format(time.RFC3339))
	}
	if job.ReconnectAttempt > 0 {
		reconnect := fmt.Sprintf("%d", job.ReconnectAttempt)
		if job.ReconnectTotal > 0 {
			reconnect += fmt.Sprintf("/%d", job.ReconnectTotal)
		}
		lines = append(lines, "- Reconnect attempt: "+reconnect)
	}
	if job.RecoveryCount > 0 {
		lines = append(lines, fmt.Sprintf("- Recovery count: %d", job.RecoveryCount))
	}
	if job.StatusDetail != "" {
		lines = append(lines, "- Detail: "+safeJobText(boundJobStatusDetail(job.StatusDetail), maxJobMetadataRunes))
	}
	if thread := shortid.Display(s.Harness.ThreadID(job.SessionKey)); thread != "" {
		lines = append(lines, "- Execution: `"+safeJobText(thread, maxJobMetadataRunes)+"`")
	}
	leaseState, leasePhase, leaseHeartbeat := job.LeaseState, job.LeasePhase, job.LeaseHeartbeatAt
	if leasePhase != "" {
		lines = append(lines, "- Phase: "+safeJobText(leasePhase, maxJobMetadataRunes))
	}
	if leaseState != "" {
		line := "- Lease: " + safeJobText(leaseState, maxJobMetadataRunes)
		if !leaseHeartbeat.IsZero() {
			age := now.Sub(leaseHeartbeat)
			if age < 0 {
				age = 0
			}
			line += " · heartbeat " + shortDuration(age) + " ago · " + leaseHeartbeat.UTC().Format(time.RFC3339)
		}
		lines = append(lines, line)
	}

	durableFile := job.DurableFile
	if hasLease && lease.File != "" {
		durableFile = lease.File
	}
	if durableFile == "" {
		lines = append(lines, "", "This job has no linked Markdown task or goal.")
		return strings.Join(lines, "\n")
	}
	lines = append(lines, "", "## Durable work", "", "- Source: "+safeJobText(filepath.Base(durableFile), maxJobMetadataRunes))
	document, err := s.readJobDocument(durableFile)
	if err != nil {
		lines = append(lines, "- Details: unavailable (the linked Markdown document could not be parsed)")
		return strings.Join(lines, "\n")
	}
	for _, field := range []struct {
		key   string
		label string
	}{
		{"title", "Title"}, {"id", "Durable ID"}, {"status", "Status"},
		{"phase", "Document phase"}, {"round", "Round"},
		{"created_at", "Created"}, {"updated_at", "Updated"},
	} {
		if value, ok := safeFrontMatterValue(document.FrontMatter[field.key]); ok {
			lines = append(lines, "- "+field.label+": "+safeJobText(value, maxJobMetadataRunes))
		}
	}
	progress := newestProgressEntries(document.Body)
	if len(progress) > 0 {
		lines = append(lines, "", fmt.Sprintf("## Recent progress (newest %d)", len(progress)), "")
		for _, entry := range progress {
			lines = append(lines, "- "+entry)
		}
	}
	return strings.Join(lines, "\n")
}

func safeFrontMatterValue(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return "", false
		}
		return typed, true
	case int:
		return strconv.Itoa(typed), true
	case int64:
		return strconv.FormatInt(typed, 10), true
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64), true
	case time.Time:
		return typed.UTC().Format(time.RFC3339), true
	default:
		return "", false
	}
}

func newestProgressEntries(body string) []string {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	inProgress := false
	entries := make([]string, 0)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.EqualFold(trimmed, "## Progress") {
			inProgress = true
			continue
		}
		if inProgress && strings.HasPrefix(trimmed, "## ") {
			break
		}
		if !inProgress || trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			entries = append(entries, strings.TrimSpace(trimmed[2:]))
		} else if len(entries) > 0 && (len(line) > 0 && unicode.IsSpace(rune(line[0]))) {
			entries[len(entries)-1] += " " + trimmed
		}
	}

	result := make([]string, 0, maxJobProgressEntries)
	total := 0
	for index := len(entries) - 1; index >= 0 && len(result) < maxJobProgressEntries; index-- {
		entry := safeJobText(entries[index], maxJobProgressRunes)
		if total+len([]rune(entry)) > maxJobProgressTotal {
			remaining := maxJobProgressTotal - total
			if remaining <= 1 {
				break
			}
			entry = boundJobText(entry, remaining)
		}
		result = append(result, entry)
		total += len([]rune(entry))
	}
	return result
}

func safeJobText(value string, limit int) string {
	value = sanitizeLogText(value)
	value = strings.Join(strings.Fields(value), " ")
	replacer := strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "<", "\\<", ">", "\\>", "#", "\\#", "|", "\\|")
	return boundJobText(replacer.Replace(value), limit)
}

func boundJobText(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}

func readBoundedJobDocument(path string) (orchestrator.Document, error) {
	file, err := os.Open(path)
	if err != nil {
		return orchestrator.Document{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxJobDocumentBytes+1))
	if err != nil {
		return orchestrator.Document{}, err
	}
	if len(data) > maxJobDocumentBytes {
		return orchestrator.Document{}, fmt.Errorf("Markdown document exceeds %d-byte inspection limit", maxJobDocumentBytes)
	}
	return orchestrator.ParseDocument(data)
}
