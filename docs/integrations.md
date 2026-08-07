# Communication integrations

The TUI, Telegram, and WhatsApp all call the same application handler. Slash commands, typed settings, hooks, Markdown semantics, history isolation, job tracking, stopping, and harness dispatch therefore stay consistent across transports.

`/help` is a concise topic index everywhere. `/status` reports the shared title, Telegram/WhatsApp health, process log/job counts, active harness/model/startup state, an abbreviated conversation thread ID, turn state, and orchestrator activity.

Configuration forms replace chat only in the TUI. Text transports use the same setting keys through `/config`, `/telegram`, and `/whatsapp`. Telegram cannot configure Telegram from Telegram, and WhatsApp cannot configure WhatsApp from WhatsApp. `/quit` is TUI-only and never terminates a remote server; `/restart` is shared, acknowledges the issuing channel, and relaunches the complete application with saved state intact.

## TUI

### Status and layout

The header is reserved for status: a two-cell pink Spynel state logo and title, compact `TG`/`WA` connection symbols, plain blue `N jobs` and `N logs`, and the operational state. The logo starts `○○`, animates through the specified left-to-right circle sequence while a harness runs, and remains `◉◉` after a completed response. Connected, connecting, error, and unconfigured transports use distinct geometric icons/colors.

The footer is reserved for exactly one context-sensitive control-hint set. Opening the command picker or a form replaces the ordinary composer hints rather than prepending another status string.

Chat, composer, command picker, and screens use right-border scrollbars. When scrolling is impossible, the ordinary border remains intact. When possible, only the proportional pink `┃` thumb differs; inactive track cells retain the regular border color.

### Composer and terminal input

The composer begins at one row, expands in place through ten visual rows, then scrolls only enough to keep the cursor on its last row. Enter sends; Shift+Enter inserts a newline; Alt+Enter follows the send path. Up/Down moves in the composer only. PageUp/PageDown scrolls chat only.

Mouse reporting is disabled so Warp and other terminals can use ordinary drag selection/copying and native wheel behavior. Spynel discards stale complete or fragmented SGR mouse reports defensively, preventing escape-sequence bytes from entering the composer after a burst. Terminal-owned shortcuts such as Cmd+K do not reach the program; a ten-second cache invalidation self-heals cleared output, and Ctrl+L forces an immediate redraw.

Typing `/` at the beginning opens the canonical picker. Continue typing to filter, use Up/Down to select, Tab to insert, Enter to send, and Escape to close. Rows are flush-left and selection uses color rather than a marker. The picker title is only `Commands`; bindings live in the footer.

Ctrl+C clears non-empty input, paste tokens, and transient menus first. Ctrl+C exits only when the composer is already empty. `/quit` exits immediately.

### Messages and Markdown

Chat uses aligned `You ` and `Spy ` labels without a `>` separator; every wrapped/explicit continuation begins at the same four-column content position. Only `You` uses a restrained blue accent.

A harness turn displays the compact spinner sequence `⠋ ⠙ ⠸ ⠴ ⠦ ⠇` immediately after the newest response character, with no “Working” label. Multiple agent-message items are joined with one newline and retained. A local slash command sent during an active turn commits the current streaming entry, renders the command normally, and opens another `Spy` spinner entry for later harness output. The local response cannot stop the underlying harness animation.

Agent Markdown renders in the terminal. One source paragraph gap remains one blank row; code blocks do not acquire extra outer blank rows. Explicit URI and absolute local-file destinations use standard OSC 8 links in supporting terminals. Relative files remain plain because the renderer cannot safely infer a base directory.

Bracketed pastes of at least 1,000 characters appear as atomic `[Pasted N chars]` tokens but dispatch their full content. Pasted/dropped readable local file paths are copied to `.spynel/attachments/` and become atomic attachment links. Left/Right jumps across a token; Backspace/Delete removes it in one action. Terminals provide dropped files as text paths and do not expose arbitrary binary image clipboard payloads.

### Screens and histories

Initialization, config, Telegram, WhatsApp/QR, harness, model, and resume views share one screen/control implementation. Up/Down or Tab/Shift+Tab selects a control; text edits in place; Space/Enter cycles selections; Ctrl+S saves a form; Escape restores chat. The harness and model views are immediate action lists instead: every choice has its own row, the current choice receives initial focus, and Space/Enter applies it without Ctrl+S. When either chooser is opened from config, the TUI preserves the full parent form—including unsaved edits, focus, advanced disclosure, and scroll—and restores it after Enter or Escape; directly opened choosers return to chat. A missing harness remains on the chooser with installation guidance. Main configuration starts with harness/model actions plus agent filesystem access, context, and startup essentials, with all other controls collapsed under Advanced settings. Every Telegram and WhatsApp form and wizard starts with live connection state plus any available detail or error text, before QR content, instructions, and controls. Telegram and WhatsApp put a guided setup wizard first, leave one blank row, then show their three essential controls and collapse all optional controls behind a reusable advanced-settings disclosure. One blank row separates the essential controls from Show/Hide advanced, and another separates Hide advanced from the expanded optional controls. Wizard screens use the same keyboard navigation, keep one blank row between an editable control and its navigation buttons, render official resources as OSC 8 terminal hyperlinks, carry secret and ordinary values only in process memory between steps, and save essentials transactionally at completion. The live WhatsApp QR step starts at its top and supports PageUp/PageDown so large terminal codes remain inspectable.

The TUI loads only a fixed newest display tail from `tui/local`. The once-per-workspace welcome is a non-interactive, non-persisted banner above the chat messages, using the pink block logo and textual command hints while leaving the composer active. `/welcome` restores that banner and scrolls to its top. `/resume` lists disk-backed histories and copies a selected one to `tui/resume-<short-id>` before switching chat to that independent branch. `/clear` removes only the current TUI conversation, its harness session, and any visible welcome banner; the marker prevents clear from reopening onboarding.

## Telegram

Telegram uses the HTTPS Bot API directly. Polling clears a prior webhook without dropping pending updates and then long-polls. Webhook mode binds the configured local listener, registers a private derived path below the public URL, optionally verifies Telegram's secret header, and processes a bounded queue. Both modes call `getMe` for mention/reply group policy.

The allow-list accepts numeric user IDs and usernames and must contain at least one entry before Telegram can be enabled. Empty lists fail closed at configuration and adapter boundaries. The form and wizard link to the third-party [@userinfobot](https://t.me/userinfobot) helper for discovering numeric IDs. Telegram's verified `getMe` identity is carried with live connection status, and `/telegram` renders the bot's `@username` as a clickable `t.me` link. In groups, `mention` responds to an `@bot` mention or reply, `all` handles every eligible message, and `off` ignores groups. New-member welcome messages, notices, and retention cleanup are optional advanced controls.

Text, captions, documents, photos, video, audio, and voice are accepted. Media is streamed to `.spynel/attachments/telegram/`; voice retains its link even when transcription is disabled or fails. Agent Markdown becomes Telegram's supported HTML subset and is split within the 4096-character limit. Group commands such as `/help@bot commands` normalize to the same shared command while keeping arguments.

An accepted message starts Telegram's typing indicator before media handling or transcription. Spynel refreshes it every four seconds while the harness turn remains active and stops refreshing only after the final response/error is delivered or the channel shuts down.

Private history/session identity is `TG-<numeric-user-id>` and group identity is `TG-group-<chat-id>`, so bot restarts reuse the same conversation.

## WhatsApp

WhatsApp uses whatsmeow with a private CGO-free SQLite multi-device store. If no device exists, the running `/whatsapp` form receives a QR code and pairing state; otherwise the stored session reconnects. The configured interval checks live connection health.

In `self-chat` mode, only messages sent by the linked user to their own chat are accepted and sent-message IDs are suppressed to prevent loops. In `dedicated` mode, messages sent by the linked account are ignored. Number allow-lists normalize punctuation. Enabled groups still require a mention or reply to the linked account.

Text, captions, documents, images, video, audio, and stickers are accepted. The SDK decrypts directly into `.spynel/attachments/whatsapp/` without holding the whole file in memory. Agent Markdown becomes WhatsApp-native bold, italic, strike, monospace, quote, and list syntax and is split into bounded messages.

An accepted message starts WhatsApp's composing presence before media handling or transcription. Spynel refreshes it every four seconds through the harness turn and sends `paused` after the final response/error. Activity is reference-counted per chat on both transports, so an overlapping command cannot silence a still-running agent turn.

Direct identity is `WA-<normalized-number>` and group identity is `WA-group-<group-id>`.

whatsmeow implements the unofficial WhatsApp Web protocol. Protect the database like a login credential and evaluate account risk before production use.

## Voice transcription

Speech is enabled with the multilingual `small` model in new workspaces. Both remote transports append the absolute audio attachment link and an explicit `[Generated voice transcription — may contain errors]` block. A single shared worker serializes all voice work, resolves or securely provisions the pinned official `whisper-cli` runtime, checks FFmpeg, downloads missing selected model weights, segments on disk, processes one bounded chunk at a time, and removes intermediates. Failure text remains in the message so the harness can inspect the original audio manually.

## Durable histories

Every stable external identity maps to a sanitized append-only JSONL file containing timestamp, role, sender, and content. The complete path is linked in the harness prompt, but only the newest window allowed by both `workspace.history_max_messages` and `workspace.history_char_limit` is read from disk. `/clear` erases only the issuing remote conversation and resets only its harness session. Remote transports cannot `/resume`, because they cannot replace their native visible history; their files remain browsable and branchable from the TUI.
