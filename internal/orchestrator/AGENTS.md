# Markdown Orchestrator DOX

## Purpose

- Own durable task/goal workflows, claims and leases, recovery, review policy, semantic heartbeat, notification decisions, and outbox reconciliation.

## Local Contracts

- Treat Markdown as source of truth, claim queued phases atomically, fence provider work with phase-specific leases, preserve task-goal round links, and enforce configured transitions and review overlays.
- Obtain agent-authored timestamps from the current environment clock; waiting requires a concrete external condition, and recovery may not bypass live ownership or required review.
- Persist terminal notification events before reconciliation, authorize prepared actions against current policy and origin, deduplicate delivery and hook effects by stable IDs, and keep private event state out of prompts and progress logs.

## Child DOX Index

No child DOX files.
