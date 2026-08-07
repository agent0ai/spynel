# Command DOX

## Purpose

- Own thin executable composition for the `spynel` binary.

## Local Contracts

- `spynel` with no subcommand launches the TUI and all enabled background services.
- `spynel serve` runs enabled server channels and orchestration without requiring a terminal UI.
- Command handlers delegate to `internal/cli`; do not put application logic in `main.go`.

## Child DOX Index

No child DOX files.

