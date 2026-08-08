# Scripts DOX

## Purpose

- Own reproducible development, smoke-test, and release helper scripts for Spynel.

## Local Contracts

- Scripts resolve paths relative to themselves and must not depend on the caller's working directory.
- Smoke tests use temporary projects and must not invoke live Codex, Claude Code, Telegram, or WhatsApp services; CLI-only smoke paths should avoid constructing a harness entirely.
- The smoke test covers offline agent-readable docs, harness-independent plain CLI status, shared deterministic commands, bounded conversation list/show/resume, strict inactive-follow-up rejection, every typed task/goal status folder, creation prompt assets, reviewed and explicit no-review task inspection, and harness-free direct task/goal helper output.
- Generated binaries stay in temporary or ignored directories and are removed when no longer required.
- `cold-cache.sh` is repository-only developer tooling for deliberately requested diagnostics. It creates one uniquely owned cache outside the repository, starts the command in a dedicated process group, validates and terminates the complete owned process tree (including descendants that escape that group), waits for it, and removes only that cache on success, failure, cancellation, and documented recovery paths.
- If host termination prevents the helper's trap from running, recovery is limited to the exact outside-repository `spynel-gocache.*` path recorded by that diagnostic. Find processes whose readable environment contains that exact `GOCACHE` value, terminate and wait for all of them, revalidate the path and its `spynel-gocache.*` ownership signature, then remove only that directory. Never delete by a broad name match or while an owned process remains live.
- `package-native.sh` accepts only path-safe release version identifiers, builds one supported host target with CGO, stages and executes it with the matching sherpa-onnx/ONNX Runtime libraries, and packages those libraries and license notices beside the executable.
- `native-evidence` runs the synthetic `internal/harness` contract suite on the native host, safely extracts that host's archive into an awkward path, exercises only provider-free packaged commands, and writes a bounded, path-minimal JSON record classified as observed native evidence.
- `capture-tui.sh` owns deterministic true-color screenshots of representative TUI states. It captures every stock theme with the same 120-by-34 fixture and produces a labeled light/dark/accessibility contact sheet. Keep its fixtures synchronized with durable layout contracts and inspect the PNGs after meaningful visual changes.

## Child DOX Index

No child DOX files.
