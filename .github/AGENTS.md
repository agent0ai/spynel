# GitHub Automation DOX

## Purpose

- Own continuous integration and release automation for repository products.

## Local Contracts

- Release workflows run the Go test/vet/build gates, smoke test, and npm mapping test before publishing artifacts.
- Release workflows operate on the root `go.mod`, `package.json`, source, scripts, and documentation without a nested product working directory.
- Build only Linux amd64/arm64 and macOS amd64/arm64 archives on matching native runners so CGO and sherpa-onnx libraries use the correct ABI. Windows has no release job while support is stubbed. Every archive includes the executable, required native libraries, and license notices.
- Each native build job runs the synthetic harness contract and packaged-archive smoke tooling on that same target and retains one target-specific bounded evidence artifact. Workflow configuration is not itself an observed result; only a retained record marked `observed-native` is native execution evidence.
- The published GitHub Release tag is the release version source; GitHub release archives and the derived npm package version must agree with it.
- npm publication uses the released root README with relative links rewritten to immutable tag-pinned GitHub targets.
- A published GitHub Release triggers verification, native builds, asset attachment, and mandatory npm publication. Stable releases publish to npm `latest`; semantic/GitHub prereleases publish to `next`.
- Prefer npm Trusted Publishing with job-scoped OIDC and provenance. Allow `NPM_TOKEN` only as the first-publication fallback because npm cannot configure a trusted publisher before the package exists.
- Cross-repository Homebrew/Scoop publishing uses an explicit release token; never embed credentials.
- Actions must pin a stable major version and request only required workflow permissions.

## Child DOX Index

Direct child DOX files:

| Child | Scope |
| --- | --- |
| [resources/AGENTS.md](resources/AGENTS.md) | Public repository and README image assets. |
| [workflows/AGENTS.md](workflows/AGENTS.md) | CI, native packaging, and release automation. |
