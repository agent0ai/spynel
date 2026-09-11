# Agent Documentation Catalog DOX

## Purpose

- Own curated, offline documentation exposed by `spynel docs` and concise help-topic metadata used by prompts and commands.

## Local Contracts

- Keep topic and section IDs stable, content classified and bounded, and text/JSON output deterministic under the documented record and size limits.
- Never read workspace state, environment values, histories, credentials, or arbitrary files; content is compiled from `content.go` and prompt guidance from `prompt.go`.
- Synchronize behavior facts with user documentation, CLI examples, and tests for pagination, search, formatting, and bounds.
- Keep model/effort/speed commands and atomic dispatch-boundary guidance synchronized with the harness capability mapping.
- Keep notification guidance on the concrete `--workdir`, exact `--origin`, and `--message` form used by task agents; do not teach task agents destination placeholders or stdin composition.
- Keep the primary-election topic explicit about environment-identity detection, bounded legacy readiness, fresh-owner fencing, and the limits of explicit Unix socket access across compatible same-kernel containers.

- The integration topic documents committed conversation subscriptions, honest retry/completion boundaries, optional private Unix sockets, and content-free headless stderr. Distinguish explicit same-kernel socket access from unchanged foreign-loopback discovery/election fences.

- Keep compiled update guidance explicit about update-by-default, `update check`, all-instance restart within the selected installation, shell `killall`, and older-instance restart limitations.

## Child DOX Index

No child DOX files.
