# CLI DOX

## Purpose

- Own public command parsing, process lifecycle, plain automation commands, static docs dispatch, and server-runtime composition.

## Local Contracts

- Route shared behavior through the same application service and local API as interactive channels; deterministic commands must not start a harness merely because no owner exists.
- Before an ownerless plain-CLI `/cleanup`, hold the shared election mutation boundary through the destructive operation and recheck that the primary lease is absent and the durable clean-release grace period has expired. If ownership appeared or failover is still fenced, join the healthy owner or fail closed during the transition; never run cleanup from a separate process-local service beside an owner or an already-open successor awaiting promotion.
- Preserve bounded stdin, attachments, final/stream/NDJSON output contracts, strict active-turn follow-up checks, and platform-specific restart behavior.
- Keep `docs` offline and workspace-independent, and keep notification actions bound to validated persisted transition events.
- Canonicalize bare interactive launch context before configuration discovery. An uninitialized child may enter its discovered parent or initialize locally only through the required pre-election choice; explicit and noninteractive commands retain deterministic non-prompting discovery.
- Poll owner shared state for TUI runtime and durable-work changes, coalesce each stream to its newest value, and leave bounded work diagnostics off the visual header.
- Admit each TUI's selected conversation into the owner's renewable live-conversation lease boundary before reading its startup history, retain that lease for its complete interactive lifetime, and renew the displayed identity after switches.
- When an interactive TUI start observed an existing fresh primary, print one pre-alternate-screen connecting line followed by success or a sanitized actionable failure. Keep first-owner, headless, remote-channel, redirected, and automation startup output unchanged.

## Child DOX Index

No child DOX files.
