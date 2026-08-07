# GitHub Automation DOX

## Purpose

- Own continuous integration and release automation for repository products.

## Local Contracts

- Release workflows run the Go test/vet/build gates, smoke test, and npm mapping test before publishing artifacts.
- Release workflows operate on the root `go.mod`, `package.json`, source, scripts, and documentation without a nested product working directory.
- Build Linux amd64/arm64, macOS amd64/arm64, and Windows amd64 archives on matching native runners so CGO and sherpa-onnx libraries use the correct ABI. Every archive includes the executable, required native libraries, and license notices.
- GitHub release archives, npm package versions, and Git tags must agree.
- A published GitHub Release triggers verification, native builds, asset attachment, and mandatory npm publication. Stable releases publish to npm `latest`; semantic/GitHub prereleases publish to `next`.
- Prefer npm Trusted Publishing with job-scoped OIDC and provenance. Allow `NPM_TOKEN` only as the first-publication fallback because npm cannot configure a trusted publisher before the package exists.
- Cross-repository Homebrew/Scoop publishing uses an explicit release token; never embed credentials.
- Actions must pin a stable major version and request only required workflow permissions.

## Child DOX Index

No child DOX files.
