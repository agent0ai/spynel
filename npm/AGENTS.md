# npm Packaging DOX

## Purpose

- Own the portable Node launcher and install-time acquisition of checksum-verified release assets.

## Local Contracts

- npm is a distribution wrapper only; runtime behavior remains in the Go binary.
- `package.json` lives at the repository root; launcher and installer sources live under `npm/`, and downloaded binaries live only in ignored `npm/vendor/` state.
- Download native release assets from `github.com/agent0ai/spynel` unless `SPYNEL_DOWNLOAD_BASE` selects a trusted compatible mirror.
- Resolve operating system and architecture explicitly and fail with actionable unsupported-platform errors.
- Support the same five native targets as GitHub releases: Linux amd64/arm64, macOS amd64/arm64, and Windows amd64. Preserve extracted companion libraries beside the executable.
- Bound archive and checksum downloads, refuse secure-to-insecure redirects, verify release checksums, reject absolute or traversal archive paths, and reject links, special files, oversized trees, or excessive entry counts after extraction.
- Do not run the Go program during package installation.
- Extract and validate downloads in sibling staging state, then replace `npm/vendor/` as one directory so a failed install retains the prior runtime whenever npm itself permits rollback.
- Proactive registry checks have a ten-second total deadline and run only before interactive TUI starts. Skip them for plain commands and `--automatic-startup`; never let registry failure prevent Spynel from starting.
- The launcher supervises explicit update requests from the Go process, runs the appropriate local or global `npm update` only after that executable exits, and starts the resulting binary with npm installation metadata in its environment.

## Child DOX Index

No child DOX files.
