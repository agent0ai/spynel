# DOX framework

- DOX is the AGENTS.md hierarchy used by this project.
- Agents must follow the applicable AGENTS.md chain before editing files.

## Core Contract

- AGENTS.md files are binding work contracts for their subtrees.
- Durable docs, source files, prompts, tests, and packaging metadata must stay understandable from the nearest applicable AGENTS.md plus every parent AGENTS.md above it.
- Spynel is the Go application rooted in this repository. It owns chat transports, harness sessions, deterministic markdown orchestration, persisted runtime state, executable project extensions, documentation, and distribution.
- The retired Python prototype and the former nested Go layout are not part of the repository. Product behavior, tests, documentation, packaging, and automation use root-relative paths.
- Produce one cross-platform application that hosts the styled TUI, long-running communication channels, coding-harness sessions, Markdown orchestration, and project extensions. Release archives include the sherpa-onnx native runtime beside the executable.
- `cmd/spynel` is composition only. Channels translate external messages into the provider-neutral application contract and never call a coding harness directly.
- Every channel conversation and orchestrated Markdown file has an independent harness session. Persisted leases prevent duplicate orchestration and recover stale work across restarts.
- Every running process in one workspace participates in a single primary-server election. The owner alone runs Telegram, WhatsApp, and continuous Markdown orchestration; every TUI reaches the owner's application service over an authenticated loopback API. The first TUI in an ownerless workspace resumes the latest TUI communication session, while additional live TUIs receive independent sessions. Five-second heartbeats, a 30-second stale threshold, lock-free discovery of atomically published leases, serialized compare-and-replace takeover, and target-fenced `/primary` handoffs prevent duplicate owners while allowing secondary startup, explicit promotion, or recovery from a dead or stalled server.
- The main communication agent is a responsive dispatcher: it answers questions and reports durable state, routes finite work into tasks and long-term objectives into goals, and leaves substantive execution to independent orchestrator sessions.
- The communication agent performs bounded inspection without narrating routine tool steps, then gives one concise consolidated result. Ordinary question, status, and delegation turns have no preamble or progress messages. Agents must obtain current UTC from the environment instead of estimating timestamps written into durable Markdown state.
- Runtime state, histories, WhatsApp credentials, and installed extensions belong in initialized `.spynel/` directories; user configuration remains `spynel.yaml` at the initialized workspace root.
- There is no OpenAI-compatible HTTP façade. Channels, harnesses, and trusted executable hooks are extended through explicit interfaces.

## Read Before Editing

1. Read the root AGENTS.md.
2. Identify every file or folder you expect to touch.
3. Walk from the repository root to each target path.
4. Read every AGENTS.md found along each route.
5. If a parent AGENTS.md lists a child AGENTS.md whose scope contains the path, read that child and continue from there.
6. Use the nearest AGENTS.md as the local contract and parent docs for repo-wide rules.
7. If docs conflict, the closer doc controls local work details, but no child doc may weaken DOX.

Do not rely on memory. Re-read the applicable DOX chain in the current session before editing.

## Update After Editing

Every meaningful change requires a DOX pass before the task is done.

Update the closest owning AGENTS.md when a change affects:

- purpose, scope, ownership, or responsibilities
- durable structure, contracts, workflows, or operating rules
- required inputs, outputs, permissions, constraints, side effects, or artifacts
- user preferences about behavior, communication, process, organization, or quality
- AGENTS.md creation, deletion, move, rename, or index contents

Update parent docs when parent-level structure, ownership, workflow, or child index changes. Update child docs when parent changes alter local rules. Remove stale or contradictory text immediately.

## Project Contracts

- The canonical repository and Go module path is `github.com/agent0ai/spynel`; release, package, issue, and documentation links use that owner and repository.
- Keep application and orchestration code harness-neutral. Codex and Claude Code behavior belongs in `internal/harness`; future harnesses register their metadata/factory there, implement the same interface, and explicitly declare native-steer or queued follow-up behavior.
- The default CLI command is `spynel`; the project configuration file is `spynel.yaml`.
- Keep the plain CLI equivalent to the non-visual application control plane: named disk-backed conversations, bounded streaming/NDJSON message input, active-turn follow-ups, structured status, conversation inspection/branching, attachments, shared slash commands, and trusted extension hooks must use the same application service as interactive channels.
- Markdown task and goal files are the durable source of state. Front matter is machine-readable; bodies and agent logs are human-readable. Tasks are single finite objectives following `todo -> working -> review -> reviewing -> done`, with waiting/failure/cancellation branches. Goals are measurable long-term outcomes following leased planning, passive task-round execution, and independent leased review; settled tasks are evidence and never complete a goal automatically. Every claimed phase has a persisted lease and orphan/stale recovery. Selected verified task transitions enqueue restart-safe, deduplicated notifications to an authorized stable conversation origin.
- Runtime prompt templates ship in the package and are copied into initialized workspaces so users can override them.
- `/task` and `/goal` append dedicated workspace Markdown directives to the normal communication-agent prompt; that agent creates or refines the durable document under the full workspace contract. They are not deterministic file-writing slash shortcuts.
- Enabled Telegram and WhatsApp transports require non-empty sender allow-lists and fail closed at both configuration and adapter boundaries; persisted credential/session artifacts alone never count as completed first-time channel setup.
- Telegram and WhatsApp deliver only the last non-continuing final response or terminal error for a remote message; streamed deltas, status events, and intermediate continuation responses remain visible only on streaming-capable local surfaces such as the TUI.
- Preserve in-flight communication turns when follow-up messages arrive. The latest message must receive a prompt response without silently abandoning still-relevant earlier coordination or taking ownership of long-running implementation work.
- Prefer standard-library dependencies unless a dependency materially improves reliability.
- Speech-enabled builds require CGO. Publish native release archives for Linux amd64/arm64, macOS amd64/arm64, and Windows amd64, with the matching sherpa-onnx and ONNX Runtime libraries included.
- Keep vendored native-source and runtime license texts beside their owning source or under `third_party/`, and include them in release archives.
- The npm wrapper performs a ten-second update check only for interactive TUI starts, never for generated automatic-login services or noninteractive commands. `/update` reports npm state through every channel; `/update install` stops the Go process before npm replaces it and then restarts through the supervising launcher.
- Track the last semantic application version per workspace. Version transitions expose exact `from_version` and `to_version` values to retry-safe compiled-in migrations and trusted `update.before`/`update.after` extension hooks before the new version is committed.

## Verification

- Run `go test ./...`, `go vet ./...`, and `go build ./cmd/spynel` before handoff after Go changes.
- Run `scripts/smoke.sh` after changes to initialization, configuration, harness dispatch, extension loading, channel lifecycle, or orchestration.
- Run `node npm/test.js` after npm launcher or release-layout changes.
- Run `scripts/package-native.sh` for the host target and execute the extracted archive after packaging or release-workflow changes; the release workflow compiles every supported target on a matching native runner.

## Child DOX Index

Direct child DOX files:

| Child | Scope |
| --- | --- |
| [.github/AGENTS.md](.github/AGENTS.md) | CI and release workflows. |
| [cmd/AGENTS.md](cmd/AGENTS.md) | Executable entry points. |
| [docs/AGENTS.md](docs/AGENTS.md) | User, protocol, configuration, architecture, and release documentation. |
| [internal/AGENTS.md](internal/AGENTS.md) | Go runtime packages and embedded workspace templates. |
| [npm/AGENTS.md](npm/AGENTS.md) | npm launcher and binary acquisition. |
| [scripts/AGENTS.md](scripts/AGENTS.md) | Developer, build, and smoke-test helpers. |
