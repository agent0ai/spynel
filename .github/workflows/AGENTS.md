# Workflow DOX

## Purpose

- Own CI, native packaging, release asset publication, and npm publication workflows.

## Local Contracts

- Keep verification gates aligned with root commands and run native builds on matching operating-system runners; workflow declarations alone are not native execution evidence.
- Write the verification executable only to ignored `.tmp-bin/spynel`; do not create a root `bin/` directory or root `spynel` file.
- Keep the release matrix limited to Linux amd64/arm64 and macOS amd64/arm64. Do not compile or upload Windows artifacts while Windows distribution is stubbed.
- Treat the published GitHub Release's `v`-prefixed semantic tag as the version source. Derive the npm manifest version from it, validate GitHub prerelease classification, and keep GitHub releases, archive versions, and npm versions in agreement.
- Publish the released root README as the npm README after pinning relative document links to the release tag on GitHub and relative image sources to the same tag on `raw.githubusercontent.com`.
- Preserve least-privilege permissions, OIDC trusted publishing, provenance, and the documented token-only bootstrap fallback.
- Archives include the executable, target-matched sherpa-onnx and ONNX Runtime libraries, license notices, a packaged-command smoke pass, and bounded target evidence.
- Linux native jobs install `patchelf`; packaging replaces absolute dependency build-directory RUNPATH entries with exactly `$ORIGIN/lib` and verifies packaged execution.

- Before publishing, every native target tests startup/updater cleanup and the plain installer/uninstaller against a verified 0.12.17 baseline archive, including simultaneous running npm/GitHub installations and modeled authorization for a regular user with no writable PATH entry.
- Every native target also runs filesystem publication and private Unix socket lifecycle/replacement regressions before packaging; Linux-only verification does not establish macOS cleanup behavior.

- All four native targets also run the isolated npm multi-instance update test against the candidate archive, including real TUI terminals and a synthetic newer executable; this gate precedes asset/npm publication.
- Native build checkouts include tags/history for the exact v0.12.14 launcher migration fixture; a shallow candidate-only checkout cannot run that regression.

- Manual release-workflow dispatch validates the selected commit with an explicit version through the same verification/native matrix, records the actual commit as evidence, and cannot enter the publication job. Use this gate before publishing a release that changes cross-platform lifecycle behavior.

- All four native jobs run test-native-supervisor.py on the packaged candidate before retaining evidence or publishing; combined supervision/privacy acceptance and exact-commit nonpublishing native gates precede public release actions.

- All four native jobs also run `test-affected-upgrade.py` with checksum-verified published 0.13.3 host bytes and the pinned authentic npm package. Retain bounded synthetic JSONL as separate `spynel-upgrade-<os>-<arch>` artifacts; old preflight refusal, real npm failed-download rollback, drained work and candidate-owned ready restart must pass with the deliberate zombie unreaped. Local Linux evidence never substitutes for matching macOS execution.

## Child DOX Index

No child DOX files.
