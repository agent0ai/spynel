# Local API DOX

## Purpose

- Own the authenticated loopback client/server contract between secondary processes and the elected workspace application service.

## Local Contracts

- Bind loopback by default or an explicitly provisioned private Unix socket, authenticate every request, identify caller instances, and expose typed bounded service operations rather than internal implementation state.
- Before dialing a loopback lease with a known environment identifier, reject a mismatch with actionable host/container guidance. Unknown legacy identifiers may receive only a bounded compatibility attempt and never weaken fresh-owner fencing.
- Readiness timeouts give conditional stopped-job guidance (resume with `fg` in the owning shell, then quit cleanly if desired) without asserting an unobserved cause or changing ownership.
- Bound readiness polling independently from long message/run-once requests, preserve only a categorized sanitized last condition, and expose stable foreign-environment and readiness-timeout errors without endpoints, tokens, or private paths.
- Preserve streaming message/event ordering and cancellation while keeping status, conversations, commands, configuration, logs, and job views non-secret.
- Carry authoritative durable task/goal counts, bounded census diagnostics, and global `RuntimeStatus.LiveJobs` in registration and shared-state snapshots so primary and attached TUIs refresh through ordinary polling; selected-conversation activity remains separately scoped.
- Carry authenticated renewable TUI conversation leases to the owner so retention cleanup protects every attached idle client, including short-lived prior identities during a conversation switch. Screen actions carry the caller instance identity so a resumed branch is registered inside the owner-side creation boundary before the response exposes it. A replacement owner fences cleanup for one full lease duration while existing clients renew into its process-local registry.
- Scope shared-state recovery activity to the authenticated caller instance's currently selected live TUI conversation and expose only its count, never a conversation identity or workspace-wide activity map. Return the first scoped snapshot with live-conversation registration so TUI startup never seeds activity from pre-registration readiness state.
- Protocol changes require coordinated client/server tests and compatibility-aware error handling.
- Test harness shutdown joins asynchronous terminal callbacks before service and temporary-workspace cleanup.
- The authenticated notify request requires exactly one of explicit `origin` or boolean `recent_authorized`; an explicit origin may carry an explicitly owner-established same-operator local or private remote fallback. Recent resolution stays owner-side and responses expose only the durable event ID, never the chosen conversation or activity evidence.

- Support versioned local integration through message, committed conversation events, atomic bounded conversation snapshots, status and ordinary notification routes. Event subscriptions share a 32-client cap across listeners, use history-owned cursors and bounded replay, set finite write deadlines, expose gap errors, and never acknowledge notifications or own workflow policy. Request response queues drain concurrently with synchronous application admission; continuing finals/errors cannot truncate a response stream.
- Message streams finish successfully only after both a terminal event and application-handler return, preserving post-acknowledgement update/control requests and handler failures before HTTP cancellation. Asynchronous admission still waits for its terminal event; a handler returning without events remains an explicitly handled empty response.
- The optional Unix listener reuses the exact authenticated HTTP service alongside loopback. Require an existing canonical user-owned 0700 parent, 0600 socket and descriptor, refuse all existing paths (including stale sockets), pin explicit clients to the descriptor workspace, and remove only owned files on close. Explicit socket selection does not participate in or weaken foreign-loopback election fences.
- Retain the exact published descriptor handle and live Unix listener until owned-path cleanup finishes, preventing inode reuse from matching replacement files. Close cleanup runs once, including concurrent callers, and releases both handles afterward.

- Screen-action failures retain an error HTTP status and may return a refreshed `SavedControl` beside the error. The client preserves both so forms can show native errors and update observed registration state together.

- Shared readiness is 40 seconds (30-second election expiry plus ten seconds startup), cancellable and still environment/lease fenced. This resolves prompt primary replacement without caller retries or lease deletion.

- Explicit fallback association may name a private Telegram/WhatsApp route established by the operator as the same operator; current authorization is checked independently at admission and delivery. No recipient is inferred from global activity. Existing request/response fields and queue-admission semantics are unchanged.
- Forward the authenticated caller instance for `/new`; the application owns selection registration before exposing its screen, as it does for resume actions. Do not duplicate outgoing-screen registration or ignore its errors in the transport.

- Ordinary notify carries optional `escalation_key` (`--escalation-key` on CLI) only with explicit original/fallback routes. It selects the owner-associated fallback without requiring primary delivery failure, preserving live route checks and ordinary outbox retry/deduplication. Reuse the same decision key/message; recent-authorized routing cannot be combined. Admission remains queue acceptance.

- Ordinary notify cancellation accepts only exact original origin plus `--cancel EVENT_ID` / `cancel_id`; it rejects send/routing alternatives. Return confirmed terminal state without exposing delivery content or claiming a retraction. Keep client/server and CLI validation synchronized; older owners lacking confirmation must fail visibly.

## Child DOX Index

No child DOX files.
