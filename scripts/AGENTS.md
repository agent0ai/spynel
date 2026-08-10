# Scripts DOX

## Purpose

- Own reproducible development, smoke-test, and release helper scripts for Spynel.

## Local Contracts

- Scripts resolve paths relative to themselves and must not depend on the caller's working directory.
- `install-dev.sh` reuses `dev.sh build`, atomically installs the executable into `$SPYNEL_DEV_BIN_DIR` or the conventional per-user `$HOME/.local/bin`, never edits shell profiles, and reports exact PATH setup or shadowing guidance so later terminals can resolve the development build safely.
- Smoke tests use temporary projects and must not invoke live Codex, Claude Code, Pi, ACP agents, Telegram, or WhatsApp services; CLI-only smoke paths should avoid constructing a harness entirely.
- The smoke test covers the user-bin development installer in an isolated target, canonical `.spynel/config.yaml` initialization without `workspace.state_dir`, exactly five valid persistent role-instruction files and content-free inspection, offline agent-readable docs, harness-independent plain CLI status, shared deterministic commands, every default-open and semantic task/goal view plus detailed and NDJSON output through their direct automation aliases, bounded conversation list/show/resume, strict inactive-follow-up rejection, every typed task/goal status folder, user-overridable prompt assets and workflow contracts, reviewed and explicit no-review task inspection, and harness-free direct task/goal helper output.
- Generated binaries stay in temporary or ignored directories and are removed when no longer required.
- `cold-cache.sh` is repository-only developer tooling for deliberately requested diagnostics. It creates one uniquely owned cache outside the repository, starts the command in a dedicated process group, validates and terminates the complete owned process tree (including descendants that escape that group), waits for it, and removes only that cache on success, failure, cancellation, and documented recovery paths.
- If host termination prevents the helper's trap from running, recovery is limited to the exact outside-repository `spynel-gocache.*` path recorded by that diagnostic. Find processes whose readable environment contains that exact `GOCACHE` value, terminate and wait for all of them, revalidate the path and its `spynel-gocache.*` ownership signature, then remove only that directory. Never delete by a broad name match or while an owned process remains live.
- `package-native.sh` accepts only path-safe release version identifiers, builds one of the four supported Linux/macOS host targets with CGO, stages and executes it with the matching sherpa-onnx/ONNX Runtime libraries, and packages those libraries and license notices beside the executable. Windows targets stop at an explicit temporary stub error before host or compilation work.
- `native-evidence` runs the synthetic `internal/harness` contract suite on the native host, safely extracts that host's archive into an awkward path, exercises only provider-free packaged commands, and writes a bounded, path-minimal JSON record classified as observed native evidence.
- `capture-tui.sh` owns deterministic true-color screenshots of representative TUI states. It captures every stock theme with the same 120-by-34 fixture and produces a labeled light/dark/accessibility contact sheet. Keep its fixtures synchronized with durable layout contracts and inspect the PNGs after meaningful visual changes.

## Child DOX Index

Direct child DOX files:

| Child | Scope |
| --- | --- |
| [doxcheck/AGENTS.md](doxcheck/AGENTS.md) | Deterministic repository DOX coverage and index validation. |
| [native-evidence/AGENTS.md](native-evidence/AGENTS.md) | Synthetic native-package execution evidence helper. |
