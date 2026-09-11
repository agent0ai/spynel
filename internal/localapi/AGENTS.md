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
- The authenticated notify request requires exactly one of explicit `origin` or boolean `recent_authorized`; recent resolution stays owner-side and responses expose only the durable event ID, never the chosen conversation or activity evidence.

- Support versioned local integration through message, committed conversation events, atomic bounded conversation snapshots, status and ordinary notification routes. Event subscriptions share a 32-client cap across listeners, use history-owned cursors and bounded replay, set finite write deadlines, expose gap errors, and never acknowledge notifications or own workflow policy. Request response queues drain concurrently with synchronous application admission; continuing finals/errors cannot truncate a response stream.
- The optional Unix listener reuses the exact authenticated HTTP service alongside loopback. Require an existing canonical user-owned 0700 parent, 0600 socket and descriptor, refuse all existing paths (including stale sockets), pin explicit clients to the descriptor workspace, and remove only owned files on close. Explicit socket selection does not participate in or weaken foreign-loopback election fences.

- Screen-action failures retain an error HTTP status and may return a refreshed `SavedControl` beside the error. The client preserves both so forms can show native errors and update observed registration state together.

## Child DOX Index

No child DOX files.
