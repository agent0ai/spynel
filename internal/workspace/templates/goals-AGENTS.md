# Goal Workflow DOX

Goal documents use YAML front matter followed by an objective, current evidence, next actions, and progress log.

Required front matter: `id`, `title`, `status`, `created_at`, `updated_at`, and `next_review_at` when another review is expected.

Default statuses:

- `active`: eligible for another review.
- `working`: claimed for the current review.
- `waiting`: deliberately paused until a documented condition or review time.
- `done`: the objective is satisfied and verified.

Goal reviews may create task files. Record created task paths in the goal before moving it to its next status folder.

