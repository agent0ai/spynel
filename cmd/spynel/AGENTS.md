# Spynel Command Package DOX

## Purpose

- Own process entry-point wiring for the `spynel` executable.

## Local Contracts

- Keep `main.go` limited to argument dispatch, signal-aware lifecycle wiring, exit-code handling, and dependency composition through `internal/cli` and `internal/app`.
- Preserve the dedicated npm update exit code and keep `docs` offline, harness-free, server-free, and workspace-independent.
- Product behavior belongs in internal packages rather than this composition package.

- Dispatch supervisor/private executor through CLI before runtime initialization. Preserve the executor's actual status or signal after the parent restores its terminal; dedicated npm exit 75 remains an update handoff.

## Child DOX Index

No child DOX files.
