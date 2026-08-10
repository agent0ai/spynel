package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

const maxWorkflowStepRunes = 320

// WorkflowItem is the bounded, non-secret durable state used by the shared
// task and goal list commands. Folder state remains authoritative when front
// matter is missing or disagrees with the document's location.
type WorkflowItem struct {
	Kind               string
	ID                 string
	Title              string
	Status             string
	FileName           string
	Step               string
	UpdatedAt          time.Time
	CreatedAt          time.Time
	WakeAt             time.Time
	NextReviewAt       time.Time
	ProviderIterations int
	Attempt            int
	ReviewAttempt      int
	PlanningAttempt    int
	Round              int
	RoundTasks         int
	GoalRound          int
	ReviewRequired     bool
	HasReviewPolicy    bool
	DetailsAvailable   bool
}

// WorkflowInventory is a bounded durable task or goal census. Diagnostics are
// deliberately suitable for user-facing inspection and never contain full
// paths or arbitrary document content.
type WorkflowInventory struct {
	Items       []WorkflowItem
	Statuses    []string
	Diagnostics []string
}

// WorkflowItems reads configured built-in task or goal route folders without
// following document symlinks. It returns at most maxStatusDocuments entries.
func (m *Manager) WorkflowItems(kind string) WorkflowInventory {
	result := WorkflowInventory{}
	var routeFound bool
	cfg := m.runtimeSnapshot()
	for _, route := range cfg.Orchestrator.Routes {
		if route.Name != kind || (kind != "tasks" && kind != "goals") {
			continue
		}
		routeFound = true
		base := filepath.Dir(cfg.Resolve(route.Source))
		statuses := workflowRouteStatuses(route.Source, route.Working, route.AllowedNext)
		result.Statuses = append(result.Statuses, statuses...)
		inspected := 0
		for _, status := range statuses {
			entries, truncated, err := readDirectoryEntries(filepath.Join(base, status), maxStatusDirectoryEntries)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				addStatusDiagnostic(&result.Diagnostics, fmt.Sprintf("%s/%s is unreadable", kind, status))
				continue
			}
			if truncated {
				addStatusDiagnostic(&result.Diagnostics, fmt.Sprintf("%s/%s entry limit reached; results are incomplete", kind, status))
			}
			for _, entry := range entries {
				if entry.IsDir() || entry.Name() == "AGENTS.md" || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
					continue
				}
				if inspected >= maxStatusDocuments {
					addStatusDiagnostic(&result.Diagnostics, fmt.Sprintf("%s document limit reached; results are incomplete", kind))
					return result
				}
				inspected++
				path := filepath.Join(base, status, entry.Name())
				item := WorkflowItem{Kind: strings.TrimSuffix(kind, "s"), Status: status, FileName: entry.Name()}
				item.Title = strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
				info, statErr := os.Lstat(path)
				if statErr == nil {
					item.UpdatedAt = info.ModTime().UTC()
				}
				if statErr != nil || !info.Mode().IsRegular() {
					addStatusDiagnostic(&result.Diagnostics, fmt.Sprintf("%s/%s is not a readable regular file", status, entry.Name()))
					result.Items = append(result.Items, item)
					continue
				}
				data, readErr := readStatusDocument(path)
				if readErr != nil {
					addStatusDiagnostic(&result.Diagnostics, fmt.Sprintf("%s/%s is unreadable", status, entry.Name()))
					result.Items = append(result.Items, item)
					continue
				}
				document, parseErr := ParseDocument(data)
				if parseErr != nil {
					addStatusDiagnostic(&result.Diagnostics, fmt.Sprintf("%s/%s has invalid front matter", status, entry.Name()))
					result.Items = append(result.Items, item)
					continue
				}
				item.DetailsAvailable = true
				item.ID = boundedWorkflowText(stringField(document, "id"), 160)
				if title := boundedWorkflowText(stringField(document, "title"), 240); title != "" {
					item.Title = title
				}
				if durableStatus := stringField(document, "status"); durableStatus != status {
					addStatusDiagnostic(&result.Diagnostics, fmt.Sprintf("%s/%s is listed as %s but front matter says %q", status, entry.Name(), status, boundedWorkflowText(durableStatus, 80)))
				}
				if updated, ok := frontMatterTime(document.FrontMatter["updated_at"]); ok {
					item.UpdatedAt = updated.UTC()
				} else {
					addStatusDiagnostic(&result.Diagnostics, fmt.Sprintf("%s/%s has no valid updated_at; file time was used", status, entry.Name()))
				}
				item.CreatedAt, _ = frontMatterTime(document.FrontMatter["created_at"])
				item.WakeAt, _ = frontMatterTime(document.FrontMatter["wake_at"])
				item.NextReviewAt, _ = frontMatterTime(document.FrontMatter["next_review_at"])
				item.ProviderIterations = frontMatterNonNegativeInt(document.FrontMatter["provider_iterations"])
				item.Attempt = frontMatterNonNegativeInt(document.FrontMatter["attempt"])
				item.ReviewAttempt = frontMatterNonNegativeInt(document.FrontMatter["review_attempt"])
				item.PlanningAttempt = frontMatterNonNegativeInt(document.FrontMatter["planning_attempt"])
				item.Round = frontMatterNonNegativeInt(document.FrontMatter["round"])
				item.GoalRound = frontMatterNonNegativeInt(document.FrontMatter["goal_round"])
				item.RoundTasks = workflowSliceLength(document.FrontMatter["round_task_ids"])
				if policy, exists := document.FrontMatter["review_required"]; exists {
					item.ReviewRequired, item.HasReviewPolicy = policy.(bool)
				}
				item.Step = latestWorkflowProgress(document.Body)
				if item.Step == "" && status == "waiting" {
					item.Step = boundedWorkflowText(stringField(document, "waiting_for"), maxWorkflowStepRunes)
				}
				result.Items = append(result.Items, item)
			}
		}
	}
	if !routeFound {
		addStatusDiagnostic(&result.Diagnostics, fmt.Sprintf("built-in %s route is not configured", kind))
	}
	return result
}

func workflowRouteStatuses(source, working string, allowed []string) []string {
	seen := map[string]bool{}
	for _, value := range append([]string{filepath.Base(source), filepath.Base(working)}, allowed...) {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || value == "." || filepath.Base(value) != value {
			continue
		}
		seen[value] = true
	}
	statuses := make([]string, 0, len(seen))
	for status := range seen {
		statuses = append(statuses, status)
	}
	sort.Strings(statuses)
	return statuses
}

func latestWorkflowProgress(body string) string {
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
		} else if len(entries) > 0 && len(line) > 0 && unicode.IsSpace(rune(line[0])) {
			entries[len(entries)-1] += " " + trimmed
		}
	}
	if len(entries) == 0 {
		return ""
	}
	return boundedWorkflowText(entries[len(entries)-1], maxWorkflowStepRunes)
}

func boundedWorkflowText(value string, limit int) string {
	value = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)), " ")
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}

func workflowSliceLength(value any) int {
	switch typed := value.(type) {
	case []any:
		return len(typed)
	case []string:
		return len(typed)
	default:
		return 0
	}
}
