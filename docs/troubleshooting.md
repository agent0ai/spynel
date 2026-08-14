# Troubleshooting

Start with:

```bash
spynel doctor
```

Use `/status` in any interface for the current owner, harness, channel, workflow, and job summary. Use `/log` for bounded runtime diagnostics and `spynel instructions` to validate persistent instruction files without printing their contents.

If the semantic heartbeat appears stuck, inspect its ordinary live job with `/jobs` or `/job info`, confirm the configured harness is available, and use `/trigger heartbeat` only when it is idle. The framework ignores all heartbeat provider output and creates no result, health, incident, escalation, fallback, or retry state. Inspect the durable task/goal progress logs for actions the heartbeat worker actually performed. Ordinary actionable task transitions invoke the notification agent directly; that agent calls the ordinary notification CLI when useful and edits the task log itself.

For heartbeat or notification jobs, an initial `Codex turn started` event proves admission only. The job should remain live and retain later output until a terminal final/error event, cancellation, or timeout; a completed archive containing only that admission status indicates a lifecycle defect.

After a job finishes or the primary restarts, use `/jobs recent`, then `/job info <number>` and `/job output <number>`. A record without an end timestamp is shown as interrupted or running; recovery of the same durable work keeps its number and continues the last atomic snapshot. Numbers wrap after 9999, so an intentionally reused number selects its newest generation. Output is capped, terminal controls and common credential forms are removed, and truncation markers identify omitted data. Archived output is debugging material only and never controls task, goal, heartbeat, or notification state.

## Installation or startup

- The public `npm install -g spynel` path is not available until the first public npm and GitHub release. For an authorized checkout, use the development steps in [getting started](getting-started.md).
- Run `spynel` from the directory that should own the workspace. Its private configuration and state live in that directory's fixed `.spynel/` folder.
- If a development install is not found, follow the exact PATH guidance printed by `scripts/install-dev.sh`, or choose a writable directory already on PATH with `--bin-dir`.
- The configuration must be `.spynel/config.yaml` and match the current schema. Unknown or obsolete fields fail validation with their source location.
- If startup says the workspace primary is active in another host/container environment, the shared workspace is advertising a loopback API that is reachable only inside the owner's environment. Stop that primary cleanly and start Spynel where you want ownership, or run both processes in the same host/container environment. This detection does not expose the API to the host and does not add a relay or port-forwarding mode.
- Do not delete or overwrite a fresh primary lease to work around a foreign-loopback error. A known mismatch fails before dialing but retains the 30-second owner fence; an older lease with a missing/invalid environment ID receives a bounded compatibility attempt and remains fenced too. Upgrade or stop a live older primary; wait for stale takeover only after confirming its process is dead.
- If an existing same-environment primary accepts no connection, startup reports a sanitized condition after ten seconds instead of waiting silently. Check the owner process and `/log`, then retry or exit it cleanly. The connection message appears only for an interactive TUI before alternate-screen rendering, not in headless services or automation output.

## Coding harness

- If no supported harness is detected, open the harness setup and follow its installation guidance. Spynel does not copy credentials; sign in with the harness itself.
- A working harness cannot be replaced while a turn is active, and Spynel will not replace it with a missing executable.
- Pi and ACP permission mappings are application-level controls, not an operating-system sandbox. Review the [harness compatibility guide](harness-compatibility.md) and [configuration](configuration.md) before relying on a profile.

## Telegram or WhatsApp

- Both channels fail closed without a valid allow-list. Telegram also needs its token, and webhook mode needs a public HTTPS URL, local listener, and secret. WhatsApp needs at least one valid allowed phone number before setup is complete.
- Channel setting changes apply live and isolate failures from the TUI and task manager. Inspect the channel's status form or `/status` for its current error.
- Native service managers may not inherit variables exported only in a shell. Store the Telegram token privately in configuration or make its environment variable available to the service account.
- WhatsApp pairing sessions retry automatically after expiry or terminal error. The manual retry action requests an immediate refresh; phone-number linking is available when QR pairing is unsuitable.

## Updates, speech, and automation

- Automatic npm update checks occur only for interactive npm-launched starts. When a new version is available, the startup offer shows both versions and skips automatically after a ten-second countdown unless you answer Yes; No or any other answer also skips safely. `/update` reports availability explicitly; `/update install` requires an npm-supervised owner. Development and release-archive binaries report that npm updates are unavailable.
- Speech accepts WAV, FLAC, MP3, and Telegram/WhatsApp Ogg/Opus voice notes. M4A/AAC, WebM, and other formats return an unsupported-format error. First supported use downloads a checksum-pinned model into the operating system's per-user cache unless `speech.model_dir` is configured.
- Plain CLI flags precede positional command arguments. Add `--stream` for text deltas or `--json` for NDJSON events; default `send` output is only the final assistant message.

Continue with [communication integrations](integrations.md), [configuration](configuration.md), or the [plain CLI guide](cli.md) for complete behavior and settings.
