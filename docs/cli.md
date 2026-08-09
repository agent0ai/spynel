# Plain CLI and automation

Spynel is a classic, non-AI orchestration program; coding harnesses provide its external intelligence. The plain CLI exposes Spynel's non-visual control plane without opening the full-screen TUI. It uses the same application service, durable histories, harness sessions, configuration transactions, jobs, logs, and trusted extension hooks as Telegram, WhatsApp, and the TUI.

## Offline documentation

```bash
spynel docs
spynel docs goals
spynel docs goals page 1
spynel docs search review
spynel docs search "primary election" page 2 --format json
```

`docs` is embedded and deterministic: it does not load `.spynel/config.yaml`, join a primary server, read workspace state, or invoke a harness. The index, topic sections, and search results use stable topic/section references. Ordinary documents render whole. Oversized output is split only between complete records using a conservative estimate of one token per three Unicode runes and a 10,000-token page budget; split pages report that budget and their estimated tokens. A separate 128-entry, 64 KiB, and 48 Ki-rune ceiling prevents pathological output. Plain output is Markdown without ANSI/control sequences, including when redirected. `--format json` emits the versioned `spynel.docs/v1` document with kind, IDs, title, content, related references, and page metadata. Unknown topics, invalid pages, and oversized input return actionable structured errors; close misspellings suggest a valid topic. Flags may appear before or after the topic/query.

`spynel instructions [--config PATH]` is a separate read-only workspace inspection command. It reports the five role names, workspace-relative paths, presence, byte counts, and validation errors without displaying saved instruction contents or starting a server or harness.

Static topics describe user commands, workflow contracts, and implementation architecture. They label runtime-only subjects such as jobs and logs and never represent live values. Use `spynel status`, `jobs`, `tasks`, `goals`, `logs`, and the actual task/goal documents for current state.

`/log`, its page ranges, and case-insensitive search read the same retained newest-4,096-entry view after restart. `/log page <start>-<end>` accepts any positive ascending range and clamps the requested end to the oldest available retained page before rendering. Private JSONL session files live under `.spynel/runtime/logs`, rotate at 2 MiB, and retain at most eight files. `/log clear` removes both the active view and those retained files. Stored entries are attributed and bounded, with terminal controls and common credential forms removed before persistence.

## Proactive notifications

`spynel notify --origin CHANNEL/CONVERSATION "message"` queues a complete assistant message without invoking a coding harness. Supported origins are `telegram/TG-<id>`, `telegram/TG-group-<chat-id>`, `whatsapp/WA-<number>`, `whatsapp/WA-group-<group-id>`, `tui/<conversation>`, and `cli/<conversation>`. `--stdin` accepts at most 512 KiB and cannot be combined with positional text. The origin must already exist in durable history and remote origins must still satisfy the current allow-list/group policy. Success prints `queued notification <id>`; disconnected remote delivery remains in `.spynel/runtime/outbox/` for retry. Automatic notification agents receive framework-prepared `--event-key` and `--outcome` arguments; those internal arguments are accepted only when they match a persisted pending task transition, its exact origin, and current authorization. In `decide` mode the framework also prepares the same bound command with `--decline`, which accepts no message and records an explicit no-send decision.

Automatic task notifications use one bounded, inspectable decision-agent job that reads only the task's newest `## Progress`, then either skips or queues one concise message. Accepted text is recorded exactly in task progress. Routine notifications omit internal paths and task IDs; those details remain available through explicit task and diagnostic inspection. Enqueue and retry delivery do not invoke a harness, and no notification-specific response or reminder state is created.

Pass command flags before positional arguments. For example, use `spynel conversations show --json telegram TG-42`, not flags after `TG-42`. Use `--` before message text that begins with a dash.

## Send messages

```bash
spynel send [--config PATH] [--conversation NAME] [--stream|--json] [--stdin] [--attach PATH]... [--] TEXT
spynel message ...
```

`message` is a compatibility alias for `send`. The default conversation is `cli/local`; each other `--conversation` value owns an independent append-only history and harness session.

- Default output is only the last assistant-message item followed by a newline. Harness progress/preamble items remain in durable history but do not pollute final-only shell output.
- `--stream` writes every text delta as it arrives, including any provider progress items, and avoids repeating the complete final response.
- `--json` writes every `core.Event` as one NDJSON object. This already streams and cannot be combined with `--stream`.
- `--stdin` reads a multiline body of at most 512 KiB and cannot be combined with positional text. Use attachments for larger inputs.
- `--attach PATH` is repeatable. Each regular file is copied with the configured byte limit into `.spynel/attachments/cli/`, and an absolute `[Attachment name](<path>)` token is appended to the harness message. An attachment may be sent without text.

If an elected workspace server exists, `send` uses its private authenticated loopback service. This preserves one writer for the live harness, runtime jobs, logs, and sessions. Without an owner, the command builds a one-shot local service, waits for the response, and exits; it does not start Telegram, WhatsApp, or continuous orchestration. Shared configuration/task/extension commands remain usable when the selected harness is missing. A `message.received` extension may also answer or reject an offline message before harness dispatch; otherwise a natural-language send reports the harness-availability error.

Examples:

```bash
spynel send --conversation deploy --stream "Check the release state"
printf '%s\n' 'Review these multiline notes.' 'Keep the answer brief.' | \
  spynel send --conversation review --stdin
spynel send --conversation review --attach ./trace.log "Diagnose this trace"
spynel send --conversation bot --json "Report current work" | jq -c .
```

## Follow up during an active turn

```bash
spynel followup [send flags] TEXT
```

`followup` targets the same `cli/<conversation>` key as `send`, but it is strict: the elected service rejects it before writing history unless that conversation currently has an active harness execution. Codex and Pi use native turn steering. Harnesses that declare queue semantics retain follow-ups in the same session; adjacent ordinary messages that accumulate before the next turn are combined in arrival order into one provider prompt. This makes a failed shell race visible instead of silently starting unrelated work.

When native steering transfers output ownership, the earlier waiting CLI process receives a terminal transport status and exits successfully; the follow-up process receives subsequent deltas and the final response. This is the same emitter handoff used to keep overlapping remote-channel typing state correct.

An ordinary second `send` still follows the shared channel behavior: it steers or queues when the conversation is active and starts a new turn when idle.

## Inspect and resume conversations

```bash
spynel conversations list [--config PATH] [--limit 1..1000] [--json]
spynel conversations show [--config PATH] [--tail 1..1000] [--chars 1..2097152] [--json] CHANNEL CONVERSATION
spynel conversations resume [--config PATH] [--json] CHANNEL CONVERSATION
```

`list` reads bounded previews from disk, newest first. Plain output is tab-separated; JSON output is an array with channel, conversation, update time, last role, preview, and path.

`show` reads only the newest bounded tail. Plain output includes timestamps and roles; JSON output contains metadata plus the history entries.

`resume` makes a point-in-time copy of any TUI, Telegram, WhatsApp, or CLI history into a new `cli/resume-<short-id>` conversation. It never rewinds or mutates the source. Plain output is only the new conversation name so it can be captured directly:

```bash
branch=$(spynel conversations resume telegram TG-42)
spynel send --conversation "$branch" "Continue this conversation from the saved context"
```

## Status and framework commands

```bash
spynel status [--config PATH] [--conversation NAME] [--json]
spynel command [--config PATH] [--conversation NAME] [--json] NAME [ARGUMENTS...]
```

`status` is a typed, non-secret snapshot rather than presentation Markdown. JSON keeps title plus abbreviated caller/primary IDs, then live-job, durable active task and waiting-subset counts, durable active goal counts, orchestrator lease/dispatch counts, semantic-heartbeat ownership/deadline, Telegram/WhatsApp state, logs, harness availability, model, sandbox, startup registration, and active-turn state. Theme and conversation thread are not status fields.

`command` routes a non-visual slash command through the shared application handler. It joins the live owner when present; an offline framework command does not start a coding harness merely to read or mutate deterministic Spynel state. Common commands also have direct aliases:

```bash
spynel jobs
spynel tasks
spynel tasks --limit 50
spynel tasks recent --days 14
spynel tasks review
spynel tasks waiting --detail
spynel tasks done
spynel tasks failed
spynel tasks --json failed
spynel goals
spynel goals active
spynel goals review
spynel goals failed
spynel goals all --days 30
spynel job info 3
spynel job message 3 "Prioritize the API regression"
spynel job ping 3
spynel job kill 3
spynel log page 2
spynel log search webhook
spynel stop --conversation deploy
spynel clear --conversation deploy
spynel harness codex
spynel model gpt-5
spynel telegram on
spynel whatsapp off
spynel update
spynel update install
spynel config set workspace.history_max_messages 40
spynel command help commands
```

Options such as `--config`, `--conversation`, and `--json` must precede alias arguments (`spynel log --json search webhook`). Prefer typed `spynel status --json` over `spynel command --json status`, whose NDJSON response follows the generic event contract.

`spynel tasks` and `spynel goals` are deterministic automation aliases for the shared `/tasks` and `/goals` durable-work inspectors; they never start a harness. They join the elected application service when available or use the same harness-free local service, so an external terminal program observes the same state as the TUI and authorized Telegram/WhatsApp conversations. Bare commands use the newest-first `open` view and show all nonterminal work, with at most 20 rendered entries. `recent` uses `updated_at` from the last three days for tasks or seven days for goals. The remaining semantic views are `active` (pre-review work), `review` (queued or claimed review), `waiting`, `done`, `failed` (failed/cancelled tasks or abandoned goals), and `all`. Every compact entry uses two logical rows for the title and a status/update/counter/current-step summary. `--days N` narrows any view, `--limit N` accepts 1 through 100, and `--detail` adds only the durable ID, Markdown basename, task review policy, and exact created/updated timestamps. Shared `--config`, `--conversation`, and `--json` flags precede the view or list options. JSON mode uses the generic NDJSON response-event contract: the terminal `final` event's `text` field contains the same bounded Markdown listing. Folder status is authoritative; bounded warnings keep invalid or unreadable documents visible without following symlinks or exposing full paths.

`spynel jobs` renders two logical rows per active execution: emphasized job number plus a bounded message/Markdown filename, then compact lifetime, cumulative provider steps (`N▶`), an optional durable task implementation-attempt count (`M↻`), canonical execution status, and origin. For example, `3h27m 13▶ 4↻` means thirteen provider steps and the fourth implementation attempt. Provider steps survive phase, recovery, control, continuation, owner, and process-local job-number changes. The semantic heartbeat appears locally as a `heartbeat` job with `semantic_audit` phase and `audit` state and remains registered until its provider actually releases, even after timeout, cancellation, or primary handoff. Live conversations show current execution age and `1▶`; goals, legacy work, and other jobs without a real task `attempt` omit `↻`. `spynel job info NUMBER` uses the same process-local snapshot and labels those same two values as `Provider steps (▶)` and, when present, `Implementation attempts (↻)`, while distinguishing current execution age from durable lifetime before adding start time, last-activity age, reconnect attempt, recovery count, bounded detail, and abbreviated execution identity. For orchestrated Markdown work it also reads only allowlisted task/goal metadata, exact lease state/heartbeat, and at most the three newest bounded `## Progress` entries; it never returns arbitrary front matter, full paths, session keys, notification origins, or harness transcripts. Live execution status is distinct from workflow phase and durable document outcome: a task in `waiting`, `done`, or `failed` is not shown as an active worker.

`spynel job message NUMBER TEXT` and `/job message NUMBER TEXT` deliver nonterminal operator guidance to an active orchestrator job without changing its objective, session, lease, emitter, review phase, or notification destination. `job ping NUMBER` is the concise progress form: it asks the existing agent to record current progress, blockers, and next action in the durable document at the next safe opportunity, then continue. Acknowledgement is bounded and reports delivered, queued, duplicate, terminal, stale, unauthorized, or backpressure state; it does not wait for provider completion. Native steering and ordered same-session queues remain harness behavior. Queues hold at most eight controls per job and identical recent retries are applied once. If a control causes a provider final while the exact durable owner/session/file is still in a claimed nonterminal phase, Spynel permits one automatic same-session continuation; a second final falls through to ordinary reconciliation/recovery. Remote channels see and control only jobs tied to their currently authorized notification origin, while local TUI and CLI operators may inspect workspace jobs.

TUI-only visual or ownership operations—theme preview, title, welcome, resume screen, primary-window promotion, and quit—are intentionally rejected. Conversation resume has the disk-backed command above. `spynel whatsapp pair` remains the plain QR-pairing command.

`spynel update` uses the shared `/update` handler and reports the npm registry result. It makes no network request for an archive/development binary. An npm registry request has a hard ten-second deadline. `spynel update install` returns control to the npm launcher, which updates only after the Go executable exits and then restarts it; when an older or direct launch has no supervising wrapper, the response provides the manual `npm update` plus `/restart` path instead.

## Tasks, goals, and extensions

Harness-free script helpers create a single task in `todo` or a goal in `proposed` using the built-in baseline schema:

```bash
spynel task "Fix the failing login test"
spynel task --no-review "Correct the README typo and verify the result"
spynel task inspect .spynel/tasks/todo/example.md
spynel goal "Keep dependencies current"
spynel run --once
spynel extension list
spynel extension install https://github.com/example/spynel-hooks.git
spynel extension remove spynel-hooks
```

The shared slash commands have different, conversation-aware behavior. `spynel command --conversation NAME task "..."` and its `/task` equivalent combine the user-overridable `.spynel/prompts/create-task.md` directive and the literal request with the ordinary communication prompt; `/goal` uses the matching goal directive. The communication agent checks for duplicates and creates or refines the complete document. These slash paths therefore require the selected harness, while the direct `spynel task` and `spynel goal` helpers remain suitable for offline scripts.

Tasks are finite objectives with `todo`, `working`, `review`, `reviewing`, `waiting`, `done`, `failed`, and `cancelled` statuses. `harness.reviews` defaults to `skip-trivial`, where review defaults safely to required but creators choose it by expected value: broad, high-risk, hard-to-reverse, or materially uncertain work normally uses review; read-only work and minor localized reversible changes may use `--no-review` with proportionate verification, evidence, and residual uncertainty. `always` overrides `--no-review`; `never` forces direct completion. Goal planners follow the same global mode for derived tasks, while independent goal outcome review—not task completion—still decides whether the goal bar has been met.

Installed extensions are trusted executable integrations. When enabled, their `message.received`, `harness.before`, and `harness.after` hooks run for CLI messages exactly as they do for interactive channels; orchestration hooks likewise run for `run --once` and the server loop. Hooks receive and return bounded JSON over standard streams and execute with Spynel's operating-system authority. Review repositories before installing them. See [Extensions and hooks](extensions.md).

## Exit status and cancellation

Argument, configuration, inactive-follow-up, hook, transport, and harness failures return an error and a nonzero process exit through the executable entry point. Ctrl+C or SIGTERM cancels the caller context; a loopback request disconnect does not impose a shorter provider timeout. Complete histories remain disk-backed even though every list/show/input operation is bounded.
