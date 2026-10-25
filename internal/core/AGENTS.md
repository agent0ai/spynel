# Core Value Types DOX

## Purpose

- Own stable cross-package message, response, screen, status, and formatting value types that prevent package cycles.

## Local Contracts

- Own the canonical slash-command catalog and parsing shared by application dispatch and remote intake classification, including Telegram mentions and existing aliases. Recognition is a scheduling hint only; transport authorization and application effects retain their existing owners.

- Keep types provider- and transport-neutral; implementation dependencies belong in consuming packages.
- Preserve bounded and optional serialization semantics, including reply references and attachment directives, without embedding secrets.
- `Message.ForwardedFrom` is optional display-only provenance: empty means direct, `unknown` means a provider-confirmed forward without a source. Normalize provider labels with `ForwardedSource`; never substitute them for sender authorization, conversation, reply routing or source correlation.
- Carry private stable source-message identity across transport, loopback, application, and history boundaries without exposing it in public status or job projections.
- Keep compact count formatting deterministic for constrained status surfaces. `RuntimeStatus.Jobs` counts registered jobs; `LiveJobs` counts executing jobs globally across channels and conversations, excluding stalled or settling records.

- Screen action results may carry an optional `SavedControl` to refresh the committed action row in the current or preserved parent form without replacing unsaved fields. `ActionMessage` carries the application-owned outcome.

- Local response events may carry the client-owned request ID and optional opaque durable ResponseID solely for correlation on authenticated message/protocol surfaces; both remain excluded from public status/job and operational-log projections. ResponseID lets the TUI reconcile a worker reply with the copy followed from history; it is not a notification acknowledgement or provider identity.

- A conversation-changing screen carries its transcript snapshot byte offset in `HistoryOffset` so the TUI follows only later durable entries without losing the handoff window or replaying restored messages.

- WorkerProtocol is the shared compatibility value for harness adoption and installation coordination. Change it when a successor cannot safely consume/control prior workers; npm package metadata must match, enforced by the updater release-metadata test. Protocol 19 includes default-on, two-state verbosity and legacy-auto normalization; protocol-18 workers can reject settings saves because their canonical digests retain auto.

- Own the shared global progress-mode interpretation and `/verbose` discovery. Optional Event.Verbose is a live presentation hint only; event kind, aggregate text, final-item metadata, continuation, activity and explicit integration streaming semantics remain unchanged.

## Child DOX Index

No child DOX files.
