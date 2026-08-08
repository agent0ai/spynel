package orchestrator

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/agent0ai/spynel/internal/config"
)

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func Create(cfg config.Config, routeName, title, body string) (string, error) {
	return CreateWithOptions(cfg, routeName, title, body, CreateOptions{})
}

type CreateOptions struct {
	Notify       bool
	Origin       string
	Outcomes     []string
	ParentTaskID string
	GoalID       string
	GoalRound    int
	NoReview     bool
}

func CreateWithOptions(cfg config.Config, routeName, title, body string, options CreateOptions) (string, error) {
	var route *config.Route
	for i := range cfg.Orchestrator.Routes {
		if cfg.Orchestrator.Routes[i].Name == routeName {
			route = &cfg.Orchestrator.Routes[i]
			break
		}
	}
	if route == nil {
		return "", fmt.Errorf("orchestrator route %q is not configured", routeName)
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return "", errors.New("title is required")
	}
	now := time.Now().UTC()
	id := routeName + "-" + now.Format("20060102-150405") + "-" + randomSuffix()
	slug := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(slug) > 64 {
		slug = slug[:64]
	}
	if slug == "" {
		slug = "item"
	}
	status := filepath.Base(filepath.Clean(route.Source))
	front := map[string]any{
		"id": id, "title": title, "status": status,
		"created_at": now.Format(time.RFC3339), "updated_at": now.Format(time.RFC3339), "attempt": 0,
	}
	taskReviewRequired := true
	if routeName == "goals" {
		front["round"] = 0
		front["review_trigger"] = "all_round_tasks_settled"
		front["success_criteria"] = []map[string]any{{
			"id": "criterion-1", "condition": title,
			"evidence_required": "Direct, reviewable evidence that the stated outcome has been achieved.",
		}}
	}
	if routeName == "tasks" {
		taskReviewRequired = !options.NoReview
		if strings.TrimSpace(options.GoalID) != "" {
			taskReviewRequired = true
		}
		front["review_required"] = taskReviewRequired
		enabled := options.Notify
		notify := map[string]any{"enabled": enabled}
		if enabled {
			if _, err := ParseOrigin(options.Origin); err != nil {
				return "", fmt.Errorf("notification origin: %w", err)
			}
			outcomes := options.Outcomes
			if len(outcomes) == 0 {
				outcomes = []string{"done", "failed", "waiting", "cancelled"}
			}
			notify["origin"] = options.Origin
			notify["on"] = outcomes
			for _, outcome := range outcomes {
				if outcome != "done" && outcome != "failed" && outcome != "waiting" && outcome != "cancelled" {
					return "", fmt.Errorf("notification outcome %q is not supported", outcome)
				}
			}
		}
		front["notify"] = notify
		if options.ParentTaskID != "" {
			front["parent_task_id"] = options.ParentTaskID
		}
		if strings.TrimSpace(options.GoalID) != "" {
			if options.GoalRound <= 0 {
				return "", errors.New("goal-linked tasks require a positive goal round")
			}
			front["goal_id"] = strings.TrimSpace(options.GoalID)
			front["goal_round"] = options.GoalRound
		}
	}
	if strings.TrimSpace(body) == "" {
		if routeName == "goals" {
			body = "# " + title + "\n\n## Objective\n\n" + title + "\n\n## Boundaries\n\n- To be refined during planning.\n\n## Target conditions\n\n- `criterion-1`: " + title + "\n\n## Current evidence\n\n- No evidence recorded yet.\n\n## Planning history\n\n- Awaiting the initial planning pass.\n\n## Review history\n\n- No reviews yet.\n\n## Progress\n\n- Created by Spynel.\n"
		} else {
			acceptance := "The requested finite outcome is implemented and independently verified."
			if !taskReviewRequired {
				acceptance = "The requested information is collected read-only, with sources, evidence boundaries, uncertainty, and an exact UTC completion time recorded."
			}
			body = "# " + title + "\n\n## Objective\n\n" + title + "\n\n## Acceptance criteria\n\n- " + acceptance + "\n\n## Context\n\n- Created by Spynel.\n\n## Progress\n\n- Not started.\n"
		}
	}
	document := Document{FrontMatter: front, Body: body}
	directory := cfg.Resolve(route.Source)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(directory, slug+"-"+randomSuffix()+".md")
	if err := WriteDocument(path, document); err != nil {
		return "", err
	}
	return path, nil
}

func randomSuffix() string {
	data := make([]byte, 3)
	if _, err := rand.Read(data); err != nil {
		return fmt.Sprintf("%06d", time.Now().UnixNano()%1000000)
	}
	return hex.EncodeToString(data)
}
