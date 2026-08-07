# Scripts DOX

## Purpose

- Own reproducible development, smoke-test, and release helper scripts for Spynel.

## Local Contracts

- Scripts resolve paths relative to themselves and must not depend on the caller's working directory.
- Smoke tests use temporary projects and must not invoke live Codex, Claude Code, Telegram, or WhatsApp services; CLI-only smoke paths should avoid constructing a harness entirely.
- The smoke test covers harness-independent plain CLI status, shared deterministic commands, bounded conversation list/show/resume, strict inactive-follow-up rejection, every typed task/goal status folder, creation prompt assets, and harness-free direct task/goal helper output.
- Generated binaries and caches stay under ignored directories.
- `package-native.sh` accepts only path-safe release version identifiers, builds one supported host target with CGO, stages and executes it with the matching sherpa-onnx/ONNX Runtime libraries, and packages those libraries and license notices beside the executable.
- `capture-tui.sh` owns deterministic true-color screenshots of representative TUI states. It captures every stock theme with the same 120-by-34 fixture and produces a labeled light/dark/accessibility contact sheet. Keep its fixtures synchronized with durable layout contracts and inspect the PNGs after meaningful visual changes.

## Child DOX Index

No child DOX files.
