# Bubble Tea Local Dependency DOX

## Purpose

- Own the MIT-licensed Bubble Tea v1.3.10 root Go package and upstream tests, selected by the root module's `replace` directive. Upstream tag commit: `9edf69c677c7353eca5fae6d3ea3986af39717b7` at https://github.com/charmbracelet/bubbletea/tree/v1.3.10.

## Local Contracts

- Keep upstream source, tests, module metadata and LICENSE intact except for the documented terminal output fix and complete-key decoding adapter. Examples, upstream automation and README are omitted.
- The terminal ownership patch discovers `ttyOutput` in `NewProgram` after options/default output are resolved, before any resize reader starts; Unix and Windows `initInput` no longer reassign it. `ReleaseTerminal`/`RestoreTerminal`, Exec and SIGWINCH share this immutable descriptor. Preserve raw-mode, initial-size and nil-renderer behavior.
- `DecodeKey` exposes the existing Program decoder for an already framed complete event; Spynel owns framing and Escape timing. The static terminal-copy path uses this adapter instead of maintaining a second function-key table. Keep upstream key semantics unchanged and retain the table-parity regression.
- This local module avoids editing the shared module cache or adding caller timing workarounds. Remove the replacement when an adopted upstream version fixes this ownership race and exposes equivalent key decoding; a v2 migration is outside this fix.
- Run `go test -race github.com/charmbracelet/bubbletea` and the real Spynel PTY regression from the repository root. `scripts/dev.sh test` includes this dependency's tests and vet because `./...` skips nested modules. `tty_race_linux_test.go` is the local owning-layer regression; the full application PTY verifies actual raw startup, F6 restore and SIGWINCH.

## Child DOX Index

No child DOX files.
