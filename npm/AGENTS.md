# npm Packaging DOX

## Purpose

- Own the portable Node launcher and install-time acquisition of signed release assets.

## Local Contracts

- npm is a distribution wrapper only; runtime behavior remains in the Go binary.
- `package.json` lives at the repository root; launcher and installer sources live under `npm/`, and downloaded binaries live only in ignored `npm/vendor/` state.
- Resolve operating system and architecture explicitly and fail with actionable unsupported-platform errors.
- Verify downloaded archive checksums when release checksums are available.
- Do not run the Go program during package installation.

## Child DOX Index

No child DOX files.
