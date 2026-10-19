# Troubleshooting

Start with:

```bash
spynel doctor
```

Use `/status` in any interface for the current owner, harness, channel, workflow, and job summary. Use `/log` for bounded runtime diagnostics and `spynel instructions` to validate persistent instruction files without printing their contents.

If the semantic heartbeat appears stuck, inspect its ordinary live job with `/jobs` or `/job info`, confirm the configured harness is available, and use `/trigger heartbeat` only when it is idle. The framework ignores all heartbeat provider output and creates no result, health, incident, escalation, fallback, or retry state. Inspect the durable task/goal progress logs for actions the heartbeat worker actually performed. Ordinary actionable task transitions invoke the notification agent directly; that agent calls the ordinary notification CLI when useful and edits the task log itself.

For heartbeat or notification jobs, an initial `Codex turn started` event proves admission only. The job should remain live and retain later output until a terminal final/error event, cancellation, or timeout; a completed archive containing only that admission status indicates a lifecycle defect.

After a job finishes or the primary restarts, use `/jobs recent`, then `/job info <number>` and `/job output <number>`. A record without an end timestamp is shown as interrupted or running; recovery of the same durable work keeps its number and continues the last atomic snapshot. Numbers wrap after 9999, so an intentionally reused number selects its newest generation. Output is capped, terminal controls and common credential forms are removed, and truncation markers identify omitted data. Archived output is debugging material only and never controls task, goal, heartbeat, or notification state.

## Process failures and missing stop records

Use the existing private log queries:

```bash
spynel log search native_stderr
spynel log search command_failed
spynel log search process_end
spynel log search native_exit
spynel log search executor_exit
```

Server/TUI launches record UTC, native PID, run identity and version/revision. `process_end` distinguishes normal quit, handled termination, restart/update and returned errors. `native_stderr` retains a bounded excerpt of terminal stderr, including unhandled Go goroutine panics, runtime fatal errors and native-library diagnostics. Recovered Bubble Tea event-loop and command panics retain their cause and stack in `command_failed`. Harness/child failures remain attributed to their existing components; they do not by themselves mean the native application failed.

On supervised Linux/macOS launches, the surviving native parent captures executor stderr and its actual Wait result (`executor_exit`), with no extra reader. It records supervisor PID, executor-start and observed-exit UTC alongside build/process identity. For signaled exits, `exit_evidence` adds available visible-cgroup OOM-kill cumulative counts and the delta over the executor lifetime; unavailable counters are explicit. Even a positive delta does not identify this child as a victim or establish the sender/reason. Timestamped host kernel victim or audit/container-controller evidence with PID mapping is still required. Direct externally managed launches use a small reader of the same executable that mirrors descriptor 2 to the original terminal and drains it through EOF, keeping at most 64 KiB for diagnosis. It preserves the start of recognizable fatal dumps and otherwise keeps a tail. Existing redaction, terminal-control removal and the 4,096-rune entry bound apply before storage. Records remain in private `.spynel/runtime/logs` JSONL files with existing 2 MiB rotation and eight-file retention; there is no separate raw crash archive. Do not publish these records: redaction recognizes common credential forms but cannot certify arbitrary failure prose as safe to share.

The reader has no restart, election or application-work authority. Normal shutdown restores stderr, flushes/closes the process logger and waits only a bounded interval for the reader. Storage, rotation or reader failure emits a content-free terminal warning and preserves the original application result. Capture begins after server configuration resolves; earlier startup failures and unavailable storage still need retained terminal stderr. Offline documentation/version commands do not start the reader.

When the npm launcher survives, `native_exit` records its child's actual exit code or signal before update/relaunch. A handled signal can produce a successful native exit; compare it with `process_end`. Supervised in-place restarts retain the supervisor PID and verify a new executor generation; direct service restarts retain the executor PID, while separate launches and concurrent instances remain distinct. `session_start` and `session_end` describe individual runtime log sessions, including primary ownership changes and diagnostic readers. They are not process exit records.

A missing `process_end` or `native_exit` means **unclean or unknown termination**, not a proven crash mechanism or termination time. Do not infer a crash from another instance's session or a rotated-away record. SIGKILL cannot run native cleanup; the surviving reader may retain preceding stderr and the npm parent may observe SIGKILL, but that signal does not establish OOM. If the reader/launcher is also killed, or the host/container disappears, obtain the host's timestamped process-exit or kernel OOM-victim record with PID-namespace/cgroup correlation. Undated cgroup OOM counters and a last-activity timestamp cannot identify the victim or date its death. These logs never authorize replaying active work.

A hang that produces neither stderr nor an exit still requires external stack/resource inspection. This capture adds no watchdog or periodic profiler.

## Native recovery and provider ownership

A native parent retries only unexpected executor failures after completed startup admission, while it survives. Provider admission does not disable crash recovery. Normal quit, an exiting Ctrl+C, deliberate stop and update/restart teardown suppress retries. Ctrl+C that copies, clears input or cancels an active turn leaves the runtime alive. A crash exhausts three delayed retries unless an attempt runs for at least a minute after admission. Immediate primary recovery may wait through the ordinary 30-second stale-election fence; the shared readiness budget is 40 seconds. Never delete a primary lease to accelerate it.

Harness ownership uses an OS lock rather than an everlasting marker. The stable lock lives under `spynel/harness/` in the operating system's per-user configuration directory and is keyed by the canonical workspace path, avoiding shared workspace filesystems that do not enforce native locks. Never delete or replace an active lock file. An old `.spynel/runtime/harness-owner` marker may remain on disk; its presence does not block the new implementation. A provider guardian holds the lock until its direct provider exits, and the operating system releases it when its holders exit. The guardian detects executor loss through a separate lifetime pipe, terminates the provider and confirms its exit before releasing ownership.

If an earlier provider is still stopping, `/status` reports that ownership is busy and reconnecting automatically. The application retries once per second and restores the harness, orchestration and heartbeat after the lock becomes free. A timed-out close is also retried after cleanup finishes. Ordinary updates and restarts need no manual unlock. An actually stuck live provider or storage/permission failure remains an explicit error; inspect `/log` and use ordinary controlled stop/restart. Shutdown failures are retained in diagnostics rather than reported as a clean quit. Keep runtime leases and session data intact, and let normal workflow recovery reconcile interrupted tasks. Arbitrary escaped descendants and remote effects still require inspection; this does not promise exactly-once side effects or recovery after simultaneous loss of every supervising process.

Use the supervisor PID or ordinary quit/stop commands for deliberate termination. Child-only SIGKILL has no explicit stop intent and is indistinguishable from an unexpected kill. Exit supervision cannot detect a frozen child and may be defeated by parent/container/host loss or resource pressure. These changes do not establish or fix the cause of any previously unexplained incident.

## Installation or startup

If Spynel was first started before its harness was installed, install the selected harness and relaunch. Spynel 0.14.1 refreshes the existing worker from the saved settings and discovers the newly available executable. You can also correct an unavailable selection in `/configure` or `.spynel/config.yaml` and relaunch. It does not guess a different harness, delete ownership records or replay work. A structural change while executions are retained is refused until that work is finished and consumed; an unverified live process or incompatible worker remains an explicit error.

- The standalone public one-liner requires URL routing and a release containing standalone installer support. If it is unavailable, use npm or the development steps in [getting started](getting-started.md).
- Run `spynel` from the directory that should own the workspace. Its private configuration and state live in that directory's fixed `.spynel/` folder.
- If a development install is not found, follow the exact PATH guidance printed by `scripts/install-dev.sh`, or choose a writable directory already on PATH with `--bin-dir`.
- The configuration must be `.spynel/config.yaml` and match the current schema. Recognized fields are validated; unused keys are ignored on load and removed on the next canonical save.
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

- Automatic npm update checks occur only for interactive npm-launched starts. When a new version is available, the startup offer shows both versions and skips automatically after a ten-second countdown unless you answer Yes; No or any other answer also skips safely. `/update check` reports availability; `/update` updates and restarts all instances of the selected managed installation. `spynel killall` stops every verified Spynel process the caller can control. If an older instance cannot receive coordinated restart, the update reports it; stop it once with the current `spynel killall` command, then relaunch. Development and manually extracted archive binaries remain unmanaged. Script installation failures preserve the prior runtime; if another installer is active, wait for it to finish and retry.
- Speech accepts WAV, FLAC, MP3, and Telegram/WhatsApp Ogg/Opus voice notes. M4A/AAC, WebM, and other formats return an unsupported-format error. First supported use downloads a checksum-pinned model into the operating system's per-user cache unless `speech.model_dir` is configured.
- Plain CLI flags precede positional command arguments. Add `--stream` for text deltas or `--json` for NDJSON events; default `send` output is only the final assistant message.

Continue with [communication integrations](integrations.md), [configuration](configuration.md), or the [plain CLI guide](cli.md) for complete behavior and settings.

## Stalled-message recovery decisions

Use `spynel log search recovery` to inspect the latest scan. `scan_completed` includes aggregate static reason counts. Up to 100 `decision` entries per scan carry its UTC identifier and conversation/candidate slots local to that scan, without recipients, private source IDs or bodies. Reasons distinguish deferred conversation/candidate/dispatch batches, structural correlation limits, corrupt/unreadable history, activation, command eligibility, exact terminal/cancellation/prior reservation, linked durable work, active/queued ownership and unavailable transport. `dispatch_skipped` explains the final admission fence; `delivery_failed` reports an ordinary router failure without private payloads.

Recovery reads complete retained history in bounded pages of up to 2,000 recent records and 8 MiB of raw input, allowing a single larger body record to make progress. Each page validates the whole file and resolves exact terminal, cancellation, reservation and admission facts, including old timestamps and baselines, before returning. Large body strings are projected to 32 KiB while validating their complete syntax; correlation metadata remains exact under a 4 MiB structural bound. Atomic reservation reads only the requested sources without a lifetime or recent-window count cap. Discovery advances lexically through 100-conversation pages and at most 20 admissions per pass; each conversation admits at most 20 sources per turn. Deferred conversation/dispatch batches coalesce an immediate continuation; later scans continue remaining sources without duplicating reservations. Recovery prompts contain at most 48 KiB of body excerpts with an authoritative history path and instructions to read complete source messages before acting. Durable source-link inspection streams directory batches and bounded YAML headers, retaining only matches for the current candidate set instead of rejecting large task inventories. Corruption anywhere still fails closed. Both history passes hold the append lock: I/O and lock duration grow with retained history, and multiple pages repeat validation. Recovery reconciles unfinished requests, while already generated but undelivered output remains outside this feature. A source patch and fake-router test do not establish running-version activation or real remote delivery.
