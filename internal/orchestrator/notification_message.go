package orchestrator

import (
	"errors"
	"fmt"
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

// notificationSummary is optional and deliberately bounded. Invalid or
// legacy metadata is ignored so task transitions and notifications cannot be
// held hostage by presentation data.
type notificationSummary struct {
	Verdict     string
	Outcome     string
	Evidence    string
	Uncertainty string
	ReviewedAt  time.Time
	CompletedAt time.Time
	ReworkCount int
	HasRework   bool
}

func parseNotificationSummary(document Document) (notificationSummary, bool) {
	raw, ok := document.FrontMatter["notification_summary"].(map[string]any)
	if !ok {
		return notificationSummary{}, false
	}
	for key := range raw {
		switch key {
		case "verdict", "outcome", "evidence", "uncertainty", "reviewed_at", "completed_at", "rework_count":
		default:
			return notificationSummary{}, false
		}
	}
	verdict, verdictOK := requiredBoundedLine(raw, "verdict", 32)
	outcome, outcomeOK := requiredBoundedLine(raw, "outcome", notificationOutcomeRunes)
	evidence, evidenceOK := optionalBoundedLine(raw, "evidence", notificationEvidenceRunes)
	uncertainty, uncertaintyOK := optionalBoundedLine(raw, "uncertainty", notificationEvidenceRunes)
	summary := notificationSummary{Verdict: verdict, Outcome: outcome, Evidence: evidence, Uncertainty: uncertainty}
	if !verdictOK || !outcomeOK || !evidenceOK || !uncertaintyOK || !validNotificationVerdict(summary.Verdict) {
		return notificationSummary{}, false
	}
	if value, exists := raw["reviewed_at"]; exists {
		switch typed := value.(type) {
		case string:
			parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(typed))
			if err != nil {
				return notificationSummary{}, false
			}
			summary.ReviewedAt = parsed
		case time.Time:
			summary.ReviewedAt = typed
		default:
			return notificationSummary{}, false
		}
	}
	if value, exists := raw["completed_at"]; exists {
		switch typed := value.(type) {
		case string:
			parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(typed))
			if err != nil {
				return notificationSummary{}, false
			}
			summary.CompletedAt = parsed
		case time.Time:
			summary.CompletedAt = typed
		default:
			return notificationSummary{}, false
		}
	}
	if (summary.Verdict == "accepted" || summary.Verdict == "rejected") && summary.ReviewedAt.IsZero() {
		return notificationSummary{}, false
	}
	if summary.Verdict == "completed" && summary.CompletedAt.IsZero() {
		return notificationSummary{}, false
	}
	if value, exists := raw["rework_count"]; exists {
		count, ok := exactNonnegativeInt(value)
		if !ok || count > 100000 {
			return notificationSummary{}, false
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

func summaryMatchesStatus(summary notificationSummary, status string) bool {
	if status == "done" {
		return summary.Verdict == "accepted" || summary.Verdict == "completed"
	}
	want := map[string]string{"waiting": "waiting", "failed": "failed", "cancelled": "cancelled"}[status]
	return summary.Verdict == want
}

func validateDirectCompletionEvidence(document Document) error {
	summary, ok := parseNotificationSummary(document)
	if !ok || summary.Verdict != "completed" {
		return errors.New("a valid completed notification_summary is required")
	}
	if summary.Evidence == "" {
		return errors.New("notification_summary.evidence must record the source boundary")
	}
	if summary.Uncertainty == "" {
		return errors.New("notification_summary.uncertainty must record remaining uncertainty")
	}
	updated, ok := timestampField(document, "updated_at")
	if !ok || !summary.CompletedAt.Equal(updated) {
		return errors.New("notification_summary.completed_at must exactly match updated_at")
	}
	_, completedOffset := summary.CompletedAt.Zone()
	_, updatedOffset := updated.Zone()
	if completedOffset != 0 || updatedOffset != 0 {
		return errors.New("notification_summary.completed_at and updated_at must be UTC")
	}
	return nil
}

// finalizeTaskNotificationSummary adds the objective rejection/rework metric
// after a review move. It never blocks or reverses the durable transition.
func (m *Manager) finalizeTaskNotificationSummary(path, status string) {
	document, err := ReadDocument(path)
	if err != nil {
		m.log("read task notification summary: " + err.Error())
		return
	}
	summary, ok := parseNotificationSummary(document)
	if !ok || (status == "done" && summary.Verdict != "accepted" && summary.Verdict != "completed") || (status == "todo" && summary.Verdict != "rejected") {
		return
	}
	reviews := numberValue(document.FrontMatter["review_attempt"])
	reworks := reviews
	if status == "done" && reworks > 0 {
		reworks--
	}
	raw := document.FrontMatter["notification_summary"].(map[string]any)
	raw["rework_count"] = max(0, reworks)
	if err := WriteDocument(path, document); err != nil {
		m.log("write task notification summary: " + err.Error())
	}
}

// FormatTaskNotification is deterministic and has no harness, filesystem, or
// delivery dependency. It therefore remains safe in terminal transition hooks.
func FormatTaskNotification(document Document, status, fallbackID string) string {
	title := truncateLine(stringField(document, "title"), notificationTitleRunes)
	if title == "" {
		title = truncateLine(fallbackID, notificationTitleRunes)
	}
	header := map[string]string{
		"done": title + " is complete.", "waiting": title + " is waiting.", "failed": title + " failed.", "cancelled": title + " was cancelled.",
	}[status]
	if header == "" {
		header = title + " reached an outcome."
		if status != "" {
			header = title + " is " + status + "."
		}
	}
	lines := []string{header}
	if summary, ok := parseNotificationSummary(document); ok && summaryMatchesStatus(summary, status) {
		lines = append(lines, summary.Outcome)
		if summary.Evidence != "" {
			label := "Verified: "
			if summary.Verdict == "completed" {
				label = "Recorded: "
			}
			lines = append(lines, label+summary.Evidence)
		}
		if summary.Verdict == "completed" && summary.Uncertainty != "" {
			lines = append(lines, "Uncertainty: "+summary.Uncertainty)
		}
	} else {
		policy, policyErr := TaskPolicyFromDocument(document)
		if status == "done" && policyErr == nil && !policy.ReviewRequired {
			lines = append(lines, "The recorded read-only result was completed without independent review.")
		} else {
			lines = append(lines, fallbackTaskOutcome(status))
		}
	}
	return strings.Join(lines, "\n")
}

func fallbackTaskOutcome(status string) string {
	switch status {
	case "done":
		return "The recorded result was independently verified."
	case "waiting":
		return "An external condition is required before work can continue."
	case "failed":
		return "Work could not be completed; inspect the task details for the recorded reason."
	case "cancelled":
		return "Work stopped without completion."
	default:
		return "Task reached " + status + "."
	}
}

func taskNotificationMetrics(document Document, status string) string {
	parts := make([]string, 0, 4)
	created, createdOK := timestampField(document, "created_at")
	updated, updatedOK := timestampField(document, "updated_at")
	if createdOK && updatedOK && !updated.Before(created) {
		parts = append(parts, conciseDuration(updated.Sub(created)))
	}
	if attempts := numberValue(document.FrontMatter["attempt"]); attempts > 0 {
		parts = append(parts, fmt.Sprintf("%d implementation %s", attempts, plural(attempts, "attempt", "attempts")))
	}
	if reviews := numberValue(document.FrontMatter["review_attempt"]); reviews > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", reviews, plural(reviews, "review", "reviews")))
	}
	if summary, ok := parseNotificationSummary(document); ok && summary.HasRework && summary.ReworkCount > 0 && summaryMatchesStatus(summary, status) {
		parts = append(parts, fmt.Sprintf("%d %s", summary.ReworkCount, plural(summary.ReworkCount, "rework", "reworks")))
	}
	return strings.Join(parts, " · ")
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

func conciseDuration(duration time.Duration) string {
	if duration < time.Minute {
		return "<1m"
	}
	minutes := int64(duration / time.Minute)
	days, minutes := minutes/(24*60), minutes%(24*60)
	hours, minutes := minutes/60, minutes%60
	parts := make([]string, 0, 3)
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	return strings.Join(parts, " ")
}

func plural(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}
