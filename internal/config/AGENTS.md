# Configuration DOX

## Purpose

- Own defaults, strict current-schema parsing, validation, path resolution, and the typed live-settings catalog for `.spynel/config.yaml`.

## Local Contracts

- Resolve relative paths from the workspace root, keep `.spynel` fixed, and reject unknown or obsolete configuration fields instead of migrating them.
- Validate settings before saving, atomically replace private configuration, reload the saved canonical file into the shared process snapshot before returning, and never expose secret values in public descriptions or status.
- Keep Telegram and WhatsApp enablement fail-closed on canonical allow-lists, validate harness/review/prefix choices, and keep extension enabled state, directory, and hook timeout as the sole restart-bound settings. Every other catalog setting applies live. Normalize the retired TUI launch key away on load and never emit it from canonical saves.
- Keep custom ACP arguments as a canonical YAML and runtime string vector, but expose them through one shared deterministic one-line command-text parser and formatter. Support quotes, empty arguments, and narrowly escaped whitespace/quotes/backslashes while preserving ordinary Windows backslashes; reject malformed, multiline, NUL, or invalid-UTF-8 input transactionally and never perform shell expansion.

## Child DOX Index

No child DOX files.
