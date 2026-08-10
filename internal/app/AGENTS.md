# Application Service DOX

## Purpose

- Compose the provider-neutral application service used by channels, local API clients, CLI commands, configuration screens, and background orchestration.

## Local Contracts

- Keep channel/harness supervisors, histories, commands, hooks, configuration, startup registration, conversations, runtime logs, durable-work lists, and agent jobs behind the shared service boundary.
- Protect shared state and job snapshots for concurrent readers; distinguish execution state, workflow phase, durable outcome, and evidence-based health. Text silence alone is never a stall signal.
- Project live background activity from the process-local orchestrator job registry only. Starting, running, reconnecting, recovering, degraded, and audit executions are live; terminal, cancelling, finishing, error, and explicitly stalled records are not.
- File-lock implementations remain platform-specific, captured logs remain bounded, and configuration changes use transactional validation and rollback paths.

## Child DOX Index

No child DOX files.
