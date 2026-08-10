# Embedded Workspace Template DOX

## Purpose

- Own defaults embedded into newly initialized workspaces: configuration, prompts, persistent instructions, workflow contracts, extension guidance, and workspace DOX.

## Local Contracts

- Treat files here as source for new workspaces while preserving user-modified copies during upgrades; migrations create only missing canonical assets or perform explicitly safe legacy reconciliation.
- Keep retired byte-for-byte migration fixtures under `migrations/`; they are comparison inputs only and must never be copied into new workspaces or rendered as current behavior.
- Keep prompt placeholders, lifecycle instructions, exact status folders, timestamp rules, review modes, privacy boundaries, and `spynel docs` guidance synchronized with orchestrator behavior and tests.
- Default configuration must validate without secrets and must not introduce a configurable state directory.

## Child DOX Index

Direct child DOX files:

| Child | Scope |
| --- | --- |
| [migrations/AGENTS.md](migrations/AGENTS.md) | Retired byte-exact workspace migration fixtures. |
| [themes/AGENTS.md](themes/AGENTS.md) | Built-in semantic theme YAML templates. |
