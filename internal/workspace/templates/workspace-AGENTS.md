# Spynel Workspace DOX

This directory is durable state owned jointly by the user, Spynel, and dispatched agents.

## Contracts

- Markdown documents are the source of truth. Keep their body human-readable and front matter machine-readable.
- Do not edit `.spynel/runtime/leases/`; Spynel owns processing leases and recovery bookkeeping.
- Do not edit or publish `.spynel/history/`, `.spynel/attachments/`, `whatsapp.db`, or harness session data; they may contain secrets, remote media, and private conversations.
- Automatically managed speech models live outside this workspace. Original voice attachments remain workspace-owned.
- Prompts in `.spynel/prompts/` are user-overridable runtime contracts.
- The chat prompt owns a lightweight communication dispatcher. It answers questions and reports status, records one finite independently verifiable objective as a task, records long-lived or multi-round outcomes with measurable bars as goals, and leaves substantive execution to orchestrated document sessions. `/task` and `/goal` add user-overridable creation prompts to that same communication session; they are not deterministic file shortcuts. The dispatcher performs routine bounded inspection silently and returns exactly one brief, natural, outcome-first result with no preamble or progress messages for ordinary question, status, or delegation turns. Routine confirmations hide task/goal files, paths, IDs, metadata, lifecycle details, and orchestration mechanics; explicit technical-detail requests and dedicated status, job, log, or document-inspection commands remain precise. Telegram and WhatsApp routine confirmations never contain local-path Markdown links.
- Themes in `.spynel/themes/` are user-overridable semantic TUI palettes. Keep every required color and a unique safe name so Spynel can validate and switch them atomically.
- Extensions in `.spynel/extensions/` are trusted executable code. Review repositories before installing them.
- Spynel alone claims queued documents: task `todo -> working`, task `review -> reviewing`, goal `proposed -> planning`, and goal `review -> reviewing`. Each claimed state has a phase-specific lease and crash recovery. Agents record progress, update `status` and `updated_at`, and move claimed files only through phase-allowed transitions. Obtain current UTC from the environment immediately before durable timestamps; never estimate it.
- Task front matter carries explicit boolean `review_required`; missing or malformed values fail safe to review. Development/change and goal-derived tasks move completed attempts to `review`, where a distinct leased review session alone may accept them into `done`. Only bounded low-risk read-only collection may opt out and complete directly with its evidence boundary and uncertainty recorded. Goal planners define the success bar and create linked task rounds without implementing them, recording the exact current cohort in `round_task_ids`. Active goals carry no lease; after that cohort settles, a fresh goal-review session compares cumulative evidence to every criterion and either returns the goal to planning or moves it to `waiting`, `done`, or `abandoned`. Task policy never bypasses goal review, and task completion never directly completes a goal.
- Optional task notification metadata selects only an authorized stable origin and supported outcomes; transport secrets and unnecessary sender data never belong in task files.

## Structure

- `tasks/` owns finite work items and their queue, claim, review, waiting, and terminal folders.
- `goals/` owns proposed, planning, active, review, waiting, and terminal long-term outcomes.
- `prompts/` owns channel, slash-command creation, task implementation/review, goal planning/review, and recovery lead messages.
- `themes/` owns named semantic TUI and terminal-Markdown palettes.
- `extensions/` owns installed Git extensions and hook manifests.
- `history/` owns independent append-only histories per channel and conversation.
- `attachments/` owns private TUI, Telegram, and WhatsApp media referenced from messages.
- `models/` owns reusable local model weights downloaded by configured processors.
- `runtime/` owns Spynel leases, harness session mappings, and transient bounded work files.
