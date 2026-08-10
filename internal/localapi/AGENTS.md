# Local API DOX

## Purpose

- Own the authenticated loopback client/server contract between secondary processes and the elected workspace application service.

## Local Contracts

- Bind only loopback endpoints, authenticate every request, identify caller instances, and expose typed bounded service operations rather than internal implementation state.
- Preserve streaming message/event ordering and cancellation while keeping status, conversations, commands, configuration, logs, and job views non-secret.
- Protocol changes require coordinated client/server tests and compatibility-aware error handling.

## Child DOX Index

No child DOX files.
