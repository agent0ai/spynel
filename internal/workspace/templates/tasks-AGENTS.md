# Task Workflow DOX

Task documents use YAML front matter followed by a human-readable body and progress log.

Required front matter: `id`, `title`, `status`, `created_at`, and `updated_at`. A goal-derived task also requires immutable `goal_id` and integer `goal_round`. The task-to-goal reference is authoritative; standalone tasks omit both fields. Moving files never changes identity.

Default statuses:

- `todo`: eligible for dispatch.
- `working`: atomically claimed for implementation and backed by an implementation lease.
- `review`: implementation is complete and awaiting an independent fresh-session review.
- `reviewing`: atomically claimed from `review` and backed by a distinct review lease.
- `waiting`: blocked on a concrete external condition; document that condition.
- `done`: requested outcome was accepted by an independent reviewer.
- `failed`: processing cannot continue; document evidence and recovery options.
- `cancelled`: deliberately stopped without completion; document the authority and reason.

An agent must update the document before moving it. Do not silently declare success in chat while leaving the durable task stale.

The normal lifecycle is `todo -> working -> review -> reviewing -> done`. Implementation agents move completed work to `review`, never `reviewing` or `done`. Spynel alone claims queued files into `working` or `reviewing`. Reviewers append evidence-backed findings and move accepted tasks to `done` or deficient tasks back to `todo` without fixing them. Resolved waiting work returns to `todo`; an optional RFC 3339 `wake_at` lets Spynel do that automatically.

Optional `notify` front matter contains only `enabled`, stable `origin` (`telegram`, `whatsapp`, `tui`, or `cli` plus `/conversation`), and selected outcomes (`done`, `failed`, `waiting`, or `cancelled`). Goal-derived and other child tasks default to notifications disabled. A task reaching `done`, `failed`, or `cancelled` is a settled result for its goal round; `waiting` is not settled.
