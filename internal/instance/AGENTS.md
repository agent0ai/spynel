# Workspace Instance Election DOX

## Purpose

- Own single-primary election and authenticated rendezvous for all Spynel processes sharing one workspace.

## Local Contracts

- Publish complete lease records atomically for lock-free discovery, renew at five seconds, and treat records stale after 30 seconds under fenced takeover rules.
- Expiry permits serialized takeover; it does not revoke an unchanged exact instance/token term. Renew it under the mutation lock only if no replacement or handoff won first. Owner-scoped admission refreshes the heartbeat before releasing that lock, including when admission returns an error, so slow admission cannot evict itself. A stale term still cannot start a new owner-scoped action without renewal.
- Emit only content-free renewal reasons and lock-wait/hold durations through an optional diagnostic observer outside the election lock. Diagnostics never grant ownership or expose tokens, endpoints or workflow data.
- Put a validated non-secret SHA-256 connectivity identifier in every new lease. Derive it from a private random token in the OS per-user configuration home so ordinary processes in one supported environment agree without publishing hostnames, paths, MAC addresses, usernames, machine IDs, boot IDs, or namespace identifiers.
- Treat a missing or invalid environment identifier as an incompatible/legacy owner, never as permission to replace a fresh lease. Preserve its heartbeat fencing until ordinary stale or targeted-handoff rules permit acquisition.
- Serialize compare-and-replace takeover and bind explicit handoff to a live target and expiry so two processes cannot become authoritative from one request.
- Expose a short exact-term ownership guard under the election mutation lock so primary-only durable admissions cannot race a stale takeover; a former term's action must not run after replacement.
- `RunForOwner` lets a separate local worker authenticate the supplied controller instance/token against the same current primary lease and local environment under that lock. It shares freshness, handoff, post-action renewal and error propagation with `RunWhileOwner`; it never accepts expired or foreign-environment admission.
- Serialize destructive ownerless cleanup with primary publication through the same election lock. Run it only when the primary lease is absent and no durable clean-release fence remains within one live-client lease duration; stale or malformed leases and release fences fail closed because a former owner or already-open successor may still retain process-local live-client state.
- Keep lock mechanics platform-specific and never expose authentication tokens through status or logs.

## Child DOX Index

No child DOX files.
