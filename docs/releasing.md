# Releasing

The root workflow `.github/workflows/release.yml` runs Go tests/vet/build, the temporary-workspace smoke test, the npm platform test, and a CGO-free Windows build before checking that the npm version equals the Git tag. It then publishes six CGO-free GitHub release archives, updates Homebrew and Scoop manifests, and publishes the npm launcher when an npm token is configured.

## Repository setup

Create these publishing targets before the first release, or update `.goreleaser.yaml` to the actual owners:

- `frdel/spynel` for GitHub archives and checksums;
- `frdel/homebrew-tap` for the generated cask;
- `frdel/scoop-bucket` for the generated Windows manifest;
- the `spynel` npm package.

Configure repository secrets:

- `RELEASE_GITHUB_TOKEN`: a fine-grained token allowed to publish the release and update both package repositories. When absent, the workflow falls back to the repository `GITHUB_TOKEN`, which normally cannot update separate tap/bucket repositories.
- `NPM_TOKEN`: npm automation token. The workflow skips npm publishing when it is absent.

## Release

Set the same semantic version in `package.json`, then create and push the matching tag:

```bash
git tag v0.2.0
git push origin v0.2.0
```

The npm postinstall script maps Node platforms to the exact GoReleaser names (`linux_amd64`, `darwin_arm64`, `windows_amd64`, and peers), downloads the archive plus `checksums.txt`, verifies SHA-256, and only then extracts the binary. `SPYNEL_DOWNLOAD_BASE` can point installation at a trusted compatible mirror.

Before tagging, reproduce the local gates:

```bash
./scripts/dev.sh test
./scripts/smoke.sh
npm run test:npm
goreleaser check
```
