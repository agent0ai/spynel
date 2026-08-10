# Workspace Initialization DOX

## Purpose

- Own initialized `.spynel` structure, embedded defaults, safe upgrades, and user-overridable workspace templates.

## Local Contracts

- Create canonical private directories and configuration without following unsafe state or instruction symlinks; preserve user-edited prompts, themes, instructions, and contracts during upgrades.
- Embed templates from `templates/` as source of truth for new workspaces and perform only explicit retry-safe migrations for legacy names and layouts. The exact retired stock JSON-triage notification prompt migrates to the current CLI-action prompt; any user-authored variation remains untouched.
- Automatically managed speech models live in the operating-system user cache, not the workspace, except for safe copy-only legacy migration.

## Child DOX Index

Direct child DOX files:

| Child | Scope |
| --- | --- |
| [templates/AGENTS.md](templates/AGENTS.md) | Embedded workspace defaults and workflow contracts. |
