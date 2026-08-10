# Terminal UI DOX

## Purpose

- Own the interactive Bubble Tea interface, chat transcript, composer, forms, wizards, selectors, activity, visual capture fixtures, and local input workflows.

## Local Contracts

- Keep rendering responsive and deterministic: bounded live history, owned async Markdown rendering, coalesced streaming, exact semantic backgrounds/padding/scrollbars, and no blocking filesystem or theme work in the event loop.
- Drive the header logo from explicit foreground activity and the runtime's authoritative live-background projection: foreground uses the existing 100 ms frame interval, background-only uses 200 ms, and idle schedules no ticks. Fence timer generations across transitions so stale ticks cannot duplicate or revive animation.
- Forms preserve dirty state through save/discard/keep-editing confirmation; selectors and wizards retain their documented navigation, screen stack, validation, and transactional persistence behavior.
- Serialize pasted-file preparation, preserve dispatch ordering and full text, bound queues and diagnostics, and inspect generated PNGs after meaningful visual or theme changes.

## Child DOX Index

Direct child DOX files:

| Child | Scope |
| --- | --- |
| [testdata/AGENTS.md](testdata/AGENTS.md) | Deterministic visual/composer capture fixtures. |
| [textarea/AGENTS.md](textarea/AGENTS.md) | Local grapheme-safe textarea derivative. |
