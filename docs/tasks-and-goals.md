# Tasks and goals

Spynel stores orchestration state as human-readable Markdown with machine-readable YAML front matter. External coding harnesses do the reasoning and implementation; Spynel owns deterministic claiming, lifecycle, review policy, recovery, and dispatch.

## Tasks

A task is one finite, independently verifiable objective. The normal reviewed lifecycle is:

```text
todo → working → review → reviewing → done
```

`harness.reviews` controls review policy. Its default, `skip-trivial`, requires each task to carry a boolean `review_required`: broad, risky, hard-to-reverse, security-sensitive, or materially uncertain work normally receives independent review, while a minor localized reversible change may complete directly with proportionate evidence and residual uncertainty. `always` forces review and `never` forces the direct-evidence path. Missing or malformed task policy fails safe under the default mode.

Task documents retain their objective, constraints, evidence, current status, and a timestamped `## Progress` handoff log. A claimed phase uses a persisted lease, so restart and stale-session recovery can continue from durable evidence rather than assuming that a provider response completed the work.

## Goals

A goal is a longer-lived outcome with measurable success criteria. Planning creates a numbered round of finite linked tasks and records the exact cohort. After that cohort settles—or at an explicitly configured checkpoint—a fresh goal review compares cumulative evidence with every success criterion.

Goal review can complete the goal, wait for a concrete external condition, abandon it, or return it to planning for another round. Task completion is evidence for this decision; it never completes a goal automatically.

## Waiting, recovery, and review

Waiting is reserved for a precise external condition. An optional RFC 3339 `wake_at` lets Spynel return due work to its queue; otherwise human input or another external change must resolve the condition.

Every implementation, planning, and review claim is journaled before its queue file moves. Persisted leases, owner fencing, bounded recovery, and status-folder checks prevent duplicate ownership across restarts. Independent reviewers start from the durable artifact rather than the implementation session. Reviewers may repair only trivial localized findings themselves; broader findings return to implementation.

The elected primary also runs a bounded semantic heartbeat worker. The framework only starts one non-overlapping stable-session turn and ignores its provider output. The agent uses ordinary Spynel CLI inspection plus the task/goal Markdown contracts to make and journal evidence-backed safe repairs; no heartbeat result, finding, incident, health, escalation, fallback, or retry state exists. Ordinary terminal and actionable unscheduled waiting task transitions invoke the dedicated notification agent directly.

Heartbeat and notification workers are ordinary asynchronous jobs. Provider admission is not completion: their job histories remain live through delayed output until a real terminal final/error event, cancellation, or bounded timeout. The first terminal boundary settles the job once; late duplicates are ignored, and heartbeat scheduling remains non-overlapping until provider release.

## Creating and inspecting work

In a conversation, `/task` and `/goal` ask the communication assistant to create or refine framework-compliant work. Dedicated `/tasks`, `/goals`, `/status`, and `/jobs` commands inspect bounded durable state without invoking a harness. Scripts can use the matching `spynel task`, `spynel goal`, `spynel tasks`, and `spynel goals` commands.

Selected task outcomes may carry an authorized notification origin. Every terminal or actionable unscheduled waiting transition directly starts one ordinary notification-agent job with the task and `spynel notify` guidance. The agent decides, calls the ordinary CLI when useful, and edits task progress with its send, skip, or CLI-failure result. Separately, the heartbeat agent may decide from bounded progress that an inactive user wait merits a reminder, use `--recent-authorized`, and journal the result. Spynel provides recent authorized channel resolution and ordinary delivery but no reminder scheduler or state. A user reply remains ordinary conversation context rather than a hidden workflow acknowledgement.

For the state machine, lease, recovery, notification, and primary-owner invariants, see [architecture](architecture.md). For command syntax and output contracts, see [plain CLI and automation](cli.md). For route and review settings, see [configuration](configuration.md).
