# Plain CLI and automation

Spynel is a classic, non-AI orchestration program; coding harnesses provide its external intelligence. The plain CLI exposes Spynel's non-visual control plane without opening the full-screen TUI. It uses the same application service, durable histories, harness sessions, configuration save/reload boundary, jobs, logs, and trusted extension hooks as Telegram, WhatsApp, and the TUI.

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

Runtime logs contain application diagnostics; per-job archives contain the ordered provider-neutral event stream for one numbered job generation. `/log clear` does not delete job archives. `/cleanup [days]` owns their age-based retention.

## Proactive notifications

`spynel notify --workdir /absolute/workspace (--origin CHANNEL/CONVERSATION | --recent-authorized) --message "message"` queues a complete assistant message without invoking a coding harness. Supported explicit origins are `telegram/TG-<id>`, `telegram/TG-group-<chat-id>`, `whatsapp/WA-<number>`, `whatsapp/WA-group-<group-id>`, `tui/<conversation>`, and `cli/<conversation>`. `--workdir` selects that workspace's canonical `.spynel/config.yaml`; `--config` and positional message text remain available for operator compatibility. The shared enqueue boundary removes ANSI/ECMA-48 sequences, terminal control strings and replies, and other unsafe C0/C1 controls while preserving tabs, newlines, Unicode, and Markdown; it rejects input left empty by normalization before delivery or history append. The origin must already exist in durable history and remote origins must still satisfy the current allow-list/group policy. Success prints `queued notification <id>`; disconnected remote delivery remains in `.spynel/runtime/outbox/` for ordinary retry. The command has no task-transition flags and never edits a task.

Automatic task notifications use one ordinary asynchronous agent dispatch with exactly one bounded task-evidence block and the CLI syntax above. Safely fitting tasks are complete; oversized tasks provide deterministic valid-UTF-8 beginning and recent-end excerpts, an explicit untrusted middle-omission marker and omitted-byte count, plus the exact absolute authoritative path for full-file inspection. The agent decides whether to call the CLI and directly appends its successful send, skip reason, or CLI failure to task progress. Spynel does not parse the provider response, create a notification result, or maintain notification-specific persistence or recovery.

`spynel notify --recent-authorized --message TEXT` is the explicit alternative to `--origin`; the two modes are mutually exclusive. The application resolves the most recent user-active conversation, revalidates current authorization, queues through the same durable outbox, and returns no recipient/history data. Multiple authorized remote users—including separate Telegram and WhatsApp principals—fail closed because Spynel does not infer identity links; groups, tied recency, revocation, unknown conversations, and missing activity also fail closed. The heartbeat agent may use this primitive after deciding from task progress that a stale wait merits a reminder; the runtime owns no reminder policy or state.

Pass command flags before positional arguments. For example, use `spynel conversations show --json telegram TG-42`, not flags after `TG-42`. Use `--` before message text that begins with a dash.

## Send messages

```bash
spynel send [--config PATH] [--conversation NAME] [--stream|--json] [--stdin] [--attach PATH]... [--] TEXT
```

The default conversation is `cli/local`; each other `--conversation` value owns an independent append-only history and harness session.

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

`spynel jobs` lists live executions. `spynel jobs recent` lists up to 20 newest workspace-local archives by job number, while `spynel job info <number>` and `spynel job output <number> [tail <bytes>]` inspect bounded metadata or the newest captured event bytes. Numbers advance from 1 through 9999 and wrap to 1; after reuse, inspection selects the newest generation. The same slash commands work through authenticated channels and the terminal API because all use the shared application handler.

`/trigger` lists triggerable processes. `/trigger orchestrator` performs an immediate serialized route pass, while `/trigger heartbeat` starts the primary semantic audit only if one is not already active. `/cleanup [days]` runs safe retention with a seven-day default; it uses strict whole-day validation and reports removed conversations, removed job archives and bytes, archived terminal tasks, protected items, and failures. `/new` is TUI-only because it switches the active durable conversation identity and shows that new conversation's normal welcome screen; the prior conversation remains available through `/resume`.

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

`spynel jobs` renders the same global workspace snapshot as authenticated Telegram, WhatsApp, TUI, and shared application/API callers. Ordinary active executions use two logical rows: emphasized job number plus a generic live-conversation label or bounded Markdown filename, then compact lifetime, cumulative provider steps (`N▶`), an optional durable task implementation-attempt count (`M↻`), canonical execution status, and provider channel. Conversation IDs and live prompt text are omitted. The semantic heartbeat appears as an ordinary `heartbeat` worker job and remains registered until its provider releases; its provider prose is ignored and no separate heartbeat-result or health rows exist. `spynel job info NUMBER` exposes the same bounded job and durable-work evidence without prompts, origins, or transcripts.

`spynel job message NUMBER TEXT` and `/job message NUMBER TEXT` deliver nonterminal operator guidance to an active orchestrator job without changing its objective, session, lease, emitter, review phase, or notification destination. `job ping NUMBER` is the concise progress form: it asks the existing agent to record current progress, blockers, and next action in the durable document at the next safe opportunity, then continue. Acknowledgement is bounded and reports delivered, queued, duplicate, terminal, stale, unauthorized, or backpressure state; it does not wait for provider completion. Native steering and ordered same-session queues remain harness behavior. Queues hold at most eight controls per job and identical recent retries are applied once. If a control causes a provider final while the exact durable owner/session/file is still in a claimed nonterminal phase, Spynel permits one automatic same-session continuation; a second final falls through to ordinary reconciliation/recovery. Every authenticated workspace interface sees and controls the same live jobs wherever the operation is supported. Conversation identity and `notify.origin` do not restrict access, and job controls do not change notification routing.

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
