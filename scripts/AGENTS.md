# Scripts DOX

## Purpose

- Own reproducible development, smoke-test, and release helper scripts for Spynel.

## Local Contracts

- Scripts resolve paths relative to themselves and must not depend on the caller's working directory.
- Smoke tests use temporary projects and must not invoke live Codex, Claude Code, Telegram, or WhatsApp services; CLI-only smoke paths should avoid constructing a harness entirely.
- Generated binaries and caches stay under ignored directories.

## Child DOX Index

No child DOX files.
