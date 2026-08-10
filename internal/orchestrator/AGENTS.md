# Markdown Orchestrator DOX

## Purpose

- Own durable task/goal workflows, claims and leases, recovery, review policy, semantic heartbeat, notification decisions, and outbox reconciliation.

## Local Contracts

- Treat Markdown as source of truth, claim queued phases atomically, fence provider work with phase-specific leases, preserve task-goal round links, and enforce configured transitions and review overlays.
- Obtain agent-authored timestamps from the current environment clock; waiting requires a concrete external condition, and recovery may not bypass live ownership or required review.
- Persist terminal notification events before reconciliation, authorize prepared actions against current policy and origin, normalize notification text at the shared enqueue boundary before journaling or persistence, reject control-only input, deduplicate delivery and hook effects by stable IDs, and keep private event state out of prompts and progress logs.
- Persist the complete admitted route generation in each lease so live route replacement cannot change completion folders, transition policy, prompts, cross-route task/goal sources, or stale-recovery timing for in-flight work. When planning activates a goal round, copy its admitted task route into managed goal front matter so the unleased active round and later review keep resolving the same cohort. Persist that same admitted task route in every terminal notification event. Apply scanner interval changes by synchronously resetting the production timer from the accepted change and fence already-selected stale timer generations through the scan lock.

## Child DOX Index

No child DOX files.
