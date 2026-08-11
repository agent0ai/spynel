# Workspace Instance Election DOX

## Purpose

- Own single-primary election and authenticated rendezvous for all Spynel processes sharing one workspace.

## Local Contracts

- Publish complete lease records atomically for lock-free discovery, renew at five seconds, and treat records stale after 30 seconds under fenced takeover rules.
- Put a validated non-secret SHA-256 connectivity identifier in every new lease. Derive it from a private random token in the OS per-user configuration home so ordinary processes in one supported environment agree without publishing hostnames, paths, MAC addresses, usernames, machine IDs, boot IDs, or namespace identifiers.
- Treat a missing or invalid environment identifier as an incompatible/legacy owner, never as permission to replace a fresh lease. Preserve its heartbeat fencing until ordinary stale or targeted-handoff rules permit acquisition.
- Serialize compare-and-replace takeover and bind explicit handoff to a live target and expiry so two processes cannot become authoritative from one request.
- Serialize destructive ownerless cleanup with primary publication through the same election lock. Run it only when the primary lease is absent and no durable clean-release fence remains within one live-client lease duration; stale or malformed leases and release fences fail closed because a former owner or already-open successor may still retain process-local live-client state.
- Keep lock mechanics platform-specific and never expose authentication tokens through status or logs.

## Child DOX Index

No child DOX files.
