# Workspace Initialization DOX

## Purpose

- Own initialized `.spynel` structure, embedded workflow defaults, repair of missing current assets, and user-overridable workspace templates.

## Local Contracts

- Create canonical private directories and configuration without following unsafe state or instruction symlinks; preserve user-edited prompts, themes, instructions, and contracts during upgrades.
- Embed current workflow templates from `templates/` as source of truth for new workspaces. General compatibility migrations and retired asset fixtures do not belong in this package. Upgrade makes one narrow validated atomic exception to remove the retired TUI launch preference; current-schema configurations remain untouched.
- Keep the stock chat template's readable evidence-grounded honesty contract aligned with the framework-owned runtime injection; initialization must materialize it for every new workspace while upgrades preserve user-edited prompt files.
- Delegate built-in palette assets to `internal/theme`; workspace initialization only asks that package to materialize editable copies.
- Automatically managed speech models live in the operating-system user cache, not the workspace.
- Create `.spynel/tasks/archive` as cold history without adding it to configured routes or ordinary workflow status sets.
- Create `.spynel/jobs` as private inspection-only job history; it is not a workflow route or authority source.

## Child DOX Index

Direct child DOX files:

| Child | Scope |
| --- | --- |
| [templates/AGENTS.md](templates/AGENTS.md) | Embedded workspace defaults and workflow contracts. |
