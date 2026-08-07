# Spynel

Spynel is a cross-platform Go application with two jobs: provide an excellent interface to coding harnesses, and run reliable Markdown-backed tasks and goals. Release archives bundle the small native inference libraries required for local speech recognition.

- A themeable, layered Bubble Tea terminal UI with guided setup and streaming Markdown.
- Telegram polling or webhook delivery, access rules, groups, media, and native Markdown conversion.
- Native WhatsApp multi-device support with in-TUI QR pairing, persisted credentials, media, and native formatting.
- Built-in Codex app-server and Claude Code harnesses with independent, resumable sessions.
- Disk-backed conversation histories with bounded prompt context and TUI/CLI browsing and branching.
- Local NVIDIA Parakeet voice transcription with strict file, duration, chunk, and memory limits.
- Durable task/goal routes, persisted leases, stale-work recovery, and numeric runtime jobs.
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
```

`followup` deliberately fails when that exact CLI conversation has no active execution; ordinary `send` starts a new turn when idle. Neither command starts the continuous orchestration loop. Use `spynel run --once` to process queued documents or `spynel serve` for continuous dispatch. See the [plain CLI reference](docs/cli.md) for output contracts, conversation branching, command aliases, attachments, and trusted extension hooks.

You can open multiple TUI windows in the same initialized folder. Spynel elects one process to own the server loops (Telegram, WhatsApp, and continuous task/goal orchestration), while every window connects to that process over an authenticated loopback endpoint. When no owner is running, the election winner resumes the latest TUI history and harness thread. A window opened beside an existing owner starts an independent `tui/local-<instance-id>` history and shows the welcome banner. The owner writes `.spynel/runtime/primary.json`, refreshes it every five seconds, and is considered stale after 30 seconds. Election updates are serialized, so one waiting window wins a takeover; graceful exits hand ownership over immediately. From an idle secondary TUI, `/primary` safely stops the old owner's services and reserves the next term for that requesting instance for ten seconds.

The main chat agent is intentionally a communication dispatcher. It answers questions and bounded status requests quickly, records finite work as durable tasks, records recurring or multi-stage objectives as durable goals, and reports the resulting path. Routine question, status, and delegation turns stay silent during inspection and produce exactly one concise consolidated result without a preamble or tool-step narration. Independent orchestrator sessions do the implementation, builds, research, and other focus-heavy work. Codex steers active follow-ups through app-server. Claude uses native streaming guidance for permission modes that need no approval callback; tool-capable modes use bounded print turns and accept follow-ups through Spynel's ordered same-session queue. Both paths preserve still-relevant earlier coordination, and any future harness without native steering receives the same conservative queue automatically. Agent-authored durable timestamps are read from the environment's UTC clock rather than guessed.

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

The composer starts at one row, grows through ten wrapped or explicit rows, then scrolls one visual row at a time. As it resizes, a history viewport already at the bottom remains bottom-anchored so the newest message stays visible; a deliberately paged-up viewport preserves its exact offset. Enter sends; Shift+Enter inserts a newline. Up/Down stays in the composer, while PageUp/PageDown scrolls chat. Ctrl+C clears non-empty input, stops an active local harness turn when input is empty, and exits only when both are idle. Mouse reporting is disabled so ordinary terminal drag selection and copying remain available; the wheel is terminal-native.

Typing `/` at the beginning opens the command picker. Up/Down selects, Tab inserts, and Enter sends. The footer always contains one left-aligned, context-sensitive control-hint set and uses compact keyboard glyphs such as `↵ send`, `⇧↵ line`, and `⇞⇟ history`. Header and footer share the selected theme's main background and use a muted blend of its user accent for half-height ribbons: `▀▀` above and `▄▄` below, with no padding around items or separators. In the default Spynel theme those ribbons are restrained light blue related to the `You` label, while the Spynel identity remains pink. The compact header carries the animated two-cell logo plus customizable title, Telegram and WhatsApp state, `N jobs`, and `N logs`. Chat history and form screens are unframed themed canvases with one blank row above and below, one left inset, one inset before the right-edge scrollbar, and no individual message backgrounds. The composer and pickers retain exact-width borders painted on the page background; compact form actions use lighter semantic control surfaces.

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
/job kill 1
/new
/clear
/welcome
```

Runtime stdout/stderr is captured instead of colliding with the alternate-screen UI. `/log` pages and searches the newest 4,096 process-local entries; `/log page <start>-<end>` shows an inclusive range of up to five consecutive pages. User-facing log output strips terminal formatting and unsafe controls while preserving readable multiline and Unicode content, and the captured source entries remain unchanged. Individual entries are bounded too. Active chat and orchestration executions receive short numeric job numbers; `/job kill <number>` interrupts the selected session, while `/stop` targets the issuing conversation. `/status` exposes the requesting and primary instance IDs, channel health, log/job counts, harness/model/thread state, startup state, and orchestrator activity everywhere. Long provider and instance IDs remain authoritative internally and are abbreviated only for display.

`/primary` is available only from a local TUI. If that TUI is secondary and no agent jobs are active, Spynel acknowledges the request, stops the current owner's API, channels, harness, and orchestrator, then publishes a fenced handoff that only the requester may claim. The reservation expires after ten seconds if the requester disappears, allowing normal election recovery.

`/restart` works from the TUI, Telegram, and WhatsApp. Spynel first sends and persists an acknowledgment, restores the terminal, stops the current runtime, and relaunches the same server mode in the same working directory. Saved configuration, histories, and harness session mappings remain available after reconnecting.

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

Voice transcription is enabled with NVIDIA Parakeet in new workspaces. Selecting `en` uses the English-only Unified EN INT8 model; `auto` or another supported European language uses multilingual TDT v3 INT8. The selected checksum-pinned model downloads into `.spynel/models/parakeet/` on first supported use. Spynel decodes WAV, FLAC, and MP3 with miniaudio and decodes Telegram/WhatsApp Ogg/Opus voice notes through an in-process pure-Go codec before running sherpa-onnx locally. There is no Python, FFmpeg, external executable, installer, or PATH requirement. M4A/AAC, WebM, and other formats are rejected explicitly; the original attachment remains available to the harness.

Only one voice transcription runs at a time across all channels. Audio is downmixed and resampled to mono 16 kHz float PCM, with only one configured-duration chunk retained at a time. File size, maximum processed duration, chunk duration, model download/extraction, and final transcript are bounded. A transcription failure preserves the audio link and tells the harness to inspect it manually.

## Markdown task manager

Initialization creates configurable routes such as:

```text
.spynel/tasks/todo → working → review → reviewing → done
.spynel/goals/proposed → planning → active → review → reviewing → done
```

Tasks are single finite objectives. Spynel atomically claims `todo` into leased `working`; implementers move completed attempts to queued `review`, which Spynel claims into leased `reviewing`. A fresh review session accepts into `done` or records findings and returns the task to `todo`. `waiting`, `failed`, and `cancelled` cover documented side outcomes. A due `wake_at` resumes a waiting task through `todo`. Spynel does not guess success from chat output or harness exit.

Goals are long-term outcomes with explicit, measurable `success_criteria`. Spynel claims `proposed` into leased `planning`; the planner creates a numbered round of finite tasks linked by immutable `goal_id` and `goal_round`, records the exact cohort in `round_task_ids`, then moves the unleased goal to `active`. After every current-round task settles—or an explicitly configured checkpoint arrives—Spynel queues and claims a fresh goal review. The reviewer compares cumulative evidence against every criterion and chooses `done`, `waiting`, `abandoned`, or a separately leased planning pass for the next round. Task completion is evidence only and never completes a goal automatically.

Every claimed phase has a persisted lease under `.spynel/runtime/leases/`. Claims are journaled before rename and record their manager owner; a newly elected manager resumes foreign-owner claims without waiting the full phase timeout, recovers stale same-owner leases, and creates recovery leases for orphaned files in `working`, `planning`, or `reviewing`. Status folders therefore remain understandable after a process crash without blindly repeating partially applied work.

Optional task `notify` front matter selects an existing `channel/conversation` origin and `done`, `failed`, `waiting`, or `cancelled` outcomes. Verified durable transitions enqueue concise messages under `.spynel/runtime/outbox/`; delivery retries across restarts without blocking task completion. The same path is available manually with `spynel notify --origin telegram/TG-123 "Task finished"`. Live TUI notifications wait for a safe stream pause and render as separate ordinary Spy messages.

`/task` and `/goal` are communication-agent commands rather than direct file shortcuts. Spynel combines the ordinary chat contract, the literal user message, and the user-overridable `.spynel/prompts/create-task.md` or `create-goal.md`; the communication agent avoids duplicates and writes the complete framework-compliant document. The shell helpers `spynel task` and `spynel goal` remain harness-free deterministic creation paths for scripts.

If a claimed file remains in place and its lease stops receiving events for `stale_after`, Spynel resumes that document session with the recovery prompt. Persisted leases and filesystem checks prevent duplicate dispatch across restarts. Routes are configuration data, so additional workflows can define their own source/working paths, prompts, recovery prompts, and allowed next states.

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

CGO-enabled release archives target Linux amd64/arm64, macOS amd64/arm64, and Windows amd64 and include the matching native speech libraries. The npm package is only a checksummed archive downloader/launcher. See [architecture](docs/architecture.md), [configuration](docs/configuration.md), [integrations](docs/integrations.md), and [releasing](docs/releasing.md).
