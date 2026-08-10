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

## Child DOX Index

No child DOX files.
