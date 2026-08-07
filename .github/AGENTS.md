# GitHub Automation DOX

## Purpose

- Own continuous integration and release automation for repository products.

## Local Contracts

- Release workflows run the Go test/vet/build gates, smoke test, npm mapping test, and a CGO-free non-host build before publishing artifacts.
- Release workflows operate on the root `go.mod`, `package.json`, `.goreleaser.yaml`, source, scripts, and documentation without a nested product working directory.
- GitHub release archives, npm package versions, and Git tags must agree.
- Cross-repository Homebrew/Scoop publishing uses an explicit release token; never embed credentials.
- Actions must pin a stable major version and request only required workflow permissions.

## Child DOX Index

No child DOX files.
