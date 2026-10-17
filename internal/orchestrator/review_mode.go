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

const waitingReminderHeartbeatInstruction = "Framework-owned stale-wait rule: agents do the work; the framework only triggers this audit and provides ordinary notification delivery. Inspect bounded blocked/waiting-task progress, missed transition alerts, delivery failures and still-unanswered user questions or decisions. Read the affected original thread's bounded recent history and existing source correlation before deciding: a relevant answer may resolve a question, but an unrelated later message, queue admission or silence does not. Respect live ownership; do not reinterpret later chat as instructions for an active implementation session. Judge whether an actionable escalation or reminder is useful and avoid repetition by reading prior progress and notifications. For task-related alerts, use the task's exact enabled notify.origin and selected notify.on outcome with `spynel notify --origin`; global recent-user ambiguity must never block that known thread. Use `--fallback-origin` only for an explicitly owner-established same-operator route, revalidated through ordinary delivery; allow-list membership, cached identity and recency alone do not establish that association. Never guess, broadcast or switch to another remote user. If the route is missing or delivery fails, record the precise missing prerequisite or CLI failure and surface it through an available explicitly authorized route; never silently claim delivery or resolution. `spynel notify --recent-authorized` is only for otherwise originless alerts with unambiguous current authorization, never a replacement for a task origin. Obtain current UTC and append the successful send result, distinguishing queue acceptance from send and receipt, or the concise skip/CLI-failure result to the affected task's Progress log. Never create reminder queues, timers, thresholds, eligibility rules, identity links, correlation tokens, acknowledgement state, deduplication state, restart contingencies, or other reminder orchestration."
