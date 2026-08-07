# DOX framework

- DOX is the AGENTS.md hierarchy used by this project.
- Agents must follow the applicable AGENTS.md chain before editing files.

## Core Contract

- AGENTS.md files are binding work contracts for their subtrees.
- Durable docs, source files, prompts, tests, and packaging metadata must stay understandable from the nearest applicable AGENTS.md plus every parent AGENTS.md above it.
- Spynel is the Go application rooted in this repository. It owns chat transports, harness sessions, deterministic markdown orchestration, persisted runtime state, executable project extensions, documentation, and distribution.
- The retired Python prototype and the former nested Go layout are not part of the repository. Product behavior, tests, documentation, packaging, and automation use root-relative paths.
- Produce one cross-platform binary that hosts the styled TUI, long-running communication channels, coding-harness sessions, Markdown orchestration, and project extensions.
- `cmd/spynel` is composition only. Channels translate external messages into the provider-neutral application contract and never call a coding harness directly.
- Every channel conversation and orchestrated Markdown file has an independent harness session. Persisted leases prevent duplicate orchestration and recover stale work across restarts.
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

- Keep application and orchestration code harness-neutral. Codex and Claude Code behavior belongs in `internal/harness`; future harnesses implement the same interface.
- The default CLI command is `spynel`; the project configuration file is `spynel.yaml`.
- Markdown task and goal files are the durable source of state. Front matter is the machine-readable portion; the body and agent logs are human-readable.
- Runtime prompt templates ship in the package and are copied into initialized workspaces so users can override them.
- Prefer standard-library dependencies unless a dependency materially improves reliability.
- Keep release builds CGO-free for Linux, macOS, and Windows on amd64 and arm64.

## Verification

- Run `go test ./...`, `go vet ./...`, and `go build ./cmd/spynel` before handoff after Go changes.
- Run `scripts/smoke.sh` after changes to initialization, configuration, harness dispatch, extension loading, channel lifecycle, or orchestration.
- Run `node npm/test.js` after npm launcher or release-layout changes.
- Cross-compile at least one non-host target with `CGO_ENABLED=0` after WhatsApp, packaging, or release-workflow changes.

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
