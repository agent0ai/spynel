# Configuration

`spynel.yaml` is the user-editable project configuration. Spynel searches parent directories for it, and every relative path resolves from the file's directory rather than the process's current directory.

The built-in task lifecycle is `todo -> working -> review -> reviewing -> done`, with `waiting`, `failed`, and `cancelled` side outcomes. `todo` and `review` are queues; `working` and `reviewing` are atomic claimed states with separate phase leases and sessions. Rejected review returns to `todo`, and an optional RFC 3339 `wake_at` returns a due waiting task to that queue. Task front matter may include `notify.enabled`, a stable `notify.origin`, and validated `notify.on` outcomes (`done`, `failed`, `waiting`, or `cancelled`). Goal-derived tasks also carry immutable `goal_id` and positive `goal_round`; standalone tasks omit both.

The built-in goal lifecycle is `proposed -> planning -> active -> review -> reviewing`, followed by `done`, `waiting`, `abandoned`, or another `planning` pass. `planning` and `reviewing` have distinct leases; `active` is deliberately unleased while Spynel observes its numbered task round. Goals require a measurable `success_criteria` list, `review_trigger`, and exact `round_task_ids` cohort for every active round. A goal can enter `done` only from review with a matching `last_review` that proves every criterion. Settled tasks (`done`, `failed`, or `cancelled`) trigger review but never complete the goal themselves.

`/task` and `/goal` combine the normal communication prompt with `.spynel/prompts/create-task.md` or `create-goal.md`, then let the communication agent create or refine the complete document. The harness-free `spynel task` and `spynel goal` shell helpers remain deterministic script-oriented constructors. Initialization and workspace upgrade add missing status folders and prompt files without overwriting user-owned templates.

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
/theme catppuccin-latte
```

Within text and secret fields, Space inserts text, Left/Right moves the cursor, Home/End jumps to either edge, and Backspace/Delete edits at the cursor. Those keys continue to cycle values when a toggle or select control is focused.

In `/telegram` and `/whatsapp`, short keys are scoped automatically: `allowed_users` becomes `channels.telegram.allowed_users`. `/config` expects the complete key. Lists use comma-separated values, and Boolean values accept `on` and `off`.

Top-level channel forms open at a live status section showing whether the transport is not configured, connecting, connected, or in error, followed by any available connection or error detail. Once Telegram connects, this section also shows its `@username` as a clickable `t.me` link that opens the bot conversation. Channel forms then provide a filled **Setup wizard** action. Wizard steps do not repeat the live connection-status section: they begin directly with their setup title, tabs, instructions, and controls. Telegram guides the user from the official [BotFather](https://t.me/BotFather) through bot creation, secret-token entry, access, and enabling. WhatsApp asks only for account mode and required access numbers; continuing from access atomically saves those essentials, enables the channel in the background, and opens pairing with [WhatsApp's official device-linking help](https://faq.whatsapp.com/1317564962315842). There is no separate WhatsApp enable-choice step. **Show QR** gives the code the complete terminal with no header, footer, help text, or form controls; any key returns to the wizard. **Retry pairing** discards an expired session and starts a fresh one. **Use pairing code** accepts the linked account's international phone number and generates WhatsApp's short-lived code for **Linked devices → Link a device → Link with phone number instead**, as described in [WhatsApp's phone-linking help](https://faq.whatsapp.com/1324084875126592). Named wizard tabs use bold labels above a continuous baseline, with the active tab highlighted only by the segment directly beneath its label. Back/Continue carries unsaved wizard values in process memory, Cancel returns without saving, and the completion transition saves all essential values as one validated transaction. WhatsApp must save and start the channel before pairing details can be produced.

WhatsApp pairing timeouts and terminal pairing errors automatically start a fresh session after a short delay. **Retry pairing** remains available only to refresh immediately; recovery never requires pressing it.

On first use, bare TUI `/telegram` and `/whatsapp` open their setup wizard directly instead of showing an empty status/config form. Telegram is considered configured after it has a resolved token and at least one allowed user. WhatsApp is considered configured only after at least one allowed phone number is saved; an enabled flag or existing session database does not bypass first-time setup by itself. After that, the ordinary form places **Enabled** inside the live status section, above all configuration. **Setup** contains the wizard and **Basic settings** contains the remaining connection essentials, with one blank row between ordinary settings and two blank rows before each section rule that follows content. Both channel forms combine their page title and status heading into a single **Telegram Status** or **WhatsApp Status** rule and omit the extra descriptive subtitle. Advanced controls start collapsed behind one combined heading and disclosure rule: select **Show Advanced Settings ↵** to reveal optional controls, and the same rule changes to **Hide Advanced Settings ↵** while expanded. Edited advanced values remain part of the same atomic Ctrl+S save even if the section is collapsed again. Main `/config` omits redundant introductory copy and begins directly with its **Core settings** rule.

Configuration is parsed into typed values, validated as one transaction, and replaced atomically with private `0600` permissions. A complete form either applies or leaves the previous configuration intact. Telegram and WhatsApp cannot be enabled with empty sender allow-lists, and attempts to clear an enabled transport's list are rejected. Secret values are masked in forms/replies and redacted from durable command history.

Telegram cannot mutate Telegram from a Telegram message, and WhatsApp cannot mutate WhatsApp from a WhatsApp message. Use the TUI or the other remote channel. Non-own channel changes apply live through an isolated transport supervisor.

## Workspace

- `state_dir`: private runtime root, default `.spynel`. Configure this directly in YAML before relying on runtime state.
- `history_max_messages`: maximum newest entries injected into a harness prompt. `0` disables history injection.
- `history_char_limit`: maximum total recent-history characters injected. `0` disables history injection.
- `attachment_max_mb`: maximum inbound or agent-sent Telegram/WhatsApp attachment size.

Prompt context is read backward from append-only JSONL and stops at both history limits. The TUI uses a separate fixed display tail, `/resume` discovers only metadata plus one preview entry per conversation, and branching copies on disk. An old conversation therefore does not have to live in RAM.

`state_dir/attachments/` holds TUI attachments; remote media uses `attachments/telegram/` and `attachments/whatsapp/`. Treat all three as conversation data.

## Coding harness (`harness`)

- `name`: `codex` or `claude-code`.
- `model`: optional model override; empty selects the harness default.
- `sandbox`: `danger-full-access`, `workspace-write`, or `read-only`. The default is `danger-full-access`, which removes Codex workspace confinement.

Spynel detects installed CLIs from `PATH` and the conventional per-user `.local/bin`, and gives Codex priority when both are present. New workspaces select a detected harness automatically; when neither is found, onboarding opens the harness chooser. The executable, workspace directory, approval policy, network flag, and default reasoning effort remain derived implementation details. `harness.sandbox` is deliberately user-controlled. Codex receives its corresponding sandbox policy. Claude Code maps `read-only` to `plan`, `workspace-write` to `acceptEdits` plus shell permission required for autonomous coding, and `danger-full-access` to permission bypass. Claude Code refuses bypass when run through root/sudo, so Spynel falls back to `acceptEdits` with all tools allowed for that privileged case and records a runtime warning. Claude's `allowedTools` approval works only with ordinary text input in current 2.1.x releases, so workspace-write and privileged unrestricted turns use bounded text print mode and queue active follow-ups; read-only and non-privileged bypass turns retain native streaming guidance. Claude's modes are native permission controls, not an operating-system filesystem sandbox; use broad access only where the harness is trusted with the host account. Existing version-one `recipient:` YAML is read as a compatibility alias and is rewritten as `harness:` on the next save.

`/harness` opens a TUI choice list with detected/install status or lists choices remotely; `/harness <name>` selects directly. Selecting a missing CLI records the choice and gives its installation link when no harness is running; selecting it again after installation connects it without a restart. `/model` opens/lists the selected harness's catalog, and `/model <exact-name>` sets a custom identifier. In either TUI list, Up/Down or Tab/Shift+Tab moves between rows, the current value receives initial focus, and Space/Enter applies immediately—Ctrl+S is not used. When opened from `/config`, applying a choice or pressing Escape returns to the exact preserved config form; direct `/harness` and `/model` screens return to chat. The model list also offers **Harness default**. Codex obtains models from app-server. Claude Code supplies its supported aliases. Claude session records include a non-secret runtime-policy marker; changing its model or permission policy starts a fresh Claude provider session while bounded Spynel history continues to provide conversation context.

A working harness is never replaced by an unavailable choice, and switching is allowed only while the current harness is idle. Each harness has its own persisted session map.

## TUI

- `channels.tui.enabled`: whether a no-argument invocation should launch the TUI after restart.
- `channels.tui.title`: initial window title.
- `channels.tui.theme`: active palette name, default `spynel`.

`/title <name>` writes a private `tui-title` override under `state_dir`, updates the running TUI, and survives restart. Remove the override file to return to the configured title.

Theme files live in `state_dir/themes/*.yaml`. Each contains a safe `name`, a `description`, optional `appearance` (`light` or `dark`) and `color_blind_friendly` metadata, and every semantic color: `background`, `surface`, `surface_elevated`, `surface_selected`, `text`, `text_muted`, `primary`, `secondary`, `border`, `user`, `success`, `warning`, `error`, `info`, and `code`. Colors use `#RRGGBB`; incomplete or duplicate themes are rejected. Metadata remains optional so existing user themes continue to load.

Fresh workspaces contain exactly twelve editable stock files. The picker groups all six dark themes first: `spynel`, `hack-the-box`, `github-colorblind-dark` (accessible), `gruvbox-dark`, `nord`, and `okabe-ito-dark` (accessible). The six light themes follow: `gruvbox-light`, `rose-pine-dawn`, `tol-muted-light` (accessible), `catppuccin-latte`, `okabe-ito-light` (accessible), and `solarized-light`. The collection therefore has exactly six choices in each appearance group and exactly two explicitly color-blind-friendly choices in each group.

Palette foundations come directly from the [Nord palette](https://www.nordtheme.com/), [Gruvbox source palette](https://github.com/morhetz/gruvbox/blob/master/colors/gruvbox.vim), [Catppuccin palette data](https://github.com/catppuccin/palette/blob/main/palette.json), [Solarized specification](https://ethanschoonover.com/solarized/), [GitHub Primer primitives](https://github.com/primer/primitives), [Rosé Pine Dawn palette](https://rosepinetheme.com/palette/ingredients/), [Paul Tol's color-blind-safe muted scheme](https://sronpersonalpages.nl/~pault/), and the established [Okabe-Ito categorical palette](https://jfly.uni-koeln.de/color/). Spynel preserves each source's characteristic canvas and accents but darkens some foreground accents, strengthens layer/border separation, and maps categorical colors to semantic UI roles to satisfy measured TUI contrast. In particular, Rosé Pine's rose, pine, and iris accents are darkened for text use; Tol Muted uses a cool paper canvas and contrast-adjusted wine/indigo/green/ochre roles; and Okabe-Ito Light assigns error to purple and warning to ochre so severe red/green-deficiency simulations retain both hue and luminance separation.

An upgraded workspace with no theme files receives the revised built-ins immediately. If theme YAML already exists, Spynel treats every file as user-owned: `spynel init --force` adds missing revised stock templates but neither overwrites nor deletes old stock-named files. To obtain exactly the revised set, run that command, then archive unwanted older palette files outside `state_dir/themes`; keep any locally modified files you still want in the picker.

Bare `/theme` reloads the theme files, then opens an inline TUI list or prints the available names and descriptions in Telegram/WhatsApp. In the TUI, Up/Down (or Tab/Shift+Tab) temporarily previews the highlighted palette across the complete interface, Enter persists it, and Escape restores the palette that was active before browsing. `/theme <name>` also reloads and applies directly from every channel, including when the active file was edited without changing its name.

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
- `webhook_url`: public HTTPS base URL; Spynel appends a per-token path that is never shown in status output.
- `webhook_listen`: local HTTP listener behind the reverse proxy.
- `webhook_secret`: Telegram secret-token verification value; required when webhook mode is enabled.
- `poll_timeout_seconds`: long-poll timeout.
- `group_mode`: `mention`, `all`, or `off`.
- `welcome_enabled` and `welcome_message`: new-member greeting; `{name}` is replaced.
- `notify_messages`: put a compact incoming-message notice into the TUI/runtime log.
- `attachment_max_age_hours`: periodic Telegram attachment retention; `0` keeps files.

Webhook mode requires `webhook_url`, `webhook_listen`, and `webhook_secret` when enabled. The local server accepts bounded POST bodies, verifies the secret in constant time, and uses a bounded update queue.

## WhatsApp

Essential controls:

- `mode`: `self-chat` or `dedicated`.
- `allowed_numbers`: required normalized phone-number allow-list; empty rejects every sender and prevents WhatsApp from being enabled.
- `enabled`: run the client.

Advanced controls:

- `database`: private persistent multi-device SQLite store.
- `allow_groups`: permit addressed group messages.
- `poll_interval_seconds`: connection health-check interval, minimum two seconds.

When groups are enabled, the message must mention or reply to the linked account. `/whatsapp` offers a full-terminal QR, retry for expired sessions, and an official phone-number linking code; `spynel whatsapp pair` remains the non-TUI QR fallback.

## Speech

- `enabled`: transcribe incoming voice notes; enabled by default in new workspaces.
- `language`: `auto` or one of the selectable Parakeet language codes below. `en` selects Unified EN; every other value selects multilingual TDT v3 with automatic language detection.
- `model_dir`: optional local directory containing `encoder.int8.onnx`, `decoder.int8.onnx`, `joiner.int8.onnx`, and `tokens.txt`; when empty, Spynel securely downloads the selected model on first supported use.
- `num_threads`: positive CPU thread count used by sherpa-onnx; default `2`.
- `max_file_mb`: accepted source-audio limit.
- `max_duration_seconds`: maximum beginning portion processed from one note.
- `chunk_seconds`: maximum mono PCM segment passed to Parakeet at a time.

Selectable values cover all 25 languages supported by multilingual Parakeet: Bulgarian (`bg`), Croatian (`hr`), Czech (`cs`), Danish (`da`), Dutch (`nl`), English (`en`), Estonian (`et`), Finnish (`fi`), French (`fr`), German (`de`), Greek (`el`), Hungarian (`hu`), Italian (`it`), Latvian (`lv`), Lithuanian (`lt`), Maltese (`mt`), Polish (`pl`), Portuguese (`pt`), Romanian (`ro`), Slovak (`sk`), Slovenian (`sl`), Spanish (`es`), Swedish (`sv`), Russian (`ru`), and Ukrainian (`uk`). `auto` is also available for multilingual automatic detection.

The first supported voice note downloads about 480 MB of compressed model data and expands it to roughly 640 MB under `.spynel/models/parakeet/`. The downloader writes a private `.partial` file, enforces the pinned maximum size, verifies SHA-256, rejects absolute or parent-traversal archive paths, verifies every required file, and atomically installs a versioned directory. An older version remains separate if a future model ID changes.

Miniaudio accepts WAV, FLAC, and MP3. Telegram and WhatsApp Ogg/Opus voice notes are detected from their container signature and decoded by an in-process pure-Go Opus implementation. Both paths downmix to mono float32 at 16 kHz. M4A/AAC, WebM, and other formats return an explicit unsupported-format error without starting the model download. No Python, FFmpeg, external ASR executable, installer, or PATH change is needed. One process-wide worker serializes voice work, reuses the active recognizer, holds only one configured PCM chunk in memory, and bounds the final transcript.

## Run at startup

`startup.enabled` registers this workspace's absolute configuration path with the native operating-system startup manager. It creates a systemd user/system service on Linux, a launchd agent/daemon on macOS, or an `ONLOGON` scheduled task on Windows. Disabling removes only this workspace's hashed entry. A failed registration/removal rolls the YAML change back. npm installations retain the Node launcher in this service command so explicit remote `/update install` requests remain safe; every generated command includes `--automatic-startup`, preventing a registry check or prompt from delaying login.

The service executes `spynel serve --config <absolute-spynel.yaml>` with the workspace root as its working directory. Moving the project or executable requires toggling startup off and on again.

Native service managers do not reliably inherit variables exported only in the current shell. For zero-manual-OS setup, store the Telegram token through the private `/telegram` form/command before enabling startup. If `token_env` is used instead, define that variable in an environment source visible to the relevant systemd/launchd/Task Scheduler account. The same caveat applies to harness API keys that are not stored by the harness's normal login flow.

## Orchestrator

- `enabled`, `interval_seconds`, and `max_parallel` control the loop.
- Each route has `name`, `source`, `working`, `prompt`, `recovery_prompt`, optional built-in `review_prompt`, `stale_after`, and `allowed_next`.
- Built-in tasks use `todo`/`working` as their implementation queue/claim and `review`/`reviewing` as their review queue/claim. Built-in goals use `proposed`/`planning` and `review`/`reviewing`.
- A claim lease is persisted before the atomic queue-to-claimed rename. Startup finishes interrupted claims, resumes stale leases, and gives orphaned claimed files a recovery lease.
- The parent of `source` plus each `allowed_next` value becomes the status folder set shown to the harness.
- Future RFC 3339 `not_before`, `next_dispatch_at`, and relevant `next_review_at` values postpone queued dispatch. Waiting documents use `wake_at`; goal waiting also uses `resume_status: planning|review`.
- Active goals move to review when all current-round linked tasks settle. Scheduled checkpoints apply only when selected by `review_trigger`.

The three scalar loop controls are available through `/config` and take effect after restart. Routes are structured configuration and remain YAML-edited rather than compressed into scalar chat commands. The names `tasks` and `goals` select the enforced built-in state machines; custom routes retain generic source/working behavior and need matching folders plus AGENTS.md workflow documentation. Orchestration templates support `{{FILE}}`, `{{ROUTE}}`, `{{ALLOWED_NEXT}}`, `{{STATUS_FOLDERS}}`, `{{STALE_AFTER}}`, `{{PHASE}}`, `{{RELATED_TASKS}}`, `{{TASK_SOURCE}}`, and `{{GOAL_SOURCE}}`. Creation-command templates additionally support `{{USER_MESSAGE}}`, `{{CHANNEL}}`, and `{{CONVERSATION}}`.

## Extensions

- `enabled`: bypass or enable every hook without deleting extensions.
- `directory`: installed Git repository root.
- `hook_timeout`: maximum duration per executable hook.

These extension controls are available through `/config` and take effect after restart. Extension repositories are trusted local code and receive the same filesystem/process authority as Spynel.
