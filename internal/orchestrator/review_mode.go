package orchestrator

import (
	"strings"

	"github.com/agent0ai/spynel/internal/config"
)

// TaskReviewModeInstruction is injected outside user-overridable templates so
// the deterministic framework policy remains visible even in custom
// prompts. The setting governs task review only; goal outcome review remains
// mandatory because it decides whether a long-running goal met its success bar.
func TaskReviewModeInstruction(mode string) string {
	switch mode {
	case config.TaskReviewsAlways:
		return "Configured task review mode: always. The framework requires independent review for every task regardless of its review_required field. Create task documents with review_required: true, and executors must move completed attempts to review rather than done."
	case config.TaskReviewsNever:
		return "Configured task review mode: never. The framework disables independent task review regardless of a task's review_required field. Create task documents with review_required: false; executors must complete through the direct-completion evidence path and must not move tasks to review. This does not bypass mandatory goal outcome review."
	default:
		return "Configured task review mode: skip-trivial. Choose review_required per task by expected risk reduction: require independent review for broad, risky, hard-to-reverse, or materially uncertain work, and allow direct completion for trivial or low-risk work with proportionate evidence. Mandatory goal outcome review remains separate."
	}
}

func appendTaskReviewModeInstruction(prompt, mode string) string {
	return strings.TrimRight(prompt, "\r\n") + "\n\n" + TaskReviewModeInstruction(mode)
}

const waitingReminderHeartbeatInstruction = "Framework-owned stale-wait rule: agents do the work; the framework only triggers this audit and provides the ordinary recent-authorized notification primitive. Inspect bounded waiting-task progress, judge whether inactivity is considerable and a reminder is useful, avoid unnecessary repetition by reading prior progress, then either call `spynel notify --recent-authorized --message TEXT` and append the successful send to that task's Progress log, or append a concise skip/CLI-failure result when a durable record is useful. Never create reminder queues, timers, thresholds, eligibility rules, identity links, correlation tokens, acknowledgement state, deduplication state, restart contingencies, or other reminder orchestration."
