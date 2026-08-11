# Configuration

`.spynel/config.yaml` is the user-editable project configuration inside the fixed private `.spynel` state directory. Spynel searches parent directories for that path, while relative configuration paths resolve from the workspace root one directory above `.spynel`, not from the process's current directory. On bare interactive startup only, an uninitialized launch directory with an initialized ancestor produces a pre-startup choice to use that parent, initialize locally, or exit. The parent choice is the default and changes the process working directory to the selected root before election; explicit config targets, server mode, and automation keep ordinary deterministic discovery without prompting. Unknown fields fail explicitly. The sole compatibility normalization accepts the retired `channels.tui.enabled` key, discards it, and omits it from the next canonical save.

The built-in reviewed task lifecycle is `todo -> working -> review -> reviewing -> done`, with `waiting`, `failed`, and `cancelled` side outcomes. `harness.reviews` controls task review globally. Its default `skip-trivial` mode preserves the agent-decided policy: boolean `review_required` defaults to `true` when missing, malformed non-booleans fail safe to review, and creators choose by expected risk reduction versus review latency and cost. Broad, high-risk, hard-to-reverse, security-sensitive, data/schema, infrastructure, deployment, release, migration, or materially uncertain work normally requires review; read-only work and minor, localized, easily reversible changes with clear verification may set `false` and use `working -> done`. `always` forces every task into independent review even when its document says `false`; `never` forces the direct-completion path even when a document says `true`. A queued task review encountered in `never` returns to `todo` so an implementation session can record valid direct-completion evidence instead of silently treating unreviewed work as reviewed. The harness-free task constructor writes the effective boolean, so `always` overrides `--no-review` and `never` overrides its absence. This setting never disables the mandatory goal outcome review that decides whether a goal met its success criteria.

Direct completion requires a bounded `completed` summary whose `outcome` states what changed or was collected, `evidence` records proportionate verification and the inspected boundary, `uncertainty` records residual risk, and UTC `completed_at` exactly matches `updated_at`. An invalid direct completion returns to `todo` without terminal effects. In `skip-trivial`, a manual move to `review` is honored. A reviewer may correct and reverify only trivial localized low-risk findings; nontrivial or uncertain findings return to `todo`.

When a notification agent or semantic heartbeat chooses to send a proactive task message, Spynel queues it in the durable outbox and appends its exact text with an exact-UTC timestamp to the affected task's `## Progress`. No separate response, acknowledgement, or reminder configuration exists. A waiting task may declare `wake_at` when a documented low-risk fallback can safely be chosen after a delay; omit it when human judgment is indispensable.

The built-in goal lifecycle is `proposed -> planning -> active -> review -> reviewing`, followed by `done`, `waiting`, `abandoned`, or another `planning` pass. `planning` and `reviewing` have distinct leases; `active` is deliberately unleased while Spynel observes its numbered task round. Goals require a measurable `success_criteria` list, `review_trigger`, and exact `round_task_ids` cohort for every active round. A goal can enter `done` only from review with a matching `last_review` that proves every criterion. Settled tasks (`done`, `failed`, or `cancelled`) trigger review but never complete the goal themselves.

`/task` and `/goal` combine the normal user-overridable communication prompt with `.spynel/prompts/create-task.md` or `create-goal.md` and the literal request, then let the communication agent create or refine the complete document and choose task review policy deliberately. Routine confirmations are brief, natural, and outcome-first and omit internal files, paths, IDs, metadata, and orchestration mechanics. Explicit requests for the file/details and dedicated diagnostic commands remain precise. Telegram and WhatsApp routine confirmations never contain local-path Markdown links; TUI/CLI local links are opt-in. The harness-free `spynel task` and `spynel goal` shell helpers remain deterministic script-oriented constructors; `spynel task --no-review REQUEST` selects direct completion for low-risk work, and `spynel task inspect FILE` shows the effective policy.

Stock communication, creation, task, goal, review, recovery, heartbeat, and notification prompts are copied into `.spynel/prompts/` when missing and remain user-overridable. Communication, task, goal, review/recovery, and notification-decision rendering inserts concise guidance for the current executable's absolute `docs` command, including for custom overrides without the stock placeholder. Explicit instructions and the nearest `AGENTS.md`/DOX contract remain authoritative. Workspace upgrade restores missing prompt files without overwriting existing customizations.

Persistent role instructions are separate owner configuration in `.spynel/instructions/{agent-chat,agent-developer,agent-reviewer,agent-notification,agent-heartbeat}.md`. The relevant UTF-8 file is loaded fresh and appended after each fully rendered prompt; initialization and upgrade create missing canonical files without overwriting edits. Files have a 64 KiB limit and must be regular, non-symlink, non-group/world-writable Markdown. Unsafe or malformed files fail the affected agent session with a bounded error; missing files use an explicit empty fallback. `spynel instructions [--config PATH]` validates presence and metadata without printing contents. Full behavior and precedence are documented in [persistent per-agent instructions](persistent-instructions.md).

## One setting catalog, two interfaces

The [configuration application matrix](configuration-live-matrix.md) inventories every exposed setting, its runtime owner, save/reload behavior, and verification. Extensions are the only restart-bound entries.

The TUI renders the scalar settings catalog as form controls. The same keys are available as text commands, which is the common denominator for Telegram, WhatsApp, scripts, and terminals without form support:

```text
/config
/config get harness.name
/config set harness.name claude-code
/config set harness.sandbox danger-full-access
/config set harness.reviews always
/config set harness.chat_agent_prefix /ultrathink
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

On first use, bare TUI `/telegram` and `/whatsapp` open their setup wizard directly instead of showing an empty status/config form. Telegram is considered configured after it has a resolved token and at least one allowed user. WhatsApp is considered configured only after at least one allowed phone number is saved; an enabled flag or existing session database does not bypass first-time setup by itself. After that, the ordinary form places **Enabled** inside the live status section, above all configuration. **Setup** contains the wizard and **Basic settings** contains the remaining connection essentials, with one blank row between ordinary settings and two blank rows before each section rule that follows content. Both channel forms combine their page title and status heading into a single **Telegram Status** or **WhatsApp Status** rule and omit the extra descriptive subtitle. Advanced controls start collapsed behind one combined heading and disclosure rule: select **Show Advanced Settings ↵** to reveal optional controls, and the same rule changes to **Hide Advanced Settings ↵** while expanded. Edited advanced values remain part of the same atomic Ctrl+S save even if the section is collapsed again. In `/config`, `/telegram`, and `/whatsapp`, Ctrl+S closes the form and restores chat after a successful atomic save; an unchanged form also closes as a no-op, while a persistence error keeps its edits visible. Escape closes an unchanged form directly. If any field changed, Escape instead opens a centered modal with **Save**, **Discard**, and **Keep editing** in that order and keeps **Keep editing** selected safely by default. Save follows the same validated persistence path as Ctrl+S, Discard exits without saving, and Escape returns to the edited form. Ctrl+C bypasses the modal, parent navigation, and dirty-change prompt and returns directly to main chat, discarding unsaved in-memory edits. Muted `←→ nav`, `␠/↵ choose`, and `␛ cancel` hints sit in the lower border separated by rule segments, with one blank interior row above them. Main `/config` omits redundant introductory copy and begins directly with its **Core settings** rule.

Configuration is parsed into typed values and validated before saving. Spynel atomically replaces the private `0600` YAML file, reloads that canonical file into the running process's shared configuration before the save returns, and publishes the refreshed snapshot to runtime owners. Subsequent operations use the reloaded values without restarting; active channels and in-flight work are preserved while cached owners reconcile the new snapshot. Telegram and WhatsApp cannot be enabled without a valid canonical sender allow-list, and attempts to clear or replace an enabled transport's list with whitespace-only, punctuation-only, malformed, or normalization-empty entries are rejected. Each adapter separately resolves and validates the live list at runtime, so direct construction or bypassed configuration validation cannot start or use a permissive transport. Secret values are masked in forms/replies and redacted from durable command history.

Telegram cannot mutate Telegram from a Telegram message, and WhatsApp cannot mutate WhatsApp from a WhatsApp message. Use the TUI or the other remote channel. Non-own channel changes apply live through an isolated transport supervisor.

## Workspace

- `history_max_messages`: maximum newest entries injected into a harness prompt. `0` disables history injection.
- `history_char_limit`: maximum total recent-history characters injected. `0` disables history injection.
- `attachment_max_mb`: maximum inbound or agent-sent Telegram/WhatsApp attachment size.

The private runtime root is always `.spynel`; it is not a configuration setting. Configuration, histories, tasks, goals, prompts, themes, credentials, attachments, leases, and other runtime state live together beneath that directory.

Prompt context is read backward from append-only JSONL and stops at both history limits. The TUI uses a separate fixed display tail, `/resume` discovers only metadata plus one preview entry per conversation, and branching copies on disk. An old conversation therefore does not have to live in RAM.

Telegram and WhatsApp replies store a compact same-message reference as `reply_to`: the native referenced-message ID and, when supplied by the inbound event, up to 100 normalized Unicode characters of referenced text or caption. Recent prompt history labels it explicitly as `[reply_to: ...]`. The complete label and native ID take priority when the character limit requires truncation; a limit too small for that identity omits the newest history window instead of exposing a partial ID. Ordinary non-replies omit the field.

`.spynel/attachments/` holds TUI attachments; remote media uses `.spynel/attachments/telegram/` and `.spynel/attachments/whatsapp/`. Treat all three as conversation data.

## Coding harness (`harness`)

- `name`: `codex`, `claude-code`, `pi`, an ACP alias (`opencode`, `qwen-code`, `kimi`, `goose`, `cursor`, `gemini-cli`, `github-copilot`, or `factory-droid`), or custom `acp`.
- `model`: optional model override; empty selects the harness default.
- `sandbox`: `danger-full-access`, `workspace-write`, or `read-only`. The default is `danger-full-access`, which removes Codex workspace confinement.
- `chat_agent_prefix`: optional one-line harness-native command, such as `/goal`, prepended to every communication-agent message; default empty.
- `developer_agent_prefix`: optional one-line harness-native command, such as `/goal`, prepended to task implementation, goal planning, and matching recovery messages; default empty.
- `reviewer_agent_prefix`: optional one-line harness-native command, such as `/goal`, prepended to task-review and goal-review messages; default empty.
- `heartbeat_agent_prefix`: optional one-line harness-native command, such as `/goal`, prepended to semantic-heartbeat audit messages; default empty.
- `reviews`: `skip-trivial` (default agent-decided task policy), `always`, or `never`. It does not disable mandatory goal outcome review.
- `acp_command`: executable name or absolute path used only by custom `acp`.
- `acp_args`: shell-free YAML string list used only by custom `acp`; settings display and accept familiar one-line command-line text.

Spynel detects installed built-ins from `PATH` and the conventional per-user `.local/bin`, in catalog order with Codex first. Native adapters use Codex app-server, Claude Code print mode, or Pi JSONL RPC. ACP aliases supply their documented executable and arguments; custom ACP is the sole manual-command exception. Stable ACP v1 uses newline-delimited JSON-RPC over stdio, not an HTTP endpoint, and arguments are passed directly without a shell. Prefixes are limited to one line and 256 bytes. Immediately before dispatch, Spynel removes leading and trailing prefix whitespace, preserves the remaining text exactly, and adds one ASCII space before the original prompt; an empty or whitespace-only prefix leaves the prompt byte-for-byte unchanged. This allows harness-native commands such as `/goal` or `/ultrathink` without interpreting them as Spynel slash commands.

All harness scalar settings live under the YAML `harness:` section and in the shared settings catalog's harness group. Agent prefixes and `reviews` apply live to subsequent messages and workflow decisions without restarting or perturbing the semantic-heartbeat timer. Harness executable/model/sandbox changes retain their existing transactional idle-session rules.

Custom ACP arguments are passed directly without a shell:

```text
/config set harness.acp_command my-acp-agent
/config set harness.acp_args --stdio --profile "work profile"
/config set harness.name acp
```

The argument field splits on whitespace and supports single or double quotes, empty quoted arguments, and backslash escapes for whitespace, quotes, and backslashes. Other backslashes remain literal, so paths such as `C:\path\agent` work without escaping every separator. The field is one line; malformed quotes, dangling escapes, NUL bytes, and invalid UTF-8 are rejected without changing the saved setting. `$`, backticks, wildcards, pipes, redirection, semicolons, and parentheses remain literal argument data: Spynel parses this text itself and passes the resulting vector directly to the process without invoking a shell.

`harness.sandbox` is deliberately user-controlled. Codex and Claude receive their native policy mappings. Pi uses a read-only tool allow-list when selected. ACP automatically accepts one-time requests outside read-only mode and rejects edit/delete/move/execute/other requests in read-only mode; fetch also requires network permission, which Spynel does not currently grant. Pi and ACP agents are still trusted local processes, and ACP agents may act without requesting permission, so neither mapping is an operating-system containment boundary. Harness names and sandbox/review values must use the exact current catalog choices after case and edge-whitespace normalization.

`/harness` opens a TUI choice list with detected/install status or lists choices remotely; `/harness <name>` selects directly. Configure the custom ACP command before selecting `acp`. Selecting a missing built-in records the choice and gives installation guidance when no harness is running. `/model` opens/lists a catalog when the selected adapter provides one, and `/model <exact-name>` sets a custom identifier. Codex and Pi query runtime catalogs; Claude Code supplies aliases; ACP applies an exact identifier through a model-category session config option and reports clearly when the agent exposes none. Session policy changes start a fresh provider session while bounded Spynel history remains the context fallback.

A working harness is never replaced by an unavailable choice, and switching is allowed only while the current harness is idle. Each harness has its own persisted session map.

## TUI

- `channels.tui.title`: initial window title.
- `channels.tui.theme`: active palette name, default `spynel`.

Startup is invocation-driven and deterministic: bare `spynel` launches the TUI, `spynel serve` is headless, and `spynel serve --tui` attaches a TUI. There is no launch-preference setting.

`/title <name>` writes a private `.spynel/tui-title` override, updates the running TUI, and survives restart. Remove the override file to return to the configured title.

Theme files live in `.spynel/themes/*.yaml`. Each contains a safe `name`, a `description`, optional `appearance` (`light` or `dark`) and `color_blind_friendly` metadata, and every semantic color: `background`, `surface`, `surface_elevated`, `surface_selected`, `text`, `text_muted`, `primary`, `secondary`, `border`, `user`, `success`, `warning`, `error`, `info`, and `code`. Colors use `#RRGGBB`; incomplete or duplicate themes are rejected. Metadata remains optional so existing user themes continue to load.

Fresh workspaces contain exactly twelve editable stock files. The picker groups all six dark themes first: `spynel`, `hack-the-box`, `github-colorblind-dark` (accessible), `gruvbox-dark`, `nord`, and `okabe-ito-dark` (accessible). The six light themes follow: `gruvbox-light`, `rose-pine-dawn`, `tol-muted-light` (accessible), `catppuccin-latte`, `okabe-ito-light` (accessible), and `solarized-light`. The collection therefore has exactly six choices in each appearance group and exactly two explicitly color-blind-friendly choices in each group.

Palette foundations come directly from the [Nord palette](https://www.nordtheme.com/), [Gruvbox source palette](https://github.com/morhetz/gruvbox/blob/master/colors/gruvbox.vim), [Catppuccin palette data](https://github.com/catppuccin/palette/blob/main/palette.json), [Solarized specification](https://ethanschoonover.com/solarized/), [GitHub Primer primitives](https://github.com/primer/primitives), [Rosé Pine Dawn palette](https://rosepinetheme.com/palette/ingredients/), [Paul Tol's color-blind-safe muted scheme](https://sronpersonalpages.nl/~pault/), and the established [Okabe-Ito categorical palette](https://jfly.uni-koeln.de/color/). Spynel preserves each source's characteristic canvas and accents but darkens some foreground accents, strengthens layer/border separation, and maps categorical colors to semantic UI roles to satisfy measured TUI contrast. In particular, Rosé Pine's rose, pine, and iris accents are darkened for text use; Tol Muted uses a cool paper canvas and contrast-adjusted wine/indigo/green/ochre roles; and Okabe-Ito Light assigns error to purple and warning to ochre so severe red/green-deficiency simulations retain both hue and luminance separation.

An upgraded workspace with no theme files receives the revised built-ins immediately. If theme YAML already exists, Spynel treats every file as user-owned: `spynel init --force` adds missing revised stock templates but neither overwrites nor deletes old stock-named files. To obtain exactly the revised set, run that command, then archive unwanted older palette files outside `.spynel/themes`; keep any locally modified files you still want in the picker.

Bare `/theme` reloads the theme files, then opens an inline TUI list or prints the available names and descriptions in Telegram/WhatsApp. In the TUI, Up/Down (or Tab/Shift+Tab) temporarily previews the highlighted palette across the complete interface, Enter persists it, and Escape restores the palette that was active before browsing. `/theme <name>` also reloads and applies directly from every channel, including when the active file was edited without changing its name.

## Telegram

One bot is configured per workspace:

Essential controls:

- `token`: embedded token; prefer `token_env`.
- `allowed_users`: required before Telegram can be enabled; accepts positive numeric IDs or case-insensitive ASCII usernames made from letters, digits, and underscores, optionally prefixed with `@`. Invalid entries do not satisfy the runtime guard. To find your numeric ID, message the third-party [@userinfobot](https://t.me/userinfobot) helper.
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
- `notify_messages`: put a compact incoming-message notice into the TUI/runtime log without recording the message body.
- `attachment_max_age_hours`: periodic Telegram attachment retention; `0` keeps files.

Webhook mode requires `webhook_url`, `webhook_listen`, and `webhook_secret` when enabled. The local server accepts bounded POST bodies, verifies the secret in constant time, and uses a bounded update queue.

## WhatsApp

Essential controls:

- `mode`: `self-chat` or `dedicated`.
- `allowed_numbers`: required normalized phone-number allow-list; formatting whitespace and punctuation are ignored, `00` and `+` international prefixes compare equivalently, while letters, normalization-empty entries, and values beyond 15 digits are invalid. Missing or invalid live authorization rejects every sender and prevents startup or pairing.
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

The first supported voice note downloads about 480 MB of compressed model data and expands it to roughly 640 MB in `spynel/speech/v1/parakeet` below the platform's per-user cache directory. Linux and macOS use the path returned by their native user-cache convention. Compatible workspaces reuse one model version. A cross-process lock protects validation and installation; the downloader writes a private `.partial` file, enforces the pinned maximum size, verifies SHA-256, rejects absolute or parent-traversal archive paths, verifies every required file and compatibility marker, and atomically publishes a versioned directory. A corrupt managed version is replaced. If the user cache cannot be resolved or created, startup reports the failure and recommends fixing its permissions or setting `speech.model_dir` explicitly.

Initialization writes the current embedded contract for new workspaces but does not overwrite an existing customized `.spynel/AGENTS.md`.

Miniaudio accepts WAV, FLAC, and MP3. Telegram and WhatsApp Ogg/Opus voice notes are detected from their container signature and decoded by an in-process pure-Go Opus implementation. Both paths downmix to mono float32 at 16 kHz. M4A/AAC, WebM, and other formats return an explicit unsupported-format error without starting the model download. No Python, FFmpeg, external ASR executable, installer, or PATH change is needed. One process-wide worker serializes voice work, reuses the active recognizer, holds only one configured PCM chunk in memory, and bounds the final transcript.

## Run at startup

`startup.enabled` registers this workspace's absolute configuration path with the native operating-system startup manager. It creates a systemd user/system service on Linux or a launchd agent/daemon on macOS. Disabling removes only this workspace's hashed entry. A failed registration/removal rolls the YAML change back. npm installations retain the Node launcher in this service command so explicit remote `/update install` requests remain safe; every generated command includes `--automatic-startup`, preventing a registry check or prompt from delaying login.

The service executes `spynel serve --config <absolute-workspace>/.spynel/config.yaml` with the workspace root as its working directory. Moving the project or executable requires toggling startup off and on again.

Native service managers do not reliably inherit variables exported only in the current shell. For zero-manual-OS setup, store the Telegram token through the private `/telegram` form/command before enabling startup. If `token_env` is used instead, define that variable in an environment source visible to the relevant systemd or launchd account. The same caveat applies to harness API keys that are not stored by the harness's normal login flow.

## Orchestrator

- `enabled`, `interval_seconds`, and `max_parallel` control ordinary route scanning and dispatch.
- `semantic_heartbeat_minutes` controls a separate primary-owned agent audit. It defaults to `15`, accepts whole minutes from `5` through `1440`, and uses `0` as an explicit disable value. Other values are rejected.
- `task_notifications` controls selected task-outcome agents live: `off` leaves pending not-yet-started events dormant, `decide` gives one asynchronous agent turn the choice to send or journal a skip reason, and `always` gives one turn the requirement to send unless a concrete safety or authorization failure is journaled. Before launch, Spynel durably marks the event invoked; provider output, silence, timeout, and failure never cause another harness turn. Re-enabling admits each still-uninvoked event once under the newest accepted mode. Once invoked, a turn keeps its admitted `decide` or `always` actions across changes between those modes. Switching to `off` rejects subsequent send actions but still allows that turn to journal or recover a skip or authorization failure. Existing pending records with prior attempts and old declined records are retired without replay. Existing configurations default to `decide`.
- Each route has `name`, `source`, `working`, `prompt`, `recovery_prompt`, optional built-in `review_prompt`, `stale_after`, and `allowed_next`. Prompt paths are workspace-relative by default and may be customized per route.
- Built-in tasks use `todo`/`working` as their implementation queue/claim and `review`/`reviewing` as their review queue/claim. Built-in goals use `proposed`/`planning` and `review`/`reviewing`.
- A claim lease is persisted before the atomic queue-to-claimed rename. Startup finishes interrupted claims, resumes stale leases, and gives orphaned claimed files a recovery lease.
- The parent of `source` plus each `allowed_next` value becomes the status folder set shown to the harness.
- Future RFC 3339 `not_before` and `next_dispatch_at` values postpone implementation or planning dispatch. Waiting documents use `wake_at`; goal waiting also uses `resume_status: planning|review`. Review queues are already eligible and are claimed immediately, so earlier-phase dates never gate task or goal review.
- Active goals move to review according to the exact `review_trigger`: `all_round_tasks_settled` waits for the complete cohort, `all_round_tasks_settled_or_checkpoint` accepts either event, and `scheduled` requires and waits for a valid RFC 3339 `next_review_at` even if tasks have settled. Settlement-only is the planning default. New checkpoints require `checkpoint_reason` and should use the shortest outcome-appropriate interval; external dependency waits use `waiting` plus `wake_at`. Harness completion requests an immediate coalesced scan, while the interval remains a recovery fallback. `next_review_at` is evaluated only in `active`; its retained value in later phases documents that round's fallback checkpoint.
- `/status` lists future active-goal checkpoints with their goal ID, time, and rationale. To override one, edit that goal's `next_review_at`/`checkpoint_reason` (or choose settlement-only), then run `/run`; do not move an eligible goal directly between status folders.

`/status` also counts durable active work only from configured built-in `tasks` and `goals` route folders, with the task `waiting` folder reported as a subset of the active task total. Task `done`, `failed`, and `cancelled`, plus goal `done` and `abandoned`, are terminal and excluded; any additional configured status remains conservatively active. A corrupt file in an active folder still counts, including in the waiting subset. Unreadable folders, oversized documents, and enumeration caps produce at most eight Unicode-control-free, 240-rune diagnostics and make the displayed count an explicit lower bound instead of failing status.

The five scalar loop controls and the structured `orchestrator.routes` array are available through `/config`; route arrays use JSON at the command boundary and YAML on disk. Saves use the typed, atomic settings transaction and all apply live. `enabled` and `semantic_heartbeat_minutes` fence the old semantic schedule before the primary scheduler publishes its replacement deadline. `interval_seconds` resets the route timer from acceptance without overlapping serialized scans. Raising `max_parallel` admits more work promptly; lowering it preserves active work and blocks new claims until the live count is below the new bound. `task_notifications` admits pending durable events once under the newest accepted mode. Route acceptance is serialized with active scans: admitted jobs keep the complete immutable route generation, goal activation records its admitted task route for the unleased active round and later review, notification events keep the task route that produced them, and the next admission or communication creation prompt uses the new array. The names `tasks` and `goals` select the enforced built-in state machines; custom routes retain generic source/working behavior and need matching folders plus AGENTS.md workflow documentation. Orchestration templates support `{{FILE}}`, `{{ROUTE}}`, `{{ALLOWED_NEXT}}`, `{{STATUS_FOLDERS}}`, `{{STALE_AFTER}}`, `{{PHASE}}`, `{{RELATED_TASKS}}`, `{{TASK_SOURCE}}`, and `{{GOAL_SOURCE}}`. Creation-command templates additionally support `{{USER_MESSAGE}}`, `{{CHANNEL}}`, and `{{CONVERSATION}}`. Spynel appends the effective task-review instruction outside user-overridable templates so custom overrides cannot bypass the configured framework mode.

The semantic heartbeat is not the ten-second route scan, the five-second primary lease renewal, a per-job lease heartbeat, or stale recovery. Only the elected primary schedules it. It uses the stable `orchestrator:semantic-heartbeat` harness session and starts at most one bounded five-minute audit at a time. Every audit is registered as an ordinary inspectable `heartbeat` job before harness dispatch and remains registered until the provider actually releases, including after timeout, cancellation, or primary handoff. Its interval is a fixed delay after terminal provider completion: status reports the active audit without a successor deadline, and only provider release arms and publishes the next exact timer using the latest live interval. Idle interval changes reset the timer from the accepted change. A provider that misses cancellation cannot block shutdown indefinitely, but remains fenced from another heartbeat and from successor scheduling until it actually exits. The audit is read-only, reads bounded workflow evidence, excludes itself from job conclusions, and must return `spynel.semantic-heartbeat/v1` JSON tied to its execution ID and observation time. Result commit is serialized against primary-term invalidation; malformed, cancelled, stale, failed-with-actions, or otherwise inconsistent results are rejected. Proven repair requests trigger the existing serialized route reconciliation/recovery scan rather than agent-authored state transitions. Notifications require an exact match to the affected document's enabled `notify.origin`, authorization for the `waiting` outcome, and current channel authorization. Rendered input and persisted incident state have hard byte and entry caps. Finding identity, recurrence counts, evidence hashes, and the last structured audit summary remain in bounded private runtime state for 30 days; identical findings are deduplicated across restarts, materially changed evidence may notify after one hour, and persistent anomalies may escalate once per 24 hours.

## Extensions

- `enabled`: bypass or enable every hook without deleting extensions.
- `directory`: installed Git repository root.
- `hook_timeout`: maximum duration per executable hook.

These extension controls are available through `/config` and take effect after restart. Extension repositories are trusted local code and receive the same filesystem/process authority as Spynel.
