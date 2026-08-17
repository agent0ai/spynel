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
	"sync/atomic"
	"time"

	"github.com/agent0ai/spynel/internal/agentdocs"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/extensions"
	"github.com/agent0ai/spynel/internal/fsx"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/instructions"
	"github.com/agent0ai/spynel/internal/shortid"
	"gopkg.in/yaml.v3"
)

type Lease struct {
	ID                     string          `json:"id"`
	ClaimID                string          `json:"claim_id,omitempty"`
	DocumentType           string          `json:"document_type,omitempty"`
	OwnerID                string          `json:"owner_id,omitempty"`
	Route                  string          `json:"route"`
	RouteSnapshot          *config.Route   `json:"route_snapshot,omitempty"`
	RoutesSnapshot         []config.Route  `json:"routes_snapshot,omitempty"`
	File                   string          `json:"file"`
	SourceFile             string          `json:"source_file,omitempty"`
	SessionKey             string          `json:"session_key"`
	ThreadID               string          `json:"thread_id,omitempty"`
	State                  string          `json:"state"`
	StartedAt              time.Time       `json:"started_at"`
	HeartbeatAt            time.Time       `json:"heartbeat_at"`
	RecoveryCount          int             `json:"recovery_count"`
	LastError              string          `json:"last_error,omitempty"`
	Phase                  string          `json:"phase,omitempty"`
	ClaimAttempt           int             `json:"claim_attempt,omitempty"`
	ImplementerThread      string          `json:"implementer_thread,omitempty"`
	TerminalHooksCompleted map[string]bool `json:"terminal_hooks_completed,omitempty"`
}

type ScheduledCheckpoint struct {
	ID     string    `json:"id"`
	Title  string    `json:"title"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason,omitempty"`
}

type Manager struct {
	Config                   config.Config
	Harness                  harness.Harness
	Hooks                    extensions.Runner
	Log                      func(string)
	JobStarted               func(lease Lease, description string, firstAssignedAt time.Time, providerIterations, implementationAttempts int) (int, error)
	JobUpdated               func(id int, lease Lease)
	JobTimingUpdated         func(id int, firstAssignedAt time.Time, providerIterations int)
	JobExecutionUpdated      func(id int, status core.ExecutionStatus)
	JobEvent                 func(id int, event core.Event)
	JobFinished              func(id int)
	Cleanup                  func(context.Context, int) (string, error)
	notificationAgentTimeout time.Duration
	runtimeConfigMu          sync.RWMutex
	runtimeConfig            config.Config

	mu                          sync.Mutex
	scanMu                      sync.Mutex
	inflight                    map[string]bool
	runtimeJobs                 map[string]int
	controlCancelled            map[string]int
	jobs                        sync.WaitGroup
	capacityMu                  sync.Mutex
	capacityActive              int
	capacityLimit               int
	capacityChanged             chan struct{}
	Outbox                      *Outbox
	ownerID                     string
	scanNow                     chan struct{}
	scanTimerMu                 sync.Mutex
	scanTimer                   *time.Timer
	scanTimerGeneration         uint64
	scanNext                    time.Time
	scanTimerChanged            chan struct{}
	heartbeatNow                func() time.Time
	heartbeatTicks              <-chan time.Time
	heartbeatTimeout            time.Duration
	heartbeatCommit             sync.Mutex
	heartbeatTerm               uint64
	heartbeatProviderActive     atomic.Bool
	heartbeatProviderMu         sync.Mutex
	heartbeatProviderDone       chan struct{}
	heartbeatProviderReleasedAt time.Time
	heartbeatRunningTerm        atomic.Uint64
	heartbeatSchedulerActive    atomic.Bool
	primaryOwned                atomic.Bool
	orchestratorEnabled         atomic.Bool
	heartbeatMinutes            atomic.Int64
	heartbeatConfigChanged      chan struct{}
	heartbeatManual             chan heartbeatManualRequest
	heartbeatConfigAcceptedAt   atomic.Int64
	heartbeatConfigGeneration   atomic.Uint64
	heartbeatAppliedGeneration  atomic.Uint64
	harnessPolicy               atomic.Value
	heartbeatTimerMu            sync.Mutex
	heartbeatTimer              *time.Timer
	heartbeatStatusMu           sync.RWMutex
	heartbeatOwned              bool
	heartbeatOwnedTerm          uint64
	heartbeatNext               time.Time
	cleanupDays                 atomic.Int64
	cleanupTicks                <-chan time.Time
	claimDocument               func(source, target, status, attemptField string, now time.Time) (Document, error)
}

// SetPrimaryOwned records whether this manager belongs to the elected
// workspace owner. The scheduler publishes its deadline separately, allowing
// status to distinguish a primary that is still starting from a secondary.
func (m *Manager) SetPrimaryOwned(owned bool) {
	m.primaryOwned.Store(owned)
}

// ApplyRuntimeConfig publishes one accepted runtime snapshot. Existing jobs
// retain their immutable admission snapshot; every subsequent scan, claim,
// notification decision, and hook dispatch observes the newest snapshot.
func (m *Manager) ApplyRuntimeConfig(cfg config.Config) {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	m.runtimeConfigMu.Lock()
	previous := m.runtimeConfig
	// Extension controls are the sole intentional restart boundary. Keep the
	// process-start snapshot even when a transaction changes them alongside
	// live settings.
	cfg.Extensions = previous.Extensions
	m.runtimeConfig = cfg
	m.runtimeConfigMu.Unlock()
	m.harnessPolicy.Store(cfg.Harness)
	m.cleanupDays.Store(int64(cfg.Workspace.CleanupRetentionDays))
	wasEnabled := m.orchestratorEnabled.Load()
	heartbeatChanged := previous.Orchestrator.Enabled != cfg.Orchestrator.Enabled || previous.Orchestrator.SemanticHeartbeatMinutes != cfg.Orchestrator.SemanticHeartbeatMinutes
	if heartbeatChanged {
		acceptedAt := m.semanticHeartbeatNow()
		m.heartbeatCommit.Lock()
		m.orchestratorEnabled.Store(cfg.Orchestrator.Enabled)
		m.heartbeatMinutes.Store(int64(cfg.Orchestrator.SemanticHeartbeatMinutes))
		m.heartbeatConfigAcceptedAt.Store(acceptedAt.UnixNano())
		m.heartbeatConfigGeneration.Add(1)
		// Fence result commit, deadline publication, timer ownership, and tick
		// dispatch synchronously with the accepted configuration change.
		m.heartbeatTerm++
		m.stopSemanticHeartbeatTimer(nil)
		m.setSemanticHeartbeatSchedule(false, time.Time{})
		m.heartbeatCommit.Unlock()
		select {
		case m.heartbeatConfigChanged <- struct{}{}:
		default:
		}
	}
	if previous.Orchestrator.IntervalSec != cfg.Orchestrator.IntervalSec {
		// Reset the production timer synchronously. ApplyRuntimeConfig holds the
		// scan lock, so an old timer event either finishes before acceptance or
		// observes the new generation and is discarded afterward.
		m.resetScanTimer(time.Now())
	}
	if previous.Orchestrator.MaxParallel != cfg.Orchestrator.MaxParallel {
		m.capacityMu.Lock()
		m.capacityLimit = max(1, cfg.Orchestrator.MaxParallel)
		m.capacityMu.Unlock()
		m.signalCapacityChanged()
		if cfg.Orchestrator.MaxParallel > previous.Orchestrator.MaxParallel {
			m.requestScan()
		}
	}
	if cfg.Orchestrator.Enabled && !wasEnabled {
		m.requestScan()
	}
}

// ApplyHarnessConfig publishes prompt prefixes and task-review policy without
// perturbing the semantic-heartbeat timer. Runtime adapter settings are still
// applied separately by the application harness supervisor.
func (m *Manager) ApplyHarnessConfig(settings config.Harness) {
	m.harnessPolicy.Store(settings)
}

// stopSemanticHeartbeatTimer stops the registered production timer. Callers
// that accept configuration hold heartbeatCommit so acceptance, publication,
// and dispatch have one ordering boundary. A nil expected value stops any
// timer.
func (m *Manager) stopSemanticHeartbeatTimer(expected *time.Timer) {
	m.heartbeatTimerMu.Lock()
	defer m.heartbeatTimerMu.Unlock()
	if expected != nil && m.heartbeatTimer != expected {
		return
	}
	if m.heartbeatTimer != nil {
		if !m.heartbeatTimer.Stop() {
			select {
			case <-m.heartbeatTimer.C:
			default:
			}
		}
		m.heartbeatTimer = nil
	}
}

func New(cfg config.Config, target harness.Harness, hooks extensions.Runner) *Manager {
	parallel := cfg.Orchestrator.MaxParallel
	if parallel <= 0 {
		parallel = 1
	}
	manager := &Manager{
		Config: cfg, runtimeConfig: cfg, Harness: target, Hooks: hooks, inflight: map[string]bool{}, runtimeJobs: map[string]int{}, controlCancelled: map[string]int{}, capacityLimit: parallel,
		Outbox:                 &Outbox{Directory: cfg.StatePath("runtime", "outbox")},
		ownerID:                fmt.Sprintf("%d-%d-%s", os.Getpid(), time.Now().UTC().UnixNano(), randomSuffix()),
		scanNow:                make(chan struct{}, 1),
		capacityChanged:        make(chan struct{}, 1),
		scanTimerChanged:       make(chan struct{}, 1),
		heartbeatConfigChanged: make(chan struct{}, 1),
		heartbeatManual:        make(chan heartbeatManualRequest),
		heartbeatNow:           time.Now, heartbeatTimeout: 5 * time.Minute,
	}
	manager.orchestratorEnabled.Store(cfg.Orchestrator.Enabled)
	manager.heartbeatMinutes.Store(int64(cfg.Orchestrator.SemanticHeartbeatMinutes))
	manager.cleanupDays.Store(int64(cfg.Workspace.CleanupRetentionDays))
	manager.harnessPolicy.Store(cfg.Harness)
	return manager
}

func (m *Manager) SetNotificationDelivery(deliver func(context.Context, Origin, string, string) error) {
	m.Outbox.Deliver = deliver
}

func (m *Manager) Run(ctx context.Context) error {
	if err := m.ScanOnce(ctx); err != nil {
		m.log("orchestrator scan: " + err.Error())
	}
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		m.runSemanticHeartbeat(ctx)
	}()
	defer func() { <-heartbeatDone }()
	cleanupDone := make(chan struct{})
	go func() {
		defer close(cleanupDone)
		m.runAutomaticCleanup(ctx)
	}()
	defer func() { <-cleanupDone }()
	timer := m.startScanTimer()
	defer m.stopScanTimer(timer)
	for {
		m.scanTimerMu.Lock()
		timer = m.scanTimer
		generation := m.scanTimerGeneration
		m.scanTimerMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.scanNow:
			if err := m.ScanOnce(ctx); err != nil {
				m.log("orchestrator event scan: " + err.Error())
			}
		case <-m.scanTimerChanged:
			continue
		case <-timer.C:
			if err := m.scanScheduled(ctx, generation); err != nil {
				m.log("orchestrator scan: " + err.Error())
			}
		}
	}
}

const automaticCleanupCadence = 8 * time.Hour

func (m *Manager) runAutomaticCleanup(ctx context.Context) {
	var ticker *time.Ticker
	ticks := m.cleanupTicks
	if ticks == nil {
		ticker = time.NewTicker(automaticCleanupCadence)
		defer ticker.Stop()
		ticks = ticker.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			if !m.primaryOwned.Load() || m.Cleanup == nil {
				continue
			}
			days := int(m.cleanupDays.Load())
			if days <= 0 {
				continue
			}
			result, err := m.Cleanup(ctx, days)
			if err != nil {
				m.log("automatic cleanup skipped or failed: " + err.Error())
				continue
			}
			m.log("automatic cleanup: " + result)
		}
	}
}

func (m *Manager) startScanTimer() *time.Timer {
	m.scanTimerMu.Lock()
	defer m.scanTimerMu.Unlock()
	now := time.Now()
	if m.scanNext.IsZero() {
		m.scanNext = now.Add(m.scanInterval())
		m.scanTimerGeneration++
	}
	delay := time.Until(m.scanNext)
	if delay < 0 {
		delay = 0
	}
	m.scanTimer = time.NewTimer(delay)
	return m.scanTimer
}

func (m *Manager) stopScanTimer(_ *time.Timer) {
	m.scanTimerMu.Lock()
	defer m.scanTimerMu.Unlock()
	if m.scanTimer == nil {
		return
	}
	if !m.scanTimer.Stop() {
		select {
		case <-m.scanTimer.C:
		default:
		}
	}
	m.scanTimer = nil
}

// resetScanTimer arms the next route scan from the accepted configuration
// instant and returns only after the production timer observes the new
// generation. When Run has not started yet, startScanTimer preserves the same
// accepted deadline.
func (m *Manager) resetScanTimer(acceptedAt time.Time) {
	m.scanTimerMu.Lock()
	defer m.scanTimerMu.Unlock()
	m.scanNext = acceptedAt.Add(m.scanInterval())
	m.scanTimerGeneration++
	if m.scanTimer == nil {
		return
	}
	if !m.scanTimer.Stop() {
		select {
		case <-m.scanTimer.C:
		default:
		}
	}
	delay := time.Until(m.scanNext)
	if delay < 0 {
		delay = 0
	}
	m.scanTimer = time.NewTimer(delay)
	select {
	case m.scanTimerChanged <- struct{}{}:
	default:
	}
}

func (m *Manager) scanScheduled(ctx context.Context, generation uint64) error {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	m.scanTimerMu.Lock()
	current := m.scanTimerGeneration
	m.scanTimerMu.Unlock()
	if current != generation {
		return nil
	}
	err := m.scanOnce(ctx)
	m.resetScanTimer(time.Now())
	return err
}

func (m *Manager) runtimeSnapshot() config.Config {
	m.runtimeConfigMu.RLock()
	defer m.runtimeConfigMu.RUnlock()
	return m.runtimeConfig
}

func (m *Manager) scanInterval() time.Duration {
	seconds := m.runtimeSnapshot().Orchestrator.IntervalSec
	if seconds < 1 {
		seconds = 1
	}
	return time.Duration(seconds) * time.Second
}

func (m *Manager) signalCapacityChanged() {
	select {
	case m.capacityChanged <- struct{}{}:
	default:
	}
}

func (m *Manager) acquireCapacity(ctx context.Context) bool {
	for {
		m.capacityMu.Lock()
		if m.capacityActive < m.capacityLimit {
			m.capacityActive++
			m.capacityMu.Unlock()
			return true
		}
		m.capacityMu.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-m.capacityChanged:
		}
	}
}

func (m *Manager) releaseCapacity() {
	m.capacityMu.Lock()
	if m.capacityActive > 0 {
		m.capacityActive--
	}
	m.capacityMu.Unlock()
	m.signalCapacityChanged()
}

func (m *Manager) requestScan() {
	select {
	case m.scanNow <- struct{}{}:
	default:
	}
}

func (m *Manager) ScanOnce(ctx context.Context) error {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	return m.scanOnce(ctx)
}

func (m *Manager) scanOnce(ctx context.Context) error {
	cfg := m.runtimeSnapshot()
	if !m.orchestratorEnabled.Load() {
		return nil
	}
	if err := os.MkdirAll(m.leaseDirectory(), 0o700); err != nil {
		return err
	}
	if err := m.ensureRouteDirectories(); err != nil {
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
	for _, route := range cfg.Orchestrator.Routes {
		var err error
		switch route.Name {
		case "tasks":
			err = m.scanPhaseQueue(ctx, route, cfg.Resolve(route.Source), cfg.Resolve(route.Working), phaseTaskImplementation)
		case "goals":
			err = m.scanPhaseQueue(ctx, route, cfg.Resolve(route.Source), cfg.Resolve(route.Working), phaseGoalPlanning)
		default:
			err = m.scanRoute(ctx, route)
		}
		if err != nil {
			return fmt.Errorf("route %s: %w", route.Name, err)
		}
	}
	for _, route := range cfg.Orchestrator.Routes {
		base := filepath.Dir(cfg.Resolve(route.Source))
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
	cfg := m.runtimeSnapshot()
	sourceDir := cfg.Resolve(route.Source)
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
		if !m.canAdmitClaim() {
			break
		}
		target := filepath.Join(cfg.Resolve(route.Working), entry.Name())
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
			Route: route.Name, RouteSnapshot: cloneRoute(route), RoutesSnapshot: cloneRoutes(cfg.Orchestrator.Routes), File: target, SessionKey: "orchestrator:" + route.Name + ":" + documentID,
			State: "processing", Phase: "implementation", StartedAt: time.Now().UTC(), HeartbeatAt: time.Now().UTC(),
		}
		if err := m.saveLease(lease); err != nil {
			return err
		}
		if m.runtimeSnapshot().Extensions.Enabled {
			output, hookErr := m.Hooks.Run(ctx, "task.claimed", map[string]any{"route": route.Name, "file": target, "id": documentID})
			if hookErr != nil {
				m.recordError(lease, hookErr)
				continue
			}
			if output.Cancel {
				lease.State = "hook_cancelled"
				lease.LastError = output.Message
				if err := m.saveLease(lease); err != nil {
					m.log("save hook-cancelled lease: " + err.Error())
				}
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
		due, dueErr := documentDueForPhase(source, time.Now(), phase)
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
		// A provider can move a document after the reconciliation pass but
		// before this queue is scanned. Do not claim the next phase while the
		// preceding phase still owns the same durable document; its lease must
		// reconcile the move first.
		_, documentLeased, leaseErr := m.leaseForDocument(route.Name, entry.Name(), "", "")
		if leaseErr != nil {
			return leaseErr
		}
		if m.isInflight(key) || m.leaseExists(key) || documentLeased {
			continue
		}
		if !m.canAdmitClaim() {
			break
		}
		if phase == phaseTaskReview && m.harnessSettings().Reviews == config.TaskReviewsNever {
			target := filepath.Join(filepath.Dir(sourceDir), "todo", entry.Name())
			note := "Independent task review is disabled by harness.reviews=never; Spynel returned this task to todo so an implementation session can record the required direct-completion evidence."
			if err := moveDocumentWithProgress(source, target, "todo", time.Now().UTC(), note); err != nil {
				m.log("bypass disabled task review " + source + ": " + err.Error())
			}
			continue
		}
		target := filepath.Join(claimedDir, entry.Name())
		now := time.Now().UTC()
		attemptField := phaseAttemptField(phase)
		attempt := numberValue(document.FrontMatter[attemptField]) + 1
		lease := Lease{
			ID: key, ClaimID: key, DocumentType: strings.TrimSuffix(route.Name, "s"), Route: route.Name,
			RouteSnapshot:  cloneRoute(route),
			RoutesSnapshot: cloneRoutes(m.runtimeSnapshot().Orchestrator.Routes),
			OwnerID:        m.ownerID,
			File:           target, SourceFile: source, SessionKey: phaseSessionKey(route.Name, documentID, phase, attempt),
			State: "claiming", Phase: phase, ClaimAttempt: attempt, StartedAt: now, HeartbeatAt: now,
		}
		if phase == phaseTaskReview {
			lease.ImplementerThread, _ = document.FrontMatter["implementation_thread"].(string)
		}
		if err := m.saveLease(lease); err != nil {
			return err
		}
		claimed, claimErr := m.claimPhaseDocument(source, target, phaseClaimedStatus(phase), attemptField, now)
		if claimErr != nil {
			_, targetErr := os.Stat(target)
			_, sourceErr := os.Stat(source)
			if targetErr != nil || !os.IsNotExist(sourceErr) {
				_ = os.Remove(m.leasePath(key))
			}
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
		if phase == phaseTaskImplementation && m.runtimeSnapshot().Extensions.Enabled {
			output, hookErr := m.Hooks.Run(ctx, "task.claimed", map[string]any{"route": route.Name, "phase": phase, "file": target, "id": documentID})
			if hookErr != nil {
				m.recordError(lease, hookErr)
				continue
			}
			if output.Cancel {
				lease.State = "hook_cancelled"
				lease.LastError = output.Message
				if err := m.saveLease(lease); err != nil {
					m.log("save hook-cancelled lease: " + err.Error())
				}
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
		RouteSnapshot:  cloneRoute(route),
		RoutesSnapshot: cloneRoutes(m.runtimeSnapshot().Orchestrator.Routes),
		OwnerID:        m.ownerID,
		File:           path, SessionKey: phaseSessionKey(route.Name, id, phase, attempt), State: state,
		Phase: phase, ClaimAttempt: attempt, StartedAt: now, HeartbeatAt: now,
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
	cfg := m.runtimeSnapshot()
	for _, route := range cfg.Orchestrator.Routes {
		paths := []string{cfg.Resolve(route.Source), cfg.Resolve(route.Working)}
		base := filepath.Dir(cfg.Resolve(route.Source))
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
		defer func() {
			m.setInflight(lease.ID, false)
			// Harness completion is the event that makes agent-authored durable
			// transitions observable. Wake the manager immediately instead of
			// waiting for the periodic recovery scan.
			m.requestScan()
		}()
		if !m.acquireCapacity(ctx) {
			return
		}
		defer m.releaseCapacity()
		promptPath := route.Prompt
		if recovery {
			promptPath = route.RecoveryPrompt
		} else if lease.Phase == phaseTaskReview || lease.Phase == phaseGoalReview || lease.Phase == "review" {
			promptPath = route.ReviewPrompt
		}
		if recovery {
			recoveryAttempt := lease.RecoveryCount + 1
			note := fmt.Sprintf("Spynel started recovery attempt %d for %s after its durable execution ownership required reconciliation; the recovery agent must record its findings and outcome here.", recoveryAttempt, strings.ReplaceAll(normalizeLeasePhase(route.Name, lease.Phase), "_", " "))
			if err := updateDocumentProgress(lease.File, time.Now().UTC(), note); err != nil {
				m.recordError(lease, fmt.Errorf("record recovery progress: %w", err))
				return
			}
		}
		prompt, err := m.renderPrompt(route, lease, promptPath)
		if err != nil {
			m.recordError(lease, err)
			return
		}
		harnessSettings := m.harnessSettings()
		prompt = config.PrependAgentPrefix(m.agentPrefix(lease.Phase, harnessSettings), prompt)
		firstAssignedAt, providerIterations, err := ReserveProviderTurn(lease.File, time.Now().UTC())
		if err != nil {
			m.recordError(lease, err)
			return
		}
		jobID := 0
		if m.JobStarted != nil {
			implementationAttempts := 0
			if route.Name == "tasks" {
				if document, readErr := ReadDocument(lease.File); readErr == nil {
					implementationAttempts = numberValue(document.FrontMatter["attempt"])
				}
			}
			jobID, err = m.JobStarted(lease, filepath.Base(lease.File), firstAssignedAt, providerIterations, implementationAttempts)
			if err != nil {
				m.recordError(lease, err)
				return
			}
			m.setRuntimeJob(lease.ID, jobID)
		}
		finish := func() { m.finishRuntimeJob(lease.ID) }
		emit := func(event core.Event) {
			terminal := event.Done && (event.Kind == core.EventFinal || event.Kind == core.EventError)
			if jobID > 0 && m.JobEvent != nil {
				m.JobEvent(jobID, event)
			}
			// Runtime job bookkeeping is process-local and must not depend on
			// the durable lease still existing. A fast agent can move its task,
			// then a concurrent recovery scan can remove the obsolete lease
			// before the provider emits its final event.
			current, err := m.loadLease(lease.ID)
			if err != nil {
				if terminal {
					finish()
				}
				return
			}
			current.HeartbeatAt = time.Now().UTC()
			if event.ThreadID != "" {
				current.ThreadID = event.ThreadID
			}
			if event.Kind == core.EventError {
				current.LastError = event.Text
			}
			if terminal {
				current.State = "awaiting_transition"
			}
			if err := m.saveLease(current); err != nil {
				m.log("save lease event state: " + err.Error())
			}
			if jobID > 0 && m.JobUpdated != nil {
				m.JobUpdated(jobID, current)
			}
			if jobID > 0 && event.Execution != nil && !(terminal && event.Execution.State == "finishing") && m.JobExecutionUpdated != nil {
				m.JobExecutionUpdated(jobID, *event.Execution)
			}
			// Terminal provider completion remains visible as awaiting_transition
			// until reconciliation observes the agent-authored durable file move.
		}
		threadID, steered, err := m.Harness.Send(ctx, lease.SessionKey, prompt, emit)
		if err != nil {
			if jobID > 0 && m.JobEvent != nil {
				m.JobEvent(jobID, core.Event{Kind: core.EventError, Text: err.Error(), Done: true})
			}
			if jobID > 0 && m.JobExecutionUpdated != nil {
				m.JobExecutionUpdated(jobID, core.ExecutionStatus{State: "error", Detail: err.Error()})
			}
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
		if err := m.saveLease(lease); err != nil {
			m.log("save lease dispatch state: " + err.Error())
		}
		if jobID > 0 && m.JobUpdated != nil {
			m.JobUpdated(jobID, lease)
		}
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
		route, ok := m.routeForLease(lease)
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
			m.finishRuntimeJob(lease.ID)
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
		m.finishRuntimeJob(lease.ID)
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
		document, readErr := ReadDocument(path)
		if readErr != nil {
			return status, path, readErr
		}
		policy, policyErr := TaskPolicyFromDocument(document)
		if policyErr != nil {
			// Persist the conservative interpretation so retries and inspection
			// see the same unambiguous policy.
			document.FrontMatter["review_required"] = true
			if err := WriteDocument(path, document); err != nil {
				return status, path, err
			}
			m.log("normalized unsafe task review policy in " + path + ": " + policyErr.Error())
		}
		// Older managers, or an in-flight scan that crossed a provider move,
		// may already have claimed review before this implementation lease is
		// reconciled. Once a review lease exists it owns every subsequent
		// transition for the document. Preserve its file and backfill the
		// implementer identity instead of stealing the claim back to review.
		reviewLease, claimed, leaseErr := m.leaseForDocument(route.Name, name, phaseTaskReview, lease.ID)
		if leaseErr != nil {
			return status, path, leaseErr
		}
		if claimed {
			document.FrontMatter["implementation_thread"] = lease.ThreadID
			document.FrontMatter["implementation_session"] = lease.SessionKey
			if err := WriteDocument(path, document); err != nil {
				return status, path, err
			}
			reviewLease.ImplementerThread = lease.ThreadID
			if err := m.saveLease(reviewLease); err != nil {
				return status, path, err
			}
			return status, path, nil
		}
		harnessSettings := m.harnessSettings()
		reviewRequired := harnessSettings.EffectiveTaskReviewRequired(policy.ReviewRequired)
		if harnessSettings.Reviews == config.TaskReviewsNever && (status == "review" || status == "reviewing") {
			var err error
			status, path, err = m.redirectTransition(path, statusPath(base, "todo", name), "todo", "Independent task review is disabled; Spynel returned this task to todo to record direct-completion evidence.")
			if err != nil {
				return status, path, err
			}
		}
		if status == "reviewing" || (status == "done" && reviewRequired) {
			var err error
			status, path, err = m.redirectTransition(path, statusPath(base, "review", name), "review", "Implementation cannot bypass independent review; Spynel redirected this task to review.")
			if err != nil {
				return status, path, err
			}
		}
		if !map[string]bool{"todo": true, "review": true, "waiting": true, "done": true, "failed": true, "cancelled": true}[status] {
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
		if status == "done" && !reviewRequired {
			if evidenceErr := validateDirectCompletionEvidence(document); evidenceErr != nil {
				var err error
				status, path, err = m.redirectTransition(path, statusPath(base, "todo", name), "todo", "Direct completion rejected: "+evidenceErr.Error())
				return status, path, err
			}
		}
		if status == "done" {
			m.finalizeTaskCompletionSummary(path, status)
		}
		if status == "done" || status == "waiting" || status == "failed" || status == "cancelled" {
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
	m.finalizeTaskCompletionSummary(path, status)
	if status == "done" {
		if err := m.completeTransition(ctx, route, lease, status, path); err != nil {
			return status, path, err
		}
	}
	return status, path, nil
}

func (m *Manager) reconcileGoalTransition(_ context.Context, route config.Route, lease Lease, phase, status, path string) (string, string, error) {
	base := filepath.Dir(m.Config.Resolve(route.Source))
	name := filepath.Base(path)
	document, err := ReadDocument(path)
	if err != nil {
		return status, path, err
	}
	if phase == phaseGoalPlanning {
		if status == "active" {
			taskRoute, ok := routeFromSnapshot(lease.RoutesSnapshot, "tasks")
			if !ok {
				taskRoute, ok = m.route("tasks")
			}
			if !ok {
				return m.redirectTransition(path, statusPath(base, "proposed", name), "proposed", "Goal activation rejected: tasks route is required for goals")
			}
			if err := m.validateGoalPlanningTransitionForRoute(document, taskRoute); err == nil {
				document.FrontMatter["round_task_route"] = *cloneRoute(taskRoute)
				document.FrontMatter["round_task_route_round"] = numberValue(document.FrontMatter["round"])
				if err := WriteDocument(path, document); err != nil {
					return status, path, err
				}
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
	now := time.Now().UTC()
	if err := moveDocumentWithProgress(path, target, status, now, note); err != nil {
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
	runtimeCfg := m.runtimeSnapshot()
	if runtimeCfg.Extensions.Enabled {
		if lease.TerminalHooksCompleted == nil {
			lease.TerminalHooksCompleted = map[string]bool{}
		}
		// Enqueue first, then persist a receipt after each extension completes
		// successfully. A crash or receipt-write failure retries with the same
		// event ID; extensions must persistently deduplicate visible effects.
		var receiptErr error
		if _, hookErr := m.Hooks.RunTracked(ctx, "task.completed", map[string]any{
			"route": route.Name, "file": path, "thread_id": lease.ThreadID,
			"outcome": status, "event_id": lease.ID + ":" + status,
		}, lease.TerminalHooksCompleted, func(extensionID string) error {
			lease.TerminalHooksCompleted[extensionID] = true
			receiptErr = m.saveLease(lease)
			return receiptErr
		}); hookErr != nil {
			m.log("task.completed hook: " + hookErr.Error())
			return hookErr
		}
	}
	if requiresTaskNotificationDecision(document, status, time.Now().UTC()) {
		m.startTaskNotificationAgent(ctx, lease, status, path)
	}
	return nil
}

func requiresTaskNotificationDecision(document Document, status string, now time.Time) bool {
	switch status {
	case "done", "failed", "cancelled":
		return true
	case "waiting":
		wakeAt := strings.TrimSpace(stringField(document, "wake_at"))
		if wakeAt == "" {
			return true
		}
		due, err := time.Parse(time.RFC3339, wakeAt)
		return err != nil || !due.After(now)
	default:
		return false
	}
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
				phase := normalizeLeasePhase(lease.Route, lease.Phase)
				field := phaseAttemptField(phase)
				document, claimErr := m.claimPhaseDocument(lease.SourceFile, lease.File, phaseClaimedStatus(phase), field, time.Now())
				if claimErr != nil {
					return claimErr
				}
				if lease.ClaimAttempt == 0 {
					lease.ClaimAttempt = numberValue(document.FrontMatter[field])
				}
			}
		}
		if _, err := os.Stat(lease.File); err == nil {
			route, ok := m.routeForLease(lease)
			if !ok {
				continue
			}
			phase := normalizeLeasePhase(lease.Route, lease.Phase)
			field := phaseAttemptField(phase)
			document, readErr := ReadDocument(lease.File)
			if readErr != nil {
				return readErr
			}
			attempt := lease.ClaimAttempt
			if attempt < 1 {
				return fmt.Errorf("claim lease %s has no phase attempt", lease.ID)
			}
			document.FrontMatter["status"] = phaseClaimedStatus(phase)
			document.FrontMatter["updated_at"] = lease.StartedAt.UTC().Format(time.RFC3339)
			if first, ok := frontMatterTime(document.FrontMatter["first_assigned_at"]); !ok || first.After(lease.StartedAt) {
				document.FrontMatter["first_assigned_at"] = lease.StartedAt.UTC().Format(time.RFC3339)
			}
			document.FrontMatter[field] = attempt
			if err := WriteDocument(lease.File, document); err != nil {
				return err
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

func (m *Manager) claimPhaseDocument(source, target, status, attemptField string, now time.Time) (Document, error) {
	if m.claimDocument != nil {
		return m.claimDocument(source, target, status, attemptField, now)
	}
	return claimDocument(source, target, status, attemptField, now)
}

func (m *Manager) recoverOrphanClaims(ctx context.Context) error {
	cfg := m.runtimeSnapshot()
	for _, route := range cfg.Orchestrator.Routes {
		base := filepath.Dir(cfg.Resolve(route.Source))
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

func (m *Manager) leaseForDocument(routeName, name, phase, exceptID string) (Lease, bool, error) {
	leases, err := m.loadLeases()
	if err != nil {
		return Lease{}, false, err
	}
	for _, lease := range leases {
		if lease.ID == exceptID || lease.Route != routeName || filepath.Base(lease.File) != name {
			continue
		}
		if phase != "" && normalizeLeasePhase(routeName, lease.Phase) != phase {
			continue
		}
		return lease, true, nil
	}
	return Lease{}, false, nil
}

func (m *Manager) wakeWaitingDocuments(ctx context.Context) error {
	now := time.Now().UTC()
	cfg := m.runtimeSnapshot()
	for _, route := range cfg.Orchestrator.Routes {
		if route.Name != "tasks" && route.Name != "goals" {
			continue
		}
		base := filepath.Dir(cfg.Resolve(route.Source))
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
			note := fmt.Sprintf("Spynel resumed this %s from waiting because its scheduled wake condition became due; it returned to %s for a fresh agent decision.", strings.TrimSuffix(route.Name, "s"), targetStatus)
			if err := moveDocumentWithProgress(path, target, targetStatus, now, note); err != nil {
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
			if _, _, moveErr := m.redirectTransition(path, target, "proposed", "Invalid active goal returned to planning queue: "+err.Error()); moveErr != nil {
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
		ready := false
		switch trigger {
		case "all_round_tasks_settled":
			ready = settled
		case "all_round_tasks_settled_or_checkpoint":
			ready = settled || checkpoint
		case "scheduled":
			ready = checkpoint
		}
		if !ready {
			continue
		}
		target := filepath.Join(base, "review", entry.Name())
		reason := "all current-round tasks settled"
		if checkpoint && !settled {
			reason = "the configured evidence checkpoint became due"
		}
		note := "Spynel queued this goal for independent review because " + reason + "."
		if err := moveDocumentWithProgress(path, target, "review", now, note); err != nil {
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
	trigger := stringField(document, "review_trigger")
	switch trigger {
	case "all_round_tasks_settled":
	case "all_round_tasks_settled_or_checkpoint":
		if _, exists := document.FrontMatter["next_review_at"]; exists {
			if _, err := checkpointDue(document, time.Now()); err != nil {
				return err
			}
		}
	case "scheduled":
		if _, exists := document.FrontMatter["next_review_at"]; !exists {
			return errors.New("scheduled review_trigger requires next_review_at")
		}
		if _, err := checkpointDue(document, time.Now()); err != nil {
			return err
		}
	default:
		return errors.New("review_trigger must be all_round_tasks_settled, all_round_tasks_settled_or_checkpoint, or scheduled")
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

func (m *Manager) validateGoalPlanningTransition(document Document) error {
	if err := m.validateGoalActivation(document); err != nil {
		return err
	}
	if _, exists := document.FrontMatter["next_review_at"]; exists && stringField(document, "checkpoint_reason") == "" {
		return errors.New("next_review_at requires checkpoint_reason")
	}
	return nil
}

func (m *Manager) validateGoalPlanningTransitionForRoute(document Document, taskRoute config.Route) error {
	copy := Document{FrontMatter: make(map[string]any, len(document.FrontMatter)+2), Body: document.Body}
	for key, value := range document.FrontMatter {
		copy.FrontMatter[key] = value
	}
	copy.FrontMatter["round_task_route"] = *cloneRoute(taskRoute)
	copy.FrontMatter["round_task_route_round"] = numberValue(document.FrontMatter["round"])
	return m.validateGoalPlanningTransition(copy)
}

func (m *Manager) goalRoundTasks(document Document) ([]linkedTask, error) {
	taskRoute, ok := roundTaskRoute(document)
	if !ok {
		taskRoute, ok = m.route("tasks")
		if !ok {
			return nil, errors.New("tasks route is required for goals")
		}
	}
	id := documentID(document)
	if id == "" {
		return nil, errors.New("goal id is required")
	}
	return linkedRoundTasks(filepath.Dir(m.Config.Resolve(taskRoute.Source)), id, numberValue(document.FrontMatter["round"]))
}

func roundTaskRoute(document Document) (config.Route, bool) {
	if numberValue(document.FrontMatter["round_task_route_round"]) != numberValue(document.FrontMatter["round"]) {
		return config.Route{}, false
	}
	value, ok := document.FrontMatter["round_task_route"]
	if !ok {
		return config.Route{}, false
	}
	data, err := yaml.Marshal(value)
	if err != nil {
		return config.Route{}, false
	}
	var route config.Route
	if err := yaml.Unmarshal(data, &route); err != nil || route.Name != "tasks" || strings.TrimSpace(route.Source) == "" {
		return config.Route{}, false
	}
	return *cloneRoute(route), true
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
		route, ok := m.routeForLease(lease)
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

func (m *Manager) renderPrompt(route config.Route, lease Lease, promptPath string) (string, error) {
	file := lease.File
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
		"{{RELATED_TASKS}}":  m.relatedTasksForGoal(file, lease.RoutesSnapshot),
		"{{TASK_SOURCE}}":    m.routeSourceFromSnapshot(lease.RoutesSnapshot, "tasks"),
		"{{GOAL_SOURCE}}":    m.routeSourceFromSnapshot(lease.RoutesSnapshot, "goals"),
	}
	prompt := string(data)
	prompt = agentdocs.InjectPromptGuidance(prompt)
	for from, to := range replacements {
		prompt = strings.ReplaceAll(prompt, from, to)
	}
	prompt = appendTaskReviewModeInstruction(prompt, m.harnessSettings().Reviews)
	role := instructions.Developer
	phase := normalizeLeasePhase(route.Name, phaseForFile(route.Name, file))
	if phase == phaseTaskReview || phase == phaseGoalReview {
		role = instructions.Reviewer
	}
	prompt = instructions.InjectRoleScopeDiscipline(prompt, role)
	return instructions.Append(prompt, m.Config.StatePath(), role)
}

func (m *Manager) harnessSettings() config.Harness {
	if value := m.harnessPolicy.Load(); value != nil {
		return value.(config.Harness)
	}
	return m.runtimeSnapshot().Harness
}

func (m *Manager) agentPrefix(phase string, settings config.Harness) string {
	switch normalizeLeasePhase("", phase) {
	case phaseTaskReview, phaseGoalReview:
		return settings.ReviewerAgentPrefix
	default:
		return settings.DeveloperAgentPrefix
	}
}

func (m *Manager) routeSource(name string) string {
	if route, ok := m.route(name); ok {
		return m.Config.Resolve(route.Source)
	}
	return "(route not configured)"
}

func (m *Manager) routeSourceFromSnapshot(routes []config.Route, name string) string {
	if route, ok := routeFromSnapshot(routes, name); ok {
		return m.Config.Resolve(route.Source)
	}
	return m.routeSource(name)
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

func (m *Manager) relatedTasksForGoal(file string, routes []config.Route) string {
	if filepath.Base(filepath.Dir(filepath.Dir(file))) != "goals" {
		return "- Not applicable."
	}
	document, err := ReadDocument(file)
	if err != nil {
		return "- Unable to read linked tasks: " + err.Error()
	}
	// Before a planned round is activated, its durable round_task_route has not
	// been written yet. Resolve linked-task evidence against the same admitted
	// route generation used for TASK_SOURCE instead of the newest live routes.
	if _, ok := roundTaskRoute(document); !ok {
		if taskRoute, exists := routeFromSnapshot(routes, "tasks"); exists {
			document.FrontMatter["round_task_route"] = *cloneRoute(taskRoute)
			document.FrontMatter["round_task_route_round"] = numberValue(document.FrontMatter["round"])
		}
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

func (m *Manager) ScheduledCheckpoints(now time.Time) ([]ScheduledCheckpoint, error) {
	route, ok := m.route("goals")
	if !ok {
		return nil, nil
	}
	directory := filepath.Join(filepath.Dir(m.Config.Resolve(route.Source)), "active")
	entries, truncated, err := readDirectoryEntries(directory, maxStatusDirectoryEntries)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	waits := make([]ScheduledCheckpoint, 0)
	inspected := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".md") || entry.Name() == "AGENTS.md" {
			continue
		}
		if inspected >= maxStatusDocuments {
			return waits, fmt.Errorf("scheduled goal checkpoints are incomplete: document limit reached")
		}
		inspected++
		data, err := readStatusDocument(filepath.Join(directory, entry.Name()))
		if err != nil {
			continue
		}
		document, err := ParseDocument(data)
		if err != nil {
			continue
		}
		trigger := stringField(document, "review_trigger")
		if trigger != "scheduled" && trigger != "all_round_tasks_settled_or_checkpoint" {
			continue
		}
		value, ok := document.FrontMatter["next_review_at"]
		if !ok {
			continue
		}
		var at time.Time
		switch typed := value.(type) {
		case string:
			at, err = time.Parse(time.RFC3339, typed)
		case time.Time:
			at = typed
		}
		if err != nil || at.IsZero() || !at.After(now) {
			continue
		}
		waits = append(waits, ScheduledCheckpoint{
			ID: documentID(document), Title: stringField(document, "title"), At: at.UTC(),
			Reason: stringField(document, "checkpoint_reason"),
		})
	}
	if truncated {
		return waits, fmt.Errorf("scheduled goal checkpoints are incomplete: directory entry limit reached")
	}
	sort.Slice(waits, func(i, j int) bool { return waits[i].At.Before(waits[j].At) })
	return waits, nil
}

func (m *Manager) route(name string) (config.Route, bool) {
	for _, route := range m.runtimeSnapshot().Orchestrator.Routes {
		if route.Name == name {
			return route, true
		}
	}
	return config.Route{}, false
}

func cloneRoute(route config.Route) *config.Route {
	snapshot := route
	snapshot.AllowedNext = append([]string(nil), route.AllowedNext...)
	return &snapshot
}

func cloneRoutes(routes []config.Route) []config.Route {
	cloned := make([]config.Route, len(routes))
	for index, route := range routes {
		cloned[index] = *cloneRoute(route)
	}
	return cloned
}

func routeFromSnapshot(routes []config.Route, name string) (config.Route, bool) {
	for _, route := range routes {
		if route.Name == name {
			return *cloneRoute(route), true
		}
	}
	return config.Route{}, false
}

func (m *Manager) routeForLease(lease Lease) (config.Route, bool) {
	if lease.RouteSnapshot != nil && lease.RouteSnapshot.Name == lease.Route {
		return *cloneRoute(*lease.RouteSnapshot), true
	}
	// Pre-snapshot leases remain recoverable while their named route exists.
	return m.route(lease.Route)
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

// LeaseForSession returns the newest persisted lease for an orchestrator
// session. The value copy remains safe if the lease is concurrently updated.
func (m *Manager) LeaseForSession(sessionKey string) (Lease, bool) {
	leases, err := m.loadLeases()
	if err != nil {
		return Lease{}, false
	}
	for index := len(leases) - 1; index >= 0; index-- {
		if leases[index].SessionKey == sessionKey {
			return leases[index], true
		}
	}
	return Lease{}, false
}

// PrepareControlContinuation performs the one durable gate immediately before
// a control-attributable continuation. It refuses moved documents, changed
// owners/sessions, cancellation/error states, and executions no longer owned
// by this manager. A successful gate returns the lease to processing; the
// provider's subsequent events remain responsible for heartbeat activity.
func (m *Manager) PrepareControlContinuation(expected Lease, expectedDocumentID string) bool {
	if !m.ControlStillValid(expected, expectedDocumentID) || !m.Harness.IsActive(expected.SessionKey) {
		return false
	}
	current, err := m.loadLease(expected.ID)
	if err != nil {
		return false
	}
	if current.State != "awaiting_transition" && current.State != "processing" && current.State != "recovering" {
		return false
	}
	current.State = "processing"
	current.LastError = ""
	if err := m.saveLease(current); err != nil {
		m.log("save control continuation lease: " + err.Error())
		return false
	}
	if jobID := m.runtimeJob(expected.ID); jobID > 0 && m.JobUpdated != nil {
		m.JobUpdated(jobID, current)
	}
	return true
}

// ReserveControlProviderTurn performs the same durable provider-turn
// reservation used by ordinary dispatch immediately before a native control,
// queued control, or guarded continuation reaches the harness.
func (m *Manager) ReserveControlProviderTurn(expected Lease, expectedDocumentID string) bool {
	if !m.ControlStillValid(expected, expectedDocumentID) {
		return false
	}
	first, iterations, err := ReserveProviderTurn(expected.File, time.Now().UTC())
	if err != nil {
		m.log("reserve control provider turn: " + err.Error())
		return false
	}
	if jobID := m.runtimeJob(expected.ID); jobID > 0 && m.JobTimingUpdated != nil {
		m.JobTimingUpdated(jobID, first, iterations)
	}
	return true
}

// ControlStillValid checks immutable execution identity and durable claimed
// state without advancing heartbeats or workflow status.
func (m *Manager) ControlStillValid(expected Lease, expectedDocumentID string) bool {
	if expected.ID == "" || expectedDocumentID == "" || m.runtimeJob(expected.ID) == 0 || m.isControlCancelled(expected.ID) {
		return false
	}
	current, err := m.loadLease(expected.ID)
	if err != nil || current.OwnerID != expected.OwnerID || current.SessionKey != expected.SessionKey || current.File != expected.File || current.Phase != expected.Phase {
		return false
	}
	if current.State != "awaiting_transition" && current.State != "processing" && current.State != "recovering" {
		return false
	}
	document, err := ReadDocument(current.File)
	if err != nil {
		return false
	}
	if documentID(document) != expectedDocumentID {
		return false
	}
	status, _ := document.FrontMatter["status"].(string)
	expectedStatus := ""
	switch normalizeLeasePhase(expected.Route, expected.Phase) {
	case phaseTaskImplementation:
		expectedStatus = "working"
	case phaseTaskReview, phaseGoalReview:
		expectedStatus = "reviewing"
	case phaseGoalPlanning:
		expectedStatus = "planning"
	}
	return expectedStatus != "" && status == expectedStatus
}

// MarkControlCancellation fences automatic control continuation before a job
// interrupt races the provider's terminal event. It is process-local: a later
// owner may still use ordinary durable stale recovery for unfinished work.
func (m *Manager) MarkControlCancellation(sessionKey string) string {
	lease, ok := m.LeaseForSession(sessionKey)
	if !ok {
		return ""
	}
	m.mu.Lock()
	m.controlCancelled[lease.ID]++
	m.mu.Unlock()
	return lease.ID
}

// RestoreControlCancellation releases one cancellation reservation after the
// provider rejects the matching interrupt. Counting reservations prevents one
// failed concurrent request from clearing another accepted cancellation.
func (m *Manager) RestoreControlCancellation(leaseID string) {
	if leaseID == "" {
		return
	}
	m.mu.Lock()
	if m.controlCancelled[leaseID] <= 1 {
		delete(m.controlCancelled, leaseID)
	} else {
		m.controlCancelled[leaseID]--
	}
	m.mu.Unlock()
}

func (m *Manager) isControlCancelled(leaseID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.controlCancelled[leaseID] > 0
}

func (m *Manager) recordError(lease Lease, err error) {
	lease.LastError = err.Error()
	lease.State = "error"
	lease.HeartbeatAt = time.Now().UTC()
	if saveErr := m.saveLease(lease); saveErr != nil {
		m.log("save failed lease: " + saveErr.Error())
	}
	m.log(fmt.Sprintf("dispatch %s: %v", lease.File, err))
}

func (m *Manager) isInflight(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inflight[key]
}

func (m *Manager) canAdmitClaim() bool {
	m.capacityMu.Lock()
	limit := m.capacityLimit
	m.capacityMu.Unlock()
	m.mu.Lock()
	count := len(m.inflight)
	m.mu.Unlock()
	return count < limit
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

func (m *Manager) setRuntimeJob(leaseID string, jobID int) {
	if jobID <= 0 {
		return
	}
	m.mu.Lock()
	m.runtimeJobs[leaseID] = jobID
	m.mu.Unlock()
}

func (m *Manager) runtimeJob(leaseID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runtimeJobs[leaseID]
}

func (m *Manager) finishRuntimeJob(leaseID string) {
	m.mu.Lock()
	jobID := m.runtimeJobs[leaseID]
	delete(m.runtimeJobs, leaseID)
	delete(m.controlCancelled, leaseID)
	m.mu.Unlock()
	if jobID > 0 && m.JobFinished != nil {
		m.JobFinished(jobID)
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
