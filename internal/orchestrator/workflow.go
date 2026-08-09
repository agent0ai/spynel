package orchestrator

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	phaseTaskImplementation = "task_implementation"
	phaseTaskReview         = "task_review"
	phaseGoalPlanning       = "goal_planning"
	phaseGoalReview         = "goal_review"
)

var taskSettledStatuses = map[string]bool{"done": true, "failed": true, "cancelled": true}

func routeBase(source string) string { return filepath.Dir(source) }

func statusPath(base, status, name string) string {
	return filepath.Join(base, status, name)
}

func documentID(document Document) string {
	id, _ := document.FrontMatter["id"].(string)
	return strings.TrimSpace(id)
}

func stringField(document Document, name string) string {
	value, _ := document.FrontMatter[name].(string)
	return strings.TrimSpace(value)
}

func phaseAttemptField(phase string) string {
	switch phase {
	case phaseTaskReview, phaseGoalReview:
		return "review_attempt"
	case phaseGoalPlanning:
		return "planning_attempt"
	default:
		return "attempt"
	}
}

func phaseClaimedStatus(phase string) string {
	switch phase {
	case phaseTaskImplementation:
		return "working"
	case phaseTaskReview, phaseGoalReview:
		return "reviewing"
	case phaseGoalPlanning:
		return "planning"
	default:
		return "working"
	}
}

func phaseSessionKey(routeName, id, phase string, attempt int) string {
	if attempt < 1 {
		attempt = 1
	}
	return fmt.Sprintf("orchestrator:%s:%s:%s:%d", routeName, phase, id, attempt)
}

func validSuccessCriteria(document Document) error {
	raw, ok := document.FrontMatter["success_criteria"]
	if !ok {
		return errors.New("success_criteria is required")
	}
	criteria, ok := raw.([]any)
	if !ok {
		// Documents created in-process can retain the concrete map slice until
		// they are serialized and read again.
		if typed, typedOK := raw.([]map[string]any); typedOK {
			criteria = make([]any, len(typed))
			for i := range typed {
				criteria[i] = typed[i]
			}
		} else {
			return errors.New("success_criteria must be a list")
		}
	}
	if len(criteria) == 0 {
		return errors.New("success_criteria cannot be empty")
	}
	seen := map[string]bool{}
	for i, item := range criteria {
		criterion, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("success_criteria[%d] must be a mapping", i)
		}
		id, _ := criterion["id"].(string)
		condition, _ := criterion["condition"].(string)
		evidence, _ := criterion["evidence_required"].(string)
		id = strings.TrimSpace(id)
		if id == "" || strings.TrimSpace(condition) == "" || strings.TrimSpace(evidence) == "" {
			return fmt.Errorf("success_criteria[%d] requires id, condition, and evidence_required", i)
		}
		if seen[id] {
			return fmt.Errorf("duplicate success criterion %q", id)
		}
		seen[id] = true
	}
	return nil
}

func goalReviewProvesDone(document Document) error {
	if err := validSuccessCriteria(document); err != nil {
		return err
	}
	raw, ok := document.FrontMatter["last_review"].(map[string]any)
	if !ok {
		return errors.New("last_review is required")
	}
	verdict, _ := raw["verdict"].(string)
	satisfied, _ := raw["criteria_satisfied"].(bool)
	if strings.TrimSpace(verdict) != "done" || !satisfied {
		return errors.New("last_review must prove a done verdict and all criteria satisfied")
	}
	if numberValue(raw["round"]) != numberValue(document.FrontMatter["round"]) {
		return errors.New("last_review.round must match the current goal round")
	}
	if reviewedAt, _ := raw["reviewed_at"].(string); strings.TrimSpace(reviewedAt) == "" {
		return errors.New("last_review.reviewed_at is required")
	} else if _, err := time.Parse(time.RFC3339, reviewedAt); err != nil {
		return errors.New("last_review.reviewed_at must be RFC 3339")
	}
	return nil
}

func scheduledWake(document Document, now time.Time) (bool, error) {
	for _, field := range []string{"wake_at", "not_before", "next_dispatch_at"} {
		value, exists := document.FrontMatter[field]
		if !exists {
			continue
		}
		var when time.Time
		switch typed := value.(type) {
		case string:
			parsed, err := time.Parse(time.RFC3339, typed)
			if err != nil {
				return false, fmt.Errorf("%s must be RFC 3339", field)
			}
			when = parsed
		case time.Time:
			when = typed
		default:
			return false, fmt.Errorf("%s must be an RFC 3339 timestamp", field)
		}
		return !when.After(now), nil
	}
	return false, nil
}

func checkpointDue(document Document, now time.Time) (bool, error) {
	value, exists := document.FrontMatter["next_review_at"]
	if !exists {
		return false, nil
	}
	var when time.Time
	switch typed := value.(type) {
	case string:
		parsed, err := time.Parse(time.RFC3339, typed)
		if err != nil {
			return false, errors.New("next_review_at must be RFC 3339")
		}
		when = parsed
	case time.Time:
		when = typed
	default:
		return false, errors.New("next_review_at must be an RFC 3339 timestamp")
	}
	return !when.After(now), nil
}

type linkedTask struct {
	ID     string
	Path   string
	Status string
}

func linkedRoundTasks(tasksBase, goalID string, round int) ([]linkedTask, error) {
	statuses := []string{"todo", "working", "review", "reviewing", "waiting", "done", "failed", "cancelled"}
	var found []linkedTask
	for _, status := range statuses {
		directory := filepath.Join(tasksBase, status)
		entries, err := os.ReadDir(directory)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".md") || entry.Name() == "AGENTS.md" {
				continue
			}
			path := filepath.Join(directory, entry.Name())
			document, err := ReadDocument(path)
			if err != nil {
				continue
			}
			if stringField(document, "goal_id") == goalID && numberValue(document.FrontMatter["goal_round"]) == round {
				found = append(found, linkedTask{ID: documentID(document), Path: path, Status: status})
			}
		}
	}
	return found, nil
}

func moveDocument(path, target, status string, now time.Time) error {
	return moveDocumentWithProgress(path, target, status, now, "")
}

func moveDocumentWithProgress(path, target, status string, now time.Time, note string) error {
	lock, err := lockProviderTurn(path)
	if err != nil {
		return err
	}
	defer unlockProviderTurn(lock)
	document, err := ReadDocument(path)
	if err != nil {
		return err
	}
	document.FrontMatter["status"] = status
	document.FrontMatter["updated_at"] = now.UTC().Format(time.RFC3339)
	if note != "" {
		appendProgress(&document, now, note)
	}
	if err := WriteDocument(path, document); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	return os.Rename(path, target)
}

func updateDocumentProgress(path string, now time.Time, note string) error {
	lock, err := lockProviderTurn(path)
	if err != nil {
		return err
	}
	defer unlockProviderTurn(lock)
	document, err := ReadDocument(path)
	if err != nil {
		return err
	}
	document.FrontMatter["updated_at"] = now.UTC().Format(time.RFC3339)
	appendProgress(&document, now, note)
	return WriteDocument(path, document)
}

func appendProgress(document *Document, now time.Time, note string) {
	note = strings.Join(strings.Fields(note), " ")
	if runes := []rune(note); len(runes) > 1000 {
		note = string(runes[:999]) + "…"
	}
	entry := "- " + now.UTC().Format(time.RFC3339) + " — " + note
	body := strings.ReplaceAll(document.Body, "\r\n", "\n")
	lines := strings.Split(body, "\n")
	for _, line := range lines {
		if line == entry {
			return
		}
	}
	progress := -1
	end := len(lines)
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if progress < 0 {
			if strings.EqualFold(trimmed, "## Progress") {
				progress = index
			}
			continue
		}
		if strings.HasPrefix(trimmed, "## ") {
			end = index
			break
		}
	}
	if progress < 0 {
		document.Body = strings.TrimRight(body, "\n") + "\n\n## Progress\n\n" + entry + "\n"
		return
	}
	prefix := strings.TrimRight(strings.Join(lines[:end], "\n"), "\n")
	suffix := strings.TrimLeft(strings.Join(lines[end:], "\n"), "\n")
	document.Body = prefix + "\n\n" + entry + "\n"
	if suffix != "" {
		document.Body += "\n" + suffix
		if !strings.HasSuffix(document.Body, "\n") {
			document.Body += "\n"
		}
	}
}
