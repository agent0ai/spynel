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
- The communication agent performs bounded inspection without narrating routine tool steps, then gives one brief, natural, outcome-first result. Ordinary question, status, and delegation turns have no preamble or progress messages. Routine task and goal confirmations hide internal files, paths, IDs, metadata, lifecycle details, and orchestration mechanics; explicit detail requests and dedicated diagnostic commands remain technically precise. Telegram and WhatsApp routine confirmations never contain local-path Markdown links. Agents must obtain current UTC from the environment instead of estimating timestamps written into durable Markdown state.
- Runtime state, histories, WhatsApp credentials, and installed extensions belong in initialized `.spynel/` directories; user configuration remains `spynel.yaml` at the initialized workspace root. Automatically managed speech models are the exception: they live in the operating system's per-user cache under Spynel's versioned speech namespace and are shared across compatible workspaces for that user.
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
- Keep `spynel docs` as a separate harness-free, server-free interface over curated compiled documentation. Stable topic/section references, deterministic bounded text/JSON output, concise `/help` metadata, prompt guidance, and repository documentation must be validated together; static docs never read or impersonate live workspace state.
- Markdown task and goal files are the durable source of state. Front matter is machine-readable; bodies and agent logs are human-readable. Tasks are single finite objectives carrying explicit boolean `review_required`; missing or malformed values fail safe to review. Development/change and goal-derived tasks follow `todo -> working -> review -> reviewing -> done`, while only bounded low-risk read-only collection may use `working -> done` with a validated collection result, evidence boundary/uncertainty, and exact UTC timestamp. Goals are measurable long-term outcomes following leased planning, passive task-round execution, and independent leased review; task policy never bypasses goal review, and settled tasks are evidence rather than automatic goal completion. Every claimed phase has a persisted lease and orphan/stale recovery. Selected terminal task transitions create stable policy-keyed triage records before terminal hooks; a bounded dedicated notification session validates a strict result, while durable outbox and action-request records isolate delivery and response tracking from implementation/review. Remote action deliveries retain bounded native message IDs privately; exact same-origin reply correlation selects one pending request, while acknowledgement requires a validated durable task transition. Explicit identity bindings, current authorization, primary-owned capped reminders, UTC quiet hours, and post-answer cancellation govern escalation. Executable hooks are delivered at least once with a stable `event_id`; persist successful per-extension receipts, retry unrecorded hooks with that ID, require consumers to persistently deduplicate visible effects, and never claim exactly-once arbitrary side effects. Bounded summaries distinguish ordinary direct completion from independently reviewed acceptance without exposing paths or routine IDs/metrics.
- Runtime prompt templates ship in the package and are copied into initialized workspaces so users can override them.
- The elected primary alone schedules the semantic workflow heartbeat. Its enabled state and interval apply live and fence any superseded in-flight audit. Scheduling is fixed-delay: while an audit or cancellation-ignoring provider is active there is no successor timer or deadline, and terminal provider release arms the next exact deadline from the latest live interval. Idle configuration changes reschedule from the accepted change. It uses one stable provider-neutral harness session, bounded evidence and runtime, strict versioned result validation, non-overlap, restart-persistent finding deduplication, and the authorized durable outbox. It complements owner/lease heartbeats, route scans, reconciliation, and stale recovery rather than replacing them.
- `/task` and `/goal` append dedicated workspace Markdown directives to the normal communication-agent prompt; that agent creates or refines the durable document under the full workspace contract. They are not deterministic file-writing slash shortcuts.
- Enabled Telegram and WhatsApp transports require non-empty sender allow-lists and fail closed at both configuration and adapter boundaries; persisted credential/session artifacts alone never count as completed first-time channel setup. Telegram may persist a minimal identity learned only from an allow-list-authenticated private inbound update so username-authorized users retain stable numeric proactive origins, but every application and delivery check must reapply the current allow-list and fail closed on absent or corrupt identity state.
- Telegram and WhatsApp deliver only the last non-continuing final response or terminal error for a remote message; streamed deltas, status events, and intermediate continuation responses remain visible only on streaming-capable local surfaces such as the TUI.
- Preserve in-flight communication turns when follow-up messages arrive. The latest message must receive a prompt response without silently abandoning still-relevant earlier coordination or taking ownership of long-running implementation work.
- Job-scoped operator guidance uses the existing orchestrator session and emitter, treats delimited input as nonterminal data, preserves durable ownership and lifecycle tracking, bounds and deduplicates ordered delivery, and never equates acceptance or provider completion with a durable transition. A control message may cause at most one automatic continuation after exact owner/session/lease/document revalidation.
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
- Ordinary Go test, race, vet, build, smoke, implementation, and review commands reuse Go's shared user cache. Do not assign `GOCACHE` to a project-local or per-command directory; Go action IDs already isolate flags, targets, tags, and toolchains.
- A deliberately requested cold-cache diagnostic must use `scripts/cold-cache.sh`, which creates a unique cache outside the project and removes it after the complete diagnostic process tree terminates. Cold-cache runs are for investigating suspected cache-dependent behavior, not routine verification.
- Write disposable verification binaries and profiles to test-managed temporary directories and clean them when the command finishes. If evidence must survive between implementation and review, keep it only under the ignored `.spynel-dev/artifacts/<task-id>/` directory, record it in the task, and remove it when review no longer needs it.

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
