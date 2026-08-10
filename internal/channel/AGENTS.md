# Channel Runtime DOX

## Purpose

- Own the common channel contract, activity signaling, and hot-reload supervisor for TUI, Telegram, and WhatsApp adapters.

## Local Contracts

- Adapters translate messages and screens through `core` and the application service; they never invoke harness implementations directly.
- The supervisor consumes refreshed shared configuration snapshots, replaces only changed adapters, revokes stale adapter authority before cancellation, isolates failures, publishes lifecycle state, and retries only eligible unchanged configurations.
- Activity references must be balanced and overlap-safe so an older turn cannot clear a newer turn's visible activity.

## Child DOX Index

Direct child DOX files:

| Child | Scope |
| --- | --- |
| [telegram/AGENTS.md](telegram/AGENTS.md) | Telegram authorization, polling/webhook, media, and delivery. |
| [tui/AGENTS.md](tui/AGENTS.md) | Interactive terminal UI, rendering, forms, and input. |
| [whatsapp/AGENTS.md](whatsapp/AGENTS.md) | WhatsApp pairing, authorization, media, and delivery. |
