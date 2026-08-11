package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var nonterminalGoalStatuses = []string{"proposed", "planning", "active", "review", "reviewing", "waiting"}

// ArchiveTerminalTasks moves old terminal tasks into cold history while
// retaining the manager's scan boundary. A task linked to any nonterminal goal
// remains live goal evidence even after that individual task has settled.
func (m *Manager) ArchiveTerminalTasks(cutoff time.Time) (archived, protected, failed int) {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()

	cfg := m.runtimeSnapshot()
	taskRoute, ok := routeFromSnapshot(cfg.Orchestrator.Routes, "tasks")
	if !ok {
		return 0, 0, 1
	}
	goalRoute, ok := routeFromSnapshot(cfg.Orchestrator.Routes, "goals")
	if !ok {
		return 0, 0, 1
	}
	goalIDs, complete := nonterminalGoalIDs(filepath.Dir(cfg.Resolve(goalRoute.Source)))

	base := filepath.Dir(cfg.Resolve(taskRoute.Source))
	archive := filepath.Join(base, "archive")
	if err := os.MkdirAll(archive, 0o700); err != nil {
		return 0, 0, 1
	}
	archiveInfo, err := os.Lstat(archive)
	if err != nil || !archiveInfo.IsDir() || archiveInfo.Mode()&os.ModeSymlink != 0 {
		return 0, 0, 1
	}

	for _, status := range []string{"done", "failed", "cancelled"} {
		directory := filepath.Join(base, status)
		directoryInfo, statErr := os.Lstat(directory)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil || !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 {
			failed++
			continue
		}
		entries, err := os.ReadDir(directory)
		if err != nil {
			failed++
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || entry.Name() == "AGENTS.md" || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
				continue
			}
			path := filepath.Join(directory, entry.Name())
			lock, err := lockProviderTurn(path)
			if err != nil {
				failed++
				continue
			}
			a, p, f := archiveTerminalTask(path, filepath.Join(archive, entry.Name()), status, cutoff, goalIDs, complete)
			unlockProviderTurn(lock)
			archived += a
			protected += p
			failed += f
		}
	}
	return archived, protected, failed
}

func nonterminalGoalIDs(base string) (map[string]bool, bool) {
	ids := map[string]bool{}
	complete := true
	for _, status := range nonterminalGoalStatuses {
		directory := filepath.Join(base, status)
		info, err := os.Lstat(directory)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			complete = false
			continue
		}
		entries, err := os.ReadDir(directory)
		if err != nil {
			complete = false
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || entry.Name() == "AGENTS.md" || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
				continue
			}
			path := filepath.Join(directory, entry.Name())
			fileInfo, err := os.Lstat(path)
			if err != nil || !fileInfo.Mode().IsRegular() {
				complete = false
				continue
			}
			document, err := ReadDocument(path)
			if err != nil || strings.TrimSpace(stringField(document, "status")) != status || documentID(document) == "" {
				complete = false
				continue
			}
			ids[documentID(document)] = true
		}
	}
	return ids, complete
}

func archiveTerminalTask(path, destination, status string, cutoff time.Time, activeGoalIDs map[string]bool, goalIndexComplete bool) (archived, protected, failed int) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return 0, 0, 1
	}
	document, err := ReadDocument(path)
	if err != nil {
		return 0, 0, 1
	}
	updated, err := retentionDocumentTime(document.FrontMatter["updated_at"])
	if err != nil || strings.TrimSpace(stringField(document, "status")) != status {
		return 0, 0, 1
	}
	if !updated.Before(cutoff) {
		return 0, 0, 0
	}
	rawGoalID, hasGoalID := document.FrontMatter["goal_id"]
	goalID := strings.TrimSpace(stringField(document, "goal_id"))
	if hasGoalID && (goalID == "" || rawGoalID == nil) {
		return 0, 0, 1
	}
	if goalID != "" && (!goalIndexComplete || activeGoalIDs[goalID]) {
		return 0, 1, 0
	}
	if _, err := os.Lstat(destination); err == nil || !os.IsNotExist(err) {
		return 0, 0, 1
	}
	if err := os.Rename(path, destination); err != nil {
		return 0, 0, 1
	}
	return 1, 0, 0
}

func retentionDocumentTime(value any) (time.Time, error) {
	switch typed := value.(type) {
	case string:
		return time.Parse(time.RFC3339, strings.TrimSpace(typed))
	case time.Time:
		return typed.UTC(), nil
	default:
		return time.Time{}, fmt.Errorf("updated_at is not an RFC 3339 timestamp")
	}
}
