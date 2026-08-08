# Spynel

Spynel is a cross-platform Go application with two jobs: provide an excellent interface to coding harnesses, and run reliable Markdown-backed tasks and goals. Release archives bundle the small native inference libraries required for local speech recognition.

- A themeable, layered Bubble Tea terminal UI with guided setup and streaming Markdown.
- Telegram polling or webhook delivery, access rules, groups, media, and native Markdown conversion.
- Native WhatsApp multi-device support with in-TUI QR pairing, persisted credentials, media, and native formatting.
- Built-in Codex app-server and Claude Code harnesses with independent, resumable sessions.
- Disk-backed conversation histories with bounded prompt context and TUI/CLI browsing and branching.
- Local NVIDIA Parakeet voice transcription with strict file, duration, chunk, and memory limits.
- Durable task/goal routes, persisted leases, stale-work recovery, and numeric runtime jobs.
- Curated offline `spynel docs` topics with bounded Markdown/search and versioned JSON for agents and scripts.
- Reversible per-workspace startup registration on Linux, macOS, and Windows.
- Trusted, language-neutral executable hooks installed from Git repositories.

There is deliberately no OpenAI-compatible HTTP façade and no Python or Node sidecar. Channels and harnesses meet explicit Go interfaces, while deterministic state remains in Spynel.

## Quick start

Install the published package with Node 18 or newer. The npm wrapper downloads the matching checksummed native archive and keeps its companion speech libraries beside the executable:

```bash
npm install --global spynel
spynel
```

On an interactive TUI start, npm installations spend at most ten seconds checking the registry. When a newer release exists, Spynel asks before running `npm update`. Headless commands do not perform this proactive check, and operating-system login services always pass an automatic-start marker that disables it. `/update` performs an explicit check in any channel; `/update install` lets the npm launcher stop, update, migrate saved workspace state, and restart safely. Release-archive and development binaries report that npm updates are unavailable instead of changing their installation.

The development helper downloads a pinned Go toolchain into the ignored repository-level `.spynel-dev/` directory when Go is unavailable:

```bash
git clone https://github.com/agent0ai/spynel.git
cd spynel
./scripts/dev.sh build

spynel_source="$(pwd)"
spynel_playground="${TMPDIR:-/tmp}/spynel-playground"
mkdir -p "$spynel_playground"
cd "$spynel_playground"
"$spynel_source/bin/spynel"
```

In a directory without `spynel.yaml`, the final command opens a pink Spynel setup screen. Choose **Initialize Spynel** to create the private workspace and continue directly to chat. Explicit initialization is also available:

```bash
spynel init --dir /path/to/spynel-playground
```

In an interactive terminal, `init` continues into setup/chat in the same process. Scripts can use `spynel init --no-start --dir ...` to initialize only.

Spynel detects and selects an installed supported harness automatically. If neither is found, it opens a two-choice setup screen with detection state and installation guidance:

- `codex` for Codex app-server sessions; or
- `claude` for Claude Code streaming print-mode sessions.

The welcome banner sits at the top of the scrollable chat, keeps the composer active, and introduces Spynel as "Spy" in concise, conversational language. Both names and the terminal logo use the active theme's primary accent. It always points to `/help` and includes Telegram or WhatsApp setup hints only while that transport is disconnected. A server can still start when its harness is unavailable so configuration and the task manager remain available. Run `doctor` from the shell after setup:

```bash
spynel doctor
```

For a headless server, use an explicit configuration path when useful:

```bash
spynel serve --config /path/to/spynel-playground/spynel.yaml
```

For scripts, tests, or a terminal without the full-screen UI, send one message and wait for its final response:

```bash
spynel send --config /path/to/spynel-playground/spynel.yaml \
  --conversation automation "What work is currently active?"
```

The named CLI conversation has its own durable history and harness session (`message` remains an alias). Add `--stream` for live text deltas, `--json` for NDJSON events, `--stdin` for a bounded multiline body, or repeat `--attach PATH` to copy files into private workspace storage. When `serve` or a TUI owner is already running, commands join that process and can steer its active CLI conversations:

```bash
spynel followup --conversation automation "Prioritize the failing API test"
spynel status --conversation automation --json
spynel conversations list --json
spynel conversations show --tail 20 telegram TG-1234
spynel conversations resume telegram TG-1234
spynel command --conversation automation config get harness.sandbox
spynel docs goals
spynel docs search review --format json
```

`followup` deliberately fails when that exact CLI conversation has no active execution; ordinary `send` starts a new turn when idle. Neither command starts the continuous orchestration loop. Use `spynel run --once` to process queued documents or `spynel serve` for continuous dispatch. See the [plain CLI reference](docs/cli.md) for output contracts, conversation branching, command aliases, attachments, and trusted extension hooks.

`spynel docs` is a separate, non-visual documentation interface. It reads only reviewed content compiled into the binary, works without an initialized workspace or primary server, and never invokes a coding harness. Use `spynel docs`, `spynel docs tasks`, `spynel docs search lease`, or add `--format json` for the versioned `spynel.docs/v1` schema; `page N` is needed only when unusually large results cross the documented budget. Static docs deliberately exclude histories, leases, credentials, notification origins, environment values, and live runtime state; use `status`, `jobs`, `logs`, and durable workspace files for those. See [agent-readable documentation](docs/agent-docs.md) for the schema and contributor workflow.

You can open multiple TUI windows in the same initialized folder. Spynel elects one process to own the server loops (Telegram, WhatsApp, and continuous task/goal orchestration), while every window connects to that process over an authenticated loopback endpoint. When no owner is running, the election winner resumes the latest TUI history and harness thread. A window opened beside an existing owner starts an independent `tui/local-<instance-id>` history and shows the welcome banner. The owner writes `.spynel/runtime/primary.json`, refreshes it every five seconds, and is considered stale after 30 seconds. Election updates are serialized, so one waiting window wins a takeover; graceful exits hand ownership over immediately. From an idle secondary TUI, `/primary` safely stops the old owner's services and reserves the next term for that requesting instance for ten seconds.

The main chat agent is intentionally a communication dispatcher. It answers questions and bounded status requests quickly, records finite work as durable tasks, and records recurring or multi-stage objectives as durable goals. Routine question, status, and delegation turns stay silent during inspection and produce exactly one brief, natural, outcome-first result without a preamble or tool-step narration. Task confirmations normally just acknowledge that work is underway; goal confirmations summarize the intended outcome and say planning has begun. Internal files, paths, IDs, metadata, and orchestration mechanics appear only when explicitly requested or through dedicated diagnostics, and routine Telegram/WhatsApp confirmations never contain local-path Markdown links. Independent orchestrator sessions do the implementation, builds, research, and other focus-heavy work. Codex steers active follow-ups through app-server. Claude uses native streaming guidance for permission modes that need no approval callback; tool-capable modes use bounded print turns and accept follow-ups through Spynel's ordered same-session queue. Both paths preserve still-relevant earlier coordination, and any future harness without native steering receives the same conservative queue automatically. Agent-authored durable timestamps are read from the environment's UTC clock rather than guessed.

Communication, task, goal, review, and recovery prompts receive one short instruction pointing at the current executable's absolute `docs` command. Agents query it only when Spynel-specific behavior is missing or potentially stale, follow stable references, and still treat explicit user instructions plus the nearest workspace/repository `AGENTS.md` or DOX contract as more specific authority.

The TUI displays live assistant progress. Telegram and WhatsApp suppress streamed deltas, transport status, and intermediate continuation responses, then send only the last terminal final response or error. Plain CLI streaming remains available only when explicitly requested with `--stream` or `--json`.

## Setup screens and commands

`/config`, `/telegram`, and `/whatsapp` replace chat temporarily with a shared borderless form canvas with one cell of padding on every side. On a fresh channel, bare `/telegram` or `/whatsapp` skips the empty status/config form and opens the setup wizard immediately; WhatsApp remains in first-time setup until at least one allowed number is saved, even if a session database already exists. Once configured, the ordinary channel form becomes the entry point. Each field puts its label on the left and a right-aligned value on the same row, with its description directly below and one blank row between settings; fixed choices show `‹ value ›`. Labeled rules such as **Core settings**, **Setup**, and **Basic settings** separate form groups and have two blank rows above them when they follow other content. Actions are compact filled buttons such as ` Wizard ↵ `, including the colored side spaces. Advanced settings use one combined clickable rule: ` Show Advanced Settings ↵ ` followed by the rule fill, changing to ` Hide Advanced Settings ↵ ` when expanded. Up/Down and Tab/Shift+Tab move between controls, typing edits text, Space/Enter cycles choices, Ctrl+S validates and saves the complete form, and Escape returns to the preserved chat. Harness/model choosers opened from config return to the exact preserved form after Enter or Escape. Configured channel forms start directly with **Telegram Status** or **WhatsApp Status**, with no redundant page heading, and place **Enabled** inside that live status section above configuration. The following **Setup** section starts with the wizard; Telegram then exposes token and allowed users under **Basic settings**, while WhatsApp exposes mode and required allowed numbers. Bold named tabs sit above a continuous line whose active-label segment is highlighted, making wizard progress clear without badges or filled tabs. Each wizard also provides clickable official help links, Back/Continue controls, concise safety guidance, and atomic essential-setting saves. WhatsApp skips a separate enable-choice step: continuing from allowed numbers saves the essentials, enables the channel in the background, and opens pairing. **Show QR** renders the QR alone across the full terminal and any key returns to the wizard. An expired pairing can be replaced with **Retry pairing** without restarting Spynel, and **Use pairing code** offers WhatsApp's phone-number linking alternative. Main configuration omits a redundant page heading and starts directly at **Core settings** with dedicated coding-harness and model selectors plus agent filesystem access, context, and startup essentials; task-loop, extension, speech, channel, and storage controls are under the combined Advanced settings disclosure.

WhatsApp pairing timeouts and terminal pairing errors automatically create a fresh session after a short delay. **Retry pairing** is only an optional immediate refresh.

The same typed catalog is usable from Telegram, WhatsApp, and the TUI:

```text
/config get workspace.history_max_messages
/config set workspace.history_max_messages 40
/config set harness.sandbox danger-full-access
/harness claude-code
/model sonnet
/theme hack-the-box
/telegram on
/telegram set allowed_users 123456789,trusted_username
/whatsapp set allowed_numbers 15551234567
/whatsapp on
```

Telegram refuses `/telegram ...` from Telegram itself, and WhatsApp refuses `/whatsapp ...` from WhatsApp itself, so a remote configuration mistake cannot disable the channel being used. Either remote channel may configure the other. Saved channel settings apply live: only the affected transport is replaced, and a failed transport is isolated and retried without terminating chat, the TUI, or task management.

`/model` asks the active harness for its catalog. Codex obtains the runtime catalog through app-server; Claude Code supplies its supported model aliases. Bare `/harness` and `/model` open real TUI choice lists: Up/Down moves between rows, the current value starts focused, and Enter applies immediately. `/model <exact-name>` remains available when a custom model identifier is needed. Spynel derives executable paths and the workspace directory, checking `PATH` and the conventional per-user `.local/bin`. `harness.sandbox` is user-controlled and defaults to `danger-full-access`; choose `workspace-write` or `read-only` when confinement is desired. Claude maps those choices to its native permission modes; workspace-write adds shell permission required for autonomous builds/tests, and because Claude refuses permission bypass for root/sudo, Spynel uses `acceptEdits` with all tools allowed as the privileged-account fallback. These are Claude's permission controls rather than an operating-system sandbox. A working harness is never replaced by a missing CLI, and replacement is refused while a turn is active.

`startup.enabled` creates or removes a workspace-specific background `spynel serve --config ...` registration:

- systemd user service, or a system service when Spynel runs as root, on Linux;
- launchd LaunchAgent, or LaunchDaemon when run as root, on macOS;
- an `ONLOGON` Task Scheduler entry on Windows.

Registration and configuration are transactional: if the operating-system change fails, Spynel restores the previous setting.

An npm installation registers the Node launcher as the service command so an explicit `/update install` can safely replace even a Windows executable after it exits. The generated service uses `--automatic-startup`, which suppresses proactive registry checks and prompts during login.

Service managers do not reliably inherit variables exported only in the current shell. For the fully guided path, save the Telegram token in the private form before enabling startup; if you keep `token_env`, make that variable available to the native service account. Harness login files normally survive because the service runs as the same user.

## Chat UI

The composer starts at one row, grows through ten wrapped or explicit rows, then scrolls one visual row at a time. As it resizes, a history viewport already at the bottom remains bottom-anchored so the newest message stays visible; a deliberately paged-up viewport preserves its exact offset. Enter sends; Shift+Enter inserts a newline. Up/Down stays in the composer and preserves visual-column movement across logical and wrapped rows; Up on the first visual row jumps to the complete input's start, while Down on the final visual row jumps to its final insertion position. PageUp/PageDown scrolls chat. Ctrl+C clears non-empty input, stops an active local harness turn when input is empty, and exits only when both are idle. Mouse reporting is disabled so ordinary terminal drag selection and copying remain available; the wheel is terminal-native.

Typing `/` at the beginning opens the command picker. Up/Down selects, Tab inserts, and Enter sends. The footer always contains one left-aligned, context-sensitive control-hint set and uses compact keyboard glyphs such as `↵ send`, `⇧↵ line`, and `⇞⇟ history`. Header and footer share the selected theme's main background and use a muted blend of its user accent for half-height ribbons: `▀▀` above and `▄▄` below, with no padding around items or separators. In the default Spynel theme those ribbons are restrained light blue related to the `You` label, while the Spynel identity remains pink. The compact header carries the animated two-cell logo plus customizable title, Telegram and WhatsApp state, and shared compact job/log counts (`999`, `1k`, `1.5k`, `15m`). Chat history and form screens are unframed themed canvases with one blank row above and below, one left inset, one inset before the right-edge scrollbar, and no individual message backgrounds. The composer and pickers retain exact-width borders painted on the page background; compact form actions use lighter semantic control surfaces.

Themes are editable YAML files under `.spynel/themes/`. The curated picker lists all dark themes first—`spynel`, `hack-the-box`, `github-colorblind-dark`, `gruvbox-dark`, `nord`, and `okabe-ito-dark`—followed by all light themes—`gruvbox-light`, `rose-pine-dawn`, `tol-muted-light`, `catppuccin-latte`, `okabe-ito-light`, and `solarized-light`. GitHub Colorblind Dark, Tol Muted Light, and both Okabe-Ito variants are explicitly color-blind friendly. Upgraded workspaces without theme files receive the same twelve as built-in fallbacks; `spynel init --force` adds missing revised files without replacing or deleting existing themes. Every TUI and terminal-Markdown color is semantic. `/theme` opens an inline list where Up/Down previews a palette immediately, Enter saves it, and Escape restores the previous one. `/theme <name>` applies directly and also works from Telegram or WhatsApp.

A running harness turn shows a compact Braille spinner immediately after its newest response character. Streamed message items are preserved in order. Sending a local slash command during the turn commits the current text, renders the command response, and opens a fresh spinner row without losing the continuing harness output.

Agent replies render GitHub-flavored Markdown. Explicit URI and absolute-file links use OSC 8 hyperlinks in supporting terminals. Telegram receives supported HTML and WhatsApp receives its native lightweight formatting. Renderer-added duplicate paragraph and code-block spacing is removed.

Bracketed pastes of at least 1,000 characters become atomic `[Pasted N chars]` display tokens while their complete text is dispatched. Pasted readable local paths are copied privately into `.spynel/attachments/` and become atomic `[Attachment filename]` links. Arrow, Backspace, and Delete treat these tokens as one unit. A terminal does not expose arbitrary binary clipboard image data to a TUI; drag or paste a filesystem path instead.

The TUI loads and retains at most 500 display entries/500,000 runes rather than holding an unbounded transcript in RAM; an omission notice links this behavior to the complete disk history. `/resume` lists the newest saved conversations from disk and copies the selected Telegram, WhatsApp, or TUI history into an independent TUI branch. The source conversation is never modified. `/clear` erases only the current branch/history and resets its harness session; it removes automatic onboarding without showing it again. `/welcome` appends and persists the same friendly introduction as an ordinary assistant message at the bottom of the conversation. The TUI message includes the primary-colored terminal logo, `/help`, and hints for only disconnected transports. Telegram and WhatsApp omit the logo and connection hints, retain native emphasis for the names, and show only the introduction plus `/help`.

## Runtime commands

All channels share the application commands. Start with `/help`; focused pages include `/help about`, `/help commands`, `/help extensions`, `/help config`, `/help channels`, and `/help workflows`.

Common operations:

```text
/status
/primary
/title Production API
/stop
/restart
/update
/update install
/task investigate and fix the failing login test
/goal keep the dependency backlog current
/run
/steer focus on the API regression first
/history
/resume
/log
/log page 2
/log page 1-3
/log search webhook
/log clear
/jobs
/job info 1
/job message 1 focus on the API regression
/job ping 1
/job kill 1
/new
/clear
/welcome
```

Every non-empty `/jobs` result places `Use /job info <number> to inspect a job.` immediately above the existing `/job kill` hint.

Attributed harness, orchestrator, media, channel, startup-registration, and extension-install diagnostics are captured instead of colliding with the alternate-screen UI. Startup helpers and Git installation retain bounded, separately attributed stdout/stderr and exit status without logging command arguments. Spynel keeps the newest 4,096 entries across restarts in private, rotating JSONL files under `.spynel/runtime/logs/`; each session file rotates at 2 MiB and at most eight files remain. Redaction, terminal-control removal, and the 4,096-rune entry bound happen before persistence. `/log` pages and searches that retained view, and `/log page <start>-<end>` accepts any positive ascending range while stopping at the oldest available retained page. `/log clear` clears memory, pending partial output, and retained files in append order. Missing, unwritable, corrupt, and partially written logs degrade to the usable in-memory view; persistence failures appear there as non-recursive diagnostics, and persistence waits are bounded to two seconds. `SIGKILL` and sudden power loss can still lose the final unsynced informational record. Active chat and orchestration executions receive short numeric job numbers. `/jobs` gives each accessible job two rows: the emphasized number plus bounded message or Markdown filename, followed by compact lifetime and cumulative provider steps such as `3h27m 13▶`; task jobs append their real durable implementation attempt, for example `4↻`. Durable work keeps provider steps across implementation, review, recovery, controls, continuations, owner restart, and process-local job-number replacement. Live conversations show current execution age and `1▶`; goals and jobs without a durable task attempt omit `↻`. `/job info <number>` labels the same provider-step and optional implementation-attempt values, distinguishes durable lifetime from current execution age, then shows status, last activity, reconnect/recovery detail, stable runtime identity, and an allowlisted sanitized view of linked task/goal metadata, lease freshness, and at most three newest progress entries. `/job message <number> <text>` safely guides an active orchestrator job through its existing session; `/job ping <number>` asks it to record progress, blockers, and next action without taking completion ownership. Controls use bounded ordered delivery, recent-retry deduplication, and at most one durable-state-gated continuation. Execution status (`running`, `reconnecting`, `recovering`, and similar) is live process state; workflow phase is implementation/planning/review, while `waiting`, `done`, and `failed` are durable document outcomes and are never presented as live worker states. `/job kill <number>` interrupts the selected session, while `/stop` targets the issuing conversation. `/status` exposes requesting/primary instance IDs, live jobs, durable active tasks with waiting as a subset, durable active goals, orchestrator activity, the actual primary-owned semantic-heartbeat deadline, channel health, harness/model/filesystem/startup state, logs, and turn state everywhere; theme and conversation thread are omitted. Long provider and instance IDs remain authoritative internally and are abbreviated only for display.

`/primary` is available only from a local TUI. If that TUI is secondary and no agent jobs are active, Spynel acknowledges the request, stops the current owner's API, channels, harness, and orchestrator, then publishes a fenced handoff that only the requester may claim. The reservation expires after ten seconds if the requester disappears, allowing normal election recovery.

`/restart` works from the TUI, Telegram, and WhatsApp. Spynel first sends and persists an acknowledgment, restores the terminal, stops the current runtime, and relaunches the same server mode in the same working directory. Saved configuration, histories, and harness session mappings remain available after reconnecting.

The TUI composer expands on the same keystroke that creates a wrapped row and keeps the cursor visible through wrapped-line navigation, paste, deletion, and explicit newlines. On an unusually short terminal, history and the picker yield before the bordered composer shrinks, so its cursor-bearing final row remains reachable inside the physical frame. Unusually narrow terminals use their reported width as a hard boundary instead of forcing a wider minimum; decorative side insets yield first so rows do not soft-wrap and the composer cursor and feasible border remain visible. When the composer grows or shrinks while you are reading older history, the transcript compensates by the same number of rows instead of jumping to the newest message; tail-follow still remains active near the newest page unless you recently scrolled upward. Transcript wrapping moves fitting words and numbers intact to the next row and only splits genuinely overwide tokens at Unicode grapheme boundaries. User text preserves repeated spaces and intentional indentation, and streaming/final Spy Markdown uses the same effective content boundary, so role labels, padding, and the scrollbar do not synthesize hyphens, leading spaces, or missing characters.

`/update` is npm-aware and bounds its registry request to ten seconds. It reports the installed and latest versions in chat. When the active server is managed by the npm launcher, `/update install` acknowledges the transition, stops Spynel, runs the appropriate local or global `npm update`, and starts the new binary. Compiled-in migration hooks and trusted `update.before`/`update.after` extension hooks receive `from_version` and `to_version` before the new version is recorded under `.spynel/runtime/`.

## Telegram

Create a bot with BotFather, then configure it from the TUI or WhatsApp. Prefer an environment variable over embedding a token:

```bash
export SPYNEL_TELEGRAM_TOKEN='replace-me'
```

```yaml
channels:
  telegram:
    enabled: true
    token_env: SPYNEL_TELEGRAM_TOKEN
    mode: polling
    allowed_users: ["123456789", "trusted_username"]
    group_mode: mention
```

Telegram cannot be enabled until `allowed_users` contains at least one numeric user ID or username, and the list cannot be cleared while Telegram remains enabled. If you do not know your numeric ID, message the third-party [@userinfobot](https://t.me/userinfobot) helper. `group_mode` is `mention`, `all`, or `off`. Webhook mode requires `webhook_url`, `webhook_listen`, and `webhook_secret`; it registers a secret-verifiable Telegram URL and binds only the configured local listener, which is intended to sit behind your HTTPS reverse proxy. Telegram private conversations use stable `TG-<numeric-user-id>` identities and groups use `TG-group-<chat-id>`.

Telegram's typing indicator and WhatsApp's composing presence begin before attachment handling or voice transcription and remain refreshed throughout the asynchronous agent turn. They end only with the final response/error or channel shutdown; overlapping work in one chat is reference-counted so a short framework command cannot hide a still-running main agent.

The setup options follow Agent Zero's proven per-bot connection controls: name/token, polling or webhook, access list, group policy, welcome message, message notice, and attachment retention. Spynel adds explicit token-environment, local webhook-listener, poll-timeout, and resource-limit controls. One Telegram bot is configured per Spynel workspace.

## WhatsApp

Spynel uses whatsmeow and a CGO-free SQLite credential store. Open `/whatsapp`, add at least one allowed number in the guided setup, and continue; Spynel enables the transport in the background. Select **Show QR** and scan the full-terminal code from **WhatsApp → Linked devices → Link a device**, or select **Use pairing code** and follow **Link with phone number instead**. If the session expires, **Retry pairing** requests fresh details. The shell command remains available for a plain terminal:

```bash
spynel whatsapp pair
```

`self-chat` accepts messages sent to your own chat and suppresses reply loops; `dedicated` treats the linked account as a bot number. At least one `allowed_numbers` entry is required before WhatsApp can be enabled; an empty list rejects every sender. When groups are enabled, Spynel responds only when mentioned or replied to. Credentials in `.spynel/whatsapp.db` are secrets. whatsmeow implements the unofficial WhatsApp Web protocol, so assess the account risk before using an important number.

Direct conversations use stable `WA-<number>` identities and groups use `WA-group-<group-id>`.

## Attachments and voice

Telegram documents/photos/video/audio/voice and WhatsApp documents/images/video/audio/stickers stream directly to private channel-specific attachment folders. The message passed to the harness includes an absolute `[Attachment filename]` link. `workspace.attachment_max_mb` is enforced during transfer, before a completed file becomes visible.

Agents can also return readable local files through either remote channel, including files outside the active workspace. Put `[Send attachment](</absolute/path/to/file>)` on its own line in the final response to send a native document, or `[Send photo](</absolute/path/to/image.png>)` to send an image as a native photo. Spynel removes the directive from visible text, resolves symlinks, requires an absolute path to a readable regular file, detects its media type, and reapplies the configured size limit to the opened file during delivery.

Voice transcription is enabled with NVIDIA Parakeet in new workspaces. Selecting `en` uses the English-only Unified EN INT8 model; `auto` or another supported European language uses multilingual TDT v3 INT8. The selected checksum-pinned model downloads on first supported use into the operating system's per-user cache under `spynel/speech/v1/parakeet`, so compatible workspaces reuse one copy. Cross-process locking, private partial paths, validation, and atomic publication prevent incomplete or duplicate final installs. A valid legacy `.spynel/models/parakeet/` version is copied safely into the shared cache without deleting its source; interrupted partials and unrelated files remain untouched. An explicit `speech.model_dir` stays authoritative, including when the platform user-cache directory is unavailable. Spynel decodes WAV, FLAC, and MP3 with miniaudio and decodes Telegram/WhatsApp Ogg/Opus voice notes through an in-process pure-Go codec before running sherpa-onnx locally. There is no Python, FFmpeg, external executable, installer, or PATH requirement. M4A/AAC, WebM, and other formats are rejected explicitly; the original attachment remains available to the harness.

Only one voice transcription runs at a time across all channels. Audio is downmixed and resampled to mono 16 kHz float PCM, with only one configured-duration chunk retained at a time. File size, maximum processed duration, chunk duration, model download/extraction, and final transcript are bounded. A transcription failure preserves the audio link and tells the harness to inspect it manually.

## Markdown task manager

Initialization creates configurable routes such as:

```text
.spynel/tasks/todo → working → review → reviewing → done
.spynel/tasks/working → done (explicit low-risk read-only policy only)
.spynel/goals/proposed → planning → active → review → reviewing → done
```

Tasks are single finite objectives. Boolean `review_required` defaults safely to `true`; malformed values also require review. Development/change and goal-derived work must use review. Only explicit bounded low-risk read-only collection whose report is the complete deliverable may use `false` and complete directly after recording sources, evidence boundaries, uncertainty, and an exact completion time. `spynel task --no-review REQUEST` selects that policy, while `spynel task inspect FILE` displays its effective value. A manual move to `review` is honored regardless of policy. `waiting`, `failed`, and `cancelled` cover documented side outcomes. Task policy never bypasses independent goal review, and Spynel does not guess success from chat output or harness exit.

Goals are long-term outcomes with explicit, measurable `success_criteria`. Spynel claims `proposed` into leased `planning`; the planner creates a numbered round of finite tasks linked by immutable `goal_id` and `goal_round`, records the exact cohort in `round_task_ids`, then moves the unleased goal to `active`. Task completion wakes prompt goal reconsideration. Settlement-only review is the default; an explicitly reasoned, short checkpoint may be configured when useful, while external waits belong in `waiting`. After eligibility Spynel immediately queues and claims a fresh goal review. The reviewer compares cumulative evidence against every criterion and chooses `done`, `waiting`, `abandoned`, or a separately leased planning pass for the next round. Task completion is evidence only and never completes a goal automatically.

Every claimed phase has a persisted lease under `.spynel/runtime/leases/`. Claims are journaled before rename and record their manager owner; a newly elected manager resumes foreign-owner claims without waiting the full phase timeout, recovers stale same-owner leases, and creates recovery leases for orphaned files in `working`, `planning`, or `reviewing`. Status folders therefore remain understandable after a process crash without blindly repeating partially applied work.

Optional task `notify` front matter selects an existing `channel/conversation` origin and `done`, `failed`, `waiting`, or `cancelled` outcomes. A selected terminal transition creates a restart-safe policy-keyed triage event. One dedicated bounded notification-agent session runs outside task/review scan locking, inspects only bounded task evidence, and returns a strict notify/skip decision, outcome-first message, optional exact question, choices, urgency, and capped follow-up policy; malformed or unavailable triage retries three times before deterministic safe fallback. Delivery remains durable and deduplicated across restarts. Actionable waiting/failure results deliver the exact question and choices on first contact and create a private action request. Native Telegram and WhatsApp outbound IDs are retained privately, so a platform reply selects the exact request; ordinary later messages receive only same-conversation pending context and are not acknowledgement by themselves. The request resolves only after the communication workflow durably moves the task away from the request's originating outcome, which also prevents duplicate resume. The primary semantic heartbeat enqueues capped reminders and cancels pending reminders after resolution. Explicit `notifications.contact_bindings` may associate authorized origins with one principal; private last-seen activity then selects that principal's most recently used authorized Telegram or WhatsApp contact. Missing bindings, revoked authorization, or no active remote contact fail closed to the origin. Optional UTC `notifications.quiet_hours` defer non-urgent reminders to the configured end while urgent requests bypass the window. `/status` exposes only triage and awaiting-response counts, never bodies or recipients; linked `/job info` output adds only action state, delivery channel, reminder due/count, and acknowledgement. The same outbox delivery path remains available manually with `spynel notify --origin telegram/TG-123 "Task finished"`.

`/task` and `/goal` are communication-agent commands rather than direct file shortcuts. Spynel combines the ordinary chat contract, the literal user message, and the user-overridable `.spynel/prompts/create-task.md` or `create-goal.md`; the communication agent avoids duplicates and writes the complete framework-compliant document. Its routine confirmation is brief and natural: it acknowledges task work or summarizes the goal outcome and says planning has begun, without exposing internal files, paths, IDs, metadata, or orchestration mechanics. Explicit detail requests and `/status`, `/jobs`, `/job info`, `/log`, and document inspection remain technically precise. The shell helpers `spynel task` and `spynel goal` remain harness-free deterministic creation paths for scripts.

If a claimed file remains in place and its lease stops receiving events for `stale_after`, Spynel resumes that document session with the recovery prompt. Persisted leases and filesystem checks prevent duplicate dispatch across restarts. Routes are configuration data, so additional workflows can define their own source/working paths, prompts, recovery prompts, and allowed next states.

The elected primary also runs a higher-level semantic workflow heartbeat with a 15-minute fixed delay by default. This is separate from five-second owner renewal, route scans, per-harness lease heartbeats, and deterministic stale recovery. Its bounded, read-only `.spynel/prompts/heartbeat.md` session audits durable tasks/goals, leases, live jobs, due waits, transitions, recovery repetition, review separation, and the notification outbox. Only validated `spynel.semantic-heartbeat/v1` results are accepted; repair requests enter existing serialized recovery, and notifications must match the affected document's enabled authorized origin. Repeated findings are persisted and deduplicated, with one-hour material-change and 24-hour persistence escalation thresholds. Set `orchestrator.semantic_heartbeat_minutes` to a whole value from 5 through 1440, or `0` to disable it. This interval and `orchestrator.enabled` apply live. An idle change resets the timer immediately; while an audit is running, status has no successor deadline, and the next exact timer starts only after terminal provider release using the latest setting.

## Extensions

Install only reviewed repositories; extension hooks execute with the full authority of the Spynel process:

```bash
spynel extension install https://github.com/example/spynel-audit-hooks.git
spynel extension list
```

See [extensions](docs/extensions.md) for the manifest and hook protocol.

## Build and verify

```bash
cd spynel
./scripts/dev.sh test
./scripts/smoke.sh
npm run test:npm
```

CGO-enabled release archives target Linux amd64/arm64, macOS amd64/arm64, and Windows amd64 and include the matching native speech libraries. The npm package is only a checksummed archive downloader/launcher. See [architecture](docs/architecture.md), [configuration](docs/configuration.md), [integrations](docs/integrations.md), the evidence-backed [coding harness compatibility matrix](docs/harness-compatibility.md), and [releasing](docs/releasing.md).
