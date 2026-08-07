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

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/extensions"
	"github.com/agent0ai/spynel/internal/fsx"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/shortid"
)

type Lease struct {
	ID                string    `json:"id"`
	ClaimID           string    `json:"claim_id,omitempty"`
	DocumentType      string    `json:"document_type,omitempty"`
	OwnerID           string    `json:"owner_id,omitempty"`
	Route             string    `json:"route"`
	File              string    `json:"file"`
	SourceFile        string    `json:"source_file,omitempty"`
	SessionKey        string    `json:"session_key"`
	ThreadID          string    `json:"thread_id,omitempty"`
	State             string    `json:"state"`
	StartedAt         time.Time `json:"started_at"`
	HeartbeatAt       time.Time `json:"heartbeat_at"`
	RecoveryCount     int       `json:"recovery_count"`
	LastError         string    `json:"last_error,omitempty"`
	Phase             string    `json:"phase,omitempty"`
	ImplementerThread string    `json:"implementer_thread,omitempty"`
}

type Manager struct {
	Config      config.Config
	Harness     harness.Harness
	Hooks       extensions.Runner
	Log         func(string)
	JobStarted  func(sessionKey, description string) int
	JobFinished func(id int)

	mu       sync.Mutex
	scanMu   sync.Mutex
	inflight map[string]bool
	jobs     sync.WaitGroup
	sem      chan struct{}
	Outbox   *Outbox
	ownerID  string
}

func New(cfg config.Config, target harness.Harness, hooks extensions.Runner) *Manager {
	parallel := cfg.Orchestrator.MaxParallel
	if parallel <= 0 {
		parallel = 1
	}
	return &Manager{
		Config: cfg, Harness: target, Hooks: hooks, inflight: map[string]bool{}, sem: make(chan struct{}, parallel),
		Outbox:  &Outbox{Directory: cfg.StatePath("runtime", "outbox")},
		ownerID: fmt.Sprintf("%d-%d-%s", os.Getpid(), time.Now().UTC().UnixNano(), randomSuffix()),
	}
}

func (m *Manager) SetNotificationDelivery(deliver func(context.Context, Origin, string, string) error) {
	m.Outbox.Deliver = deliver
}

func (m *Manager) Run(ctx context.Context) error {
	if !m.Config.Orchestrator.Enabled {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := m.ScanOnce(ctx); err != nil {
		m.log("orchestrator scan: " + err.Error())
	}
	ticker := time.NewTicker(time.Duration(m.Config.Orchestrator.IntervalSec) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := m.ScanOnce(ctx); err != nil {
				m.log("orchestrator scan: " + err.Error())
			}
		}
	}
}

func (m *Manager) ScanOnce(ctx context.Context) error {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	if !m.Config.Orchestrator.Enabled {
		return nil
	}
	if err := os.MkdirAll(m.leaseDirectory(), 0o700); err != nil {
		return err
	}
	if err := m.ensureRouteDirectories(); err != nil {
		return err
	}
	if err := m.migrateLegacyGoals(); err != nil {
		return err
	}
	if err := m.resumeInterruptedClaims(ctx); err != nil {
		return err
	}
	if err := m.reconcileTransitions(ctx); err != nil {
		return err
	}
	if err := m.recoverStale(ctx); err != nil {
		return err
	}
	if err := m.recoverOrphanClaims(ctx); err != nil {
		return err
	}
	if err := m.wakeWaitingDocuments(ctx); err != nil {
		return err
	}
	if err := m.advanceActiveGoals(); err != nil {
		return err
	}
	for _, route := range m.Config.Orchestrator.Routes {
		var err error
		switch route.Name {
		case "tasks":
			err = m.scanPhaseQueue(ctx, route, m.Config.Resolve(route.Source), m.Config.Resolve(route.Working), phaseTaskImplementation)
		case "goals":
			err = m.scanPhaseQueue(ctx, route, m.Config.Resolve(route.Source), m.Config.Resolve(route.Working), phaseGoalPlanning)
		default:
			err = m.scanRoute(ctx, route)
		}
		if err != nil {
			return fmt.Errorf("route %s: %w", route.Name, err)
		}
	}
	for _, route := range m.Config.Orchestrator.Routes {
		base := filepath.Dir(m.Config.Resolve(route.Source))
		var phase string
		switch route.Name {
		case "tasks":
			phase = phaseTaskReview
		case "goals":
			phase = phaseGoalReview
		default:
			continue
		}
		if err := m.scanPhaseQueue(ctx, route, filepath.Join(base, "review"), filepath.Join(base, "reviewing"), phase); err != nil {
			return err
		}
	}
	if err := m.Outbox.Process(ctx); err != nil {
		m.log("notification delivery deferred: " + err.Error())
	}
	return nil
}

func (m *Manager) scanRoute(ctx context.Context, route config.Route) error {
	sourceDir := m.Config.Resolve(route.Source)
	entries, err := os.ReadDir(sourceDir)
	if os.IsNotExist(err) {
		return os.MkdirAll(sourceDir, 0o700)
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".md") || entry.Name() == "AGENTS.md" {
			continue
		}
		source := filepath.Join(sourceDir, entry.Name())
		due, err := DocumentDue(source, time.Now())
		if err != nil {
			m.log(fmt.Sprintf("read %s: %v", source, err))
			continue
		}
		if !due {
			continue
		}
		key := leaseID(route.Name, source)
		if m.isInflight(key) || m.leaseExists(key) {
			continue
		}
		target := filepath.Join(m.Config.Resolve(route.Working), entry.Name())
		document, err := ClaimDocument(source, target, filepath.Base(filepath.Clean(route.Working)), time.Now())
		if err != nil {
			m.log(fmt.Sprintf("claim %s: %v", source, err))
			continue
		}
		documentID, _ := document.FrontMatter["id"].(string)
		if documentID == "" {
			documentID = key
		}
		lease := Lease{
			ID: key, ClaimID: key, DocumentType: strings.TrimSuffix(route.Name, "s"), OwnerID: m.ownerID,
			Route: route.Name, File: target, SessionKey: "orchestrator:" + route.Name + ":" + documentID,
			State: "processing", Phase: "implementation", StartedAt: time.Now().UTC(), HeartbeatAt: time.Now().UTC(),
		}
		if err := m.saveLease(lease); err != nil {
			return err
		}
		if m.Config.Extensions.Enabled {
			output, hookErr := m.Hooks.Run(ctx, "task.claimed", map[string]any{"route": route.Name, "file": target, "id": documentID})
			if hookErr != nil {
				m.recordError(lease, hookErr)
				continue
			}
			if output.Cancel {
				lease.State = "hook_cancelled"
				lease.LastError = output.Message
				_ = m.saveLease(lease)
				continue
			}
		}
		m.dispatch(ctx, route, lease, false)
	}
	return nil
}

func (m *Manager) scanPhaseQueue(ctx context.Context, route config.Route, sourceDir, claimedDir, phase string) error {
	entries, err := os.ReadDir(sourceDir)
	if os.IsNotExist(err) {
		return os.MkdirAll(sourceDir, 0o700)
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".md") || entry.Name() == "AGENTS.md" {
			continue
		}
		source := filepath.Join(sourceDir, entry.Name())
		due, dueErr := DocumentDue(source, time.Now())
		if dueErr != nil {
			m.log("read queued document " + source + ": " + dueErr.Error())
			continue
		}
		if !due {
			continue
		}
		document, readErr := ReadDocument(source)
		if readErr != nil {
			m.log("read queued document " + source + ": " + readErr.Error())
			continue
		}
		documentID := documentID(document)
		if documentID == "" {
			documentID = leaseID(route.Name+":"+phase, source)
		}
		key := leaseID(route.Name+":"+phase, documentID)
		if m.isInflight(key) || m.leaseExists(key) {
			continue
		}
		target := filepath.Join(claimedDir, entry.Name())
		now := time.Now().UTC()
		attemptField := phaseAttemptField(phase)
		attempt := numberValue(document.FrontMatter[attemptField]) + 1
		lease := Lease{
			ID: key, ClaimID: key, DocumentType: strings.TrimSuffix(route.Name, "s"), Route: route.Name,
			OwnerID: m.ownerID,
			File:    target, SourceFile: source, SessionKey: phaseSessionKey(route.Name, documentID, phase, attempt),
			State: "claiming", Phase: phase, StartedAt: now, HeartbeatAt: now,
		}
		if phase == phaseTaskReview {
			lease.ImplementerThread, _ = document.FrontMatter["implementation_thread"].(string)
		}
		if err := m.saveLease(lease); err != nil {
			return err
		}
		claimed, claimErr := ClaimDocument(source, target, phaseClaimedStatus(phase), now)
		if claimErr != nil {
			_ = os.Remove(m.leasePath(key))
			m.log("claim " + source + ": " + claimErr.Error())
			continue
		}
		claimed.FrontMatter[attemptField] = attempt
		if err := WriteDocument(target, claimed); err != nil {
			return err
		}
		lease.State = "processing"
		lease.SourceFile = ""
		if err := m.saveLease(lease); err != nil {
			return err
		}
		if phase == phaseTaskImplementation && m.Config.Extensions.Enabled {
			output, hookErr := m.Hooks.Run(ctx, "task.claimed", map[string]any{"route": route.Name, "phase": phase, "file": target, "id": documentID})
			if hookErr != nil {
				m.recordError(lease, hookErr)
				continue
			}
			if output.Cancel {
				lease.State = "hook_cancelled"
				lease.LastError = output.Message
				_ = m.saveLease(lease)
				continue
			}
		}
		m.dispatch(ctx, route, lease, false)
	}
	return nil
}

func (m *Manager) startExistingClaim(ctx context.Context, route config.Route, path, phase string, recovery, incrementAttempt bool) error {
	document, err := ReadDocument(path)
	if err != nil {
		return err
	}
	id := documentID(document)
	if id == "" {
		id = leaseID(route.Name+":"+phase, path)
	}
	key := leaseID(route.Name+":"+phase, id)
	if m.leaseExists(key) || m.hasLeaseForFile(path) || m.isInflight(key) {
		return nil
	}
	field := phaseAttemptField(phase)
	attempt := numberValue(document.FrontMatter[field])
	if incrementAttempt || attempt == 0 {
		attempt++
		document.FrontMatter[field] = attempt
		document.FrontMatter["status"] = phaseClaimedStatus(phase)
		document.FrontMatter["updated_at"] = time.Now().UTC().Format(time.RFC3339)
		if err := WriteDocument(path, document); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	state := "processing"
	if recovery {
		state = "recovering"
	}
	lease := Lease{
		ID: key, ClaimID: key, DocumentType: strings.TrimSuffix(route.Name, "s"), Route: route.Name,
		OwnerID: m.ownerID,
		File:    path, SessionKey: phaseSessionKey(route.Name, id, phase, attempt), State: state,
		Phase: phase, StartedAt: now, HeartbeatAt: now,
	}
	if phase == phaseTaskReview {
		lease.ImplementerThread, _ = document.FrontMatter["implementation_thread"].(string)
	}
	if err := m.saveLease(lease); err != nil {
		return err
	}
	m.dispatch(ctx, route, lease, recovery)
	return nil
}

func numberValue(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

func (m *Manager) ensureRouteDirectories() error {
	for _, route := range m.Config.Orchestrator.Routes {
		paths := []string{m.Config.Resolve(route.Source), m.Config.Resolve(route.Working)}
		base := filepath.Dir(m.Config.Resolve(route.Source))
		for _, status := range route.AllowedNext {
			paths = append(paths, filepath.Join(base, status))
		}
		for _, path := range paths {
			if err := os.MkdirAll(path, 0o700); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) dispatch(ctx context.Context, route config.Route, lease Lease, recovery bool) {
	m.setInflight(lease.ID, true)
	m.jobs.Add(1)
	go func() {
		defer m.jobs.Done()
		defer m.setInflight(lease.ID, false)
		select {
		case m.sem <- struct{}{}:
			defer func() { <-m.sem }()
		case <-ctx.Done():
			return
		}
		promptPath := route.Prompt
		if recovery {
			promptPath = route.RecoveryPrompt
		} else if lease.Phase == phaseTaskReview || lease.Phase == phaseGoalReview || lease.Phase == "review" {
			promptPath = route.ReviewPrompt
		}
		prompt, err := m.renderPrompt(route, lease.File, promptPath)
		if err != nil {
			m.recordError(lease, err)
			return
		}
		jobID := 0
		if m.JobStarted != nil {
			jobID = m.JobStarted(lease.SessionKey, filepath.Base(lease.File))
		}
		var finishJob sync.Once
		finish := func() {
			finishJob.Do(func() {
				if jobID > 0 && m.JobFinished != nil {
					m.JobFinished(jobID)
				}
			})
		}
		emit := func(event core.Event) {
			// Runtime job bookkeeping is process-local and must not depend on
			// the durable lease still existing. A fast agent can move its task,
			// then a concurrent recovery scan can remove the obsolete lease
			// before the provider emits its final event.
			if event.Done {
				finish()
			}
			current, err := m.loadLease(lease.ID)
			if err != nil {
				return
			}
			current.HeartbeatAt = time.Now().UTC()
			if event.ThreadID != "" {
				current.ThreadID = event.ThreadID
			}
			if event.Kind == core.EventError {
				current.LastError = event.Text
			}
			if event.Done {
				current.State = "awaiting_transition"
			}
			_ = m.saveLease(current)
		}
		threadID, steered, err := m.Harness.Send(ctx, lease.SessionKey, prompt, emit)
		if err != nil {
			finish()
			m.recordError(lease, err)
			return
		}
		current, loadErr := m.loadLease(lease.ID)
		if os.IsNotExist(loadErr) {
			return
		}
		if loadErr == nil {
			lease = current
		}
		lease.ThreadID = threadID
		lease.HeartbeatAt = time.Now().UTC()
		if recovery {
			lease.RecoveryCount++
			if lease.State != "awaiting_transition" {
				lease.State = "recovering"
			}
		}
		_ = m.saveLease(lease)
		verb := "started"
		if steered {
			verb = "steered"
		}
		m.log(fmt.Sprintf("%s %s in thread %s", verb, lease.File, shortid.Display(threadID)))
	}()
}

func (m *Manager) reconcileTransitions(ctx context.Context) error {
	leases, err := m.loadLeases()
	if err != nil {
		return err
	}
	for _, lease := range leases {
		if lease.State == "claiming" {
			continue
		}
		if _, statErr := os.Stat(lease.File); statErr == nil {
			continue
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
		route, ok := m.route(lease.Route)
		if !ok {
			continue
		}
		base := filepath.Dir(m.Config.Resolve(route.Source))
		name := filepath.Base(lease.File)
		status, path := "", ""
		for _, candidate := range route.AllowedNext {
			test := filepath.Join(base, candidate, name)
			if _, e := os.Stat(test); e == nil {
				status, path = candidate, test
				break
			}
		}
		if status == "" {
			_ = os.Remove(m.leasePath(lease.ID))
			continue
		}
		phase := normalizeLeasePhase(route.Name, lease.Phase)
		switch route.Name {
		case "tasks":
			status, path, err = m.reconcileTaskTransition(ctx, route, lease, phase, status, path)
		case "goals":
			status, path, err = m.reconcileGoalTransition(ctx, route, lease, phase, status, path)
		}
		if err != nil {
			return err
		}
		_ = os.Remove(m.leasePath(lease.ID))
		if route.Name == "goals" && phase == phaseGoalReview && status == "planning" {
			if err := m.startExistingClaim(ctx, route, path, phaseGoalPlanning, false, true); err != nil {
				return err
			}
		}
	}
	return nil
}

func normalizeLeasePhase(routeName, phase string) string {
	if phase == "review" {
		if routeName == "goals" {
			return phaseGoalReview
		}
		return phaseTaskReview
	}
	if phase == "implementation" {
		if routeName == "goals" {
			return phaseGoalPlanning
		}
		return phaseTaskImplementation
	}
	return phase
}

func (m *Manager) reconcileTaskTransition(ctx context.Context, route config.Route, lease Lease, phase, status, path string) (string, string, error) {
	base := filepath.Dir(m.Config.Resolve(route.Source))
	name := filepath.Base(path)
	if phase == phaseTaskImplementation {
		if status == "done" || status == "reviewing" {
			var err error
			status, path, err = m.redirectTransition(path, statusPath(base, "review", name), "review", "Implementation cannot bypass independent review; Spynel redirected this task to review.")
			if err != nil {
				return status, path, err
			}
		}
		if !map[string]bool{"todo": true, "review": true, "waiting": true, "failed": true, "cancelled": true}[status] {
			var err error
			status, path, err = m.redirectTransition(path, statusPath(base, "todo", name), "todo", "Invalid task implementation transition; Spynel returned this task to todo.")
			if err != nil {
				return status, path, err
			}
		}
		if status == "review" {
			document, err := ReadDocument(path)
			if err != nil {
				return status, path, err
			}
			document.FrontMatter["implementation_thread"] = lease.ThreadID
			document.FrontMatter["implementation_session"] = lease.SessionKey
			if err := WriteDocument(path, document); err != nil {
				return status, path, err
			}
		}
		if status == "waiting" || status == "failed" || status == "cancelled" {
			if err := m.completeTransition(ctx, route, lease, status, path); err != nil {
				return status, path, err
			}
		}
		return status, path, nil
	}

	if status == "done" && lease.ImplementerThread != "" && lease.ThreadID == lease.ImplementerThread {
		var err error
		status, path, err = m.redirectTransition(path, statusPath(base, "todo", name), "todo", "Independent review rejected automatically because the reviewer reused the implementation harness thread.")
		return status, path, err
	}
	if status != "done" && status != "todo" {
		var err error
		status, path, err = m.redirectTransition(path, statusPath(base, "todo", name), "todo", "Invalid task-review transition; review may only accept into done or return findings to todo.")
		if err != nil {
			return status, path, err
		}
	}
	if status == "done" {
		if err := m.completeTransition(ctx, route, lease, status, path); err != nil {
			return status, path, err
		}
	}
	return status, path, nil
}

func (m *Manager) reconcileGoalTransition(_ context.Context, route config.Route, _ Lease, phase, status, path string) (string, string, error) {
	base := filepath.Dir(m.Config.Resolve(route.Source))
	name := filepath.Base(path)
	document, err := ReadDocument(path)
	if err != nil {
		return status, path, err
	}
	if phase == phaseGoalPlanning {
		if status == "active" {
			if err := m.validateGoalActivation(document); err == nil {
				return status, path, nil
			} else {
				return m.redirectTransition(path, statusPath(base, "proposed", name), "proposed", "Goal activation rejected: "+err.Error())
			}
		}
		if status == "waiting" || status == "abandoned" {
			return status, path, nil
		}
		return m.redirectTransition(path, statusPath(base, "proposed", name), "proposed", "Invalid goal-planning transition; planning must create a valid active round, wait on a precise condition, or record explicit abandonment.")
	}

	if status == "done" {
		if err := goalReviewProvesDone(document); err != nil {
			return m.redirectTransition(path, statusPath(base, "review", name), "review", "Goal completion rejected: "+err.Error())
		}
		return status, path, nil
	}
	if status == "planning" || status == "waiting" || status == "abandoned" {
		return status, path, nil
	}
	return m.redirectTransition(path, statusPath(base, "review", name), "review", "Invalid goal-review transition; review must choose planning, waiting, done, or abandoned.")
}

func (m *Manager) redirectTransition(path, target, status, note string) (string, string, error) {
	document, err := ReadDocument(path)
	if err != nil {
		return status, path, err
	}
	now := time.Now().UTC()
	document.FrontMatter["status"] = status
	document.FrontMatter["updated_at"] = now.Format(time.RFC3339)
	document.Body += "\n- " + now.Format(time.RFC3339) + " — " + note + "\n"
	if err := WriteDocument(path, document); err != nil {
		return status, path, err
	}
	if err := os.Rename(path, target); err != nil {
		return status, path, err
	}
	m.log(note + " " + target)
	return status, target, nil
}

func (m *Manager) completeTransition(ctx context.Context, route config.Route, lease Lease, status, path string) error {
	document, err := ReadDocument(path)
	if err != nil {
		return err
	}
	policy, err := NotificationFromDocument(document)
	if err != nil {
		m.log("invalid notification metadata in " + path + ": " + err.Error())
		return nil
	}
	if m.Config.Extensions.Enabled {
		if _, hookErr := m.Hooks.Run(ctx, "task.completed", map[string]any{"route": route.Name, "file": path, "thread_id": lease.ThreadID, "outcome": status}); hookErr != nil {
			m.log("task.completed hook: " + hookErr.Error())
		}
	}
	if !policy.Enabled || !policy.Outcomes[status] {
		return nil
	}
	id, _ := document.FrontMatter["id"].(string)
	title, _ := document.FrontMatter["title"].(string)
	if id == "" {
		id = lease.ID
	}
	if title == "" {
		title = filepath.Base(path)
	}
	summary := "Task reached " + status + "."
	if status == "done" {
		summary = "Independent review accepted the implementation."
	}
	message := fmt.Sprintf("%s — %s\n%s\nTask ID: %s\nStatus: spynel status; task: %s", title, status, summary, id, path)
	_, err = m.Outbox.Enqueue(id, status, policy.Origin.Channel+"/"+policy.Origin.Conversation, message)
	return err
}

func (m *Manager) resumeInterruptedClaims(ctx context.Context) error {
	leases, err := m.loadLeases()
	if err != nil {
		return err
	}
	for _, lease := range leases {
		if lease.State != "claiming" || m.isInflight(lease.ID) || m.Harness.IsActive(lease.SessionKey) {
			continue
		}
		if _, err := os.Stat(lease.File); os.IsNotExist(err) && lease.SourceFile != "" {
			if _, sourceErr := os.Stat(lease.SourceFile); sourceErr == nil {
				document, claimErr := ClaimDocument(lease.SourceFile, lease.File, phaseClaimedStatus(normalizeLeasePhase(lease.Route, lease.Phase)), time.Now())
				if claimErr != nil {
					return claimErr
				}
				field := phaseAttemptField(normalizeLeasePhase(lease.Route, lease.Phase))
				if numberValue(document.FrontMatter[field]) == 0 {
					document.FrontMatter[field] = 1
					if err := WriteDocument(lease.File, document); err != nil {
						return err
					}
				}
			}
		}
		if _, err := os.Stat(lease.File); err == nil {
			route, ok := m.route(lease.Route)
			if !ok {
				continue
			}
			lease.State = "recovering"
			lease.OwnerID = m.ownerID
			lease.SourceFile = ""
			lease.HeartbeatAt = time.Now().UTC()
			if err := m.saveLease(lease); err != nil {
				return err
			}
			m.dispatch(ctx, route, lease, true)
			continue
		}
		// Neither side of the interrupted claim remains. Let ordinary
		// transition reconciliation locate a moved status file.
		lease.State = "processing"
		if err := m.saveLease(lease); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) recoverOrphanClaims(ctx context.Context) error {
	for _, route := range m.Config.Orchestrator.Routes {
		base := filepath.Dir(m.Config.Resolve(route.Source))
		var phases map[string]string
		switch route.Name {
		case "tasks":
			phases = map[string]string{"working": phaseTaskImplementation, "reviewing": phaseTaskReview}
		case "goals":
			phases = map[string]string{"planning": phaseGoalPlanning, "reviewing": phaseGoalReview}
		default:
			continue
		}
		for status, phase := range phases {
			directory := filepath.Join(base, status)
			entries, err := os.ReadDir(directory)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".md") || entry.Name() == "AGENTS.md" {
					continue
				}
				path := filepath.Join(directory, entry.Name())
				if m.hasLeaseForFile(path) {
					continue
				}
				m.log("recovering claimed document without lease: " + path)
				if err := m.startExistingClaim(ctx, route, path, phase, true, false); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (m *Manager) hasLeaseForFile(path string) bool {
	leases, err := m.loadLeases()
	if err != nil {
		return false
	}
	want := filepath.Clean(path)
	for _, lease := range leases {
		if filepath.Clean(lease.File) == want {
			return true
		}
	}
	return false
}

func (m *Manager) wakeWaitingDocuments(ctx context.Context) error {
	now := time.Now().UTC()
	for _, route := range m.Config.Orchestrator.Routes {
		if route.Name != "tasks" && route.Name != "goals" {
			continue
		}
		base := filepath.Dir(m.Config.Resolve(route.Source))
		directory := filepath.Join(base, "waiting")
		entries, err := os.ReadDir(directory)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".md") || entry.Name() == "AGENTS.md" {
				continue
			}
			path := filepath.Join(directory, entry.Name())
			document, err := ReadDocument(path)
			if err != nil {
				m.log("read waiting document " + path + ": " + err.Error())
				continue
			}
			due, err := scheduledWake(document, now)
			if err != nil {
				m.log("waiting schedule " + path + ": " + err.Error())
				continue
			}
			if !due {
				continue
			}
			targetStatus := "todo"
			if route.Name == "goals" {
				targetStatus = stringField(document, "resume_status")
				if targetStatus != "planning" && targetStatus != "review" {
					targetStatus = "review"
				}
			}
			target := filepath.Join(base, targetStatus, entry.Name())
			if err := moveDocument(path, target, targetStatus, now); err != nil {
				return err
			}
			if route.Name == "goals" && targetStatus == "planning" {
				if err := m.startExistingClaim(ctx, route, target, phaseGoalPlanning, false, true); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (m *Manager) advanceActiveGoals() error {
	route, ok := m.route("goals")
	if !ok {
		return nil
	}
	base := filepath.Dir(m.Config.Resolve(route.Source))
	directory := filepath.Join(base, "active")
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".md") || entry.Name() == "AGENTS.md" {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		document, err := ReadDocument(path)
		if err != nil {
			m.log("read active goal " + path + ": " + err.Error())
			continue
		}
		if err := m.validateGoalActivation(document); err != nil {
			target := filepath.Join(base, "proposed", entry.Name())
			if _, _, moveErr := m.redirectTransition(path, target, "proposed", "Legacy or invalid active goal returned to planning queue: "+err.Error()); moveErr != nil {
				return moveErr
			}
			continue
		}
		tasks, err := m.goalRoundTasks(document)
		if err != nil {
			return err
		}
		settled := len(tasks) > 0
		for _, task := range tasks {
			settled = settled && taskSettledStatuses[task.Status]
		}
		trigger := stringField(document, "review_trigger")
		checkpoint := false
		if trigger == "scheduled" || trigger == "all_round_tasks_settled_or_checkpoint" {
			checkpoint, err = checkpointDue(document, now)
			if err != nil {
				m.log("goal checkpoint " + path + ": " + err.Error())
				continue
			}
		}
		if !settled && !checkpoint {
			continue
		}
		target := filepath.Join(base, "review", entry.Name())
		if err := moveDocument(path, target, "review", now); err != nil {
			return err
		}
		m.log("goal round ready for review: " + target)
	}
	return nil
}

func (m *Manager) validateGoalActivation(document Document) error {
	if err := validSuccessCriteria(document); err != nil {
		return err
	}
	if stringField(document, "review_trigger") == "" {
		return errors.New("review_trigger is required")
	}
	if numberValue(document.FrontMatter["round"]) <= 0 {
		return errors.New("round must be positive before activation")
	}
	tasks, err := m.goalRoundTasks(document)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		return errors.New("the current round has no linked tasks")
	}
	rawIDs, ok := document.FrontMatter["round_task_ids"].([]any)
	if !ok || len(rawIDs) == 0 {
		return errors.New("round_task_ids must declare the complete current-round task batch")
	}
	declared := map[string]bool{}
	for _, raw := range rawIDs {
		id, ok := raw.(string)
		id = strings.TrimSpace(id)
		if !ok || id == "" || declared[id] {
			return errors.New("round_task_ids must contain unique non-empty task IDs")
		}
		declared[id] = true
	}
	linked := map[string]bool{}
	for _, task := range tasks {
		if task.ID == "" {
			return errors.New("every linked task requires an id")
		}
		linked[task.ID] = true
	}
	if len(declared) != len(linked) {
		return errors.New("round_task_ids does not match the linked current-round tasks")
	}
	for id := range declared {
		if !linked[id] {
			return fmt.Errorf("round_task_ids references missing task %q", id)
		}
	}
	return nil
}

func (m *Manager) goalRoundTasks(document Document) ([]linkedTask, error) {
	taskRoute, ok := m.route("tasks")
	if !ok {
		return nil, errors.New("tasks route is required for goals")
	}
	id := documentID(document)
	if id == "" {
		return nil, errors.New("goal id is required")
	}
	return linkedRoundTasks(filepath.Dir(m.Config.Resolve(taskRoute.Source)), id, numberValue(document.FrontMatter["round"]))
}

func (m *Manager) migrateLegacyGoals() error {
	route, ok := m.route("goals")
	if !ok {
		return nil
	}
	base := filepath.Dir(m.Config.Resolve(route.Source))
	legacy := filepath.Join(base, "working")
	entries, err := os.ReadDir(legacy)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".md") || entry.Name() == "AGENTS.md" {
			continue
		}
		source := filepath.Join(legacy, entry.Name())
		target := filepath.Join(base, "planning", entry.Name())
		if _, err := os.Stat(target); err == nil {
			continue
		}
		if err := moveDocument(source, target, "planning", time.Now()); err != nil {
			return err
		}
		leases, _ := m.loadLeases()
		for _, lease := range leases {
			if filepath.Clean(lease.File) != filepath.Clean(source) {
				continue
			}
			lease.File = target
			lease.Phase = phaseGoalPlanning
			lease.DocumentType = "goal"
			if err := m.saveLease(lease); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) recoverStale(ctx context.Context) error {
	leases, err := m.loadLeases()
	if err != nil {
		return err
	}
	now := time.Now()
	for _, lease := range leases {
		if lease.State == "hook_cancelled" {
			continue
		}
		if _, err := os.Stat(lease.File); os.IsNotExist(err) {
			// A missing working path is a durable status transition. Leave the
			// lease for reconcileTransitions so review enforcement, hooks, and
			// notification enqueueing cannot be skipped by a concurrent scan.
			continue
		}
		route, ok := m.route(lease.Route)
		if !ok {
			continue
		}
		foreignOwner := lease.OwnerID != "" && lease.OwnerID != m.ownerID
		if (!foreignOwner && now.Sub(lease.HeartbeatAt) < route.StaleDuration()) || m.isInflight(lease.ID) || m.Harness.IsActive(lease.SessionKey) {
			continue
		}
		lease.OwnerID = m.ownerID
		lease.HeartbeatAt = time.Now().UTC()
		if err := m.saveLease(lease); err != nil {
			return err
		}
		m.dispatch(ctx, route, lease, true)
	}
	return nil
}

func (m *Manager) renderPrompt(route config.Route, file, promptPath string) (string, error) {
	data, err := os.ReadFile(m.Config.Resolve(promptPath))
	if err != nil {
		return "", err
	}
	statuses := make([]string, 0, len(route.AllowedNext))
	base := filepath.Dir(m.Config.Resolve(route.Source))
	for _, status := range route.AllowedNext {
		statuses = append(statuses, fmt.Sprintf("- %s: %s", status, filepath.Join(base, status)))
	}
	replacements := map[string]string{
		"{{FILE}}": file, "{{ROUTE}}": route.Name,
		"{{ALLOWED_NEXT}}":   strings.Join(route.AllowedNext, ", "),
		"{{STATUS_FOLDERS}}": strings.Join(statuses, "\n"),
		"{{STALE_AFTER}}":    route.StaleAfter,
		"{{PHASE}}":          phaseForFile(route.Name, file),
		"{{RELATED_TASKS}}":  m.relatedTasksForGoal(file),
		"{{TASK_SOURCE}}":    m.routeSource("tasks"),
		"{{GOAL_SOURCE}}":    m.routeSource("goals"),
	}
	prompt := string(data)
	for from, to := range replacements {
		prompt = strings.ReplaceAll(prompt, from, to)
	}
	return prompt, nil
}

func (m *Manager) routeSource(name string) string {
	if route, ok := m.route(name); ok {
		return m.Config.Resolve(route.Source)
	}
	return "(route not configured)"
}

func phaseForFile(routeName, file string) string {
	status := filepath.Base(filepath.Dir(file))
	switch routeName + ":" + status {
	case "tasks:working":
		return phaseTaskImplementation
	case "tasks:reviewing":
		return phaseTaskReview
	case "goals:planning":
		return phaseGoalPlanning
	case "goals:reviewing":
		return phaseGoalReview
	default:
		return status
	}
}

func (m *Manager) relatedTasksForGoal(file string) string {
	if filepath.Base(filepath.Dir(filepath.Dir(file))) != "goals" {
		return "- Not applicable."
	}
	document, err := ReadDocument(file)
	if err != nil {
		return "- Unable to read linked tasks: " + err.Error()
	}
	tasks, err := m.goalRoundTasks(document)
	if err != nil {
		return "- Unable to resolve linked tasks: " + err.Error()
	}
	if len(tasks) == 0 {
		return "- No tasks are linked to the current round."
	}
	lines := make([]string, 0, len(tasks))
	for _, task := range tasks {
		lines = append(lines, "- ["+task.Status+"] "+task.Path)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func (m *Manager) Wait() { m.jobs.Wait() }

func (m *Manager) WaitForIdle(ctx context.Context) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		leases, err := m.loadLeases()
		if err != nil {
			return err
		}
		busy := false
		for _, lease := range leases {
			if m.Harness.IsActive(lease.SessionKey) || m.isInflight(lease.ID) {
				busy = true
				break
			}
		}
		if !busy {
			m.jobs.Wait()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (m *Manager) Status() (int, int, error) {
	leases, err := m.loadLeases()
	if err != nil {
		return 0, 0, err
	}
	m.mu.Lock()
	active := len(m.inflight)
	m.mu.Unlock()
	return len(leases), active, nil
}

func (m *Manager) route(name string) (config.Route, bool) {
	for _, route := range m.Config.Orchestrator.Routes {
		if route.Name == name {
			return route, true
		}
	}
	return config.Route{}, false
}

func (m *Manager) leaseDirectory() string     { return m.Config.StatePath("runtime", "leases") }
func (m *Manager) leasePath(id string) string { return filepath.Join(m.leaseDirectory(), id+".json") }

func (m *Manager) leaseExists(id string) bool {
	_, err := os.Stat(m.leasePath(id))
	return err == nil
}

func (m *Manager) saveLease(lease Lease) error {
	if err := os.MkdirAll(m.leaseDirectory(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(lease, "", "  ")
	if err != nil {
		return err
	}
	return fsx.AtomicWriteFile(m.leasePath(lease.ID), append(data, '\n'), 0o600)
}

func (m *Manager) loadLease(id string) (Lease, error) {
	data, err := os.ReadFile(m.leasePath(id))
	if err != nil {
		return Lease{}, err
	}
	var lease Lease
	err = json.Unmarshal(data, &lease)
	return lease, err
}

func (m *Manager) loadLeases() ([]Lease, error) {
	entries, err := os.ReadDir(m.leaseDirectory())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var leases []Lease
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		lease, err := m.loadLease(id)
		if err != nil {
			m.log("invalid lease " + entry.Name() + ": " + err.Error())
			continue
		}
		leases = append(leases, lease)
	}
	sort.Slice(leases, func(i, j int) bool { return leases[i].StartedAt.Before(leases[j].StartedAt) })
	return leases, nil
}

func (m *Manager) recordError(lease Lease, err error) {
	lease.LastError = err.Error()
	lease.State = "error"
	lease.HeartbeatAt = time.Now().UTC()
	_ = m.saveLease(lease)
	m.log(fmt.Sprintf("dispatch %s: %v", lease.File, err))
}

func (m *Manager) isInflight(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inflight[key]
}

func (m *Manager) setInflight(key string, value bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if value {
		m.inflight[key] = true
	} else {
		delete(m.inflight, key)
	}
}

func (m *Manager) log(message string) {
	if m.Log != nil {
		m.Log(message)
	}
}

func leaseID(route, path string) string {
	hash := sha256.Sum256([]byte(route + "\x00" + filepath.Clean(path)))
	return hex.EncodeToString(hash[:12])
}
