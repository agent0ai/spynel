package app

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/agent0ai/spynel/internal/core"
)

const (
	logPageEntries      = 20
	maxLogSearchEntries = 20
	maxLogEntries       = 4096
	maxLogEntryRunes    = 4096
	maxJobDescription   = 80
	maxJobStatusDetail  = 160
	jobActivityInterval = 15 * time.Second
	maxJobNumber        = 9999
)

type JobExecutionState string

const (
	JobStarting           JobExecutionState = "starting"
	JobRunning            JobExecutionState = "running"
	JobReconnecting       JobExecutionState = "reconnecting"
	JobRecovering         JobExecutionState = "recovering"
	JobAwaitingTransition JobExecutionState = "awaiting_transition"
	JobCancelling         JobExecutionState = "cancelling"
	JobFinishing          JobExecutionState = "finishing"
	JobStalled            JobExecutionState = "stalled"
	JobDegraded           JobExecutionState = "degraded"
	JobError              JobExecutionState = "error"
	JobAudit              JobExecutionState = "audit"
)

type JobHealthState string

const (
	JobHealthHealthy  JobHealthState = "healthy"
	JobHealthDegraded JobHealthState = "degraded"
	JobHealthStalled  JobHealthState = "stalled"
)

type LogEntry struct {
	At        time.Time `json:"at"`
	Level     string    `json:"level,omitempty"`
	Component string    `json:"component,omitempty"`
	Event     string    `json:"event,omitempty"`
	Instance  string    `json:"instance,omitempty"`
	Text      string    `json:"text"`
}

type Job struct {
	ID                     int    // process-local execution handle
	Number                 int    // durable user-facing workspace reference
	StableID               string // private collision-proof archive identity
	Generation             uint64
	SessionKey             string
	Channel                string
	Conversation           string
	Description            string
	Provider               string
	WorkID                 string
	ParentID               string
	StartedAt              time.Time
	FirstAssignedAt        time.Time
	ProviderIterations     int
	ImplementationAttempts int
	PhaseAttempt           int
	Durable                bool
	Kind                   string
	Route                  string
	DurableFile            string
	Execution              JobExecutionState
	Health                 JobHealthState
	StatusDetail           string
	LastActivityAt         time.Time
	ReconnectAttempt       int
	ReconnectTotal         int
	RecoveryCount          int
	LeaseState             string
	LeasePhase             string
	LeaseHeartbeatAt       time.Time
	StateChangedAt         time.Time
}

type JobDetails struct {
	Kind                   string
	Route                  string
	DurableFile            string
	FirstAssignedAt        time.Time
	ProviderIterations     int
	ImplementationAttempts int
	PhaseAttempt           int
	Provider               string
	WorkID                 string
	ParentID               string
	Phase                  string
}

// Runtime owns the bounded retained operational-log view and interruptible
// process-local jobs. Its update channel keeps only the newest counts for the TUI.
type Runtime struct {
	mu               sync.Mutex
	logs             []LogEntry
	logStart         int
	jobs             map[int]Job
	byNumber         map[int]int
	bySession        map[string]int
	jobCancellations map[string]jobCancellationReservation
	nextJobID        int
	updates          chan core.RuntimeStatus

	writerMu  sync.Mutex
	partial   []byte
	partials  map[string][]byte
	closeOnce sync.Once

	persist      *runtimeLogPersistence
	persistDone  chan struct{}
	logEventHook func() // test-only scheduling hook at the memory/disk boundary
	archive      *JobArchive
	Now          func() time.Time
}

type jobCancellationReservation struct {
	count    int
	previous Job
}

func NewRuntime() *Runtime {
	return &Runtime{
		jobs:             map[int]Job{},
		byNumber:         map[int]int{},
		bySession:        map[string]int{},
		jobCancellations: map[string]jobCancellationReservation{},
		updates:          make(chan core.RuntimeStatus, 1),
		Now:              time.Now,
	}
}

// NewRuntimeAt restores the retained runtime-log view and appends this
// process's entries to a new session file below directory. Persistence is
// best-effort: an unavailable or damaged log directory never prevents Spynel
// from starting or using its in-memory log.
func NewRuntimeAt(directory, instance string) *Runtime {
	r := NewRuntime()
	r.persist = newRuntimeLogPersistence(directory, instance)
	r.persistDone = make(chan struct{})
	go r.capturePersistenceFailures()
	entries, restoreErr := r.persist.restore()
	for _, entry := range entries {
		r.appendLocked(entry)
	}
	if restoreErr != nil {
		r.appendPersistenceFailureLocked("restore_failed", restoreErr)
	}
	r.LogEvent("info", "runtime", "session_start", "Spynel runtime session started")
	return r
}

// ConfigureJobArchive attaches the workspace-local archive after configuration
// resolution. It is intentionally independent from the process runtime log.
func (r *Runtime) ConfigureJobArchive(directory string) {
	r.mu.Lock()
	r.archive = newJobArchive(directory)
	r.mu.Unlock()
}

func (r *Runtime) capturePersistenceFailures() {
	defer close(r.persistDone)
	for err := range r.persist.failures {
		r.mu.Lock()
		r.appendPersistenceFailureLocked("append_failed", err)
		r.publishLocked()
		r.mu.Unlock()
	}
}

func (r *Runtime) Log(message string) {
	r.LogEvent("info", "runtime", "output", message)
}

// LogEvent is the common structured operational logging boundary. Redaction,
// control removal, and entry bounds are applied before either memory or disk.
func (r *Runtime) LogEvent(level, component, event, message string) {
	r.logEvent(level, component, event, message, false)
}

func (r *Runtime) logEvent(level, component, event, message string, forceFlush bool) {
	message = boundAndRedactLogText(message)
	if message == "" {
		return
	}
	level, component, event = logField(level, "info"), logField(component, "runtime"), logField(event, "event")
	r.mu.Lock()
	entry := LogEntry{At: time.Now().UTC(), Level: level, Component: component, Event: event, Text: message}
	if r.persist != nil {
		entry.Instance = r.persist.instance
	}
	r.appendLocked(entry)
	r.publishLocked()
	if r.logEventHook != nil {
		r.logEventHook()
	}
	if r.persist != nil {
		if err := r.persist.append(entry, forceFlush || level == "error" || level == "fatal"); err != nil {
			r.appendPersistenceFailureLocked("append_failed", err)
			r.publishLocked()
		}
	}
	r.mu.Unlock()
}

// appendPersistenceFailureLocked records an in-memory-only diagnostic. It must
// never re-enter persistence, otherwise a storage failure could recurse.
func (r *Runtime) appendPersistenceFailureLocked(event string, err error) {
	if err == nil {
		return
	}
	entry := LogEntry{
		At: time.Now().UTC(), Level: "error", Component: "runtime_log", Event: event,
		Text: boundAndRedactLogText(err.Error()),
	}
	if r.persist != nil {
		entry.Instance = r.persist.instance
	}
	r.appendLocked(entry)
}

func (r *Runtime) appendLocked(entry LogEntry) {
	if len(r.logs) < maxLogEntries {
		r.logs = append(r.logs, entry)
	} else {
		r.logs[r.logStart] = entry
		r.logStart = (r.logStart + 1) % maxLogEntries
	}
}

// Writer returns a line-oriented writer with stable attribution. It is used
// for diagnostics from subprocesses and background components; protocol and
// user-content streams must keep their dedicated writers.
func (r *Runtime) Writer(component string) io.Writer {
	return &attributedLogWriter{runtime: r, component: component}
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
		r.LogEvent("info", "process", "stderr", strings.TrimSuffix(string(r.partial[:index]), "\r"))
		r.partial = r.partial[index+1:]
	}
	if len(r.partial) > 4096 {
		r.LogEvent("info", "process", "stderr", string(r.partial))
		r.partial = nil
	}
	return length, nil
}

func (r *Runtime) writeAttributed(component string, data []byte) (int, error) {
	r.writerMu.Lock()
	defer r.writerMu.Unlock()
	if r.partials == nil {
		r.partials = map[string][]byte{}
	}
	length := len(data)
	partial := append(r.partials[component], data...)
	for {
		index := bytes.IndexByte(partial, '\n')
		if index < 0 {
			break
		}
		r.LogEvent("info", component, "output", strings.TrimSuffix(string(partial[:index]), "\r"))
		partial = partial[index+1:]
	}
	if len(partial) > maxLogEntryRunes*4 {
		r.LogEvent("info", component, "output", string(partial))
		partial = nil
	}
	r.partials[component] = partial
	return length, nil
}

func (r *Runtime) BeginJob(sessionKey, channel, conversation, description string) int {
	return r.BeginJobWithDetails(sessionKey, channel, conversation, description, JobDetails{})
}

func (r *Runtime) BeginJobWithDetails(sessionKey, channel, conversation, description string, details JobDetails) int {
	id, err := r.TryBeginJobWithDetails(sessionKey, channel, conversation, description, details)
	r.reportJobArchiveError("admit", err)
	return id
}

// TryBeginJobWithDetails admits a job only after its durable user-facing
// number has been allocated. Callers that would start provider work must use
// this error-returning boundary so an unavailable counter fails closed.
func (r *Runtime) TryBeginJobWithDetails(sessionKey, channel, conversation, description string, details JobDetails) (int, error) {
	id, _, err := r.tryBeginJobWithDetails(sessionKey, channel, conversation, description, details)
	return id, err
}

// tryBeginJobWithDetails also reports whether this call created the runtime
// job, allowing pre-provider callers to unwind only ownership they admitted.
func (r *Runtime) tryBeginJobWithDetails(sessionKey, channel, conversation, description string, details JobDetails) (int, bool, error) {
	r.mu.Lock()
	if id, ok := r.bySession[sessionKey]; ok {
		job := r.jobs[id]
		if job.Durable && details.DurableFile != "" {
			if !details.FirstAssignedAt.IsZero() && (job.FirstAssignedAt.IsZero() || details.FirstAssignedAt.Before(job.FirstAssignedAt)) {
				job.FirstAssignedAt = details.FirstAssignedAt.UTC()
			}
			if details.ProviderIterations > job.ProviderIterations {
				job.ProviderIterations = details.ProviderIterations
			}
			if details.ImplementationAttempts > job.ImplementationAttempts {
				job.ImplementationAttempts = details.ImplementationAttempts
			}
			r.jobs[id] = job
			r.publishLocked()
		}
		r.mu.Unlock()
		return id, false, nil
	}
	now := r.Now().UTC()
	archive := r.archive
	stableID := newStableJobID(now)
	generation := uint64(0)
	number := r.nextJobID%maxJobNumber + 1
	if archive != nil {
		var err error
		number, generation, stableID, err = archive.allocate(now, details.WorkID, details.Kind, details.Phase, details.PhaseAttempt)
		if err != nil {
			r.mu.Unlock()
			return 0, false, fmt.Errorf("allocate durable job number: %w", err)
		}
	}
	r.nextJobID++
	description = strings.Join(strings.Fields(description), " ")
	if runes := []rune(description); len(runes) > maxJobDescription {
		description = string(runes[:maxJobDescription-1]) + "…"
	}
	firstAssignedAt := details.FirstAssignedAt.UTC()
	durable := details.DurableFile != ""
	if !durable || firstAssignedAt.IsZero() || firstAssignedAt.After(now) {
		firstAssignedAt = now
	}
	iterations := details.ProviderIterations
	if iterations < 1 {
		iterations = 1
	}
	job := Job{
		ID: r.nextJobID, Number: number, StableID: stableID, Generation: generation, SessionKey: sessionKey, Channel: channel,
		Conversation: conversation, Description: description, StartedAt: now,
		Provider: details.Provider, WorkID: details.WorkID, ParentID: details.ParentID,
		FirstAssignedAt: firstAssignedAt, ProviderIterations: iterations, ImplementationAttempts: max(0, details.ImplementationAttempts), PhaseAttempt: max(0, details.PhaseAttempt), Durable: durable,
		Kind: details.Kind, Route: details.Route, DurableFile: details.DurableFile,
		LeasePhase: details.Phase, Execution: JobStarting, Health: JobHealthHealthy, LastActivityAt: now, StateChangedAt: now,
	}
	if archive != nil {
		persistedID, err := archive.begin(job)
		if err != nil {
			r.mu.Unlock()
			return 0, false, fmt.Errorf("persist durable job archive: %w", err)
		}
		job.StableID = persistedID
	}
	r.jobs[job.ID] = job
	if currentID, ok := r.byNumber[job.Number]; !ok || newerJobGeneration(job, r.jobs[currentID]) {
		r.byNumber[job.Number] = job.ID
	}
	r.bySession[sessionKey] = job.ID
	r.publishLocked()
	r.mu.Unlock()
	r.LogEvent("info", "jobs", "job_started", fmt.Sprintf("job_id=%d channel=%s kind=%s", job.Number, logField(job.Channel, "unknown"), logField(job.Kind, "chat")))
	return job.ID, true, nil
}

// UpdateJobDurableTiming applies only non-regressing durable observations to
// an active job, allowing controls and guarded continuations to refresh the
// same process-local execution handle without resetting its current execution age.
func (r *Runtime) UpdateJobDurableTiming(id int, firstAssignedAt time.Time, providerIterations int) bool {
	r.mu.Lock()
	job, ok := r.jobs[id]
	if !ok || !job.Durable {
		r.mu.Unlock()
		return false
	}
	changed := false
	if !firstAssignedAt.IsZero() && (job.FirstAssignedAt.IsZero() || firstAssignedAt.Before(job.FirstAssignedAt)) {
		job.FirstAssignedAt = firstAssignedAt.UTC()
		changed = true
	}
	if providerIterations > job.ProviderIterations {
		job.ProviderIterations = providerIterations
		changed = true
	}
	if changed {
		r.jobs[id] = job
		r.publishLocked()
	}
	archive := r.archive
	r.mu.Unlock()
	if changed && archive != nil {
		r.reportJobArchiveError("update", archive.update(job))
	}
	return true
}

// UpdateJob applies a lifecycle update only to the still-active private
// execution handle. Handles are never reused in a process, so late events
// cannot mutate a newer generation after its public number wraps. Activity-only updates are
// throttled and do not publish TUI count/status notifications.
func (r *Runtime) UpdateJob(id int, status core.ExecutionStatus) bool {
	r.mu.Lock()
	job, ok := r.jobs[id]
	if !ok {
		r.mu.Unlock()
		return false
	}
	at := status.At.UTC()
	if at.IsZero() {
		at = time.Now().UTC()
	}
	state := normalizeExecutionState(status.State)
	if state == "" {
		state = job.Execution
	}
	if job.Execution == JobCancelling || executionStateIsTerminal(job.Execution) && !executionStateIsTerminal(state) {
		state = job.Execution
	}
	detail := boundJobStatusDetail(status.Detail)
	if !job.StateChangedAt.IsZero() && at.Before(job.StateChangedAt) {
		if at.After(job.LastActivityAt) {
			job.LastActivityAt = at
			r.jobs[id] = job
		}
		r.mu.Unlock()
		return true
	}
	material := state != job.Execution || detail != job.StatusDetail ||
		status.ReconnectAttempt != job.ReconnectAttempt || status.ReconnectTotal != job.ReconnectTotal
	if !material && !job.LastActivityAt.IsZero() && at.Sub(job.LastActivityAt) < jobActivityInterval {
		r.mu.Unlock()
		return true
	}
	job.Execution = state
	job.Health = healthFromExecution(state)
	job.StatusDetail = detail
	job.ReconnectAttempt = max(0, status.ReconnectAttempt)
	job.ReconnectTotal = max(0, status.ReconnectTotal)
	if at.After(job.LastActivityAt) {
		job.LastActivityAt = at
	}
	if material && at.After(job.StateChangedAt) {
		job.StateChangedAt = at
	}
	r.jobs[id] = job
	if material {
		r.publishLocked()
	}
	archive := r.archive
	r.mu.Unlock()
	if archive != nil {
		r.reportJobArchiveError("update", archive.update(job))
	}
	return true
}

// ReserveJobCancellation atomically snapshots the original live state, counts
// this interrupt request, and publishes cancelling before the provider can
// synchronously emit its terminal event. Concurrent callers share the first
// snapshot so one rejected request cannot roll back another accepted request.
func (r *Runtime) ReserveJobCancellation(id int) (Job, bool) {
	r.mu.Lock()
	job, ok := r.jobs[id]
	if !ok {
		r.mu.Unlock()
		return Job{}, false
	}
	reservation := r.jobCancellations[job.StableID]
	if reservation.count == 0 {
		reservation.previous = job
	}
	reservation.count++
	r.jobCancellations[job.StableID] = reservation
	job.Execution = JobCancelling
	job.Health = healthFromExecution(JobCancelling)
	now := r.Now().UTC()
	job.LastActivityAt = now
	job.StateChangedAt = now
	r.jobs[job.ID] = job
	r.publishLocked()
	archive := r.archive
	r.mu.Unlock()
	if archive != nil {
		r.reportJobArchiveError("update", archive.update(job))
	}
	return job, true
}

// RestoreJobAfterFailedCancellation releases one interrupt reservation. The
// original state is restored only after every concurrent request for the same
// stable job has failed and the provider has not already finalized the job.
func (r *Runtime) RestoreJobAfterFailedCancellation(job Job) bool {
	r.mu.Lock()
	current, ok := r.jobs[job.ID]
	reservation, reserved := r.jobCancellations[job.StableID]
	if !ok || current.StableID != job.StableID || !reserved || reservation.count < 1 {
		r.mu.Unlock()
		return false
	}
	reservation.count--
	if reservation.count > 0 {
		r.jobCancellations[job.StableID] = reservation
		r.mu.Unlock()
		return false
	}
	delete(r.jobCancellations, job.StableID)
	if current.Execution != JobCancelling {
		r.mu.Unlock()
		return false
	}
	previous := reservation.previous
	current.Execution = previous.Execution
	current.Health = previous.Health
	current.StatusDetail = previous.StatusDetail
	current.LastActivityAt = previous.LastActivityAt
	current.StateChangedAt = previous.StateChangedAt
	current.ReconnectAttempt = previous.ReconnectAttempt
	current.ReconnectTotal = previous.ReconnectTotal
	r.jobs[current.ID] = current
	r.publishLocked()
	archive := r.archive
	r.mu.Unlock()
	if archive != nil {
		r.reportJobArchiveError("update", archive.update(current))
	}
	return true
}

// SetJobRunningIfStarting provides the fallback lifecycle edge for harnesses
// that do not emit structured start events. The check and transition share one
// lock, so a concurrent reconnect signal cannot be overwritten.
func (r *Runtime) SetJobRunningIfStarting(id int) bool {
	r.mu.Lock()
	job, ok := r.jobs[id]
	if !ok || job.Execution != JobStarting {
		r.mu.Unlock()
		return false
	}
	now := time.Now().UTC()
	job.Execution = JobRunning
	job.Health = JobHealthHealthy
	job.LastActivityAt = now
	job.StateChangedAt = now
	r.jobs[id] = job
	r.publishLocked()
	archive := r.archive
	r.mu.Unlock()
	if archive != nil {
		r.reportJobArchiveError("update", archive.update(job))
	}
	return true
}

func normalizeExecutionState(state string) JobExecutionState {
	switch JobExecutionState(state) {
	case JobStarting, JobRunning, JobReconnecting, JobRecovering, JobAwaitingTransition,
		JobCancelling, JobFinishing, JobStalled, JobDegraded, JobError, JobAudit:
		return JobExecutionState(state)
	case "":
		return ""
	default:
		return JobDegraded
	}
}

// healthFromExecution deliberately uses only structured lifecycle evidence.
// Elapsed time and text-delta silence are not sufficient evidence of a stall:
// a provider may be performing a legitimate long-running tool or CPU operation.
func healthFromExecution(state JobExecutionState) JobHealthState {
	switch state {
	case JobStalled:
		return JobHealthStalled
	case JobReconnecting, JobRecovering, JobDegraded, JobError:
		return JobHealthDegraded
	default:
		return JobHealthHealthy
	}
}

func executionStateIsTerminal(state JobExecutionState) bool {
	return state == JobAwaitingTransition || state == JobCancelling || state == JobFinishing || state == JobError
}

func (r *Runtime) UpdateJobFromLease(id int, state, phase, detail string, heartbeat time.Time, recoveryCount int) bool {
	r.mu.Lock()
	job, ok := r.jobs[id]
	if !ok {
		r.mu.Unlock()
		return false
	}
	heartbeat = heartbeat.UTC()
	if !heartbeat.IsZero() && !job.LeaseHeartbeatAt.IsZero() && heartbeat.Before(job.LeaseHeartbeatAt) {
		r.mu.Unlock()
		return true
	}
	execution := executionFromLeaseState(state)
	if job.Kind == "heartbeat" && state == "working" {
		execution = JobAudit
	}
	staleExecution := !heartbeat.IsZero() && heartbeat.Before(job.StateChangedAt) && job.Execution != JobStarting
	// A processing lease confirms ownership, but only the provider can confirm
	// that a reported reconnect or explicit watchdog stall has cleared.
	if staleExecution || execution == JobRunning && (job.Execution == JobReconnecting || job.Execution == JobStalled) {
		execution = job.Execution
	}
	resumeFromTransition := job.Execution == JobAwaitingTransition &&
		(execution == JobRunning || execution == JobRecovering) && !staleExecution
	if executionStateIsTerminal(job.Execution) && !executionStateIsTerminal(execution) && !resumeFromTransition {
		execution = job.Execution
	}
	cleanState := boundPlainJobText(state, maxJobStatusDetail)
	cleanPhase := boundPlainJobText(phase, maxJobStatusDetail)
	cleanDetail := boundJobStatusDetail(detail)
	if staleExecution {
		cleanDetail = job.StatusDetail
	}
	count := max(0, recoveryCount)
	material := job.Execution != execution || job.StatusDetail != cleanDetail || job.LeaseState != cleanState ||
		job.LeasePhase != cleanPhase || job.RecoveryCount != count
	job.Execution = execution
	job.Health = healthFromExecution(execution)
	job.StatusDetail = cleanDetail
	job.LeaseState = cleanState
	job.LeasePhase = cleanPhase
	job.RecoveryCount = count
	if !heartbeat.IsZero() {
		if heartbeat.After(job.LeaseHeartbeatAt) {
			job.LeaseHeartbeatAt = heartbeat
		}
		if heartbeat.After(job.LastActivityAt) {
			job.LastActivityAt = heartbeat
		}
	}
	if material && !heartbeat.IsZero() && heartbeat.After(job.StateChangedAt) {
		job.StateChangedAt = heartbeat.UTC()
	}
	r.jobs[id] = job
	if material {
		r.publishLocked()
	}
	archive := r.archive
	r.mu.Unlock()
	if archive != nil {
		r.reportJobArchiveError("update", archive.update(job))
	}
	return true
}

func executionFromLeaseState(state string) JobExecutionState {
	switch state {
	case "claiming":
		return JobStarting
	case "processing", "working", "acting":
		return JobRunning
	case "recovering":
		return JobRecovering
	case "awaiting_transition":
		return JobAwaitingTransition
	case "hook_cancelled":
		return JobCancelling
	case "error":
		return JobError
	case "audit":
		return JobAudit
	default:
		return JobDegraded
	}
}

func boundPlainJobText(value string, limit int) string {
	value = strings.Join(strings.Fields(sanitizeLogText(value)), " ")
	if runes := []rune(value); len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return value
}

var absoluteJobPathPattern = regexp.MustCompile(`(^|[\s("'=:\[])(?:[A-Za-z]:[\\/]|/|\\\\(?:\?\\)?)[^\s"'<>]*`)

func boundJobStatusDetail(value string) string {
	value = boundAndRedactLogText(value)
	value = absoluteJobPathPattern.ReplaceAllString(value, "${1}[PATH]")
	return boundPlainJobText(value, maxJobStatusDetail)
}

func (r *Runtime) EndJob(id int) {
	r.mu.Lock()
	job, ok := r.jobs[id]
	if !ok {
		r.mu.Unlock()
		return
	}
	delete(r.jobs, id)
	if r.byNumber[job.Number] == id {
		delete(r.byNumber, job.Number)
	}
	delete(r.bySession, job.SessionKey)
	delete(r.jobCancellations, job.StableID)
	r.publishLocked()
	archive := r.archive
	r.mu.Unlock()
	if archive != nil {
		r.reportJobArchiveError("finish", archive.finish(job, r.Now().UTC()))
	}
	r.LogEvent("info", "jobs", "job_finished", fmt.Sprintf("job_id=%d channel=%s kind=%s duration=%s", job.Number, logField(job.Channel, "unknown"), logField(job.Kind, "chat"), time.Since(job.StartedAt).Round(time.Millisecond)))
}

// RecordJobEvent captures the provider-neutral terminal stream without
// interpreting its text or using it for any workflow decision.
func (r *Runtime) RecordJobEvent(id int, event core.Event) {
	r.mu.Lock()
	job, ok := r.jobs[id]
	archive := r.archive
	r.mu.Unlock()
	if ok && archive != nil {
		r.reportJobArchiveError("event", archive.event(job.StableID, r.Now().UTC(), event))
	}
}

func (r *Runtime) reportJobArchiveError(operation string, err error) {
	if err != nil {
		r.LogEvent("error", "job_archive", operation+"_failed", err.Error())
	}
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

func (r *Runtime) JobByNumber(number int) (Job, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.byNumber[number]
	if !ok {
		return Job{}, false
	}
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

// NumericJobs returns only jobs that are currently addressable through their
// public number. An older recovered generation may remain active for archive
// continuity after wraparound, but it must not appear beside (or later replace)
// the newer generation selected by that number.
func (r *Runtime) NumericJobs() []Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	jobs := make([]Job, 0, len(r.byNumber))
	for _, id := range r.byNumber {
		if job, ok := r.jobs[id]; ok {
			jobs = append(jobs, job)
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	return jobs
}

func newerJobGeneration(candidate, current Job) bool {
	if candidate.Generation != current.Generation {
		return candidate.Generation > current.Generation
	}
	return candidate.ID > current.ID
}

func (r *Runtime) RecentArchivedJobs(limit int) ([]JobArchiveSummary, error) {
	r.mu.Lock()
	archive := r.archive
	r.mu.Unlock()
	if archive == nil {
		return nil, nil
	}
	return archive.recent(limit)
}

func (r *Runtime) ArchivedJob(ref any) (JobArchiveSummary, string, error) {
	r.mu.Lock()
	archive := r.archive
	r.mu.Unlock()
	if archive == nil {
		return JobArchiveSummary{}, "", os.ErrNotExist
	}
	return archive.get(ref)
}

func (r *Runtime) CleanupArchivedJobs(cutoff time.Time) (int, int64, int, int) {
	r.mu.Lock()
	archive := r.archive
	live := make(map[string]bool, len(r.jobs))
	for _, job := range r.jobs {
		live[job.StableID] = true
	}
	r.mu.Unlock()
	if archive == nil {
		return 0, 0, 0, 0
	}
	return archive.cleanup(cutoff.UTC(), live)
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

// ClearLogsResult removes completed and partially captured runtime output,
// publishes the new count to status consumers, and reports retention failures.
func (r *Runtime) ClearLogsResult() (int, error) {
	r.writerMu.Lock()
	defer r.writerMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	count := len(r.logs)
	r.logs = nil
	r.logStart = 0
	r.partial = nil
	r.partials = nil
	var persistErr error
	if r.persist != nil {
		persistErr = r.persist.clear()
		if persistErr != nil {
			r.appendPersistenceFailureLocked("clear_failed", persistErr)
		}
	}
	r.publishLocked()
	return count, persistErr
}

// Close marks a clean boundary and flushes the current session file.
func (r *Runtime) Close() {
	r.closeOnce.Do(func() {
		r.writerMu.Lock()
		partial := append([]byte(nil), r.partial...)
		partials := r.partials
		r.partial = nil
		r.partials = nil
		r.writerMu.Unlock()
		if len(partial) > 0 {
			r.logEvent("info", "process", "stderr_partial", string(partial), true)
		}
		for component, data := range partials {
			if len(data) > 0 {
				r.logEvent("info", component, "output_partial", string(data), true)
			}
		}
		r.logEvent("info", "runtime", "session_end", "Spynel runtime session stopped", true)
		if r.persist != nil {
			if err := r.persist.close(); err != nil {
				r.mu.Lock()
				r.appendPersistenceFailureLocked("close_failed", err)
				r.publishLocked()
				r.mu.Unlock()
			}
			if r.persistDone != nil {
				select {
				case <-r.persistDone:
				case <-time.After(runtimeLogWait):
					r.mu.Lock()
					r.appendPersistenceFailureLocked("close_failed", errors.New("runtime log failure reporting shutdown timed out"))
					r.publishLocked()
					r.mu.Unlock()
				}
			}
		}
	})
}

// RecoverPanic records evidence at an owned goroutine boundary and then
// re-panics so logging never changes the program's crash semantics.
func (r *Runtime) RecoverPanic(component, event string) {
	if value := recover(); value != nil {
		r.LogEvent("fatal", component, event, fmt.Sprintf("panic: %v\n%s", value, debug.Stack()))
		panic(value)
	}
}

func (r *Runtime) Status() core.RuntimeStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.statusLocked()
}

func (r *Runtime) Updates() <-chan core.RuntimeStatus { return r.updates }

func (r *Runtime) publishLocked() {
	status := r.statusLocked()
	select {
	case <-r.updates:
	default:
	}
	r.updates <- status
}

func (r *Runtime) statusLocked() core.RuntimeStatus {
	background := 0
	for _, job := range r.jobs {
		if job.Channel == "orchestrator" && executionStateIsLive(job.Execution) {
			background++
		}
	}
	return core.RuntimeStatus{Logs: len(r.logs), Jobs: len(r.jobs), LiveBackgroundJobs: background}
}

// executionStateIsLive deliberately follows the structured process-local job
// registry. Durable queue entries never enter this registry, and terminal,
// cancelling, explicitly stalled, or failed executions do not keep activity
// indicators alive while their final cleanup remains observable.
func executionStateIsLive(state JobExecutionState) bool {
	switch state {
	case JobStarting, JobRunning, JobReconnecting, JobRecovering, JobDegraded, JobAudit:
		return true
	default:
		return false
	}
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
		lines = append(lines, "", "Use `/log page <number>` or `/log page <start>-<end>`; page 1 is newest and ranges stop at the oldest available page.")
	}
	return strings.Join(lines, "\n")
}

func formatLogSearch(entries []LogEntry, query string) string {
	queryLower := strings.ToLower(query)
	matches := make([]LogEntry, 0)
	for _, entry := range entries {
		searchable := strings.Join([]string{entry.Level, entry.Component, entry.Event, entry.Instance, sanitizeLogText(entry.Text)}, " ")
		if strings.Contains(strings.ToLower(searchable), queryLower) {
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
	metadata := strings.Trim(strings.Join([]string{entry.Level, entry.Component, entry.Event, entry.Instance}, "/"), "/")
	if metadata == "" {
		metadata = "runtime"
	}
	lines := []string{fmt.Sprintf("- `%s` `%s` %s", entry.At.UTC().Format(time.RFC3339), metadata, textLines[0])}
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

// decodeLogRunes preserves valid UTF-8 while recovering raw single-byte C1
// controls so the terminal parser can remove them instead of rendering a
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
	return formatJobsAt(jobs, time.Now())
}

func formatJobsAt(jobs []Job, now time.Time) string {
	if len(jobs) == 0 {
		return "# Jobs\n\nNo agent jobs are running."
	}
	lines := []string{"# Jobs", ""}
	for _, job := range jobs {
		description := "conversation"
		origin := job.Channel
		if job.DurableFile != "" {
			description = filepath.Base(job.DurableFile)
		} else if job.Kind != "" && job.Kind != "conversation" {
			description = job.Description
		}
		started := job.StartedAt
		if job.Durable && !job.FirstAssignedAt.IsZero() && !job.FirstAssignedAt.After(now) {
			started = job.FirstAssignedAt
		}
		counters := fmt.Sprintf("%d▶", max(1, job.ProviderIterations))
		if job.ImplementationAttempts > 0 {
			counters += fmt.Sprintf(" %d↻", job.ImplementationAttempts)
		}
		lines = append(lines,
			fmt.Sprintf("- **Job %d** %s  ", publicJobNumber(job), safeJobText(description, maxJobDescription)),
			fmt.Sprintf("  %s %s · %s · %s", shortDuration(now.Sub(started)), counters, formatExecutionStatus(job), safeJobText(origin, maxJobMetadataRunes)),
		)
	}
	lines = append(lines, "", "Use `/job info <number>` to inspect a job.", "Use `/job kill <number>` to stop a job.")
	return strings.Join(lines, "\n")
}

func publicJobNumber(job Job) int {
	if job.Number > 0 {
		return job.Number
	}
	return job.ID
}

func formatExecutionStatus(job Job) string {
	label := strings.ReplaceAll(string(job.Execution), "_", " ")
	if label == "" {
		label = string(JobStarting)
	}
	if job.Execution == JobAudit {
		label = "audit running"
	}
	if job.Execution == JobReconnecting && job.ReconnectAttempt > 0 {
		label += fmt.Sprintf(" %d", job.ReconnectAttempt)
		if job.ReconnectTotal > 0 {
			label += fmt.Sprintf("/%d", job.ReconnectTotal)
		}
	}
	return label
}

func formatJobHealth(job Job) string {
	health := job.Health
	if health == "" {
		health = healthFromExecution(job.Execution)
	}
	return string(health)
}

func shortDuration(duration time.Duration) string {
	duration = max(0, duration).Round(time.Second)
	if duration < time.Second {
		return "0s"
	}
	seconds := int64(duration / time.Second)
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	minutes := seconds / 60
	if minutes < 60 {
		return fmt.Sprintf("%dm", minutes)
	}
	hours := minutes / 60
	minutes %= 60
	if hours < 24 {
		return fmt.Sprintf("%dh%02dm", hours, minutes)
	}
	days := hours / 24
	hours %= 24
	return fmt.Sprintf("%dd%dh", days, hours)
}
