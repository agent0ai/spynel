# Task Workflow DOX

Task documents use YAML front matter followed by a human-readable body and progress log.

Spynel owns `first_assigned_at` and `provider_iterations`: the former is set on the first real claim, while the latter increments under a cross-process lock immediately before each provider dispatch, recovery, control, or continuation. Agents must preserve both fields and must not repurpose phase-specific attempt counters as provider iterations. Implementation claims increment `attempt`; independent review claims increment `review_attempt` and never change `attempt`.

Required front matter: `id`, `title`, `status`, `created_at`, `updated_at`, and boolean `review_required`. Older files without the policy require review. Malformed values are unsafe and must never permit direct completion. A goal-derived task also requires immutable `goal_id` and integer `goal_round`. The task-to-goal reference is authoritative; standalone tasks omit both fields. Moving files never changes identity.

Default statuses:

- `todo`: eligible for dispatch.
- `working`: atomically claimed for implementation and backed by an implementation lease.
- `review`: implementation is complete and awaiting an independent fresh-session review.
- `reviewing`: atomically claimed from `review` and backed by a distinct review lease.
- `waiting`: blocked on a concrete external condition; document that condition.
- `done`: requested outcome completed under its review policy; independently reviewed acceptance is distinguished from permitted direct read-only completion.
- `failed`: processing cannot continue; document evidence and recovery options.
- `cancelled`: deliberately stopped without completion; document the authority and reason.

An agent must update the document before moving it. Do not silently declare success in chat while leaving the durable task stale.

Use `review_required: true` for every source, configuration, prompt, test, behavior-linked documentation, build, packaging, migration, security, infrastructure, destructive, goal-derived, or other development/change task. `false` is limited to bounded low-risk read-only collection or reporting whose information is itself the deliverable. The choice is semantic and explicit; never infer it from title, origin, duration, notifications, or observed file changes. Child-task, notification, and goal-review policy remain independent.

The reviewed lifecycle is `todo -> working -> review -> reviewing -> done`. A valid no-review task may instead move `working -> done` only with a valid `completed` summary: `outcome` records what was collected, required `evidence` records the source/evidence boundary, required `uncertainty` records what remains unknown, and `completed_at` exactly matches `updated_at` in UTC. Spynel returns an invalid direct completion to `todo` without terminal hooks or notification. A no-review task manually placed in `review` receives normal independent review. Spynel alone claims queued files into `working` or `reviewing`. Reviewers append evidence-backed findings and move accepted tasks to `done` or deficient tasks back to `todo` without fixing them. Resolved waiting work returns to `todo`; an optional RFC 3339 `wake_at` lets Spynel do that automatically.

Optional `notify` front matter contains only `enabled`, stable `origin` (`telegram`, `whatsapp`, `tui`, or `cli` plus `/conversation`), and selected outcomes (`done`, `failed`, `waiting`, or `cancelled`). Goal-derived and other child tasks default to notifications disabled. A task reaching `done`, `failed`, or `cancelled` is a settled result for its goal round; `waiting` is not settled.

Optional `notification_summary` contains only bounded `verdict`, `outcome`, `evidence`, `uncertainty`, `reviewed_at`, `completed_at`, and code-managed `rework_count` fields. Reviewers record an `accepted` or `rejected` task-specific summary with `reviewed_at`; direct no-review implementers must record `completed` with required `evidence`, required `uncertainty`, and matching `completed_at`; other terminal implementers use matching `waiting`, `failed`, or `cancelled` verdicts. User-facing text distinguishes recorded direct completion from independently verified acceptance. Spynel calculates duration, attempt, review, and rework metrics. Missing or malformed summaries remain non-blocking legacy input except when they are the required evidence gate for direct no-review completion.
