# Plain CLI and automation

The plain CLI exposes Spynel's non-visual control plane without opening the full-screen TUI. It uses the same application service, durable histories, harness sessions, configuration transactions, jobs, logs, and trusted extension hooks as Telegram, WhatsApp, and the TUI.

## Offline documentation

```bash
spynel docs
spynel docs goals
spynel docs goals page 1
spynel docs search review
spynel docs search "primary election" page 2 --format json
```

`docs` is embedded and deterministic: it does not load `spynel.yaml`, join a primary server, read workspace state, or invoke a harness. The index, topic sections, and search results use stable topic/section references. Ordinary documents render whole. Oversized output is split only between complete records using a conservative estimate of one token per three Unicode runes and a 10,000-token page budget; split pages report that budget and their estimated tokens. A separate 128-entry, 64 KiB, and 48 Ki-rune ceiling prevents pathological output. Plain output is Markdown without ANSI/control sequences, including when redirected. `--format json` emits the versioned `spynel.docs/v1` document with kind, IDs, title, content, related references, and page metadata. Unknown topics, invalid pages, and oversized input return actionable structured errors; close misspellings suggest a valid topic. Flags may appear before or after the topic/query.

Static topics describe user commands, workflow contracts, and implementation architecture. They label runtime-only subjects such as jobs and logs and never represent live values. Use `spynel status`, `jobs`, `logs`, and the actual task/goal documents for current state.

`/log`, its page ranges, and case-insensitive search read the same retained newest-4,096-entry view after restart. `/log page <start>-<end>` accepts any positive ascending range and clamps the requested end to the oldest available retained page before rendering. Private JSONL session files live under `.spynel/runtime/logs`, rotate at 2 MiB, and retain at most eight files. `/log clear` removes both the active view and those retained files. Stored entries are attributed and bounded, with terminal controls and common credential forms removed before persistence.

## Proactive notifications

`spynel notify --origin CHANNEL/CONVERSATION "message"` queues a complete assistant message without invoking a coding harness. Supported origins are `telegram/TG-<id>`, `telegram/TG-group-<chat-id>`, `whatsapp/WA-<number>`, `whatsapp/WA-group-<group-id>`, `tui/<conversation>`, and `cli/<conversation>`. `--stdin` accepts at most 512 KiB and cannot be combined with positional text. The origin must already exist in durable history and remote origins must still satisfy the current allow-list/group policy. Success prints `queued notification <id>`; disconnected remote delivery remains in `.spynel/runtime/outbox/` for retry.

Automatic terminal-task notifications lead with what finished and the practical result from a valid bounded summary. Routine notifications omit internal paths, task IDs, and duration/attempt/review/rework metrics; those details remain available through explicit task and diagnostic inspection. Task completion never invokes a harness from notification formatting, enqueue, or retry delivery.

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

`followup` targets the same `cli/<conversation>` key as `send`, but it is strict: the elected service rejects it before writing history unless that conversation currently has an active harness execution. Codex uses native turn steering. Harnesses that declare queue semantics retain the follow-up in arrival order and run it in the same session. This makes a failed shell race visible instead of silently starting unrelated work.

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

`spynel jobs` renders two logical rows per active execution: emphasized job number plus a bounded message/Markdown filename, then compact lifetime, cumulative provider steps (`N▶`), an optional durable task implementation-attempt count (`M↻`), canonical execution status, and origin. For example, `3h27m 13▶ 4↻` means thirteen provider steps and the fourth implementation attempt. Provider steps survive phase, recovery, control, continuation, owner, and process-local job-number changes. Live conversations show current execution age and `1▶`; goals, legacy work, and other jobs without a real task `attempt` omit `↻`. `spynel job info NUMBER` uses the same process-local snapshot and labels those same two values as `Provider steps (▶)` and, when present, `Implementation attempts (↻)`, while distinguishing current execution age from durable lifetime before adding start time, last-activity age, reconnect attempt, recovery count, bounded detail, and abbreviated execution identity. For orchestrated Markdown work it also reads only allowlisted task/goal metadata, exact lease state/heartbeat, and at most the three newest bounded `## Progress` entries; it never returns arbitrary front matter, full paths, session keys, notification origins, or harness transcripts. Live execution status is distinct from workflow phase and durable document outcome: a task in `waiting`, `done`, or `failed` is not shown as an active worker.

`spynel job message NUMBER TEXT` and `/job message NUMBER TEXT` deliver nonterminal operator guidance to an active orchestrator job without changing its objective, session, lease, emitter, review phase, or notification destination. `job ping NUMBER` is the concise progress form: it asks the existing agent to record current progress, blockers, and next action in the durable document at the next safe opportunity, then continue. Acknowledgement is bounded and reports delivered, queued, duplicate, terminal, stale, unauthorized, or backpressure state; it does not wait for provider completion. Native steering and ordered same-session queues remain harness behavior. Queues hold at most eight controls per job and identical recent retries are applied once. If a control causes a provider final while the exact durable owner/session/file is still in a claimed nonterminal phase, Spynel permits one automatic same-session continuation; a second final falls through to ordinary reconciliation/recovery. Remote channels see and control only jobs tied to their currently authorized notification origin, while local TUI and CLI operators may inspect workspace jobs.

TUI-only visual or ownership operations—theme preview, title, welcome, resume screen, primary-window promotion, and quit—are intentionally rejected. Conversation resume has the disk-backed command above. `spynel whatsapp pair` remains the plain QR-pairing command.

`spynel update` uses the shared `/update` handler and reports the npm registry result. It makes no network request for an archive/development binary. An npm registry request has a hard ten-second deadline. `spynel update install` returns control to the npm launcher, which updates only after the Go executable exits and then restarts it; when an older or direct launch has no supervising wrapper, the response provides the manual `npm update` plus `/restart` path instead.

## Tasks, goals, and extensions

Harness-free script helpers create a single task in `todo` or a goal in `proposed` using the built-in baseline schema:

```bash
spynel task "Fix the failing login test"
spynel task --no-review "Collect the current service status and report uncertainty"
spynel task inspect .spynel/tasks/todo/example.md
spynel goal "Keep dependencies current"
spynel run --once
spynel extension list
spynel extension install https://github.com/example/spynel-hooks.git
spynel extension remove spynel-hooks
```

The shared slash commands have different, conversation-aware behavior. `spynel command --conversation NAME task "..."` and its `/task` equivalent load `.spynel/prompts/create-task.md`; `/goal` loads `create-goal.md`. Spynel combines that directive and the literal request with the ordinary communication prompt, then the communication agent checks for duplicates and creates or refines the complete document. These slash paths therefore require the selected harness, while the direct `spynel task` and `spynel goal` helpers remain suitable for offline scripts.

Tasks are finite objectives with `todo`, `working`, `review`, `reviewing`, `waiting`, `done`, `failed`, and `cancelled` statuses. Review is required by default and for every development/change or goal-derived task. `--no-review` is limited to explicit bounded low-risk read-only collection whose report is the deliverable; `task inspect` reports the effective fail-safe policy. Goals have a separate `proposed`, `planning`, `active`, `review`, `reviewing`, `waiting`, `done`, and `abandoned` lifecycle. Goal planners create numbered linked task rounds; independent goal review—not task completion—decides whether the bar has been met or another planning pass is required.

Installed extensions are trusted executable integrations. When enabled, their `message.received`, `harness.before`, and `harness.after` hooks run for CLI messages exactly as they do for interactive channels; orchestration hooks likewise run for `run --once` and the server loop. Hooks receive and return bounded JSON over standard streams and execute with Spynel's operating-system authority. Review repositories before installing them. See [Extensions and hooks](extensions.md).

## Exit status and cancellation

Argument, configuration, inactive-follow-up, hook, transport, and harness failures return an error and a nonzero process exit through the executable entry point. Ctrl+C or SIGTERM cancels the caller context; a loopback request disconnect does not impose a shorter provider timeout. Complete histories remain disk-backed even though every list/show/input operation is bounded.
