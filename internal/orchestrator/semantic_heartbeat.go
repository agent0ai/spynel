package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/fsx"
	"github.com/agent0ai/spynel/internal/instructions"
)

const (
	semanticHeartbeatSchema  = "spynel.semantic-heartbeat/v1"
	semanticHeartbeatSession = "orchestrator:semantic-heartbeat"
	maxHeartbeatOutputBytes  = 64 << 10
	maxHeartbeatPromptBytes  = 128 << 10
	maxHeartbeatRouteBytes   = 64 << 10
	maxHeartbeatStateBytes   = 1 << 20
	maxHeartbeatStateEntries = 1024
)

var semanticCategories = map[string]bool{
	"healthy_or_progressing": true, "stale_or_orphaned_claim": true,
	"due_waiting_condition": true, "inconsistent_durable_transition": true,
	"dead_job_live_lease": true, "live_job_missing_ownership": true,
	"repeated_recovery": true, "review_phase_mismatch": true,
	"failed_outbox_delivery": true, "external_input_required": true,
}

type semanticHeartbeatResult struct {
	Schema      string                     `json:"schema"`
	ExecutionID string                     `json:"execution_id"`
	ObservedAt  time.Time                  `json:"observed_at"`
	Status      string                     `json:"status"`
	Findings    []semanticHeartbeatFinding `json:"findings"`
}

type semanticHeartbeatFinding struct {
	Category           string `json:"category"`
	WorkflowID         string `json:"workflow_id"`
	Evidence           string `json:"evidence"`
	Action             string `json:"action"`
	NotificationOrigin string `json:"notification_origin"`
	Notification       string `json:"notification"`
}

type semanticHeartbeatState struct {
	Findings  map[string]semanticFindingState `json:"findings"`
	LastAudit semanticAuditDiagnostic         `json:"last_audit"`
}

type semanticFindingState struct {
	FirstSeen    time.Time `json:"first_seen"`
	LastSeen     time.Time `json:"last_seen"`
	EvidenceHash string    `json:"evidence_hash"`
	Occurrences  int       `json:"occurrences"`
}

type semanticAuditDiagnostic struct {
	ExecutionID string         `json:"execution_id"`
	ObservedAt  time.Time      `json:"observed_at"`
	Status      string         `json:"status"`
	Findings    map[string]int `json:"finding_categories"`
}

func (m *Manager) runSemanticHeartbeat(ctx context.Context) {
	var (
		term        uint64
		termActive  bool
		auditCtx    context.Context
		auditCancel context.CancelFunc
		timer       *time.Timer
		ticks       <-chan time.Time
		auditActive bool
		armed       bool
	)
	auditDone := make(chan time.Time, 1)
	waitForProvider := func() {
		auditActive = true
		m.jobs.Add(1)
		go func() {
			defer m.jobs.Done()
			completedAt, ok := m.waitForSemanticHeartbeatProviderRelease(ctx)
			if !ok {
				return
			}
			select {
			case auditDone <- completedAt:
			default:
			}
		}()
	}
	stopTimer := func() {
		if timer != nil {
			m.stopSemanticHeartbeatTimer(timer)
			timer = nil
		}
		if m.heartbeatTicks != nil {
			ticks = m.heartbeatTicks
		} else {
			ticks = nil
		}
		armed = false
	}
	stopTerm := func() {
		if auditCancel != nil {
			auditCancel()
			auditCancel = nil
		}
		if termActive {
			m.endSemanticHeartbeatTerm(term)
			termActive = false
		}
	}
	armTimer := func(base time.Time) {
		stopTimer()
		m.heartbeatCommit.Lock()
		defer m.heartbeatCommit.Unlock()
		if !termActive || m.heartbeatTerm != term {
			return
		}
		minutes := m.heartbeatMinutes.Load()
		if !m.orchestratorEnabled.Load() || minutes <= 0 {
			return
		}
		deadline := base.Add(time.Duration(minutes) * time.Minute)
		if m.heartbeatTicks != nil {
			ticks = m.heartbeatTicks
		} else {
			timer = time.NewTimer(time.Until(deadline))
			m.heartbeatTimerMu.Lock()
			m.heartbeatTimer = timer
			m.heartbeatTimerMu.Unlock()
			ticks = timer.C
		}
		armed = true
		m.setSemanticHeartbeatScheduleForTerm(true, deadline, term)
	}
	configure := func() {
		stopTimer()
		stopTerm()
		generation := m.heartbeatConfigGeneration.Load()
		changed := generation != m.heartbeatAppliedGeneration.Load()
		minutes := m.heartbeatMinutes.Load()
		if !m.orchestratorEnabled.Load() || minutes <= 0 {
			m.setSemanticHeartbeatSchedule(false, time.Time{})
			m.heartbeatAppliedGeneration.Store(generation)
			return
		}
		term = m.beginSemanticHeartbeatTerm()
		termActive = true
		auditCtx, auditCancel = context.WithCancel(ctx)
		if auditActive || m.semanticHeartbeatProviderInFlight() {
			// A superseded provider may ignore cancellation. Do not publish or
			// arm its successor until that provider has actually released the
			// global non-overlap fence.
			if !auditActive {
				waitForProvider()
			}
			m.setSemanticHeartbeatScheduleForTerm(true, time.Time{}, term)
			m.heartbeatAppliedGeneration.Store(generation)
			return
		}
		accepted := int64(0)
		if changed {
			accepted = m.heartbeatConfigAcceptedAt.Load()
		}
		base := time.Time{}
		if accepted != 0 {
			base = time.Unix(0, accepted).UTC()
		} else {
			base = m.semanticHeartbeatNow()
		}
		armTimer(base)
		m.heartbeatAppliedGeneration.Store(generation)
	}
	configure()
	defer func() {
		stopTimer()
		stopTerm()
		m.setSemanticHeartbeatSchedule(false, time.Time{})
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.heartbeatConfigChanged:
			if m.heartbeatConfigGeneration.Load() != m.heartbeatAppliedGeneration.Load() {
				configure()
			}
		case <-ticks:
			if !armed || auditActive {
				continue
			}
			m.heartbeatCommit.Lock()
			if m.heartbeatTerm != term || !m.orchestratorEnabled.Load() || m.heartbeatMinutes.Load() <= 0 {
				m.heartbeatCommit.Unlock()
				stopTimer()
				continue
			}
			stopTimer()
			m.setSemanticHeartbeatScheduleForTerm(true, time.Time{}, term)
			auditActive = true
			m.jobs.Add(1)
			go func(runCtx context.Context, runTerm uint64) {
				defer m.jobs.Done()
				providerStarted := m.runSemanticHeartbeatOnceForTerm(runCtx, runTerm)
				completedAt := time.Time{}
				if providerStarted {
					var ok bool
					completedAt, ok = m.waitForSemanticHeartbeatProviderRelease(ctx)
					if !ok {
						return
					}
				} else {
					completedAt = m.semanticHeartbeatNow()
				}
				select {
				case auditDone <- completedAt:
				default:
				}
			}(auditCtx, term)
			m.heartbeatCommit.Unlock()
		case completedAt := <-auditDone:
			auditActive = false
			if termActive && m.orchestratorEnabled.Load() && m.heartbeatMinutes.Load() > 0 {
				if accepted := m.heartbeatConfigAcceptedAt.Load(); accepted != 0 {
					acceptedAt := time.Unix(0, accepted).UTC()
					if acceptedAt.After(completedAt) {
						completedAt = acceptedAt
					}
				}
				armTimer(completedAt)
			}
		}
	}
}

func (m *Manager) semanticHeartbeatNow() time.Time {
	if m.heartbeatNow != nil {
		return m.heartbeatNow().UTC()
	}
	return time.Now().UTC()
}

func (m *Manager) setSemanticHeartbeatSchedule(owned bool, next time.Time) {
	m.setSemanticHeartbeatScheduleForTerm(owned, next, 0)
}

func (m *Manager) setSemanticHeartbeatScheduleForTerm(owned bool, next time.Time, term uint64) {
	m.heartbeatStatusMu.Lock()
	m.heartbeatOwned = owned
	m.heartbeatOwnedTerm = term
	m.heartbeatNext = next.UTC()
	m.heartbeatStatusMu.Unlock()
}

func (m *Manager) runSemanticHeartbeatOnce(parent context.Context) {
	term := m.beginSemanticHeartbeatTerm()
	defer m.endSemanticHeartbeatTerm(term)
	m.runSemanticHeartbeatOnceForTerm(parent, term)
}

func (m *Manager) runSemanticHeartbeatOnceForTerm(parent context.Context, term uint64) (providerStarted bool) {
	m.heartbeatProviderMu.Lock()
	if !m.heartbeatProviderActive.CompareAndSwap(false, true) {
		m.heartbeatProviderMu.Unlock()
		m.log("semantic heartbeat tick coalesced: provider from previous audit still active")
		return false
	}
	m.heartbeatProviderDone = make(chan struct{})
	m.heartbeatProviderReleasedAt = time.Time{}
	providerDoneSignal := m.heartbeatProviderDone
	m.heartbeatProviderMu.Unlock()
	providerStarted = true
	m.heartbeatRunningTerm.Store(term)
	providerOwnsActive := false
	releaseProvider := func() {
		releasedAt := m.semanticHeartbeatNow()
		m.heartbeatRunningTerm.CompareAndSwap(term, 0)
		m.heartbeatProviderMu.Lock()
		m.heartbeatProviderReleasedAt = releasedAt
		m.heartbeatProviderActive.Store(false)
		close(providerDoneSignal)
		m.heartbeatProviderMu.Unlock()
	}
	defer func() {
		if !providerOwnsActive {
			releaseProvider()
		}
	}()
	now := time.Now().UTC().Truncate(time.Second)
	if m.heartbeatNow != nil {
		now = m.heartbeatNow().UTC().Truncate(time.Second)
	}
	executionID := fmt.Sprintf("heartbeat-%d-%s", now.Unix(), randomSuffix())
	prompt, err := m.semanticHeartbeatPrompt(executionID, now)
	if err != nil {
		m.log("semantic heartbeat prompt rejected: " + err.Error())
		return
	}
	timeout := m.heartbeatTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	lease := Lease{DocumentType: "heartbeat", Route: "semantic-heartbeat", SessionKey: semanticHeartbeatSession, State: "audit", Phase: "semantic_audit", StartedAt: now, HeartbeatAt: now}
	jobID := 0
	if m.JobStarted != nil {
		jobID = m.JobStarted(lease, "semantic workflow heartbeat", time.Time{}, 0, 0)
	}
	type providerResult struct {
		output string
		err    error
	}
	providerDone := make(chan providerResult, 1)
	providerOwnsActive = true
	go func() {
		if jobID > 0 && m.JobFinished != nil {
			defer m.JobFinished(jobID)
		}
		defer releaseProvider()
		var output strings.Builder
		emit := func(event core.Event) {
			if jobID > 0 && m.JobUpdated != nil {
				activity := lease
				activity.HeartbeatAt = m.semanticHeartbeatNow()
				m.JobUpdated(jobID, activity)
			}
			text := event.Text
			if event.FinalText != nil {
				text = *event.FinalText
			}
			if event.Kind == core.EventFinal && text != "" {
				output.Reset()
			}
			remaining := maxHeartbeatOutputBytes - output.Len()
			if remaining > 0 && text != "" {
				if len(text) > remaining {
					text = text[:remaining]
				}
				output.WriteString(text)
			}
		}
		message := config.PrependAgentPrefix(m.harnessSettings().HeartbeatAgentPrefix, prompt)
		_, _, sendErr := m.Harness.Send(ctx, semanticHeartbeatSession, message, emit)
		providerDone <- providerResult{output: output.String(), err: sendErr}
	}()
	var completed providerResult
	select {
	case completed = <-providerDone:
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			m.log("semantic heartbeat timed out; provider remains fenced until it exits")
		}
		return
	}
	if completed.err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			m.log("semantic heartbeat timed out; future ticks will continue after cancellation completes")
		} else if !errors.Is(completed.err, context.Canceled) {
			m.log("semantic heartbeat provider failure: " + completed.err.Error())
		}
		return
	}
	// Some providers can return successfully after cancellation. Fence that
	// late result before it can act for a superseded primary term.
	if err := ctx.Err(); err != nil {
		m.log("semantic heartbeat late result rejected: " + err.Error())
		return
	}
	result, err := parseSemanticHeartbeatResult(completed.output, executionID, now)
	if err != nil {
		m.log("semantic heartbeat result rejected: " + err.Error())
		return
	}
	if err := ctx.Err(); err != nil {
		m.log("semantic heartbeat stale result rejected: " + err.Error())
		return
	}
	if err := m.recordSemanticHeartbeatResultForTerm(ctx, result, term); err != nil {
		m.log("semantic heartbeat result handling failed: " + err.Error())
		return
	}
	m.log(fmt.Sprintf("semantic heartbeat complete: status=%s findings=%d", result.Status, len(result.Findings)))
	return providerStarted
}

func (m *Manager) semanticHeartbeatProviderInFlight() bool {
	m.heartbeatProviderMu.Lock()
	defer m.heartbeatProviderMu.Unlock()
	return m.heartbeatProviderActive.Load()
}

func (m *Manager) waitForSemanticHeartbeatProviderRelease(ctx context.Context) (time.Time, bool) {
	m.heartbeatProviderMu.Lock()
	done := m.heartbeatProviderDone
	releasedAt := m.heartbeatProviderReleasedAt
	m.heartbeatProviderMu.Unlock()
	if done == nil {
		return m.semanticHeartbeatNow(), true
	}
	select {
	case <-ctx.Done():
		return time.Time{}, false
	case <-done:
	}
	m.heartbeatProviderMu.Lock()
	releasedAt = m.heartbeatProviderReleasedAt
	m.heartbeatProviderMu.Unlock()
	return releasedAt, true
}

func (m *Manager) semanticHeartbeatPrompt(executionID string, now time.Time) (string, error) {
	path := m.Config.StatePath("prompts", "heartbeat.md")
	data, err := readFileLimit(path, maxHeartbeatPromptBytes, "heartbeat prompt")
	if err != nil {
		return "", err
	}
	cfg := m.runtimeSnapshot()
	if len(cfg.Orchestrator.Routes) > 128 {
		return "", errors.New("heartbeat route summary exceeds 128 routes")
	}
	routes := make([]string, 0, len(cfg.Orchestrator.Routes))
	routeBytes := 0
	for _, route := range cfg.Orchestrator.Routes {
		fieldBytes := len(route.Name) + len(route.Source) + len(route.Working) + len(route.StaleAfter)
		if fieldBytes > 8<<10 || routeBytes+fieldBytes > maxHeartbeatRouteBytes {
			return "", errors.New("heartbeat route summary exceeds bounded input limit")
		}
		allowed, err := joinBounded(route.AllowedNext, ",", (8<<10)-fieldBytes)
		if err != nil {
			return "", errors.New("heartbeat route summary exceeds bounded input limit")
		}
		fieldBytes += len(allowed)
		if fieldBytes > 8<<10 || routeBytes+fieldBytes > maxHeartbeatRouteBytes {
			return "", errors.New("heartbeat route summary exceeds bounded input limit")
		}
		line := fmt.Sprintf("- %s: source=%s, working=%s, allowed=%s, stale_after=%s", route.Name, route.Source, route.Working, allowed, route.StaleAfter)
		routeBytes += len(line) + 1
		routes = append(routes, line)
	}
	prompt := string(data)
	prompt = strings.ReplaceAll(prompt, "{{EXECUTION_ID}}", executionID)
	prompt = strings.ReplaceAll(prompt, "{{NOW_UTC}}", now.Format(time.RFC3339))
	prompt = strings.ReplaceAll(prompt, "{{ROUTES}}", strings.Join(routes, "\n"))
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	prompt = strings.ReplaceAll(prompt, "{{SPYNEL_EXECUTABLE}}", filepath.ToSlash(executable))
	prompt = appendTaskReviewModeInstruction(prompt, m.harnessSettings().Reviews)
	prompt, err = instructions.Append(prompt, m.Config.StatePath(), instructions.Heartbeat)
	if err != nil {
		return "", err
	}
	if len(prompt) > maxHeartbeatPromptBytes {
		return "", errors.New("rendered heartbeat prompt exceeds 128 KiB")
	}
	return prompt, nil
}

func parseSemanticHeartbeatResult(raw, executionID string, started time.Time) (semanticHeartbeatResult, error) {
	if len(raw) > maxHeartbeatOutputBytes {
		return semanticHeartbeatResult{}, errors.New("heartbeat result exceeds 64 KiB")
	}
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```json") && strings.HasSuffix(raw, "```") {
		raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(raw, "```json"), "```"))
	}
	var result semanticHeartbeatResult
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, fmt.Errorf("malformed JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err != nil {
			return result, fmt.Errorf("malformed trailing JSON: %w", err)
		}
		return result, errors.New("multiple JSON values")
	}
	if result.Schema != semanticHeartbeatSchema || result.ExecutionID != executionID {
		return result, errors.New("stale or incompatible execution identity")
	}
	if !result.ObservedAt.Equal(started) {
		return result, errors.New("stale observed_at timestamp")
	}
	if result.Status != "healthy" && result.Status != "findings" && result.Status != "failed" {
		return result, errors.New("invalid status")
	}
	if result.Status == "healthy" && len(result.Findings) != 0 {
		return result, errors.New("healthy result contains findings")
	}
	if result.Status == "findings" && len(result.Findings) == 0 {
		return result, errors.New("findings result is empty")
	}
	if result.Status == "failed" && len(result.Findings) != 0 {
		return result, errors.New("failed result contains actionable findings")
	}
	if len(result.Findings) > 64 {
		return result, errors.New("too many findings")
	}
	for index := range result.Findings {
		if result.Findings[index].Action == "notify" {
			result.Findings[index].Notification = cleanNotificationLine(result.Findings[index].Notification)
		}
	}
	for _, finding := range result.Findings {
		if !semanticCategories[finding.Category] {
			return result, fmt.Errorf("invalid finding category %q", finding.Category)
		}
		if strings.TrimSpace(finding.WorkflowID) == "" || strings.TrimSpace(finding.Evidence) == "" {
			return result, errors.New("finding requires workflow_id and evidence")
		}
		if len(finding.Evidence) > 2048 || len(finding.Notification) > 800 || len(finding.WorkflowID) > 256 || len(finding.NotificationOrigin) > 512 {
			return result, errors.New("finding exceeds bounded field limits")
		}
		switch finding.Action {
		case "none", "request_reconcile", "request_recover", "request_requeue", "notify":
		default:
			return result, fmt.Errorf("invalid action %q", finding.Action)
		}
		hasNotification := strings.TrimSpace(finding.Notification) != "" || strings.TrimSpace(finding.NotificationOrigin) != ""
		if finding.Category == "healthy_or_progressing" && finding.Action != "none" {
			return result, errors.New("healthy or progressing finding must not act")
		}
		switch finding.Action {
		case "none":
			if hasNotification {
				return result, errors.New("none action contains notification fields")
			}
		case "notify":
			if strings.TrimSpace(finding.Notification) == "" || strings.TrimSpace(finding.NotificationOrigin) == "" {
				return result, errors.New("notify action requires notification and notification_origin")
			}
		case "request_reconcile":
			if hasNotification || (finding.Category != "inconsistent_durable_transition" && finding.Category != "review_phase_mismatch") {
				return result, errors.New("request_reconcile action is incompatible with finding")
			}
		case "request_recover":
			if hasNotification || (finding.Category != "stale_or_orphaned_claim" && finding.Category != "dead_job_live_lease" && finding.Category != "repeated_recovery") {
				return result, errors.New("request_recover action is incompatible with finding")
			}
		case "request_requeue":
			if hasNotification || finding.Category != "due_waiting_condition" {
				return result, errors.New("request_requeue action is incompatible with finding")
			}
		}
		if finding.NotificationOrigin != "" {
			if _, err := ParseOrigin(finding.NotificationOrigin); err != nil {
				return result, fmt.Errorf("invalid notification origin: %w", err)
			}
		}
	}
	return result, nil
}

func (m *Manager) recordSemanticHeartbeatResult(ctx context.Context, result semanticHeartbeatResult) error {
	term := m.beginSemanticHeartbeatTerm()
	defer m.endSemanticHeartbeatTerm(term)
	return m.recordSemanticHeartbeatResultForTerm(ctx, result, term)
}

func (m *Manager) recordSemanticHeartbeatResultForTerm(ctx context.Context, result semanticHeartbeatResult, term uint64) error {
	// Serialize commit with term invalidation. The primary scheduler invalidates
	// its term under this same lock before ownership can hand off, so either an
	// authorized commit finishes first or every side effect is rejected.
	m.heartbeatCommit.Lock()
	defer m.heartbeatCommit.Unlock()
	if m.heartbeatTerm != term {
		return errors.New("semantic heartbeat primary term is stale")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	statePath := m.Config.StatePath("runtime", "semantic-heartbeat.json")
	state := semanticHeartbeatState{Findings: map[string]semanticFindingState{}}
	if data, err := readFileLimit(statePath, maxHeartbeatStateBytes, "semantic heartbeat state"); err == nil {
		if err := json.Unmarshal(data, &state); err != nil {
			return fmt.Errorf("decode semantic heartbeat state: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if state.Findings == nil {
		state.Findings = map[string]semanticFindingState{}
	}
	state.LastAudit = semanticAuditDiagnostic{ExecutionID: result.ExecutionID, ObservedAt: result.ObservedAt, Status: result.Status, Findings: map[string]int{}}
	actionWorkflowIDs := map[string]bool{}
	for _, finding := range result.Findings {
		if finding.Action != "none" {
			actionWorkflowIDs[finding.WorkflowID] = true
		}
	}
	actionDocuments, err := m.semanticDocuments(actionWorkflowIDs)
	if err != nil {
		return err
	}
	requestManagerScan := false
	seen := map[string]bool{}
	for _, finding := range result.Findings {
		if err := ctx.Err(); err != nil {
			return err
		}
		state.LastAudit.Findings[finding.Category]++
		identityHash := sha256.Sum256([]byte(finding.Category + "\x00" + finding.WorkflowID))
		identity := hex.EncodeToString(identityHash[:16])
		seen[identity] = true
		evidenceHashBytes := sha256.Sum256([]byte(finding.Evidence))
		evidenceHash := hex.EncodeToString(evidenceHashBytes[:16])
		tracked := state.Findings[identity]
		newFinding := tracked.FirstSeen.IsZero()
		evidenceChanged := tracked.EvidenceHash != "" && tracked.EvidenceHash != evidenceHash
		if tracked.FirstSeen.IsZero() {
			tracked.FirstSeen = result.ObservedAt
		}
		tracked.LastSeen = result.ObservedAt
		tracked.Occurrences++
		tracked.EvidenceHash = evidenceHash
		switch finding.Action {
		case "request_reconcile", "request_recover", "request_requeue":
			if matched, exists := actionDocuments[finding.WorkflowID]; exists {
				if finding.Action == "request_requeue" {
					due, wakeErr := scheduledWake(matched.Document, result.ObservedAt)
					if stringField(matched.Document, "status") != "waiting" || wakeErr != nil || !due {
						m.log("semantic heartbeat requeue rejected: workflow has no due validated waiting condition")
						break
					}
				}
				if newFinding || evidenceChanged {
					note := fmt.Sprintf("Semantic heartbeat recorded %s and requested %s through Spynel's serialized recovery path; no direct agent-authored state mutation was trusted.", strings.ReplaceAll(finding.Category, "_", " "), strings.TrimPrefix(strings.ReplaceAll(finding.Action, "_", " "), "request "))
					if err := updateDocumentProgress(matched.Path, result.ObservedAt, note); err != nil {
						return fmt.Errorf("journal semantic heartbeat repair before recovery: %w", err)
					}
				}
				requestManagerScan = true
			} else {
				m.log("semantic heartbeat repair rejected: workflow is absent or ambiguous")
			}
		}
		if finding.Action == "notify" {
			matched, exists := actionDocuments[finding.WorkflowID]
			origin, authorized := authorizedSemanticOriginFromDocument(matched.Document, exists, finding.NotificationOrigin)
			if !authorized || m.AuthorizeNotificationOrigin == nil || m.AuthorizeNotificationOrigin(origin) != nil {
				m.log("semantic heartbeat notification rejected: origin is not authorized")
			} else {
				if err := ctx.Err(); err != nil {
					return err
				}
				eventKey := fmt.Sprintf("semantic-heartbeat:%s:%s", result.ExecutionID, identity)
				status := stringField(matched.Document, "status")
				entry, err := m.Outbox.Enqueue(eventKey, status, finding.NotificationOrigin, finding.Notification)
				if err != nil {
					return err
				}
				note := "Semantic heartbeat queued this proactive notification: " + entry.Message
				if err := updateDocumentProgress(matched.Path, result.ObservedAt, note); err != nil {
					return fmt.Errorf("journal semantic heartbeat notification: %w", err)
				}
				requestManagerScan = true
			}
		}
		state.Findings[identity] = tracked
	}
	cutoff := result.ObservedAt.Add(-30 * 24 * time.Hour)
	for identity, tracked := range state.Findings {
		if (result.Status == "healthy" || result.Status == "findings") && !seen[identity] {
			delete(state.Findings, identity)
		} else if tracked.LastSeen.Before(cutoff) {
			delete(state.Findings, identity)
		}
	}
	pruneSemanticFindingState(state.Findings, maxHeartbeatStateEntries)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if len(data)+1 > maxHeartbeatStateBytes {
		return errors.New("semantic heartbeat state exceeds 1 MiB")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := fsx.AtomicWriteFile(statePath, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if requestManagerScan {
		m.requestScan()
	}
	return nil
}

func (m *Manager) beginSemanticHeartbeatTerm() uint64 {
	m.heartbeatCommit.Lock()
	defer m.heartbeatCommit.Unlock()
	m.heartbeatTerm++
	return m.heartbeatTerm
}

func (m *Manager) endSemanticHeartbeatTerm(term uint64) {
	m.heartbeatCommit.Lock()
	defer m.heartbeatCommit.Unlock()
	if m.heartbeatTerm == term {
		m.heartbeatTerm++
	}
}

func authorizedSemanticOriginFromDocument(document Document, exists bool, proposed string) (Origin, bool) {
	if !exists {
		return Origin{}, false
	}
	policy, err := NotificationFromDocument(document)
	status := stringField(document, "status")
	if err != nil || !policy.Enabled || !policy.Outcomes[status] {
		return Origin{}, false
	}
	expected := policy.Origin.Channel + "/" + policy.Origin.Conversation
	if proposed != expected {
		return Origin{}, false
	}
	return policy.Origin, true
}

type semanticDocumentMatch struct {
	Document Document
	Path     string
}

func (m *Manager) semanticDocuments(workflowIDs map[string]bool) (map[string]semanticDocumentMatch, error) {
	matched := make(map[string]semanticDocumentMatch, len(workflowIDs))
	ambiguous := map[string]bool{}
	if len(workflowIDs) == 0 {
		return matched, nil
	}
	examined := 0
	cfg := m.runtimeSnapshot()
	for routeIndex, route := range cfg.Orchestrator.Routes {
		if routeIndex >= 128 {
			return nil, errors.New("semantic workflow lookup exceeds route limit")
		}
		base := filepath.Dir(cfg.Resolve(route.Source))
		for statusIndex, status := range route.AllowedNext {
			if statusIndex >= 128 || examined >= 2048 {
				return nil, errors.New("semantic workflow lookup exceeds entry limit")
			}
			directory, err := os.Open(filepath.Join(base, status))
			if err != nil {
				continue
			}
			entries, err := directory.ReadDir(2048 - examined + 1)
			_ = directory.Close()
			if err != nil && !errors.Is(err, io.EOF) {
				continue
			}
			if len(entries) > 2048-examined {
				return nil, errors.New("semantic workflow lookup exceeds entry limit")
			}
			examined += len(entries)
			for _, entry := range entries {
				if entry.IsDir() || entry.Name() == "AGENTS.md" || !strings.HasSuffix(strings.ToLower(entry.Name()), ".md") {
					continue
				}
				document, err := readSemanticDocument(filepath.Join(base, status, entry.Name()))
				if err != nil {
					continue
				}
				id := documentID(document)
				if !workflowIDs[id] || ambiguous[id] {
					continue
				}
				if _, duplicate := matched[id]; duplicate {
					delete(matched, id)
					ambiguous[id] = true
					continue
				}
				matched[id] = semanticDocumentMatch{Document: document, Path: filepath.Join(base, status, entry.Name())}
			}
		}
	}
	return matched, nil
}

func readFileLimit(path string, limit int64, description string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds read limit", description)
	}
	return data, nil
}

func joinBounded(values []string, separator string, limit int) (string, error) {
	if limit < 0 {
		return "", errors.New("negative join limit")
	}
	var result strings.Builder
	for index, value := range values {
		required := len(value)
		if index > 0 {
			required += len(separator)
		}
		if required > limit-result.Len() {
			return "", errors.New("joined value exceeds limit")
		}
		if index > 0 {
			result.WriteString(separator)
		}
		result.WriteString(value)
	}
	return result.String(), nil
}

func pruneSemanticFindingState(findings map[string]semanticFindingState, limit int) {
	for len(findings) > limit {
		var oldestID string
		var oldest time.Time
		for identity, finding := range findings {
			if oldestID == "" || finding.LastSeen.Before(oldest) || (finding.LastSeen.Equal(oldest) && identity < oldestID) {
				oldestID = identity
				oldest = finding.LastSeen
			}
		}
		delete(findings, oldestID)
	}
}

func readSemanticDocument(path string) (Document, error) {
	file, err := os.Open(path)
	if err != nil {
		return Document{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return Document{}, err
	}
	if len(data) > 1<<20 {
		return Document{}, errors.New("workflow document exceeds heartbeat inspection limit")
	}
	return ParseDocument(data)
}
