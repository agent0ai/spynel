# Releasing

Publishing a GitHub Release is the release trigger and its `v<semantic version>` tag is the release version source. `.github/workflows/release.yml` checks out that tag, derives the npm package version from it, runs Go test/vet/build, the smoke test, npm launcher tests, and `npm pack --dry-run`. Semantic prerelease tags must be published as GitHub prereleases.

Four CGO-enabled jobs build on matching native GitHub-hosted runners:

- Linux amd64 and arm64;
- macOS amd64 and arm64.

Windows distribution is temporarily unsupported. The workflow has no Windows job, native packaging rejects every Windows target before compilation, and the npm wrapper fails with explicit Linux/macOS guidance instead of requesting a missing artifact. Each supported archive contains the executable, matching sherpa-onnx and ONNX Runtime libraries, and required license notices. Model weights remain checksum-pinned first-use downloads rather than release assets.

After every native job succeeds, the publish job creates `checksums.txt`, attaches all archives and checksums to the already-published GitHub Release, and publishes the npm wrapper. Stable releases use npm's `latest` distribution tag; GitHub prereleases use `next`. npm publication is not optional or silently skipped, so a missing or invalid publisher configuration fails the release visibly.

## npm publisher setup

The preferred steady-state credential is npm Trusted Publishing (OIDC), not a long-lived secret. The workflow already grants only its publish job `id-token: write`, uses a GitHub-hosted runner with Node 24/npm 11, and invokes `npm publish --provenance` directly. Trusted publishing also enables provenance automatically for this public package.

The package must exist before npm permits a trusted publisher to be configured. For the first publication:

1. Create or use the npm account that will own the unscoped `spynel` package and enable 2FA.
2. Create a granular npm access token that can publish new public packages and bypasses interactive 2FA for automation.
3. Save it as the GitHub Actions repository secret `NPM_TOKEN` and publish the first matching GitHub Release.
4. In the new package's npm settings, add a GitHub Actions trusted publisher with organization/user `agent0ai`, repository `spynel`, workflow filename `release.yml`, and `npm publish` permission.
5. Remove `NPM_TOKEN` after one successful OIDC publication. The workflow passes the secret only as a fallback for the bootstrap release; an empty value is valid once trusted publishing is active.

The same setup can be created with a current npm CLI after the first package version exists:

```bash
npm trust github spynel --repo agent0ai/spynel --file release.yml --allow-publish
```

No GitHub personal access token is required for same-repository release assets: the job-scoped `GITHUB_TOKEN` receives `contents: write`. A separate fine-grained token is needed only if future workflow steps push metadata into another repository such as Homebrew or Scoop.

See npm's official [Trusted Publishing](https://docs.npmjs.com/trusted-publishers/) and [public package publishing](https://docs.npmjs.com/creating-and-publishing-unscoped-public-packages/) documentation for account-side controls.

## Release procedure

Create a GitHub Release with a `v`-prefixed semantic version tag, for example `v0.2.1`. Mark a version such as `v0.3.0-beta.1` as a GitHub prerelease. Publishing the release starts the workflow; creating or editing a draft does not. The committed `package.json` uses `0.0.0-development` because npm requires a version field; both verification and publication replace it with the tag-derived version in their isolated checkouts.

The npm package uses the root repository `README.md` from the released commit. Before publication, relative Markdown document links are pinned to that GitHub tag and relative Markdown or HTML image sources are pinned to `raw.githubusercontent.com` at the same tag. Consequently, an npm version keeps a stable README snapshot even when the default branch changes later.

The npm postinstall script maps supported Node platforms to the four archive names, downloads the matching archive and `checksums.txt`, verifies SHA-256, validates the executable, and atomically replaces `npm/vendor/` with the complete extracted runtime. It rejects Windows before any download and never runs Spynel during package installation. `SPYNEL_DOWNLOAD_BASE` may point installation at a trusted compatible mirror.

Before publishing, reproduce the local gates and build a runnable archive for the host target:

```bash
./scripts/dev.sh test
./scripts/smoke.sh
npm run test:npm
npm pack --dry-run
./scripts/package-native.sh 0.0.0 "$(go env GOOS)" "$(go env GOARCH)" /tmp/spynel-release
```

Extract the host archive and execute `spynel --version` with its companion libraries still in the staged layout. The release workflow performs that execution before it creates every archive.
