# Harness Adapter DOX

## Purpose

- Own the provider-neutral harness interface, catalog/factories, transactional supervisor, and Codex, Claude Code, Pi, and ACP process adapters.

## Local Contracts

- Keep application and orchestration code harness-neutral; adapters map native events into the common response and follow-up contract.
- Declare native-steer versus queued follow-up behavior explicitly, fence active executions, and invoke durable iteration reservation exactly once immediately before accepted provider delivery.
- Build commands without shell interpolation, bound scanner/event payloads, surface startup and negotiation diagnostics, and keep portable fixture providers synthetic.

## Child DOX Index

No child DOX files.
