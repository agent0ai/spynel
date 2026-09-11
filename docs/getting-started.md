# Getting started and development

Spynel supports standalone script installation and the unscoped npm package on Linux and macOS. Both amd64 and arm64 are supported.

## Public-release quick start

Standalone installation needs POSIX `sh`, curl, tar, and sha256sum or shasum; it needs no Node.js, Go, or compiler:

```sh
curl -LsSf https://spynel.agent-zero.ai/install.sh | sh
spynel
```

Standalone installation requires release 0.12.0 or newer; earlier releases do not contain its native installer.

The installer places its launcher on your existing PATH and verifies it before reporting success. Root and regular users can run `spynel` immediately in the same terminal. If no PATH directory is writable, it requests administrator authorization through `sudo`; no manual PATH edit or new terminal is required. Existing unrelated executables are preserved, and a conflicting command fails explicitly.

`SPYNEL_INSTALL_DIR` and `SPYNEL_BIN_DIR` select absolute alternative directories. An explicit off-PATH bin override configures shell startup files and prints an immediately usable absolute command. `SPYNEL_VERSION` selects a stable version. `SPYNEL_DOWNLOAD_BASE` selects a trusted compatible release-asset mirror; `SPYNEL_GITHUB_API_URL` overrides runtime release discovery. Mirrors supply executable code and must be trusted.

To uninstall:

```sh
curl -LsSf https://spynel.agent-zero.ai/uninstall.sh | sh
```

This stops the installations' processes, removes their startup registrations, and removes both the standalone GitHub installation and the current global npm installation when present. Workspace data and unrelated files are kept. A custom standalone installation uses the same `SPYNEL_INSTALL_DIR` during removal. Native cleanup requires release 0.12.4 or newer; the script obtains that helper independently of the installed version.

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

`spynel update` updates its own installation and restarts every running instance of that installation across workspaces, including primaries, headless servers and secondary TUIs. `/update` in a channel selects the workspace primary's installation and performs the same operation. npm installations use npm; script installations use verified stable GitHub bundles. `spynel update check` or `/update check` only checks versions. `update install` also applies the update. Even when already current, an update request restarts the instances. Saved workspace configuration, histories and task/job state remain in place.

`spynel killall` stops all verified running Spynel processes the caller can control, including other installations and older releases. It stops matching autostart services first so they do not immediately respawn, while retaining their registrations for the next boot/login. Coordinated restart uses private per-user process records: run the command with the same account and configuration home as the instances. Releases before 0.12.8 cannot receive this restart request; after installing the current command, run `spynel killall` once and relaunch those instances. An older npm launcher still supervising an updated binary also requires this one-time stop/relaunch. A detected older instance or failed restart produces an error instead of a success claim.

Only interactive TUI starts perform proactive checks, bounded to ten seconds and refreshed asynchronously at most hourly. npm retains its existing explicit startup offer; the standalone TUI shows update availability and waits for `/update`. Headless services and noninteractive commands do not check automatically. `SPYNEL_SKIP_UPDATE_CHECK=1` suppresses proactive checks. No unattended upgrade is introduced.

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
