# Core Value Types DOX

## Purpose

- Own stable cross-package message, response, screen, status, and formatting value types that prevent package cycles.

## Local Contracts

- Keep types provider- and transport-neutral; implementation dependencies belong in consuming packages.
- Preserve bounded and optional serialization semantics, including reply references and attachment directives, without embedding secrets.
- Carry private stable source-message identity across transport, loopback, application, and history boundaries without exposing it in public status or job projections.
- Keep compact count formatting deterministic for constrained status surfaces. `RuntimeStatus.Jobs` counts registered jobs; `LiveJobs` counts executing jobs globally across channels and conversations, excluding stalled or settling records.

- Screen action results may carry an optional `SavedControl` to refresh the committed action row in the current or preserved parent form without replacing unsaved fields. `ActionMessage` carries the application-owned outcome.

- Local response events may carry the client-owned request ID and optional opaque durable ResponseID solely for correlation on authenticated message/protocol surfaces; both remain excluded from public status/job and operational-log projections. ResponseID lets the TUI reconcile a worker reply with the copy followed from history; it is not a notification acknowledgement or provider identity.

- A conversation-changing screen carries its transcript snapshot byte offset in `HistoryOffset` so the TUI follows only later durable entries without losing the handoff window or replaying restored messages.

- WorkerProtocol is the shared compatibility value for harness adoption and installation coordination. Change it when a successor cannot safely consume/control prior workers; npm package metadata must match, enforced by the updater release-metadata test. Protocol 17 includes the canonical speech idle timeout; protocol-16 workers would omit it on save.

## Child DOX Index

No child DOX files.
