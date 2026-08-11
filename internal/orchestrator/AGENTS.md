# Markdown Orchestrator DOX

## Purpose

- Own durable task/goal workflows, claims and leases, recovery, review policy, semantic heartbeat, notification decisions, and outbox reconciliation.

## Local Contracts

- Treat Markdown as source of truth, claim queued phases atomically, fence provider work with phase-specific leases, preserve task-goal round links, and enforce configured transitions and review overlays.
- Obtain agent-authored timestamps from the current environment clock; waiting requires a concrete external condition, and recovery may not bypass live ownership or required review.
- Persist terminal notification events before reconciliation, claim each selected event under a shared cross-process lock with an authoritative reread before its one asynchronous harness invocation, and never infer or retry a decision from provider output or completion. Freeze `decide` or `always` as the invoked turn's prepared-action contract; later changes between those modes do not invalidate it, while a later `off` rejects subsequent send actions but still permits its skip/failure audit. Authorize fixed send/skip/failure actions against current task policy and origin, normalize text before journaling or persistence, and make the first persisted action kind and text authoritative before any outbox effect; recovery may finish only that exact action without replaying the harness. Retire legacy attempted/declined records without replay, deduplicate delivery and hook effects by stable IDs, and keep private event state out of prompts and progress logs.
- Hold that shared per-event lock across current-policy authorization, authoritative event reread, first send/skip/failure intent persistence, outbox effect, progress receipt, and recovery so overlapping managers cannot accept contradictory actions.
- Admit manual semantic heartbeats through the same scheduler loop and provider fence as timer ticks; provider release after either mode owns the next fixed-delay deadline. The elected primary alone invokes the shared cleanup callback on an eight-hour cadence using the live retention value.
- Serialize terminal-task retention with goal scanning. Keep every settled task linked to any nonterminal goal in the live status folders until that goal reaches a terminal outcome; an incomplete nonterminal-goal index must fail safe by protecting goal-linked tasks.
- Persist the complete admitted route generation in each lease so live route replacement cannot change completion folders, transition policy, prompts, cross-route task/goal sources, or stale-recovery timing for in-flight work. When planning activates a goal round, copy its admitted task route into managed goal front matter so the unleased active round and later review keep resolving the same cohort. Persist that same admitted task route in every terminal notification event. Apply scanner interval changes by synchronously resetting the production timer from the accepted change and fence already-selected stale timer generations through the scan lock.

## Child DOX Index

No child DOX files.
