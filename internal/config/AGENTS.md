# Configuration DOX

## Purpose

- Own defaults, current-schema parsing, validation, path resolution, and the typed live-settings catalog for `.spynel/config.yaml`.

## Local Contracts

- Resolve relative paths from the workspace root, keep `.spynel` fixed, and ignore unused configuration keys on load without modifying the file; canonical saves emit only current settings. Validate all recognized values, types, and duplicate YAML keys; explicit setting commands still reject unknown names.
- Validate settings before saving, atomically replace private configuration, reload the saved canonical file into the shared process snapshot before returning, and never expose secret values in public descriptions or status.
- `startup.enabled` remains the saved autostart preference and text-command input. The application renders one action selected from native registration state and validates OS registration/removal for every explicit request; a saved preference alone is not registration evidence.
- Keep Telegram and WhatsApp enablement fail-closed on canonical allow-lists, validate harness/review/prefix choices, and keep extension enabled state, directory, and hook timeout as the sole restart-bound settings. Every other catalog setting applies live. Ignore the retired TUI launch key like every unused key and never emit it from canonical saves.
- Keep custom ACP arguments as a canonical YAML and runtime string vector, but expose them through one shared deterministic one-line command-text parser and formatter. Support quotes, empty arguments, and narrowly escaped whitespace/quotes/backslashes while preserving ordinary Windows backslashes; reject malformed, multiline, NUL, or invalid-UTF-8 input transactionally and never perform shell expansion.
- Keep optional `harness.reasoning_effort` backward-compatible: omission decodes to the historical `medium` runtime value and retains queryable legacy provenance, while explicit empty or `inherit` selects the provider default. Persist safely bounded provider-advertised effort identifiers without a closed enum; application capability validation owns model-specific combinations. Omitted or inherited `harness.service_mode` uses the provider default. Before validating a harness-name transaction, clear stored inference properties known to be unsupported by the destination; still reject explicitly requested unsupported combinations.
- Default `workspace.cleanup_retention_days` to 30 for omitted and generated configurations, validate 1 through 36500 whole days, and apply it live to conversation cleanup, job-archive removal, and terminal-task archiving.
- Default `orchestrator.retrigger_unresponded_messages` on for omitted and generated configurations, expose it with “Automatically processes stalled messages after restarts and disconnects.”, and apply it live so disabling prevents later scans without cancelling an admitted turn.

- Orchestrator workflows belong to `internal/orchestrator`; configuration contains no route definitions, paths, transitions, prompt selection, or stale thresholds.

## Child DOX Index

No child DOX files.
