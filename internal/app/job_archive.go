package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/fsx"
)

const (
	maxJobArchiveBytes      = 512 << 10
	maxJobArchiveEventBytes = 64 << 10
	jobArchiveRecentLimit   = 20
	jobArchiveSnapshotEvery = 100 * time.Millisecond
)

type archivedJob struct {
	StableID              string
	LiveID                int
	Generation            uint64
	Title                 string
	Kind                  string
	Origin                string
	Provider              string
	WorkID                string
	ParentID              string
	StartedAt             time.Time
	EndedAt               time.Time
	State                 string
	Phase                 string
	ProviderIterations    int
	ImplementationAttempt int
	PhaseAttempt          int
	RecoveryCount         int
	Output                strings.Builder
	Truncated             bool
	Sequence              int
	LastWriteAt           time.Time
	path                  string
	owner                 *os.File
}

var stableJobFallback atomic.Uint64

type JobArchive struct {
	mu        sync.Mutex
	directory string
	active    map[string]*archivedJob
	claimed   map[string]*os.File
	boundary  error
}

type jobCounter struct {
	Number     int    `json:"number"`
	Generation uint64 `json:"generation"`
}

func (a *JobArchive) allocate(now time.Time, workID, kind, phase string, phaseAttempt int) (int, uint64, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.boundary != nil {
		return 0, 0, "", a.boundary
	}
	if err := ensureJobArchiveDirectory(a.directory); err != nil {
		return 0, 0, "", err
	}
	lockPath := filepath.Join(a.directory, ".counter.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return 0, 0, "", err
	}
	defer lock.Close()
	if err := lockJobCounter(lock); err != nil {
		return 0, 0, "", err
	}
	defer unlockJobCounter(lock)

	if workID != "" && phase != "" && phaseAttempt > 0 {
		if item, ok := a.newestDispatchLocked(workID, kind, phase, phaseAttempt); ok {
			claimed, claimErr := a.claimOwnershipLocked(item.ID)
			if claimErr != nil {
				return 0, 0, "", claimErr
			}
			if claimed {
				return item.Number, item.Generation, item.ID, nil
			}
		}
	}
	counter, err := a.readCounterLocked()
	if err != nil {
		return 0, 0, "", err
	}
	counter.Number = counter.Number%maxJobNumber + 1
	counter.Generation++
	data, err := json.Marshal(counter)
	if err != nil {
		return 0, 0, "", err
	}
	stableID := newStableJobID(now)
	claimed, err := a.claimOwnershipLocked(stableID)
	if err != nil {
		return 0, 0, "", err
	}
	if !claimed {
		return 0, 0, "", errors.New("could not claim a unique job archive")
	}
	if err := fsx.AtomicWriteFile(filepath.Join(a.directory, ".counter.json"), append(data, '\n'), 0o600); err != nil {
		a.releaseClaimLocked(stableID)
		return 0, 0, "", err
	}
	return counter.Number, counter.Generation, stableID, nil
}

func (a *JobArchive) readCounterLocked() (jobCounter, error) {
	path := filepath.Join(a.directory, ".counter.json")
	data, err := os.ReadFile(path)
	if err == nil {
		var counter jobCounter
		if json.Unmarshal(data, &counter) != nil || counter.Number < 0 || counter.Number > maxJobNumber {
			return jobCounter{}, errors.New("invalid job counter")
		}
		return counter, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return jobCounter{}, err
	}
	items, err := a.scanLocked()
	if err != nil {
		return jobCounter{}, err
	}
	var counter jobCounter
	if len(items) > 0 {
		counter.Number = items[0].Number
		counter.Generation = items[0].Generation
		if counter.Generation == 0 {
			counter.Generation = uint64(len(items))
		}
	}
	return counter, nil
}

func (a *JobArchive) newestDispatchLocked(workID, kind, phase string, phaseAttempt int) (JobArchiveSummary, bool) {
	items, err := a.scanLocked()
	if err != nil {
		return JobArchiveSummary{}, false
	}
	for _, item := range items {
		if item.WorkID == workID && item.Kind == kind && item.Phase == phase && item.PhaseAttempt == phaseAttempt {
			return item, true
		}
	}
	return JobArchiveSummary{}, false
}

func (a *JobArchive) claimOwnershipLocked(stableID string) (bool, error) {
	if a.claimed[stableID] != nil || a.active[stableID] != nil {
		return false, nil
	}
	path := filepath.Join(a.directory, stableID+".owner.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, err
	}
	pathInfo, pathErr := os.Lstat(path)
	fileInfo, fileErr := file.Stat()
	if pathErr != nil || fileErr != nil || !pathInfo.Mode().IsRegular() || pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(pathInfo, fileInfo) {
		file.Close()
		return false, errors.New("job archive owner lock must be a regular file")
	}
	locked, err := tryLockJobOwner(file)
	if err != nil || !locked {
		file.Close()
		return false, err
	}
	a.claimed[stableID] = file
	return true, nil
}

func (a *JobArchive) releaseClaimLocked(stableID string) {
	file := a.claimed[stableID]
	delete(a.claimed, stableID)
	if file != nil {
		unlockJobCounter(file)
		_ = file.Close()
	}
}

func newJobArchive(directory string) *JobArchive {
	archive := &JobArchive{directory: filepath.Clean(directory), active: map[string]*archivedJob{}, claimed: map[string]*os.File{}}
	archive.boundary = ensureJobArchiveDirectory(archive.directory)
	return archive
}

func ensureJobArchiveDirectory(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return err
	}
	if filepath.Clean(resolved) != filepath.Clean(absolute) {
		return errors.New("job archive directory must not cross a symbolic link")
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("job archive path must be a real directory")
	}
	return nil
}

func newStableJobID(now time.Time) string {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		// The clock and process-local counter-like nanoseconds retain a usable,
		// restart-safe fallback without exposing a session or conversation key.
		return fmt.Sprintf("j-%s-%x%x", now.UTC().Format("20060102T150405.000000000Z"), os.Getpid(), stableJobFallback.Add(1))
	}
	return "j-" + now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(random)
}

func (a *JobArchive) begin(job Job) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.boundary != nil {
		return job.StableID, a.boundary
	}
	owner := a.claimed[job.StableID]
	if owner == nil {
		return job.StableID, errors.New("job archive ownership was not claimed")
	}
	title := archiveField(job.Description, maxJobDescription)
	if job.Channel != "orchestrator" {
		title = "conversation"
	}
	record := &archivedJob{
		StableID: job.StableID, LiveID: job.Number, Generation: job.Generation, Title: title,
		Kind: archiveField(job.Kind, 80), Origin: archiveOrigin(job), Provider: archiveField(job.Provider, 80),
		WorkID: archiveField(job.WorkID, 160), ParentID: archiveField(job.ParentID, 160), StartedAt: job.StartedAt.UTC(), State: string(job.Execution),
		Phase: archiveField(job.LeasePhase, 80), ProviderIterations: job.ProviderIterations,
		ImplementationAttempt: job.ImplementationAttempts, RecoveryCount: job.RecoveryCount,
		PhaseAttempt: job.PhaseAttempt,
		LastWriteAt:  job.StartedAt.UTC(), path: filepath.Join(a.directory, job.StableID+".md"), owner: owner,
	}
	if record.Kind == "" {
		record.Kind = "conversation"
	}
	if record.State == "" {
		record.State = string(JobStarting)
	}
	if err := ensureJobArchiveDirectory(a.directory); err != nil {
		a.releaseClaimLocked(job.StableID)
		return record.StableID, err
	}
	if existing, output, err := a.readByStableIDLocked(record.StableID); err == nil && existing.WorkID == record.WorkID && existing.Number == record.LiveID && existing.Generation == record.Generation {
		record.StartedAt = existing.StartedAt
		record.Output.WriteString(output)
		record.Sequence = strings.Count(output, "\n## ")
		if err := a.writeLocked(record); err != nil {
			a.releaseClaimLocked(job.StableID)
			return record.StableID, err
		}
		delete(a.claimed, record.StableID)
		a.active[record.StableID] = record
		return record.StableID, nil
	}
	if err := fsx.AtomicCreateFile(record.path, a.renderLocked(record), 0o600); err != nil {
		a.releaseClaimLocked(job.StableID)
		return record.StableID, err
	}
	record.LastWriteAt = time.Now().UTC()
	delete(a.claimed, record.StableID)
	a.active[record.StableID] = record
	return record.StableID, nil
}

func archiveOrigin(job Job) string {
	if job.Channel != "orchestrator" {
		return "chat/" + archiveField(job.Channel, 40)
	}
	switch job.Kind {
	case "heartbeat":
		return "heartbeat"
	case "notification":
		return "notification"
	case "task":
		if strings.Contains(job.LeasePhase, "review") {
			return "task-review"
		}
		return "task-implementation"
	case "goal":
		if strings.Contains(job.LeasePhase, "review") {
			return "goal-review"
		}
		return "goal-planning"
	default:
		return "orchestrator"
	}
}

func (a *JobArchive) update(job Job) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	record := a.active[job.StableID]
	if record == nil {
		return nil
	}
	record.State = string(job.Execution)
	record.Phase = archiveField(job.LeasePhase, 80)
	record.ProviderIterations = job.ProviderIterations
	record.ImplementationAttempt = job.ImplementationAttempts
	record.PhaseAttempt = job.PhaseAttempt
	record.RecoveryCount = job.RecoveryCount
	record.Origin = archiveOrigin(job)
	return a.writeLocked(record)
}

func (a *JobArchive) event(stableID string, at time.Time, event core.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	record := a.active[stableID]
	if record == nil {
		return nil
	}
	kind := archiveField(event.Kind, 40)
	if kind == "" {
		kind = "event"
	}
	text := redactJobOutput(event.Text)
	if event.Execution != nil {
		status := archiveField(event.Execution.State, 80)
		if text == "" {
			text = status
		} else if status != "" {
			text = "execution=" + status + "\n" + text
		}
	}
	if text == "" {
		return nil
	}
	if len(text) > maxJobArchiveEventBytes {
		text = truncateUTF8(text, maxJobArchiveEventBytes) + "\n[EVENT TRUNCATED]"
	}
	record.Sequence++
	entry := fmt.Sprintf("\n## %06d %s %s\n\n%s\n", record.Sequence, at.UTC().Format(time.RFC3339Nano), kind, text)
	remaining := maxJobArchiveBytes - record.Output.Len()
	if remaining <= 0 {
		record.Truncated = true
		return a.writeLocked(record)
	}
	if len(entry) > remaining {
		entry = truncateUTF8(entry, max(0, remaining-len("\n[JOB OUTPUT TRUNCATED]\n"))) + "\n[JOB OUTPUT TRUNCATED]\n"
		record.Truncated = true
	}
	record.Output.WriteString(entry)
	if !event.Done && time.Since(record.LastWriteAt) < jobArchiveSnapshotEvery && !record.Truncated {
		return nil
	}
	return a.writeLocked(record)
}

func redactJobOutput(text string) string {
	// Reuse the central secret and terminal-control boundary. Captured output is
	// inspection-only, but it must not become a second unredacted credential log.
	return redactLogText(text)
}

func truncateUTF8(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) && len(value) > 0 {
		value = value[:len(value)-1]
	}
	return value
}

func (a *JobArchive) finish(job Job, endedAt time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	record := a.active[job.StableID]
	if record == nil {
		return nil
	}
	record.EndedAt = endedAt.UTC()
	record.State = string(job.Execution)
	if record.State == "" || record.State == string(JobStarting) || record.State == string(JobRunning) || record.State == string(JobFinishing) || record.State == string(JobAwaitingTransition) || record.State == string(JobAudit) {
		record.State = "completed"
	}
	err := a.writeLocked(record)
	delete(a.active, job.StableID)
	if record.owner != nil {
		unlockJobCounter(record.owner)
		_ = record.owner.Close()
		record.owner = nil
	}
	return err
}

func (a *JobArchive) close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for stableID, record := range a.active {
		if record.owner != nil {
			unlockJobCounter(record.owner)
			_ = record.owner.Close()
		}
		delete(a.active, stableID)
	}
	for stableID := range a.claimed {
		a.releaseClaimLocked(stableID)
	}
}

func (a *JobArchive) writeLocked(record *archivedJob) error {
	if err := ensureJobArchiveDirectory(a.directory); err != nil {
		return err
	}
	if err := fsx.AtomicWriteFile(record.path, a.renderLocked(record), 0o600); err != nil {
		return err
	}
	record.LastWriteAt = time.Now().UTC()
	return nil
}

func (a *JobArchive) renderLocked(record *archivedJob) []byte {
	var body strings.Builder
	body.WriteString("---\n")
	writeArchiveField(&body, "id", record.StableID)
	writeArchiveField(&body, "number", strconv.Itoa(record.LiveID))
	writeArchiveField(&body, "generation", strconv.FormatUint(record.Generation, 10))
	writeArchiveField(&body, "title", record.Title)
	writeArchiveField(&body, "type", record.Kind)
	writeArchiveField(&body, "origin", record.Origin)
	writeArchiveField(&body, "provider", record.Provider)
	writeArchiveField(&body, "work_id", record.WorkID)
	writeArchiveField(&body, "parent", record.ParentID)
	writeArchiveField(&body, "started_at", record.StartedAt.Format(time.RFC3339Nano))
	if !record.EndedAt.IsZero() {
		writeArchiveField(&body, "ended_at", record.EndedAt.Format(time.RFC3339Nano))
		writeArchiveField(&body, "duration", record.EndedAt.Sub(record.StartedAt).Round(time.Millisecond).String())
	}
	writeArchiveField(&body, "state", record.State)
	writeArchiveField(&body, "phase", record.Phase)
	writeArchiveField(&body, "provider_iterations", strconv.Itoa(record.ProviderIterations))
	writeArchiveField(&body, "implementation_attempt", strconv.Itoa(record.ImplementationAttempt))
	writeArchiveField(&body, "phase_attempt", strconv.Itoa(record.PhaseAttempt))
	writeArchiveField(&body, "recovery_count", strconv.Itoa(record.RecoveryCount))
	writeArchiveField(&body, "truncated", strconv.FormatBool(record.Truncated))
	body.WriteString("---\n\n# Output\n")
	body.WriteString(record.Output.String())
	return []byte(body.String())
}

func writeArchiveField(body *strings.Builder, key, value string) {
	if value == "" {
		return
	}
	body.WriteString(key + ": " + strconv.Quote(value) + "\n")
}

func archiveField(value string, limit int) string {
	value = strings.Join(strings.Fields(sanitizeLogText(value)), " ")
	if runes := []rune(value); len(runes) > limit {
		value = string(runes[:limit-1]) + "…"
	}
	return value
}

type JobArchiveSummary struct {
	ID           string
	Number       int
	Generation   uint64
	Title        string
	Kind         string
	Origin       string
	Provider     string
	WorkID       string
	ParentID     string
	Phase        string
	PhaseAttempt int
	State        string
	StartedAt    time.Time
	EndedAt      time.Time
	Path         string
}

func (a *JobArchive) recent(limit int) ([]JobArchiveSummary, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.boundary != nil {
		return nil, a.boundary
	}
	if err := ensureJobArchiveDirectory(a.directory); err != nil {
		return nil, err
	}
	all := limit < 0
	if limit == 0 || limit > jobArchiveRecentLimit {
		limit = jobArchiveRecentLimit
	}
	items, err := a.scanLocked()
	if err != nil {
		return nil, err
	}
	newest := make([]JobArchiveSummary, 0, len(items))
	seen := map[int]bool{}
	for _, item := range items {
		if !seen[item.Number] {
			seen[item.Number] = true
			newest = append(newest, item)
		}
	}
	items = newest
	if !all && len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (a *JobArchive) get(ref any) (JobArchiveSummary, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.boundary != nil {
		return JobArchiveSummary{}, "", a.boundary
	}
	if err := ensureJobArchiveDirectory(a.directory); err != nil {
		return JobArchiveSummary{}, "", err
	}
	if stableID, ok := ref.(string); ok {
		return a.readByStableIDLocked(stableID)
	}
	number, ok := ref.(int)
	if !ok || number < 1 || number > maxJobNumber {
		return JobArchiveSummary{}, "", os.ErrNotExist
	}
	items, err := a.scanLocked()
	if err != nil {
		return JobArchiveSummary{}, "", err
	}
	for _, item := range items {
		if item.Number == number {
			return a.readByStableIDLocked(item.ID)
		}
	}
	return JobArchiveSummary{}, "", os.ErrNotExist
}

func (a *JobArchive) readByStableIDLocked(ref string) (JobArchiveSummary, string, error) {
	if !validStableJobID(ref) {
		return JobArchiveSummary{}, "", os.ErrNotExist
	}
	path := filepath.Join(a.directory, ref+".md")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		if err != nil {
			return JobArchiveSummary{}, "", err
		}
		return JobArchiveSummary{}, "", os.ErrNotExist
	}
	item, err := readJobArchive(path, a.active[ref] != nil)
	if err != nil {
		return JobArchiveSummary{}, "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return JobArchiveSummary{}, "", err
	}
	marker := []byte("\n# Output\n")
	index := strings.Index(string(data), string(marker))
	if index < 0 {
		return item, "", nil
	}
	return item, string(data[index+len(marker):]), nil
}

func (a *JobArchive) scanLocked() ([]JobArchiveSummary, error) {
	entries, err := os.ReadDir(a.directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	items := make([]JobArchiveSummary, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		ref := strings.TrimSuffix(entry.Name(), ".md")
		item, readErr := readJobArchive(filepath.Join(a.directory, entry.Name()), a.active[ref] != nil)
		if readErr == nil {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Generation != items[j].Generation {
			return items[i].Generation > items[j].Generation
		}
		if !items[i].StartedAt.Equal(items[j].StartedAt) {
			return items[i].StartedAt.After(items[j].StartedAt)
		}
		return items[i].ID > items[j].ID
	})
	return items, nil
}

func validStableJobID(ref string) bool {
	if !strings.HasPrefix(ref, "j-") || len(ref) > 64 {
		return false
	}
	for _, value := range ref {
		if !(value == '-' || value == '.' || value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value == 'T' || value == 'Z' || value == 'j') {
			return false
		}
	}
	return !strings.Contains(ref, "..")
}

func readJobArchive(path string, active bool) (JobArchiveSummary, error) {
	filename := filepath.Base(path)
	if !strings.HasSuffix(filename, ".md") {
		return JobArchiveSummary{}, errors.New("invalid job archive filename")
	}
	expectedID := strings.TrimSuffix(filename, ".md")
	if !validStableJobID(expectedID) {
		return JobArchiveSummary{}, errors.New("invalid job archive filename")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return JobArchiveSummary{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxJobArchiveBytes+16<<10 {
		return JobArchiveSummary{}, errors.New("invalid job archive file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return JobArchiveSummary{}, err
	}
	fields := map[string]string{}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || lines[0] != "---" {
		return JobArchiveSummary{}, errors.New("invalid job archive")
	}
	for _, line := range lines[1:] {
		if line == "---" {
			break
		}
		key, value, found := strings.Cut(line, ": ")
		if !found {
			continue
		}
		if decoded, decodeErr := strconv.Unquote(value); decodeErr == nil {
			fields[key] = decoded
		}
	}
	started, err := time.Parse(time.RFC3339Nano, fields["started_at"])
	if err != nil || fields["id"] != expectedID {
		return JobArchiveSummary{}, errors.New("invalid job archive metadata")
	}
	ended, _ := time.Parse(time.RFC3339Nano, fields["ended_at"])
	number, numberErr := strconv.Atoi(fields["number"])
	if numberErr != nil {
		number, numberErr = strconv.Atoi(fields["live_alias"])
	}
	if numberErr != nil || number < 1 || number > maxJobNumber {
		return JobArchiveSummary{}, errors.New("invalid job archive number")
	}
	generation, _ := strconv.ParseUint(fields["generation"], 10, 64)
	state := fields["state"]
	if ended.IsZero() && !active {
		state = "interrupted"
	}
	phaseAttempt, _ := strconv.Atoi(fields["phase_attempt"])
	return JobArchiveSummary{ID: fields["id"], Number: number, Generation: generation, Title: fields["title"], Kind: fields["type"], Origin: fields["origin"], Provider: fields["provider"], WorkID: fields["work_id"], ParentID: fields["parent"], Phase: fields["phase"], PhaseAttempt: phaseAttempt, State: state, StartedAt: started, EndedAt: ended, Path: path}, nil
}

func (a *JobArchive) cleanup(cutoff time.Time, live map[string]bool) (removed int, bytes int64, protected int, failed int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.boundary != nil || ensureJobArchiveDirectory(a.directory) != nil {
		return 0, 0, 0, 1
	}
	counterLock, err := os.OpenFile(filepath.Join(a.directory, ".counter.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return 0, 0, 0, 1
	}
	defer counterLock.Close()
	if err := lockJobCounter(counterLock); err != nil {
		return 0, 0, 0, 1
	}
	defer unlockJobCounter(counterLock)
	entries, err := os.ReadDir(a.directory)
	if err != nil {
		return 0, 0, 0, 1
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		ref := strings.TrimSuffix(entry.Name(), ".md")
		if !validStableJobID(ref) {
			failed++
			continue
		}
		if live[ref] || a.active[ref] != nil {
			protected++
			continue
		}
		owned, ownerErr := a.claimOwnershipLocked(ref)
		if ownerErr != nil {
			failed++
			continue
		}
		if !owned {
			protected++
			continue
		}
		releaseOwner := func(remove bool) {
			a.releaseClaimLocked(ref)
			if remove {
				_ = os.Remove(filepath.Join(a.directory, ref+".owner.lock"))
			}
		}
		path := filepath.Join(a.directory, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 {
			releaseOwner(false)
			failed++
			continue
		}
		item, readErr := readJobArchive(path, false)
		if readErr != nil {
			info, infoErr := entry.Info()
			if infoErr != nil || !info.ModTime().UTC().Before(cutoff) {
				releaseOwner(false)
				failed++
				continue
			}
			if removeErr := os.Remove(path); removeErr != nil {
				releaseOwner(false)
				failed++
				continue
			}
			releaseOwner(true)
			removed++
			bytes += info.Size()
			continue
		}
		eligibleAt := item.EndedAt
		if eligibleAt.IsZero() {
			eligibleAt = item.StartedAt
		}
		if !eligibleAt.Before(cutoff) {
			releaseOwner(false)
			continue
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			releaseOwner(false)
			failed++
			continue
		}
		if removeErr := os.Remove(path); removeErr != nil {
			releaseOwner(false)
			failed++
			continue
		}
		releaseOwner(true)
		removed++
		bytes += info.Size()
	}
	return
}
