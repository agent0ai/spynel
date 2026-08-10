# Local API DOX

## Purpose

- Own the authenticated loopback client/server contract between secondary processes and the elected workspace application service.

## Local Contracts

- Bind only loopback endpoints, authenticate every request, identify caller instances, and expose typed bounded service operations rather than internal implementation state.
- Preserve streaming message/event ordering and cancellation while keeping status, conversations, commands, configuration, logs, and job views non-secret.
- Carry authoritative durable task/goal counts and bounded census diagnostics in shared-state snapshots so every attached TUI can refresh through ordinary polling.
- Protocol changes require coordinated client/server tests and compatibility-aware error handling.

## Child DOX Index

No child DOX files.
