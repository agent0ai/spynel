# Embedded Workspace Template DOX

## Purpose

- Own defaults embedded into newly initialized workspaces: configuration, prompts, persistent instructions, workflow contracts, extension guidance, and workspace DOX.

## Local Contracts

- Treat files here as current source for new workspaces while preserving user-modified copies when missing assets are repaired.
- Do not store theme assets, migration fixtures, retired prompts, or other package-owned data in this directory.
- Keep prompt placeholders, lifecycle instructions, exact status folders, timestamp rules, review modes, privacy boundaries, and `spynel docs` guidance synchronized with orchestrator behavior and tests.
- Keep the task-notification template explicit that Spynel has already validated and filled in the absolute workspace and exact authorized task origin, and that the agent changes only the example `--message` text; do not instruct task agents to use stdin, pipes, or destination placeholders.
- Default configuration must validate without secrets and must not introduce a configurable state directory.

## Child DOX Index

No child DOX files.
