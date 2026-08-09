package orchestrator

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/agent0ai/spynel/internal/config"
)

const (
	maxStatusFolders          = 32
	maxStatusDocuments        = 4096
	maxStatusDirectoryEntries = 8192
	maxStatusDocumentSize     = 1 << 20
	maxStatusDiagnostics      = 8
)

// WorkStatus is the bounded durable-work and semantic-scheduler view used by
// every structured and rendered status surface.
type WorkStatus struct {
	TasksActive      int       `json:"tasks_active"`
	TasksWaiting     int       `json:"tasks_waiting"`
	GoalsActive      int       `json:"goals_active"`
	CountDiagnostics []string  `json:"count_diagnostics,omitempty"`
	HeartbeatState   string    `json:"heartbeat_state"`
	NextHeartbeatAt  time.Time `json:"next_heartbeat_at,omitempty"`
}

// AddCountDiagnostic appends one diagnostic using the same count, control,
// and length bounds as the durable census. Status collectors outside this
// package must use this boundary rather than exposing raw errors directly.
func (s *WorkStatus) AddCountDiagnostic(value string) {
	addStatusDiagnostic(&s.CountDiagnostics, value)
}

// WorkStatus counts files in configured built-in active route folders. A
// corrupt document in an active folder still counts so broken work cannot
// disappear. Unreadable folders and enumeration caps produce a bounded
// diagnostic and make the reported value an explicit lower bound.
func (m *Manager) WorkStatus() WorkStatus {
	status := WorkStatus{}
	diagnostics := make([]string, 0, maxStatusDiagnostics)
	for _, route := range m.Config.Orchestrator.Routes {
		switch route.Name {
		case "tasks":
			status.TasksActive, status.TasksWaiting = m.countActiveRoute(route, map[string]bool{"done": true, "failed": true, "cancelled": true}, &diagnostics)
		case "goals":
			status.GoalsActive, _ = m.countActiveRoute(route, map[string]bool{"done": true, "abandoned": true}, &diagnostics)
		}
	}
	status.CountDiagnostics = diagnostics
	status.HeartbeatState, status.NextHeartbeatAt = m.semanticHeartbeatSchedule()
	return status
}

func (m *Manager) semanticHeartbeatSchedule() (string, time.Time) {
	if !m.orchestratorEnabled.Load() || m.heartbeatMinutes.Load() == 0 {
		return "disabled", time.Time{}
	}
	if !m.primaryOwned.Load() {
		return "not_primary", time.Time{}
	}
	m.heartbeatStatusMu.RLock()
	owned, ownedTerm, next := m.heartbeatOwned, m.heartbeatOwnedTerm, m.heartbeatNext
	m.heartbeatStatusMu.RUnlock()
	if !owned {
		return "unavailable", time.Time{}
	}
	if ownedTerm != 0 && m.heartbeatRunningTerm.Load() == ownedTerm {
		return "running", time.Time{}
	}
	if next.IsZero() {
		return "unavailable", time.Time{}
	}
	return "scheduled", next
}

func (m *Manager) countActiveRoute(route config.Route, terminal map[string]bool, diagnostics *[]string) (int, int) {
	base := filepath.Dir(m.Config.Resolve(route.Source))
	statuses := make(map[string]bool)
	for _, value := range append([]string{filepath.Base(route.Source), filepath.Base(route.Working)}, route.AllowedNext...) {
		value = strings.TrimSpace(value)
		if value == "" || value == "." || filepath.Base(value) != value || terminal[value] {
			continue
		}
		statuses[value] = true
	}
	names := make([]string, 0, len(statuses))
	for name := range statuses {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > maxStatusFolders {
		addStatusDiagnostic(diagnostics, fmt.Sprintf("%s count is a lower bound: status-folder limit reached", route.Name))
		names = names[:maxStatusFolders]
	}
	count, waiting, inspected := 0, 0, 0
	for _, statusName := range names {
		entries, truncated, err := readDirectoryEntries(filepath.Join(base, statusName), maxStatusDirectoryEntries)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			addStatusDiagnostic(diagnostics, fmt.Sprintf("%s count is a lower bound: %s is unreadable", route.Name, statusName))
			continue
		}
		if truncated {
			addStatusDiagnostic(diagnostics, fmt.Sprintf("%s count is a lower bound: %s entry limit reached", route.Name, statusName))
		}
		for _, entry := range entries {
			if entry.IsDir() || entry.Name() == "AGENTS.md" || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
				continue
			}
			if inspected >= maxStatusDocuments {
				addStatusDiagnostic(diagnostics, fmt.Sprintf("%s count is a lower bound: document limit reached", route.Name))
				return count, waiting
			}
			inspected++
			count++
			if route.Name == "tasks" && statusName == "waiting" {
				waiting++
			}
			path := filepath.Join(base, statusName, entry.Name())
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() {
				addStatusDiagnostic(diagnostics, fmt.Sprintf("%s/%s counts active but is not a readable regular file", statusName, entry.Name()))
				continue
			}
			data, err := readStatusDocument(path)
			if err != nil {
				addStatusDiagnostic(diagnostics, fmt.Sprintf("%s/%s counts active but is unreadable", statusName, entry.Name()))
				continue
			}
			document, err := ParseDocument(data)
			if err != nil {
				addStatusDiagnostic(diagnostics, fmt.Sprintf("%s/%s counts active but has invalid front matter", statusName, entry.Name()))
				continue
			}
			if durable, _ := document.FrontMatter["status"].(string); durable != statusName {
				addStatusDiagnostic(diagnostics, fmt.Sprintf("%s/%s counts active but front matter says %q", statusName, entry.Name(), durable))
			}
		}
	}
	return count, waiting
}

func readDirectoryEntries(path string, limit int) ([]os.DirEntry, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, false, err
	}
	if !info.IsDir() {
		return nil, false, fmt.Errorf("not a directory")
	}
	directory, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer directory.Close()
	entries, err := directory.ReadDir(limit + 1)
	if err != nil && err != io.EOF {
		return nil, false, err
	}
	truncated := len(entries) > limit
	if truncated {
		entries = entries[:limit]
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, truncated, nil
}

func readStatusDocument(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("document is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxStatusDocumentSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxStatusDocumentSize {
		return nil, fmt.Errorf("document exceeds %d bytes", maxStatusDocumentSize)
	}
	return data, nil
}

func addStatusDiagnostic(diagnostics *[]string, value string) {
	if len(*diagnostics) < maxStatusDiagnostics {
		value = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, value)
		const maxRunes = 240
		if runes := []rune(value); len(runes) > maxRunes {
			value = string(runes[:maxRunes-1]) + "…"
		}
		*diagnostics = append(*diagnostics, value)
	}
}
