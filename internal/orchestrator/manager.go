package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/frdel/spynel/internal/config"
	"github.com/frdel/spynel/internal/core"
	"github.com/frdel/spynel/internal/extensions"
	"github.com/frdel/spynel/internal/fsx"
	"github.com/frdel/spynel/internal/harness"
	"github.com/frdel/spynel/internal/shortid"
)

type Lease struct {
	ID            string    `json:"id"`
	Route         string    `json:"route"`
	File          string    `json:"file"`
	SessionKey    string    `json:"session_key"`
	ThreadID      string    `json:"thread_id,omitempty"`
	State         string    `json:"state"`
	StartedAt     time.Time `json:"started_at"`
	HeartbeatAt   time.Time `json:"heartbeat_at"`
	RecoveryCount int       `json:"recovery_count"`
	LastError     string    `json:"last_error,omitempty"`
}

type Manager struct {
	Config      config.Config
	Harness     harness.Harness
	Hooks       extensions.Runner
	Log         func(string)
	JobStarted  func(sessionKey, description string) int
	JobFinished func(id int)

	mu       sync.Mutex
	inflight map[string]bool
	jobs     sync.WaitGroup
	sem      chan struct{}
}

func New(cfg config.Config, target harness.Harness, hooks extensions.Runner) *Manager {
	parallel := cfg.Orchestrator.MaxParallel
	if parallel <= 0 {
		parallel = 1
	}
	return &Manager{Config: cfg, Harness: target, Hooks: hooks, inflight: map[string]bool{}, sem: make(chan struct{}, parallel)}
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
	if !m.Config.Orchestrator.Enabled {
		return nil
	}
	if err := os.MkdirAll(m.leaseDirectory(), 0o700); err != nil {
		return err
	}
	if err := m.ensureRouteDirectories(); err != nil {
		return err
	}
	if err := m.recoverStale(ctx); err != nil {
		return err
	}
	for _, route := range m.Config.Orchestrator.Routes {
		if err := m.scanRoute(ctx, route); err != nil {
			return fmt.Errorf("route %s: %w", route.Name, err)
		}
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
			ID: key, Route: route.Name, File: target, SessionKey: "orchestrator:" + route.Name + ":" + documentID,
			State: "processing", StartedAt: time.Now().UTC(), HeartbeatAt: time.Now().UTC(),
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
				finish()
				if m.Config.Extensions.Enabled {
					_, hookErr := m.Hooks.Run(context.Background(), "task.completed", map[string]any{"route": route.Name, "file": current.File, "thread_id": current.ThreadID, "error": current.LastError})
					if hookErr != nil {
						current.LastError = hookErr.Error()
						m.log("task.completed hook: " + hookErr.Error())
					}
				}
				if _, statErr := os.Stat(current.File); os.IsNotExist(statErr) {
					_ = os.Remove(m.leasePath(current.ID))
					return
				}
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
			_ = os.Remove(m.leasePath(lease.ID))
			continue
		}
		route, ok := m.route(lease.Route)
		if !ok {
			continue
		}
		if now.Sub(lease.HeartbeatAt) < route.StaleDuration() || m.isInflight(lease.ID) {
			continue
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
	}
	prompt := string(data)
	for from, to := range replacements {
		prompt = strings.ReplaceAll(prompt, from, to)
	}
	return prompt, nil
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
