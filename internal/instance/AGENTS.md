# Workspace Instance Election DOX

## Purpose

- Own single-primary election and authenticated rendezvous for all Spynel processes sharing one workspace.

## Local Contracts

- Publish complete lease records atomically for lock-free discovery, renew at five seconds, and treat records stale after 30 seconds under fenced takeover rules.
- Serialize compare-and-replace takeover and bind explicit handoff to a live target and expiry so two processes cannot become authoritative from one request.
- Keep lock mechanics platform-specific and never expose authentication tokens through status or logs.

## Child DOX Index

No child DOX files.
