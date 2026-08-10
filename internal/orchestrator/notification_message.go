package orchestrator

import (
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	notificationTitleRunes    = 120
	notificationOutcomeRunes  = 280
	notificationEvidenceRunes = 280
)

// completionSummary is optional and deliberately bounded. Invalid presentation
// metadata is ignored so reviewed transitions cannot be held hostage by it;
// direct completion validates the same structure as required evidence.
type completionSummary struct {
	Verdict     string
	Outcome     string
	Evidence    string
	Uncertainty string
	ReviewedAt  time.Time
	CompletedAt time.Time
	ReworkCount int
	HasRework   bool
}

func parseCompletionSummary(document Document) (completionSummary, bool) {
	raw, ok := document.FrontMatter["completion_summary"].(map[string]any)
	if !ok {
		return completionSummary{}, false
	}
	for key := range raw {
		switch key {
		case "verdict", "outcome", "evidence", "uncertainty", "reviewed_at", "completed_at", "rework_count":
		default:
			return completionSummary{}, false
		}
	}
	verdict, verdictOK := requiredBoundedLine(raw, "verdict", 32)
	outcome, outcomeOK := requiredBoundedLine(raw, "outcome", notificationOutcomeRunes)
	evidence, evidenceOK := optionalBoundedLine(raw, "evidence", notificationEvidenceRunes)
	uncertainty, uncertaintyOK := optionalBoundedLine(raw, "uncertainty", notificationEvidenceRunes)
	summary := completionSummary{Verdict: verdict, Outcome: outcome, Evidence: evidence, Uncertainty: uncertainty}
	if !verdictOK || !outcomeOK || !evidenceOK || !uncertaintyOK || !validNotificationVerdict(summary.Verdict) {
		return completionSummary{}, false
	}
	if value, exists := raw["reviewed_at"]; exists {
		switch typed := value.(type) {
		case string:
			parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(typed))
			if err != nil {
				return completionSummary{}, false
			}
			summary.ReviewedAt = parsed
		case time.Time:
			summary.ReviewedAt = typed
		default:
			return completionSummary{}, false
		}
	}
	if value, exists := raw["completed_at"]; exists {
		switch typed := value.(type) {
		case string:
			parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(typed))
			if err != nil {
				return completionSummary{}, false
			}
			summary.CompletedAt = parsed
		case time.Time:
			summary.CompletedAt = typed
		default:
			return completionSummary{}, false
		}
	}
	if (summary.Verdict == "accepted" || summary.Verdict == "rejected") && summary.ReviewedAt.IsZero() {
		return completionSummary{}, false
	}
	if summary.Verdict == "completed" && summary.CompletedAt.IsZero() {
		return completionSummary{}, false
	}
	if value, exists := raw["rework_count"]; exists {
		count, ok := exactNonnegativeInt(value)
		if !ok || count > 100000 {
			return completionSummary{}, false
		}
		summary.ReworkCount = count
		summary.HasRework = true
	}
	return summary, true
}

func validNotificationVerdict(value string) bool {
	switch value {
	case "accepted", "completed", "rejected", "waiting", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func boundedLine(value any, limit int) (string, bool) {
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	text = cleanNotificationLine(text)
	if text == "" || utf8.RuneCountInString(text) > limit || containsAbsolutePath(text) {
		return "", false
	}
	return text, true
}

func requiredBoundedLine(raw map[string]any, key string, limit int) (string, bool) {
	value, exists := raw[key]
	if !exists {
		return "", false
	}
	return boundedLine(value, limit)
}

func optionalBoundedLine(raw map[string]any, key string, limit int) (string, bool) {
	value, exists := raw[key]
	if !exists {
		return "", true
	}
	return boundedLine(value, limit)
}

func exactNonnegativeInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, typed >= 0
	case int64:
		return int(typed), typed >= 0
	case float64:
		integer := int(typed)
		return integer, typed >= 0 && typed == float64(integer)
	default:
		return 0, false
	}
}

func truncateLine(value string, limit int) string {
	value = cleanNotificationLine(value)
	if containsAbsolutePath(value) {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return strings.TrimSpace(string(runes[:limit-1])) + "…"
}

func containsAbsolutePath(value string) bool {
	runes := []rune(value)
	boundary := func(index int) bool {
		return index == 0 || (!unicode.IsLetter(runes[index-1]) && !unicode.IsNumber(runes[index-1]))
	}
	for index, character := range runes {
		if character == '/' && boundary(index) {
			return true
		}
		if character == '\\' && boundary(index) && index+1 < len(runes) && runes[index+1] == '\\' {
			return true
		}
		if unicode.IsLetter(character) && boundary(index) && index+2 < len(runes) && runes[index+1] == ':' && (runes[index+2] == '/' || runes[index+2] == '\\') {
			return true
		}
	}
	return false
}

func cleanNotificationLine(value string) string {
	value = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return ' '
		}
		return character
	}, value)
	return strings.Join(strings.FieldsFunc(value, unicode.IsSpace), " ")
}

func validateDirectCompletionEvidence(document Document) error {
	summary, ok := parseCompletionSummary(document)
	if !ok || summary.Verdict != "completed" {
		return errors.New("a valid completion_summary with verdict completed is required")
	}
	if summary.Evidence == "" {
		return errors.New("completion_summary.evidence must record verification and the inspected boundary")
	}
	if summary.Uncertainty == "" {
		return errors.New("completion_summary.uncertainty must record remaining uncertainty")
	}
	updated, ok := timestampField(document, "updated_at")
	if !ok || !summary.CompletedAt.Equal(updated) {
		return errors.New("completion_summary.completed_at must exactly match updated_at")
	}
	_, completedOffset := summary.CompletedAt.Zone()
	_, updatedOffset := updated.Zone()
	if completedOffset != 0 || updatedOffset != 0 {
		return errors.New("completion_summary.completed_at and updated_at must be UTC")
	}
	return nil
}

// finalizeTaskCompletionSummary adds the objective rejection/rework metric
// after a review move. It never blocks or reverses the durable transition.
func (m *Manager) finalizeTaskCompletionSummary(path, status string) {
	document, err := ReadDocument(path)
	if err != nil {
		m.log("read task completion summary: " + err.Error())
		return
	}
	summary, ok := parseCompletionSummary(document)
	if !ok || (status == "done" && summary.Verdict != "accepted" && summary.Verdict != "completed") || (status == "todo" && summary.Verdict != "rejected") {
		return
	}
	reviews := numberValue(document.FrontMatter["review_attempt"])
	reworks := reviews
	if status == "done" && reworks > 0 {
		reworks--
	}
	raw := document.FrontMatter["completion_summary"].(map[string]any)
	raw["rework_count"] = max(0, reworks)
	if err := WriteDocument(path, document); err != nil {
		m.log("write task completion summary: " + err.Error())
	}
}

func timestampField(document Document, field string) (time.Time, bool) {
	value, ok := document.FrontMatter[field]
	if !ok {
		return time.Time{}, false
	}
	switch typed := value.(type) {
	case string:
		parsed, err := time.Parse(time.RFC3339, typed)
		return parsed, err == nil
	case time.Time:
		return typed, true
	default:
		return time.Time{}, false
	}
}
