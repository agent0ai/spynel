# Task Workflow DOX

Task documents use YAML front matter followed by a human-readable body and progress log.

Required front matter: `id`, `title`, `status`, `created_at`, and `updated_at`.

Default statuses:

- `todo`: eligible for dispatch.
- `working`: claimed by Spynel or actively processed.
- `waiting`: blocked on a concrete external condition; document that condition.
- `done`: requested outcome is complete and verified.
- `failed`: processing cannot continue; document evidence and recovery options.

An agent must update the document before moving it. Do not silently declare success in chat while leaving the durable task stale.

