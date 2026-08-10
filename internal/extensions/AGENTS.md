# Extension DOX

## Purpose

- Own trusted executable hook discovery, invocation receipts, and Git-backed extension installation.

## Local Contracts

- Treat installed extensions as trusted code but validate manifests, paths, event names, and repository state before execution or installation.
- Accept only the currently emitted `message.received`, `harness.before`, `harness.after`, `task.claimed`, and `task.completed` hooks; reject unknown hook names rather than silently retaining dead configuration.
- Deliver hook events at least once with stable IDs; successful receipts are durable and consumers remain responsible for persistent visible-effect deduplication.
- Preserve local repositories and configuration on installation failure; never embed credentials or arbitrary shell interpolation.

## Child DOX Index

No child DOX files.
