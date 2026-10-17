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

const waitingReminderHeartbeatInstruction = "Framework-owned stale-wait rule: agents do the work; the framework only triggers this audit and provides ordinary notification delivery. Inspect bounded blocked/waiting-task progress, missed transition alerts, delivery failures and still-unanswered user questions or decisions. Trace the actionable root dependency: verify whether it is actually unresolved, a premise or policy is misapplied, existing authorization permits action, or independent work can continue. Resume proven safe executable work through the ordinary durable lifecycle, respecting live ownership and required review; do not loop requeues, repeat unchanged gates, or treat a passive active goal or stale claim as progress.\n\n" +
	"Read the affected task's exact routing metadata privately and its original thread's bounded recent question/reply history and existing source correlation. Privacy forbids exposing identities or transcripts, not using an authorized exact origin or inspecting necessary bounded evidence. A relevant answer may resolve a question, but an unrelated later message, queue admission or silence does not. A failed or absent attempt is not a delivered question; verify actual question/delivery evidence before treating a later reply as its answer. Do not reinterpret later chat as instructions for an active implementation session. After a validated relevant answer, resume only the affected resolved dependency through its ordinary lifecycle; avoid duplicate escalation after resolution.\n\n" +
	"A first prerequisite question is an actionable request, not an optional repeat reminder. When required input is genuinely unavailable and no setup question has been delivered, ask concretely through the known authorized original thread during this audit; freshness of the wait and absence of prior alerts never justify suppressing that first request. A recoverable or no-input-needed prerequisite calls for safe resumption, not a new user gate. Ask only for genuinely unavailable external assets. Judge repeat reminders from prior progress and actual notifications, without treating a prior failed send or skip as delivery.\n\n" +
	"For task-related alerts, use the task's exact enabled notify.origin and selected notify.on outcome with `spynel notify --origin`; global recent-user ambiguity must never block that known thread. Use `--fallback-origin` only for an explicitly owner-established same-operator route, revalidated through ordinary delivery; allow-list membership, cached identity and recency alone do not establish that association. Never guess, broadcast or switch to another remote user. If the route is missing or local/remote delivery fails, inspect the ordinary delivery evidence, record the precise missing prerequisite or failure and surface it through an available explicitly authorized route; never silently claim delivery or resolution. `spynel notify --recent-authorized` is only for otherwise originless alerts with unambiguous current authorization, never a replacement for a task origin. Obtain current UTC and append the successful send result, distinguishing queue acceptance from send and receipt, or the concise skip/CLI-failure result to the affected task's Progress log. Never create reminder queues, timers, thresholds, eligibility rules, identity links, correlation tokens, acknowledgement state, deduplication state, restart contingencies, or other reminder orchestration."
