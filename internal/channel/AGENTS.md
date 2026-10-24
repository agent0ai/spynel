# Channel Runtime DOX

## Purpose

- Own the common channel contract, activity signaling, and hot-reload supervisor for TUI, Telegram, and WhatsApp adapters.

## Local Contracts

- Remote adapters use one ordered ordinary worker and one ordered command worker through RunInbox, with at most 64 waiting events in each lane. Classify only recognized commands in authored text without media using the shared core catalog/grammar; never derive priority from speech, quotes, or arbitrary slash-looking text. Both lanes use the same application handler and existing authentication, addressing, correlation and delivery paths. Cancel and join both workers with their adapter generation; recheck cancellation and live authority after preparation before application effects. Overflow is rejected before admission, never handled by spawning another worker.

- Adapters translate messages and screens through `core` and the application service; they never invoke harness implementations directly.
- The supervisor consumes refreshed shared configuration snapshots, replaces only changed adapters, revokes stale adapter authority before cancellation, isolates failures, publishes lifecycle state, and retries only eligible unchanged configurations.
- Reconciliation checks owner cancellation before construction and again after construction/authorization before admitting a replacement. A cancelled build revokes its replacement authority without starting or retaining it; selected configuration/retry events cannot rebuild channels after shutdown is already observed.
- Activity references must be balanced and overlap-safe so an older turn cannot clear a newer turn's visible activity.
- Route proactive canonical conversation events only through the currently connected channel generation and bind their activity references to that generation's cancellation context so disconnect, replacement, and shutdown clean up safely.
- The proactive delivery contract carries validated attachments as well as text without communication activity. Canonical terminal events forward the same payload through it. Adapters upload files before the caption and return native upload/send failures; the outbox retries the whole message under each transport's existing identity contract.

- Local history-followed conversation responses carry `Notification.Continues` plus private request/response correlation through the existing recovery presentation path. ResponseID reuses the existing application history receipt, pairing its partial and result records; EventID remains reserved for ordinary notification acknowledgement. Partial/intermediate output never settles activity and never enters task-notification acknowledgement.

- Remote verbose progress uses one bounded editable preview per conversation, coalescing only assistant deltas and non-streamed continuing replies at most once per second with a 3000-rune tail. Run native progress sends outside intake/provider callbacks, with a three-second bound and live mode/authorization checks. Stop and join pending progress before final delivery; replace the preview with the first final chunk, then use ordinary chunk/media delivery. Failed/cancelled previews never suppress a final; native edit failure falls back to ordinary delivery with existing transport uncertainty. Generation cancellation releases previews; no replay or durable progress queue exists.

## Child DOX Index

Direct child DOX files:

| Child | Scope |
| --- | --- |
| [telegram/AGENTS.md](telegram/AGENTS.md) | Telegram authorization, polling/webhook, media, and delivery. |
| [tui/AGENTS.md](tui/AGENTS.md) | Interactive terminal UI, rendering, forms, and input. |
| [whatsapp/AGENTS.md](whatsapp/AGENTS.md) | WhatsApp pairing, authorization, media, and delivery. |
