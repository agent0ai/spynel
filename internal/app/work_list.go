package app

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/orchestrator"
)

const (
	defaultWorkflowListLimit = 20
	maxWorkflowListLimit     = 100
	maxWorkflowListDays      = 3650
)

type workflowListOptions struct {
	view         string
	viewExplicit bool
	days         int
	daysExplicit bool
	limit        int
	detail       bool
}

func (s *Service) workflowListCommand(message core.Message, kind, remainder string, emit core.Emit) error {
	options, err := parseWorkflowListOptions(kind, remainder)
	if err != nil {
		return s.localReply(message, err.Error()+"\n\n"+workflowListUsage(kind), emit)
	}
	inventory := s.Orchestrator.WorkflowItems(kind)
	if kind == "tasks" {
		settings := s.Settings.Snapshot().Harness
		for index := range inventory.Items {
			documentRequiresReview := true
			if inventory.Items[index].HasReviewPolicy {
				documentRequiresReview = inventory.Items[index].ReviewRequired
			}
			inventory.Items[index].ReviewRequired = settings.EffectiveTaskReviewRequired(documentRequiresReview)
			inventory.Items[index].HasReviewPolicy = true
		}
	}
	return s.localReply(message, formatWorkflowList(kind, inventory, options, time.Now().UTC()), emit)
}

func parseWorkflowListOptions(kind, input string) (workflowListOptions, error) {
	options := workflowListOptions{
		view:  "open",
		days:  workflowDefaultDays(kind),
		limit: defaultWorkflowListLimit,
	}
	tokens := strings.Fields(input)
	for index := 0; index < len(tokens); index++ {
		token := strings.ToLower(tokens[index])
		switch token {
		case "help", "--help", "-h":
			return options, fmt.Errorf("Options for /%s", kind)
		case "open", "recent", "active", "review", "waiting", "done", "failed", "all":
			if options.viewExplicit {
				return options, fmt.Errorf("Choose only one view: open, recent, active, review, waiting, done, failed, or all.")
			}
			options.view, options.viewExplicit = token, true
		case "--detail", "--details", "-v":
			options.detail = true
		case "--compact":
			options.detail = false
		case "--days", "-d":
			value, next, err := workflowOptionValue(tokens, index, token)
			if err != nil {
				return options, err
			}
			index = next
			if options.days, err = workflowBoundedNumber(value, "days", 1, maxWorkflowListDays); err != nil {
				return options, err
			}
			options.daysExplicit = true
		case "--limit", "-n":
			value, next, err := workflowOptionValue(tokens, index, token)
			if err != nil {
				return options, err
			}
			index = next
			if options.limit, err = workflowBoundedNumber(value, "limit", 1, maxWorkflowListLimit); err != nil {
				return options, err
			}
		default:
			if strings.HasPrefix(token, "--days=") {
				value := strings.TrimPrefix(token, "--days=")
				var err error
				if options.days, err = workflowBoundedNumber(value, "days", 1, maxWorkflowListDays); err != nil {
					return options, err
				}
				options.daysExplicit = true
				continue
			}
			if strings.HasPrefix(token, "--limit=") {
				value := strings.TrimPrefix(token, "--limit=")
				var err error
				if options.limit, err = workflowBoundedNumber(value, "limit", 1, maxWorkflowListLimit); err != nil {
					return options, err
				}
				continue
			}
			if strings.HasPrefix(token, "-") {
				return options, fmt.Errorf("Unknown option %s.", safeJobText(tokens[index], 120))
			}
			return options, fmt.Errorf("Unknown view %s.", safeJobText(tokens[index], 120))
		}
	}
	return options, nil
}

func workflowOptionValue(tokens []string, index int, option string) (string, int, error) {
	if index+1 >= len(tokens) {
		return "", index, fmt.Errorf("%s requires a value.", option)
	}
	return tokens[index+1], index + 1, nil
}

func workflowBoundedNumber(value, name string, minimum, maximum int) (int, error) {
	number, err := strconv.Atoi(value)
	if err != nil || number < minimum || number > maximum {
		return 0, fmt.Errorf("%s must be a whole number from %d to %d.", name, minimum, maximum)
	}
	return number, nil
}

func workflowDefaultDays(kind string) int {
	if kind == "goals" {
		return 7
	}
	return 3
}

func workflowListUsage(kind string) string {
	return fmt.Sprintf("Usage: /%s [open|recent|active|review|waiting|done|failed|all] [--days N] [--limit N] [--detail]\n\nOpen is the default. Recent uses the default %d-day window; `--days` narrows any view.", kind, workflowDefaultDays(kind))
}

func formatWorkflowList(kind string, inventory orchestrator.WorkflowInventory, options workflowListOptions, now time.Time) string {
	windowed := options.view == "recent" || options.daysExplicit
	cutoff := now.Add(-time.Duration(options.days) * 24 * time.Hour)
	terminal := workflowTerminalStatuses(kind)
	items := make([]orchestrator.WorkflowItem, 0, len(inventory.Items))
	for _, item := range inventory.Items {
		if !workflowItemMatchesView(kind, options.view, item.Status, terminal) {
			continue
		}
		if windowed && (item.UpdatedAt.IsZero() || item.UpdatedAt.Before(cutoff)) {
			continue
		}
		items = append(items, item)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].UpdatedAt.Equal(items[j].UpdatedAt) {
			return items[i].UpdatedAt.After(items[j].UpdatedAt)
		}
		if items[i].Status != items[j].Status {
			return items[i].Status < items[j].Status
		}
		if items[i].Title != items[j].Title {
			return items[i].Title < items[j].Title
		}
		return items[i].FileName < items[j].FileName
	})
	total := len(items)
	if len(items) > options.limit {
		items = items[:options.limit]
	}

	title := strings.ToUpper(kind[:1]) + kind[1:]
	lines := []string{"# " + title + " · " + workflowViewLabel(options), ""}
	if len(items) == 0 {
		lines = append(lines, workflowEmptyMessage(kind, options))
	} else {
		for _, item := range items {
			lines = append(lines, formatWorkflowItem(item, options.detail, now)...)
		}
		lines = append(lines, "")
		if total > len(items) {
			lines = append(lines, fmt.Sprintf("Showing %d of %d matching %s.", len(items), total, kind))
		} else {
			lines = append(lines, fmt.Sprintf("Showing %d %s.", total, pluralWord(total, strings.TrimSuffix(kind, "s"), kind)))
		}
	}
	if len(inventory.Diagnostics) > 0 {
		lines = append(lines, "", "## Warnings", "")
		for _, diagnostic := range inventory.Diagnostics {
			lines = append(lines, "- "+safeJobText(diagnostic, 240))
		}
	}
	lines = append(lines, "", workflowListHint(kind))
	return strings.Join(lines, "\n")
}

func workflowListHint(kind string) string {
	commands := make([]string, 0, 7)
	for _, view := range []string{"recent", "active", "review", "waiting", "done", "failed", "all"} {
		commands = append(commands, fmt.Sprintf("`/%s %s`", kind, view))
	}
	return fmt.Sprintf("Run %s for other views. Add `--days N`, `--limit N`, or `--detail`.", strings.Join(commands, ", "))
}

func workflowItemMatchesView(kind, view, status string, terminal map[string]bool) bool {
	switch view {
	case "open":
		return !terminal[status]
	case "recent", "all":
		return true
	case "active":
		if kind == "goals" {
			return status == "proposed" || status == "planning" || status == "active"
		}
		return status == "todo" || status == "working"
	case "review":
		return status == "review" || status == "reviewing"
	case "waiting":
		return status == "waiting"
	case "done":
		return status == "done"
	case "failed":
		if kind == "goals" {
			return status == "abandoned"
		}
		return status == "failed" || status == "cancelled"
	default:
		return false
	}
}

func workflowTerminalStatuses(kind string) map[string]bool {
	if kind == "goals" {
		return map[string]bool{"done": true, "abandoned": true}
	}
	return map[string]bool{"done": true, "failed": true, "cancelled": true}
}

func workflowViewLabel(options workflowListOptions) string {
	parts := make([]string, 0, 2)
	if options.view == "recent" {
		parts = append(parts, fmt.Sprintf("recent %dd", options.days))
	} else {
		parts = append(parts, options.view)
		if options.daysExplicit {
			parts = append(parts, fmt.Sprintf("%dd", options.days))
		}
	}
	return strings.Join(parts, " · ")
}

func workflowEmptyMessage(kind string, options workflowListOptions) string {
	if options.view == "recent" {
		return fmt.Sprintf("No %s were active in the last %d days.", kind, options.days)
	}
	return fmt.Sprintf("No %s match this view.", kind)
}

func formatWorkflowItem(item orchestrator.WorkflowItem, detail bool, now time.Time) []string {
	title := safeJobText(item.Title, 240)
	if title == "" {
		title = safeJobText(item.FileName, 240)
	}
	parts := []string{safeJobText(item.Status, 80)}
	if item.UpdatedAt.IsZero() {
		parts = append(parts, "update time unavailable")
	} else {
		age := now.Sub(item.UpdatedAt)
		if age < 0 {
			age = 0
		}
		parts = append(parts, "updated "+shortDuration(age)+" ago")
	}
	if item.Kind == "goal" {
		parts = append(parts, fmt.Sprintf("round %d", item.Round))
		if item.RoundTasks > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", item.RoundTasks, pluralWord(item.RoundTasks, "task", "tasks")))
		}
	} else if item.GoalRound > 0 {
		parts = append(parts, fmt.Sprintf("goal round %d", item.GoalRound))
	}
	if item.ProviderIterations > 0 {
		parts = append(parts, fmt.Sprintf("%d▶", item.ProviderIterations))
	}
	if item.Kind == "task" && item.Attempt > 0 {
		parts = append(parts, fmt.Sprintf("%d↻", item.Attempt))
	}
	if item.Kind == "goal" && item.PlanningAttempt > 0 {
		parts = append(parts, fmt.Sprintf("plan %d", item.PlanningAttempt))
	}
	if item.ReviewAttempt > 0 {
		parts = append(parts, fmt.Sprintf("review %d", item.ReviewAttempt))
	}
	if !item.DetailsAvailable {
		parts = append(parts, "details unavailable")
	} else {
		step := item.Step
		if step == "" {
			step = workflowStatusStep(item.Kind, item.Status)
		}
		if step != "" {
			parts = append(parts, "step: "+safeJobText(step, 180))
		}
	}
	lines := []string{
		"- **" + title + "**  ",
		"  " + strings.Join(parts, " · "),
	}
	if item.WakeAt.IsZero() == false {
		lines[1] += " · " + workflowDueLabel("wake", item.WakeAt, now)
	} else if item.NextReviewAt.IsZero() == false {
		lines[1] += " · " + workflowDueLabel("review", item.NextReviewAt, now)
	}
	if detail {
		details := make([]string, 0, 6)
		if item.ID != "" {
			details = append(details, "ID "+safeJobText(item.ID, 160))
		}
		if item.FileName != "" {
			details = append(details, "file "+safeJobText(item.FileName, 200))
		}
		if item.Kind == "task" && item.HasReviewPolicy {
			policy := "direct low-risk completion allowed"
			if item.ReviewRequired {
				policy = "review required"
			}
			details = append(details, policy)
		}
		if !item.CreatedAt.IsZero() {
			details = append(details, "created "+item.CreatedAt.UTC().Format(time.RFC3339))
		}
		if !item.UpdatedAt.IsZero() {
			details = append(details, "updated "+item.UpdatedAt.UTC().Format(time.RFC3339))
		}
		if len(details) > 0 {
			lines = append(lines, "  "+strings.Join(details, " · "))
		}
	}
	return lines
}

func workflowStatusStep(kind, status string) string {
	if kind == "goal" {
		return map[string]string{
			"proposed": "awaiting planning", "planning": "planning the next task round", "active": "current-round tasks are in progress",
			"review": "awaiting evidence review", "reviewing": "evidence review in progress", "waiting": "waiting on an external condition",
			"done": "success criteria verified", "abandoned": "ended without satisfying the bar",
		}[status]
	}
	return map[string]string{
		"todo": "queued for implementation", "working": "implementation in progress", "review": "awaiting independent review",
		"reviewing": "independent review in progress", "waiting": "waiting on an external condition", "done": "completed",
		"failed": "cannot continue", "cancelled": "cancelled",
	}[status]
}

func workflowDueLabel(name string, due, now time.Time) string {
	if due.After(now) {
		return name + " in " + shortDuration(due.Sub(now))
	}
	return name + " due " + shortDuration(now.Sub(due)) + " ago"
}

func pluralWord(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}
