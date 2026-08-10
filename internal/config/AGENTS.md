# Configuration DOX

## Purpose

- Own defaults, parsing, validation, migration, path resolution, and the typed live-settings catalog for `.spynel/config.yaml`.

## Local Contracts

- Resolve relative paths from the workspace root and keep `.spynel` fixed; reject unsupported custom legacy state roots without moving private state implicitly.
- Validate settings as a complete transaction, write private configuration atomically, and never expose secret values in public descriptions or status.
- Keep Telegram and WhatsApp enablement fail-closed on canonical allow-lists, validate harness/review/prefix choices, and classify restart-bound versus live settings accurately.

## Child DOX Index

No child DOX files.
