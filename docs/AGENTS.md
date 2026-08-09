# Documentation DOX

## Purpose

- Own durable user, protocol, configuration, architecture, and release documentation.

## Local Contracts

- Commands and configuration examples must match executable behavior and embedded defaults.
- Keep public positioning aligned with the root product contract and `docs/vision.md`: Spynel is a classic non-AI orchestration program, external harnesses provide intelligence, and the single “agent” in the relationship slogan denotes the human-facing assistant interface. Adapt copy length to its surface without inventing product facts or treating conceptual scale as a resource guarantee.
- Document `.spynel/config.yaml` as the canonical private configuration, workspace-root-relative path resolution, the fixed non-configurable `.spynel` state directory, and bounded legacy root-config migration.
- Keep `docs/agent-docs.md`, the curated `internal/agentdocs` catalog, concise `/help` metadata, harness prompt guidance, and CLI examples synchronized. Static documentation must remain offline, bounded, classified, and free of live/private workspace data.
- Clearly distinguish server-only operation, the interactive TUI, and the non-visual automation CLI.
- Document persistent per-agent instruction role mapping, precedence, fresh loading, safe file constraints, memory-edit behavior, and the content-free inspection command distinctly from prompts, DOX, workflows, history, and harness configuration.
- Document secret handling, fail-closed Telegram and WhatsApp access lists, required Telegram webhook verification, transport delivery/account modes, WhatsApp QR/session persistence, Codex, Claude Code, Pi, ACP-alias, and custom-ACP prerequisites, local Parakeet languages/formats/resource limits, startup registration, conversation branching, and extension trust.
- Document that WhatsApp setup automatically saves and enables the channel after access configuration, then opens pairing without a separate enable-choice step.
- Document that WhatsApp QR pairing uses a chrome-free full-terminal view exited by any key, expired/error sessions retry automatically without restarting Spynel, the manual retry action is only an immediate override, and phone-number linking codes are the supported non-QR alternative.
- Document transport-specific Markdown rendering and terminal limitations around pasted file data.
- Document response-delivery differences: the TUI exposes live progress, explicit CLI streaming remains opt-in, and Telegram/WhatsApp send only the last terminal response or error.
- Document the human-facing communication boundary: routine delegation and completion confirmations are brief, natural, outcome-first, and hide internal paths/IDs/metrics; explicit detail requests and dedicated diagnostic commands remain precise, and remote routine confirmations never render local-path links.
- Distinguish bounded in-memory/display windows from complete disk-backed histories and attachments.
- Document plain-CLI output contracts precisely: the last assistant-message item by default, all response deltas with `--stream`, NDJSON events with `send --json` and shared command aliases using `--json`, structured JSON for status/history queries, and flags before positional command arguments.
- Keep the finite-task and multi-round-goal workflow distinction, queue/claimed statuses, phase leases and crash recovery, controlled phase-owned dead-end recovery, runtime `## Progress` journaling, task-goal round links, waiting wakeups, default-open semantic task and goal listings, their direct CLI/NDJSON and authorized remote-channel exposure, slash-command creation prompts, simple progress-recorded notification origin/outbox behavior without a response/reminder lifecycle, independent review semantics, inspectable heartbeat and notification-decision jobs, and `spynel notify` consistent across the CLI, configuration, integrations, architecture, and README documentation.
- Document npm release triggering, native asset coverage, first-publication credentials versus OIDC trusted publishing, bounded interactive update checks, automatic-startup suppression, `/update`, and version-transition hook retry semantics.
- Keep authenticated provider canaries gated by the reviewed threat model: synthetic repositories only, disposable identities and homes, verified artifacts, bounded egress/cost/time, sanitized evidence, and per-run authorization. A plan or CI definition is not evidence that a provider was executed.

## Child DOX Index

No child DOX files.
