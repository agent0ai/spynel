# npm Packaging DOX

## Purpose

- Own the portable Node launcher and install-time acquisition of checksum-verified release assets.

## Local Contracts

- npm is a distribution wrapper only; runtime behavior remains in the Go binary.
- `package.json` lives at the repository root; launcher and installer sources live under `npm/`, and downloaded binaries live only in ignored `npm/vendor/` state.
- Keep the committed npm version as the required development placeholder. Release preparation validates the GitHub release tag and prerelease classification, derives the published package version from that tag, and rewrites relative root-README links to immutable tag-pinned GitHub URLs.
- Download native release assets from `github.com/agent0ai/spynel` unless `SPYNEL_DOWNLOAD_BASE` selects a trusted compatible mirror.
- Resolve operating system and architecture explicitly and fail with actionable unsupported-platform errors.
- Support the same four native targets as GitHub releases: Linux amd64/arm64 and macOS amd64/arm64. Preserve extracted companion libraries beside the executable. Fail every Windows architecture immediately with explicit temporary unsupported-platform guidance.
- Bound archive and checksum downloads, refuse secure-to-insecure redirects, verify release checksums, reject absolute or traversal archive paths, and reject links, special files, oversized trees, or excessive entry counts after extraction.
- Do not run the Go program during package installation.
- Extract and validate downloads in sibling staging state, then replace `npm/vendor/` as one directory so a failed install retains the prior runtime whenever npm itself permits rollback.
- Proactive registry checks have a ten-second total deadline and run only before interactive TUI starts. When an update is found, present the available/current versions and an explicit styled yes/no offer with a live ten-second countdown; timeout, EOF, interruption, and unrelated answers skip safely. Skip checks and prompts for noninteractive starts, plain commands, and `--automatic-startup`; never let registry failure prevent Spynel from starting.
- The launcher supervises explicit update requests from the Go process, runs the appropriate local or global `npm update` only after that executable exits, and starts the resulting binary with npm installation metadata in its environment.

## Child DOX Index

Direct child DOX files:

| Child | Scope |
| --- | --- |
| [bin/AGENTS.md](bin/AGENTS.md) | Installed executable shim. |
