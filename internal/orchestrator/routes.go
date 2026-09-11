package orchestrator

import (
	"path/filepath"
	"time"
)

type workflowRoute struct {
	Name           string
	Source         string
	Working        string
	Prompt         string
	RecoveryPrompt string
	ReviewPrompt   string
	StaleAfter     time.Duration
	AllowedNext    []string
}

// workflowRoutes defines the two fixed workspace workflows. It is never configuration.
func workflowRoutes() []workflowRoute {
	return []workflowRoute{
		{Name: "tasks", Source: ".spynel/tasks/todo", Working: ".spynel/tasks/working", Prompt: ".spynel/prompts/task.md", RecoveryPrompt: ".spynel/prompts/recovery.md", ReviewPrompt: ".spynel/prompts/review.md", StaleAfter: 30 * time.Minute, AllowedNext: []string{"todo", "working", "review", "reviewing", "waiting", "done", "failed", "cancelled"}},
		{Name: "goals", Source: ".spynel/goals/proposed", Working: ".spynel/goals/planning", Prompt: ".spynel/prompts/goal.md", RecoveryPrompt: ".spynel/prompts/recovery.md", ReviewPrompt: ".spynel/prompts/goal-review.md", StaleAfter: 2 * time.Hour, AllowedNext: []string{"proposed", "planning", "active", "review", "reviewing", "waiting", "done", "abandoned"}},
	}
}

func routeByName(name string) (workflowRoute, bool) {
	for _, route := range workflowRoutes() {
		if route.Name == name {
			return route, true
		}
	}
	return workflowRoute{}, false
}

// WorkflowDirectories lists only canonical live task and goal status folders.
func (m *Manager) WorkflowDirectories() []string {
	var paths []string
	for _, route := range workflowRoutes() {
		for _, status := range route.AllowedNext {
			paths = append(paths, filepath.Join(filepath.Dir(m.Config.Resolve(route.Source)), status))
		}
	}
	return paths
}
