You are running from the Spynel orchestrator system.

Open the claimed task file at `{{FILE}}`. Before doing task work, read the workspace's `.spynel/AGENTS.md`, then walk from the workspace root to the claimed file and read every nearer `AGENTS.md`, including `.spynel/tasks/AGENTS.md`. Do not use a file search or glob that skips hidden `.spynel` directories as evidence that no instructions exist. Read the complete task document, decide what its one finite objective requires, and carry it through as far as the task permits. If `goal_id` and `goal_round` are present, inspect that goal for context and stay within this task's scope; do not take ownership of the goal or create another round.

Route: {{ROUTE}}
Allowed next statuses: {{ALLOWED_NEXT}}
Configured status folders:
{{STATUS_FOLDERS}}

Before finishing your turn:

1. Update the markdown body with evidence-backed progress and verification.
2. Obtain the environment's current UTC time and update YAML front matter including `status` and the exact clock-derived `updated_at`; never estimate or invent the time.
3. Move the file out of `working/` into the folder matching the chosen next status.
4. When implementation is complete, move the task to `review`, never directly to `reviewing` or `done`. Only Spynel may claim `review` into `reviewing`, and only that independent review session may move it to `done`.
5. Use `waiting` only for a precise external condition and record `waiting_for` plus an optional RFC 3339 `wake_at`. Use `failed` for evidenced inability to complete and `cancelled` only for an explicit stop decision.

The markdown file is the durable source of truth. A chat response without the file update is not completion.
