# Architecture

Spynel keeps deterministic state and lifecycle management separate from harness intelligence.

| Boundary | Ownership |
| --- | --- |
| `cli` | Command parsing, process composition, initialization fallback, and lifecycle. |
| `core` | Transport-neutral messages, streamed events, form screens, controls, and slash-command metadata. |
| `config` | Typed defaults/validation, atomic private persistence, and newest-only live snapshots. |
| `app` | Shared commands, screen construction, configuration transactions, per-conversation dispatch, status, logs/jobs, onboarding, and branching. |
| `channel` | TUI/Telegram/WhatsApp adapters plus isolated hot-reload supervision. |
| `harness` | Harness contract, idle-only runtime switching, Codex app-server, and Claude Code print-mode sessions. |
| `history` | Append-only JSONL, bounded reverse reads, discovery previews, clearing, and point-in-time branch copies. |
| `media` | Private streamed attachment commits and serialized disk-backed Whisper processing. |
| `markdown` | GitHub-flavored terminal rendering plus Telegram HTML and WhatsApp-native conversion. |
| `orchestrator` | Markdown claims, routes, leases, heartbeats, jobs, and stale recovery. |
| `startup` | Reversible workspace-specific systemd, launchd, and Task Scheduler registration. |
| `extensions` | Trusted Git installation and bounded executable-hook protocol. |
| `workspace` | Embedded editable configuration/prompts/DOX templates and private directory creation. |

`cmd/spynel` is composition only. A channel translates external traffic into `core.Message` and calls the application; it never knows how Codex or Claude Code works. The application and orchestrator depend only on `harness.Harness`.

## Screen contract

A `core.Screen` is a transport-neutral form or action list rendered instead of chat. The TUI preserves transcript/composer state while a screen is open. A child screen names its parent; the TUI snapshots and restores the parent's controls, unsaved edits, focus, disclosure state, and scroll position when the child completes or is escaped. Scalar form controls and text commands use the same `config.Setting` keys, so validation and persistence do not diverge between local and remote setup.

Initialization is a required screen because no application service exists before `spynel.yaml`. Once `workspace.Init` succeeds, the same process loads the new project. It enters the welcome/chat surface when Codex or Claude Code was detected, otherwise it opens the coding-harness chooser first. Interactive `spynel init` follows the same path; `--no-start` is the explicit automation mode. Welcome, configuration, harness/model selection, WhatsApp QR pairing, and conversation browsing use the same screen machinery.

## Harness lifecycle

The harness supervisor owns at most one running harness and exposes readiness without preventing the rest of Spynel from starting. A missing selection or executable can be recorded for setup when no harness is running. Reconfiguration constructs a replacement before swapping a working harness and refuses to switch while the current harness has active work.

Codex starts one `codex app-server --stdio` process, performs the initialize handshake, and maps stable Spynel session keys to persisted thread IDs. An idle message uses `turn/start`; a message arriving during an active turn uses `turn/steer`; `/stop` uses `turn/interrupt` for that conversation only. `model/list` supplies its model catalog.

Claude Code starts a bounded print-mode process for each turn with stream-JSON/partial-output support. The stored Claude session ID is resumed on the next turn, and the process is interruptible. Its documented model aliases form the selector catalog; exact configured identifiers are preserved.

Stable session keys include:

- `chat:tui:<local-or-branch>`;
- `chat:telegram:TG-<user-id>` and `chat:telegram:TG-group-<chat-id>`;
- `chat:whatsapp:WA-<number>` and `chat:whatsapp:WA-group-<group-id>`;
- `orchestrator:<route>:<document-id>`.

Codex and Claude session mappings use separate private files. `/clear` first resets the inactive mapping, then clears history; if reset fails, history is preserved. `/new` resets only the harness session and leaves history intact.

## Channel lifecycle

The channel supervisor consumes only the newest persisted settings snapshot. A fingerprint change cancels the stale channel instance and starts a replacement; disabling publishes an unconfigured state. Build/start failures become connection errors and runtime logs, while a five-second reconciliation tick retries enabled transports. No failed bot is allowed to terminate the TUI or task manager.

Telegram and WhatsApp each use a bounded incoming queue. External media streams directly into a private temporary file with a byte cap, fsyncs, and becomes visible through a collision-safe commit only after success. Voice transcription uses one shared serial token, on-disk FFmpeg segments, one Whisper child at a time, bounded child output, and a bounded final transcript.

## Process lifecycle

`/restart` is dispatched by the shared application service on every channel. The service persists and emits its final acknowledgment before publishing a coalesced process request. The CLI gives the TUI enough time to render that event, cancels the server context, closes the harness, restores the terminal, and relaunches the current executable with the active server arguments and working directory. Unix replaces the process in place so service-manager identity is preserved; Windows starts the successor with inherited standard streams before the old process exits. A TUI entered through `spynel init` restarts as the equivalent `spynel serve --tui --config <initialized-file>` invocation instead of trying to initialize the existing workspace again.

## History and memory

Each channel/conversation has an independent append-only JSONL file. Harness context reads backward in 64 KiB blocks and stops at both the configured message and character limits. Oversized single records are bounded rather than forcing the complete file into memory.

The TUI startup and live display is a separate 500-entry/500,000-rune tail with an explicit omission notice. `/resume` scans filenames, reads at most one newest entry for each preview, and returns at most 100 choices. Selecting one captures the source size under its conversation lock, then streams exactly that point-in-time prefix to a new random eight-character TUI branch. Only the fixed display tail is loaded; the complete copied JSONL remains on disk. Remote channels continue using their stable histories and are never rewound.

Process logs are bounded in memory and runtime updates use newest-only channels. Jobs contain only active execution metadata. Audio input, models, attachments, complete conversations, and obsolete harness output are disk-backed instead of retained as application state.

## Configuration transactions

Commands and forms clone the current config, parse every change, validate the complete result, and atomically persist it. The public harness contract contains `name`, `model`, and `sandbox`; executable discovery, working directory, approval policy, network flag, and effort are derived during composition. Sandbox values are normalized to each provider's native protocol, with unrestricted access as the default. Harness changes are staged before persistence. Startup registration is applied after persistence but rolls YAML and the staged harness back if the OS operation fails. Secret values never enter replies, status, or durable command history.

## Orchestration durability

Spynel atomically renames an eligible Markdown document from `source` to `working`, updates its front matter, and records a lease under `.spynel/runtime/leases/`. The in-memory in-flight map prevents duplicate goroutines; the durable lease and claimed path prevent duplicate work across restarts.

Every harness event refreshes the heartbeat. A completed harness turn does not remove the lease by itself, because the agent still owns the durable file transition. If the file remains claimed beyond `stale_after`, Spynel resumes the document session with the recovery prompt. Completion is therefore based on explicit filesystem state, not optimistic interpretation of chat text.

## Extension direction

New built-in transports implement `channel.Channel`; new coding agents implement `harness.Harness`. Project extensions remain executable hooks with JSON stdin/stdout so they are portable across languages and Go versions. They are intentionally trusted code rather than a misleading partial sandbox.
