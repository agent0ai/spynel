# Application Service DOX

## Purpose

- Compose the provider-neutral application service used by channels, local API clients, CLI commands, configuration screens, and background orchestration.

## Local Contracts

- Keep channel/harness supervisors, histories, commands, hooks, configuration, startup registration, conversations, runtime logs, durable-work lists, and agent jobs behind the shared service boundary.
- Keep `/new` as a durable TUI conversation-identity switch with the ordinary welcome screen, and route `/trigger` and manual/automatic cleanup through shared scheduler and retention boundaries rather than command-specific workers. Protect every conversation leased by an authenticated live TUI; serialize startup registration and resume branch creation-through-registration with cleanup through history deletion, retain prior identities briefly across screen switches, expire crashed-client leases before cleanup eligibility, and fence cleanup for one lease duration after owner acquisition so live clients can renew into the replacement owner's registry.
- Protect shared state and job snapshots for concurrent readers; distinguish execution state, workflow phase, durable outcome, and evidence-based health. Text silence alone is never a stall signal.
- Project the orchestrator's bounded authoritative nonterminal task/goal census and its bounded diagnostics into shared TUI state; do not derive durable work from jobs or histories.
- Project live background activity from the process-local orchestrator job registry only. Starting, running, reconnecting, recovering, degraded, and audit executions are live; terminal, cancelling, finishing, error, and explicitly stalled records are not.
- File-lock implementations remain platform-specific, captured logs remain bounded, and configuration changes validate, save, reload the canonical file into shared memory, then invoke only required cached-runtime hooks.

## Child DOX Index

No child DOX files.
