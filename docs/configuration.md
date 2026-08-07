# Configuration

`spynel.yaml` is the user-editable project configuration. Spynel searches parent directories for it, and every relative path resolves from the file's directory rather than the process's current directory.

## One setting catalog, two interfaces

The TUI renders the scalar settings catalog as form controls. The same keys are available as text commands, which is the common denominator for Telegram, WhatsApp, scripts, and terminals without form support:

```text
/config
/config get harness.name
/config set harness.name claude-code
/config set harness.sandbox danger-full-access
/telegram get allowed_users
/telegram set allowed_users 123456789,trusted_username
/telegram on
/whatsapp set mode dedicated
/whatsapp off
```

Within text and secret fields, Space inserts text, Left/Right moves the cursor, Home/End jumps to either edge, and Backspace/Delete edits at the cursor. Those keys continue to cycle values when a toggle or select control is focused.

In `/telegram` and `/whatsapp`, short keys are scoped automatically: `allowed_users` becomes `channels.telegram.allowed_users`. `/config` expects the complete key. Lists use comma-separated values, and Boolean values accept `on` and `off`.

Channel forms and wizard steps open at a live status section showing whether the transport is not configured, connecting, connected, or in error, followed by any available connection or error detail. Once Telegram connects, this section also shows its `@username` as a clickable `t.me` link that opens the bot conversation. Channel forms then start with a **Setup wizard** action. Telegram guides the user from the official [BotFather](https://t.me/BotFather) through bot creation, secret-token entry, access, and enabling. WhatsApp guides account mode and access selection, enables the channel, then updates a live QR step with links to [WhatsApp's official device-linking video](https://www.youtube.com/watch?v=2PzIAa3M8rM) and [help page](https://faq.whatsapp.com/1317564962315842). Back/Continue carries unsaved wizard values in process memory, Cancel returns without saving, and the completion step saves all essential values as one validated transaction. WhatsApp must save and start the channel before its own pairing QR can be produced.

Outside the wizard, channel forms put their essential controls first and start with advanced controls collapsed. Select **Show advanced settings** to reveal optional connection, group, notification, and storage controls; edited advanced values remain part of the same atomic Ctrl+S save even if the section is collapsed again.

Configuration is parsed into typed values, validated as one transaction, and replaced atomically with private `0600` permissions. A complete form either applies or leaves the previous configuration intact. Secret values are masked in forms/replies and redacted from durable command history.

Telegram cannot mutate Telegram from a Telegram message, and WhatsApp cannot mutate WhatsApp from a WhatsApp message. Use the TUI or the other remote channel. Non-own channel changes apply live through an isolated transport supervisor.

## Workspace

- `state_dir`: private runtime root, default `.spynel`. Configure this directly in YAML before relying on runtime state.
- `history_max_messages`: maximum newest entries injected into a harness prompt. `0` disables history injection.
- `history_char_limit`: maximum total recent-history characters injected. `0` disables history injection.
- `attachment_max_mb`: maximum downloaded Telegram/WhatsApp attachment size.

Prompt context is read backward from append-only JSONL and stops at both history limits. The TUI uses a separate fixed display tail, `/resume` discovers only metadata plus one preview entry per conversation, and branching copies on disk. An old conversation therefore does not have to live in RAM.

`state_dir/attachments/` holds TUI attachments; remote media uses `attachments/telegram/` and `attachments/whatsapp/`. Treat all three as conversation data.

## Coding harness (`harness`)

- `name`: `codex` or `claude-code`.
- `model`: optional model override; empty selects the harness default.
- `sandbox`: `danger-full-access`, `workspace-write`, or `read-only`. The default is `danger-full-access`, which removes Codex workspace confinement.

Spynel detects installed CLIs from `PATH` and gives Codex priority when both are present. New workspaces select a detected harness automatically; when neither is found, onboarding opens the harness chooser. The executable, workspace directory, approval policy, network flag, and default reasoning effort remain derived implementation details. `harness.sandbox` is deliberately user-controlled: `danger-full-access` maps to Codex's unrestricted policy (and Claude Code's closest unrestricted permission mode), while the other choices request their closest native restricted modes. Use unrestricted access only where the harness is trusted with the host account. Existing version-one `recipient:` YAML is read as a compatibility alias and is rewritten as `harness:` on the next save.

`/harness` opens a TUI choice list with detected/install status or lists choices remotely; `/harness <name>` selects directly. Selecting a missing CLI records the choice and gives its installation link when no harness is running; selecting it again after installation connects it without a restart. `/model` opens/lists the selected harness's catalog, and `/model <exact-name>` sets a custom identifier. In either TUI list, Up/Down or Tab/Shift+Tab moves between rows, the current value receives initial focus, and Space/Enter applies immediately—Ctrl+S is not used. When opened from `/config`, applying a choice or pressing Escape returns to the exact preserved config form; direct `/harness` and `/model` screens return to chat. The model list also offers **Harness default**. Codex obtains models from app-server. Claude Code supplies its supported aliases.

A working harness is never replaced by an unavailable choice, and switching is allowed only while the current harness is idle. Each harness has its own persisted session map.

## TUI

- `channels.tui.enabled`: whether a no-argument invocation should launch the TUI after restart.
- `channels.tui.title`: initial window title.

`/title <name>` writes a private `tui-title` override under `state_dir`, updates the running TUI, and survives restart. Remove the override file to return to the configured title.

## Telegram

One bot is configured per workspace:

Essential controls:

- `token`: embedded token; prefer `token_env`.
- `allowed_users`: required before Telegram can be enabled; accepts numeric IDs or usernames. To find your numeric ID, message the third-party [@userinfobot](https://t.me/userinfobot) helper.
- `enabled`: run the bot.

Advanced controls:

- `name`: friendly bot name.
- `token_env`: environment variable containing the token, default `SPYNEL_TELEGRAM_TOKEN`.
- `mode`: `polling` or `webhook`.
- `webhook_url`: public HTTPS base URL; Spynel appends a private per-token path.
- `webhook_listen`: local HTTP listener behind the reverse proxy.
- `webhook_secret`: optional Telegram secret-token verification value.
- `poll_timeout_seconds`: long-poll timeout.
- `group_mode`: `mention`, `all`, or `off`.
- `welcome_enabled` and `welcome_message`: new-member greeting; `{name}` is replaced.
- `notify_messages`: put a compact incoming-message notice into the TUI/runtime log.
- `attachment_max_age_hours`: periodic Telegram attachment retention; `0` keeps files.

Webhook mode requires both `webhook_url` and `webhook_listen` when enabled. The local server accepts bounded POST bodies, verifies the secret in constant time, and uses a bounded update queue.

## WhatsApp

Essential controls:

- `mode`: `self-chat` or `dedicated`.
- `allowed_numbers`: normalized phone-number allow-list; empty permits everyone allowed by mode.
- `enabled`: run the client.

Advanced controls:

- `database`: private persistent multi-device SQLite store.
- `allow_groups`: permit addressed group messages.
- `poll_interval_seconds`: connection health-check interval, minimum two seconds.

When groups are enabled, the message must mention or reply to the linked account. Pairing codes appear in the `/whatsapp` TUI screen; `spynel whatsapp pair` is the non-TUI fallback.

## Speech

- `enabled`: transcribe incoming voice notes; enabled by default in new workspaces.
- `command`: whisper.cpp-compatible executable, default `whisper-cli`. If that default is not installed on a supported Linux or Windows architecture, Spynel downloads a pinned official whisper.cpp runtime and verifies its SHA-256 digest before use. Explicit custom commands are never replaced or downloaded.
- `ffmpeg_command`: FFmpeg executable used to normalize and segment audio.
- `model`: `tiny`, `base`, `small` (default), `medium`, `large-v3`, or `large-v3-turbo`.
- `model_path`: optional local ggml model file; when empty, Spynel downloads the selected weights on first use.
- `language`: language code or `auto`.
- `max_file_mb`: accepted source-audio limit.
- `max_duration_seconds`: maximum beginning portion processed from one note.
- `chunk_seconds`: maximum segment passed to Whisper at a time.

Spynel resolves or provisions Whisper and checks FFmpeg before downloading a model. The first voice note may therefore download both the runtime and selected weights; both are cached privately for later messages. One process-wide worker handles voice messages serially. Intermediate PCM and transcription outputs remain in a private temporary directory and are deleted after each note; only bounded command output and transcript text enter memory.

## Run at startup

`startup.enabled` registers this workspace's absolute configuration path with the native operating-system startup manager. It creates a systemd user/system service on Linux, a launchd agent/daemon on macOS, or an `ONLOGON` scheduled task on Windows. Disabling removes only this workspace's hashed entry. A failed registration/removal rolls the YAML change back.

The service executes `spynel serve --config <absolute-spynel.yaml>` with the workspace root as its working directory. Moving the project or executable requires toggling startup off and on again.

Native service managers do not reliably inherit variables exported only in the current shell. For zero-manual-OS setup, store the Telegram token through the private `/telegram` form/command before enabling startup. If `token_env` is used instead, define that variable in an environment source visible to the relevant systemd/launchd/Task Scheduler account. The same caveat applies to harness API keys that are not stored by the harness's normal login flow.

## Orchestrator

- `enabled`, `interval_seconds`, and `max_parallel` control the loop.
- Each route has `name`, `source`, `working`, `prompt`, `recovery_prompt`, `stale_after`, and `allowed_next`.
- A due source file is renamed into `working` before dispatch.
- The parent of `source` plus each `allowed_next` value becomes the status folder set shown to the harness.
- A future RFC 3339 `not_before`, `next_dispatch_at`, or `next_review_at` front-matter timestamp postpones dispatch.

The three scalar loop controls are available through `/config` and take effect after restart. Routes are structured configuration and remain YAML-edited rather than compressed into scalar chat commands. Custom routes need matching folders and AGENTS.md workflow documentation. Templates support `{{FILE}}`, `{{ROUTE}}`, `{{ALLOWED_NEXT}}`, `{{STATUS_FOLDERS}}`, and `{{STALE_AFTER}}`.

## Extensions

- `enabled`: bypass or enable every hook without deleting extensions.
- `directory`: installed Git repository root.
- `hook_timeout`: maximum duration per executable hook.

These extension controls are available through `/config` and take effect after restart. Extension repositories are trusted local code and receive the same filesystem/process authority as Spynel.
