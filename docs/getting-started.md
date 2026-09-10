# Getting started and development

Spynel supports standalone script installation and the unscoped npm package on Linux and macOS. Both amd64 and arm64 are supported; Windows distribution is temporarily stubbed and fails before downloading an artifact.

## Public-release quick start

Standalone installation needs POSIX `sh`, curl, tar, and sha256sum or shasum; it needs no Node.js, Go, or compiler:

```sh
curl -LsSf https://spynel.agent-zero.ai/install.sh | sh
spynel
```

**Availability:** the new script path requires a GitHub release containing the native standalone installer and the public URL routing. Releases predating that support cannot complete the bootstrap. Routing is managed separately from this repository; this documentation does not certify that the URL is live.

The installer places complete native bundles and licenses under `~/.local/share/spynel`, with an entry point at `~/.local/bin/spynel`. It prints PATH and shadowing guidance without editing shell profiles. Existing executables, including npm launchers, are preserved; if the user-bin name is occupied, use the printed standalone path. `SPYNEL_INSTALL_DIR` and `SPYNEL_BIN_DIR` select absolute alternative directories. `SPYNEL_VERSION` selects a stable version for the bootstrap. `SPYNEL_DOWNLOAD_BASE` may select a trusted compatible release-asset mirror; `SPYNEL_GITHUB_API_URL` overrides runtime release discovery. Mirrors supply executable code and must be trusted.

Alternatively, npm requires Node.js 18 or newer:

```bash
npm install -g spynel
spynel
```

The npm launcher downloads the checksummed native archive for the current platform and keeps the speech runtime libraries beside the executable. On first start in a directory without `.spynel/config.yaml`, choose **Initialize Spynel**. Spynel creates the private workspace state under that directory's fixed `.spynel/` folder and continues to setup or chat.

If that directory is nested below an initialized workspace, bare interactive `spynel` pauses before starting any workspace owner. The startup screen offers **Use parent workspace** (the default), **Initialize here**, or **Exit**. Using the parent changes the process working directory to its root; initializing creates a distinct local `.spynel`; exiting, Escape, and Ctrl+C leave both locations unchanged. Explicit `--config` commands, `spynel serve`, and other automation retain deterministic ancestor discovery and never wait for this choice.

Explicit initialization is also available:

```bash
spynel init --dir /path/to/workspace
```

Interactive `init` continues into the application. Automation can initialize without starting it:

```bash
spynel init --no-start --dir /path/to/workspace
```

Spynel detects supported coding harnesses. If none is available, setup shows installation guidance; authentication remains the responsibility of the selected harness. Run `spynel doctor` after setup to check the configured environment. See [configuration](configuration.md) and [harness compatibility](harness-compatibility.md) for the supported profiles and exact settings.

## Updates

`/update` (or `spynel update`) checks the installation that owns the workspace primary: npm installations check npm; script installations check stable GitHub Releases. `/update install` downloads, verifies, updates and restarts that installation through the shared application command. When no primary is running, the standalone CLI updates its own installation and prints the restarted version. Saved workspace configuration, histories and task/job state remain in place.

Only interactive TUI starts perform proactive checks, bounded to ten seconds and refreshed asynchronously at most hourly. npm retains its existing explicit startup offer; the standalone TUI shows update availability and waits for `/update install`. Headless services and noninteractive commands do not check automatically. `SPYNEL_SKIP_UPDATE_CHECK=1` suppresses proactive checks. No unattended upgrade is introduced.

Standalone updates stage a complete verified bundle before switching the stable launcher; failed downloads or validation leave the previous bundle usable. Older bundles and any interrupted temporary stages are retained so running processes keep their libraries. If reclaiming that space manually, stop every process using that installation first and preserve the bundle targeted by `current`. A concurrent installer returns a retry message; a crashed install automatically releases its lock. Development builds and manually extracted archives remain unmanaged and must be replaced using their original installation method.

## Run from a development checkout

The development helper can download a pinned Go toolchain into the ignored, disposable repository-level `.tmp-toolchains/` directory when Go is unavailable:

```bash
git clone https://github.com/agent0ai/spynel.git
cd spynel
./scripts/dev.sh build
```

Run the resulting binary from a separate directory so the repository itself is not initialized as the test workspace:

```bash
spynel_source="$(pwd)"
spynel_playground="${TMPDIR:-/tmp}/spynel-playground"
mkdir -p "$spynel_playground"
cd "$spynel_playground"
"$spynel_source/.tmp-bin/spynel"
```

To install the development executable under your user account:

```bash
./scripts/install-dev.sh
spynel version
```

The installer defaults to `~/.local/bin`, replaces only its `spynel` file, and prints PATH guidance when necessary. Use `SPYNEL_DEV_BIN_DIR=/absolute/bin` or `--bin-dir /absolute/bin` to choose another destination.

## Development verification

Run the repository checks relevant to a complete local change:

```bash
./scripts/dev.sh test
./scripts/smoke.sh
npm run test:npm
```

Release packaging has additional native-archive checks documented in [releasing](releasing.md). For non-visual operation, named conversations, streaming, and automation output, continue with the [plain CLI guide](cli.md).
