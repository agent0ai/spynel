# Spynel

Spynel is one cross-platform Go binary with two jobs: provide an excellent interface to coding harnesses, and run reliable Markdown-backed tasks and goals.

- A styled Bubble Tea terminal UI with guided setup and streaming Markdown.
- Telegram polling or webhook delivery, access rules, groups, media, and native Markdown conversion.
- Native WhatsApp multi-device support with in-TUI QR pairing, persisted credentials, media, and native formatting.
- Built-in Codex app-server and Claude Code harnesses with independent, resumable sessions.
- Disk-backed conversation histories with bounded prompt context and TUI browsing/branching.
- Local Whisper voice transcription with strict file, duration, chunk, and memory limits.
- Durable task/goal routes, persisted leases, stale-work recovery, and numeric runtime jobs.
- Reversible per-workspace startup registration on Linux, macOS, and Windows.
- Trusted, language-neutral executable hooks installed from Git repositories.

There is deliberately no OpenAI-compatible HTTP façade and no Python or Node sidecar. Channels and harnesses meet explicit Go interfaces, while deterministic state remains in Spynel.

## Quick start

The development helper downloads a pinned Go toolchain into the ignored repository-level `.spynel-dev/` directory when Go is unavailable:

```bash
cd /workspace/spynel
./scripts/dev.sh build

mkdir -p /tmp/spynel-playground
cd /tmp/spynel-playground
/workspace/spynel/bin/spynel
```

In a directory without `spynel.yaml`, the final command opens a pink Spynel setup screen. Choose **Initialize Spynel** to create the private workspace and continue directly to chat. Explicit initialization is also available:

```bash
/workspace/spynel/bin/spynel init --dir /tmp/spynel-playground
```

In an interactive terminal, `init` continues into setup/chat in the same process. Scripts can use `spynel init --no-start --dir ...` to initialize only.

Spynel detects and selects an installed supported harness automatically. If neither is found, it opens a two-choice setup screen with detection state and installation guidance:

- `codex` for Codex app-server sessions; or
- `claude` for Claude Code print-mode sessions.

The welcome banner sits at the top of the scrollable chat, keeps the composer active, and shows command guidance instead of buttons. A server can still start when its harness is unavailable so configuration and the task manager remain available. Run `doctor` from the shell after setup:

```bash
/workspace/spynel/bin/spynel doctor
```

For a headless server, use an explicit configuration path when useful:

```bash
/workspace/spynel/bin/spynel serve --config /tmp/spynel-playground/spynel.yaml
```

## Setup screens and commands

`/config`, `/telegram`, and `/whatsapp` replace chat temporarily with a shared form-screen UI. Up/Down and Tab/Shift+Tab move between controls, typing edits text, Space/Enter cycles choices, Ctrl+S validates and saves the complete form, and Escape returns to the preserved chat. Harness/model choosers opened from config return to the exact preserved form after Enter or Escape. Both channel forms start on a **Setup wizard** button. Telegram walks through BotFather, token, allowed users, and enabling; WhatsApp walks through account mode, allowed numbers, enabling, and live QR pairing. The wizard provides clickable official help links, Back/Continue controls, concise safety guidance, and atomic essential-setting saves. The ordinary Telegram form shows token, allowed users, and enabled; WhatsApp shows mode, allowed numbers, and enabled. Each keeps optional controls behind **Show advanced settings**. Main configuration starts with dedicated coding-harness and model selectors plus agent filesystem access, context, and startup essentials; task-loop, extension, speech, channel, and storage controls are under **Advanced settings**.

The same typed catalog is usable from Telegram, WhatsApp, and the TUI:

```text
/config get workspace.history_max_messages
/config set workspace.history_max_messages 40
/config set harness.sandbox danger-full-access
/harness claude-code
/model sonnet
/telegram on
/telegram set allowed_users 123456789,trusted_username
/whatsapp on
/whatsapp set allowed_numbers 15551234567
```

Telegram refuses `/telegram ...` from Telegram itself, and WhatsApp refuses `/whatsapp ...` from WhatsApp itself, so a remote configuration mistake cannot disable the channel being used. Either remote channel may configure the other. Saved channel settings apply live: only the affected transport is replaced, and a failed transport is isolated and retried without terminating chat, the TUI, or task management.

`/model` asks the active harness for its catalog. Codex obtains the runtime catalog through app-server; Claude Code supplies its supported model aliases. Bare `/harness` and `/model` open real TUI choice lists: Up/Down moves between rows, the current value starts focused, and Enter applies immediately. `/model <exact-name>` remains available when a custom model identifier is needed. Spynel derives executable paths and the workspace directory. `harness.sandbox` is user-controlled and defaults to `danger-full-access`; choose `workspace-write` or `read-only` when confinement is desired. A working harness is never replaced by a missing CLI, and replacement is refused while a turn is active.

`startup.enabled` creates or removes a workspace-specific background `spynel serve --config ...` registration:

- systemd user service, or a system service when Spynel runs as root, on Linux;
- launchd LaunchAgent, or LaunchDaemon when run as root, on macOS;
- an `ONLOGON` Task Scheduler entry on Windows.

Registration and configuration are transactional: if the operating-system change fails, Spynel restores the previous setting.

Service managers do not reliably inherit variables exported only in the current shell. For the fully guided path, save the Telegram token in the private form before enabling startup; if you keep `token_env`, make that variable available to the native service account. Harness login files normally survive because the service runs as the same user.

## Chat UI

The composer starts at one row, grows through ten wrapped or explicit rows, then scrolls one visual row at a time. Enter sends; Shift+Enter inserts a newline. Up/Down stays in the composer, while PageUp/PageDown scrolls chat. Mouse reporting is disabled so ordinary terminal drag selection and copying remain available; the wheel is terminal-native.

Typing `/` at the beginning opens the command picker. Up/Down selects, Tab inserts, and Enter sends. The footer always contains one context-sensitive control hint set. The header contains only status: the animated two-cell pink Spynel logo and title, Telegram and WhatsApp state, plain blue `N jobs` and `N logs`, and the current operational state.

A running harness turn shows a compact Braille spinner immediately after its newest response character. Streamed message items are preserved in order. Sending a local slash command during the turn commits the current text, renders the command response, and opens a fresh spinner row without losing the continuing harness output.

Agent replies render GitHub-flavored Markdown. Explicit URI and absolute-file links use OSC 8 hyperlinks in supporting terminals. Telegram receives supported HTML and WhatsApp receives its native lightweight formatting. Renderer-added duplicate paragraph and code-block spacing is removed.

Bracketed pastes of at least 1,000 characters become atomic `[Pasted N chars]` display tokens while their complete text is dispatched. Pasted readable local paths are copied privately into `.spynel/attachments/` and become atomic `[Attachment filename]` links. Arrow, Backspace, and Delete treat these tokens as one unit. A terminal does not expose arbitrary binary clipboard image data to a TUI; drag or paste a filesystem path instead.

The TUI loads and retains at most 500 display entries/500,000 runes rather than holding an unbounded transcript in RAM; an omission notice links this behavior to the complete disk history. `/resume` lists the newest saved conversations from disk and copies the selected Telegram, WhatsApp, or TUI history into an independent TUI branch. The source conversation is never modified. `/clear` erases only the current branch/history and resets its harness session; it removes the inline welcome without showing it again. `/welcome` restores the welcome banner above chat manually.

## Runtime commands

All channels share the application commands. Start with `/help`; focused pages include `/help about`, `/help commands`, `/help extensions`, `/help config`, `/help channels`, and `/help workflows`.

Common operations:

```text
/status
/title Production API
/stop
/restart
/task investigate and fix the failing login test
/goal keep the dependency backlog current
/run
/steer focus on the API regression first
/history
/resume
/log
/log page 2
/log search webhook
/log clear
/jobs
/job kill 1
/new
/clear
/welcome
```

Runtime stdout/stderr is captured instead of colliding with the alternate-screen UI. `/log` pages and searches the newest 4,096 process-local entries; individual entries are bounded too. Active chat and orchestration executions receive short numeric job numbers; `/job kill <number>` interrupts the selected session, while `/stop` targets the issuing conversation. `/status` exposes channel health, log/job counts, harness/model/thread state, startup state, and orchestrator activity everywhere. Long provider IDs remain authoritative internally and are abbreviated only for display.

`/restart` works from the TUI, Telegram, and WhatsApp. Spynel first sends and persists an acknowledgment, restores the terminal, stops the current runtime, and relaunches the same server mode in the same working directory. Saved configuration, histories, and harness session mappings remain available after reconnecting.

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

Telegram cannot be enabled until `allowed_users` contains at least one numeric user ID or username, and the list cannot be cleared while Telegram remains enabled. If you do not know your numeric ID, message the third-party [@userinfobot](https://t.me/userinfobot) helper. `group_mode` is `mention`, `all`, or `off`. Webhook mode registers a secret-verifiable Telegram URL and binds only the configured local listener, which is intended to sit behind your HTTPS reverse proxy. Telegram private conversations use stable `TG-<numeric-user-id>` identities and groups use `TG-group-<chat-id>`.

Telegram's typing indicator and WhatsApp's composing presence begin before attachment handling or voice transcription and remain refreshed throughout the asynchronous agent turn. They end only with the final response/error or channel shutdown; overlapping work in one chat is reference-counted so a short framework command cannot hide a still-running main agent.

The setup options follow Agent Zero's proven per-bot connection controls: name/token, polling or webhook, access list, group policy, welcome message, message notice, and attachment retention. Spynel adds explicit token-environment, local webhook-listener, poll-timeout, and resource-limit controls. One Telegram bot is configured per Spynel workspace.

## WhatsApp

Spynel uses whatsmeow and a CGO-free SQLite credential store. Set `/whatsapp on`, open `/whatsapp`, and scan the displayed code in **WhatsApp → Linked devices**. The shell command remains available for a plain terminal:

```bash
/workspace/spynel/bin/spynel whatsapp pair
```

`self-chat` accepts messages sent to your own chat and suppresses reply loops; `dedicated` treats the linked account as a bot number. Empty `allowed_numbers` means open access. When groups are enabled, Spynel responds only when mentioned or replied to. Credentials in `.spynel/whatsapp.db` are secrets. whatsmeow implements the unofficial WhatsApp Web protocol, so assess the account risk before using an important number.

Direct conversations use stable `WA-<number>` identities and groups use `WA-group-<group-id>`.

## Attachments and voice

Telegram documents/photos/video/audio/voice and WhatsApp documents/images/video/audio/stickers stream directly to private channel-specific attachment folders. The message passed to the harness includes an absolute `[Attachment filename]` link. `workspace.attachment_max_mb` is enforced during transfer, before a completed file becomes visible.

Voice transcription is enabled with the multilingual Whisper `small` model in new workspaces. Voice notes keep the original attachment and add a clearly labeled generated transcription. If the default `whisper-cli` is absent on a supported Linux or Windows architecture, Spynel downloads a pinned official whisper.cpp runtime and verifies its SHA-256 digest. If `speech.model_path` is empty, the selected open model weights download into `.spynel/models/whisper/` on first use. FFmpeg must be installed or configured through `speech.ffmpeg_command`; custom Whisper commands remain supported and are never provisioned automatically.

Only one voice transcription runs at a time across all channels. Audio is normalized and segmented on disk; one chunk is transcribed at a time. File size, maximum processed duration, chunk duration, command output, model download, and final transcript are bounded. A transcription failure preserves the audio link and tells the harness to inspect it manually.

## Markdown task manager

Initialization creates configurable routes such as:

```text
.spynel/tasks/todo    → .spynel/tasks/working → agent-selected next folder
.spynel/goals/active → .spynel/goals/working → agent-selected next folder
```

Spynel atomically claims a Markdown file, updates machine-readable front matter, persists a lease under `.spynel/runtime/leases/`, and dispatches a separate harness session with the route prompt. The agent must update and move the file. Spynel does not guess task success from chat output.

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
cd /workspace/spynel
./scripts/dev.sh test
./scripts/smoke.sh
npm run test:npm
```

Release archives target Linux, macOS, and Windows on amd64/arm64 with `CGO_ENABLED=0`. The npm package is only a checksummed binary downloader/launcher. See [architecture](docs/architecture.md), [configuration](docs/configuration.md), [integrations](docs/integrations.md), and [releasing](docs/releasing.md).
