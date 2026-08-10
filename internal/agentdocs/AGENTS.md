# Agent Documentation Catalog DOX

## Purpose

- Own curated, offline documentation exposed by `spynel docs` and concise help-topic metadata used by prompts and commands.

## Local Contracts

- Keep topic and section IDs stable, content classified and bounded, and text/JSON output deterministic under the documented record and size limits.
- Never read workspace state, environment values, histories, credentials, or arbitrary files; content is compiled from `content.go` and prompt guidance from `prompt.go`.
- Synchronize behavior facts with user documentation, CLI examples, and tests for pagination, search, formatting, and bounds.

## Child DOX Index

No child DOX files.
