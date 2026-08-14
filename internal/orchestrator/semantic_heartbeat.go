package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/instructions"
)

const (
	semanticHeartbeatSession = "orchestrator:semantic-heartbeat"
	maxHeartbeatPromptBytes  = 128 << 10
	maxHeartbeatRouteBytes   = 64 << 10
)

type heartbeatManualRequest struct {
	result chan bool
}

// TriggerSemanticHeartbeat asks the primary scheduler to start one audit. The
// scheduler is the single admission point for timed and manual runs, so both
// share the provider non-overlap fence and fixed-delay rescheduling.
func (m *Manager) TriggerSemanticHeartbeat(ctx context.Context) (bool, error) {
	if !m.primaryOwned.Load() || !m.heartbeatSchedulerActive.Load() {
		return false, errors.New("semantic heartbeat scheduler is unavailable")
	}
	result := make(chan bool, 1)
	select {
	case m.heartbeatManual <- heartbeatManualRequest{result: result}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	select {
	case started := <-result:
		return started, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (m *Manager) runSemanticHeartbeat(ctx context.Context) {
	m.heartbeatSchedulerActive.Store(true)
	defer m.heartbeatSchedulerActive.Store(false)
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
	startAudit := func() bool {
		if auditActive || m.semanticHeartbeatProviderInFlight() || !termActive || !m.orchestratorEnabled.Load() || m.heartbeatMinutes.Load() <= 0 {
			return false
		}
		m.heartbeatCommit.Lock()
		defer m.heartbeatCommit.Unlock()
		if m.heartbeatTerm != term || auditActive || m.semanticHeartbeatProviderInFlight() || !m.orchestratorEnabled.Load() || m.heartbeatMinutes.Load() <= 0 {
			return false
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
		return true
	}
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
		case request := <-m.heartbeatManual:
			request.result <- startAudit()
		case <-ticks:
			if !armed || auditActive {
				continue
			}
			_ = startAudit()
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
	releaseProvider := func() {
		releasedAt := m.semanticHeartbeatNow()
		m.heartbeatRunningTerm.CompareAndSwap(term, 0)
		m.heartbeatProviderMu.Lock()
		m.heartbeatProviderReleasedAt = releasedAt
		m.heartbeatProviderActive.Store(false)
		close(providerDoneSignal)
		m.heartbeatProviderMu.Unlock()
	}
	providerOwnsFence := false
	defer func() {
		if !providerOwnsFence {
			releaseProvider()
		}
	}()
	now := time.Now().UTC().Truncate(time.Second)
	if m.heartbeatNow != nil {
		now = m.heartbeatNow().UTC().Truncate(time.Second)
	}
	executionID := fmt.Sprintf("heartbeat-%d-%s", now.Unix(), randomSuffix())
	timeout := m.heartbeatTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	prompt, err := m.semanticHeartbeatPrompt(executionID, now)
	if err != nil {
		m.log("semantic heartbeat prompt rejected: " + err.Error())
		providerStarted = false
		return
	}
	lease := Lease{DocumentType: "heartbeat", Route: "semantic-heartbeat", SessionKey: semanticHeartbeatSession, State: "working", Phase: "semantic_heartbeat", StartedAt: now, HeartbeatAt: now}
	message := config.PrependAgentPrefix(m.harnessSettings().HeartbeatAgentPrefix, prompt)
	resultReady := make(chan ordinaryAgentResult, 1)
	providerOwnsFence = true
	go func() {
		result := m.runOrdinaryAgentTurn(parent, lease, "semantic workflow heartbeat", message, timeout, 0)
		if !result.providerSent || !m.Harness.IsActive(semanticHeartbeatSession) {
			releaseProvider()
			resultReady <- result
			return
		}
		resultReady <- result
		// Timeout or cancellation ends the ordinary job. Cadence remains
		// fenced until an adapter that ignored interruption actually releases.
		for result.providerSent && m.Harness.IsActive(semanticHeartbeatSession) {
			select {
			case <-parent.Done():
				releaseProvider()
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
		releaseProvider()
	}()
	result := <-resultReady
	if !result.admitted {
		m.log("semantic heartbeat job admission failed: " + result.err.Error())
		providerStarted = false
		return
	}
	if result.err != nil {
		m.log("semantic heartbeat job ended with an error: " + result.err.Error())
	}
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
	prompt = instructions.InjectScopeDiscipline(prompt)
	prompt, err = instructions.Append(prompt, m.Config.StatePath(), instructions.Heartbeat)
	if err != nil {
		return "", err
	}
	if len(prompt) > maxHeartbeatPromptBytes {
		return "", errors.New("rendered heartbeat prompt exceeds 128 KiB")
	}
	return prompt, nil
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
