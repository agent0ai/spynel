# Plain CLI and automation

The plain CLI exposes Spynel's non-visual control plane without opening the full-screen TUI. It uses the same application service, durable histories, harness sessions, configuration transactions, jobs, logs, and trusted extension hooks as Telegram, WhatsApp, and the TUI.

## Proactive notifications

`spynel notify --origin CHANNEL/CONVERSATION "message"` queues a complete assistant message without invoking a coding harness. Supported origins are `telegram/TG-<id>`, `telegram/TG-group-<chat-id>`, `whatsapp/WA-<number>`, `whatsapp/WA-group-<group-id>`, `tui/<conversation>`, and `cli/<conversation>`. `--stdin` accepts at most 512 KiB and cannot be combined with positional text. The origin must already exist in durable history and remote origins must still satisfy the current allow-list/group policy. Success prints `queued notification <id>`; disconnected remote delivery remains in `.spynel/runtime/outbox/` for retry.

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

`status` is a typed, non-secret snapshot rather than presentation Markdown. JSON includes title/theme, abbreviated caller and primary IDs, Telegram/WhatsApp state, log/job counts, harness availability, model, sandbox, startup registration, abbreviated thread, active-turn state, and orchestrator counts.

`command` routes a non-visual slash command through the shared application handler. It joins the live owner when present; an offline framework command does not start a coding harness merely to read or mutate deterministic Spynel state. Common commands also have direct aliases:

```bash
spynel jobs
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

TUI-only visual or ownership operations—theme preview, title, welcome, resume screen, primary-window promotion, and quit—are intentionally rejected. Conversation resume has the disk-backed command above. `spynel whatsapp pair` remains the plain QR-pairing command.

`spynel update` uses the shared `/update` handler and reports the npm registry result. It makes no network request for an archive/development binary. An npm registry request has a hard ten-second deadline. `spynel update install` returns control to the npm launcher, which updates only after the Go executable exits and then restarts it; when an older or direct launch has no supervising wrapper, the response provides the manual `npm update` plus `/restart` path instead.

## Tasks, goals, and extensions

Harness-free script helpers create a single task in `todo` or a goal in `proposed` using the built-in baseline schema:

```bash
spynel task "Fix the failing login test"
spynel goal "Keep dependencies current"
spynel run --once
spynel extension list
spynel extension install https://github.com/example/spynel-hooks.git
spynel extension remove spynel-hooks
```

The shared slash commands have different, conversation-aware behavior. `spynel command --conversation NAME task "..."` and its `/task` equivalent load `.spynel/prompts/create-task.md`; `/goal` loads `create-goal.md`. Spynel combines that directive and the literal request with the ordinary communication prompt, then the communication agent checks for duplicates and creates or refines the complete document. These slash paths therefore require the selected harness, while the direct `spynel task` and `spynel goal` helpers remain suitable for offline scripts.

Tasks are finite objectives with `todo`, `working`, `review`, `reviewing`, `waiting`, `done`, `failed`, and `cancelled` statuses. Goals have a separate `proposed`, `planning`, `active`, `review`, `reviewing`, `waiting`, `done`, and `abandoned` lifecycle. Goal planners create numbered linked task rounds; independent goal review—not task completion—decides whether the bar has been met or another planning pass is required.

Installed extensions are trusted executable integrations. When enabled, their `message.received`, `harness.before`, and `harness.after` hooks run for CLI messages exactly as they do for interactive channels; orchestration hooks likewise run for `run --once` and the server loop. Hooks receive and return bounded JSON over standard streams and execute with Spynel's operating-system authority. Review repositories before installing them. See [Extensions and hooks](extensions.md).

## Exit status and cancellation

Argument, configuration, inactive-follow-up, hook, transport, and harness failures return an error and a nonzero process exit through the executable entry point. Ctrl+C or SIGTERM cancels the caller context; a loopback request disconnect does not impose a shorter provider timeout. Complete histories remain disk-backed even though every list/show/input operation is bounded.
