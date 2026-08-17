# Terminal UI DOX

## Purpose

- Own the interactive Bubble Tea interface, chat transcript, composer, forms, wizards, selectors, activity, visual capture fixtures, and local input workflows.

## Local Contracts

- Keep rendering responsive and deterministic: bounded live history, owned async Markdown rendering, coalesced streaming, exact semantic backgrounds/padding/scrollbars, and no blocking filesystem or theme work in the event loop.
- Keep alternate-screen rendering single-owned and bounded across sleep/display restoration: filter invalid and duplicate dimensions before Bubble Tea's renderer, re-establish alternate-screen ownership and repaint on terminal focus or explicit Ctrl+L, and never drive full-frame cache invalidation from an unconditional timer.
- Drive the header logo from explicit foreground activity and the runtime's authoritative live-background projection: foreground uses the existing 100 ms frame interval, background-only uses 200 ms, and idle immediately renders `○○` and schedules no ticks regardless of prior frames or transcript state. Fence timer generations across transitions so stale ticks cannot duplicate or revive animation.
- Render the header's right side as Telegram, WhatsApp, authoritative nonterminal goals, authoritative nonterminal tasks, live jobs, retained logs, and finally the meaningful embedded build version with a `v` prefix. For a launcher-validated npm TUI, refresh update availability asynchronously no more than hourly and place one semantic-warning-colored `⚠` beside the version only when a strictly newer version is known. Omit unavailable/development placeholders and drop the complete version/warning item before higher-priority status on narrow terminals. Poll durable counts through shared owner state, keep bounded census diagnostics out of the header, and choose singular labels only for an exact uncompressed count of one.
- Forms preserve dirty state through save/discard/keep-editing confirmation; selectors and wizards retain their documented navigation, screen stack, validation, and transactional persistence behavior.
- Required pre-workspace screens use the canonical theme and selection controls. The ancestor-workspace chooser defaults to its parent action, exposes exactly parent/local/exit actions, and maps Escape and Ctrl+C to non-mutating exit.
- A `/new` welcome screen may carry a new durable conversation identity; apply it atomically with clearing only the live transcript while leaving the prior history resumable.
- Register the displayed conversation with the owner before entering the TUI, renew its bounded live lease while open, update the renewal identity on conversation switches, and release it on clean exit.
- Serialize pasted-file preparation, preserve dispatch ordering and full text, bound queues and diagnostics, and inspect generated PNGs after meaningful visual or theme changes.
- Generate one private stable source-message identity before each local submission and retain it across loopback dispatch so retries cannot duplicate work.
- Follow explicitly marked local stalled-message recovery terminals from the private conversation history so a recovery that finishes outside an inbound TUI request stream appears exactly once in the live transcript; reconcile its observed activity placeholder without disturbing a newer response stream, do not acknowledge it as a task notification, follow it at the conversation tail, and preserve intentional upward scroll with a visible new-message status.
- Consume the caller-scoped canonical recovery activity count from owner shared state for the selected conversation only, preserving overlap references without exposing conversation identities in shared projections. When a durable terminal arrives ahead of its polled inactive edge, settle its visible reference once and credit that exact later edge so it cannot consume another overlapping recovery.

## Child DOX Index

Direct child DOX files:

| Child | Scope |
| --- | --- |
| [testdata/AGENTS.md](testdata/AGENTS.md) | Deterministic visual/composer capture fixtures. |
| [textarea/AGENTS.md](textarea/AGENTS.md) | Local grapheme-safe textarea derivative. |
