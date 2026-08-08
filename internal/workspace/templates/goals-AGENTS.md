# Goal Workflow DOX

Goal documents use YAML front matter followed by an objective, boundaries, target conditions, current evidence, planning history, review history, and progress log. A goal is a long-term, recurring, or multi-round outcome. It is not a large task: it coordinates finite tasks and is completed only by a distinct evidence-based review against its bar.

Spynel owns `first_assigned_at` and `provider_iterations`: the former is set on the first real planning claim, while the latter increments under a cross-process lock immediately before each provider dispatch, recovery, control, or continuation. Agents must preserve both fields and must not repurpose planning/review attempt counters as provider iterations.

Required front matter: `id`, `title`, `status`, `created_at`, `updated_at`, non-negative integer `round`, non-empty `success_criteria`, and `review_trigger`. Each success criterion has a stable `id`, measurable `condition`, and `evidence_required`. `review_trigger` is `all_round_tasks_settled`, `all_round_tasks_settled_or_checkpoint`, or `scheduled`. Default to settlement-only. Any new `next_review_at` requires a valid RFC 3339 time plus non-empty `checkpoint_reason`; use it only for a concrete, short, development-appropriate active-round checkpoint. A precise external dependency belongs in `waiting` with `wake_at`. A checkpoint is consulted only while the goal is `active`; once the goal enters `review`, its retained value is durable round history and never delays the review claim.

Default statuses:

- `proposed`: captured and eligible for its initial planning claim.
- `planning`: claimed by a planning lease to refine the bar and create the next linked task round.
- `active`: no goal agent owns the file; Spynel observes current-round tasks and checkpoints.
- `review`: the round settled or a checkpoint arrived, so independent goal review is queued.
- `reviewing`: claimed from `review` by a fresh goal-review lease and session.
- `waiting`: deliberately paused until a documented condition or `wake_at`; `resume_status` records whether it returns to planning or review.
- `done`: a goal review proved every required success criterion.
- `abandoned`: deliberately ended without satisfying the bar.

The normal lifecycle is `proposed -> planning -> active -> review -> reviewing`. A review moves to `done`, `waiting`, `abandoned`, or back to `planning` for another round. Planning and reviewing always have separate leases and sessions. The reviewer records criterion verdicts but never implements work or creates tasks; a continuing review hands the goal to a new planning session.

Planning creates focused task files in `tasks/todo/` with `review_required: true`, the goal's `id` as `goal_id`, and the next integer as `goal_round`, verifies the complete batch, records its exact stable IDs in `round_task_ids`, then commits that round on the goal and moves it to `active`. Task IDs—not moving paths—are durable references, and Spynel rejects activation when the declared cohort differs from the linked task documents. `active` carries no goal lease. Spynel moves it to `review` when all tasks in the current round are settled (`done`, `failed`, or `cancelled`) or a configured checkpoint is due. Task completion alone never completes a goal, regardless of its task review policy.

A goal may enter `done` only from `reviewing` with `last_review.verdict: done`, `last_review.criteria_satisfied: true`, the reviewed round, an exact clock-derived review time, and criterion-by-criterion evidence in the body. Never weaken success criteria to justify completion.
