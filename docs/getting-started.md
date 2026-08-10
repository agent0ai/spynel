# Getting started and development

Spynel is distributed through the unscoped npm package on Linux and macOS. Both amd64 and arm64 are supported; Windows distribution is temporarily stubbed and fails before downloading an artifact.

## Public-release quick start

Node.js 18 or newer is required. From the directory you want to initialize as a Spynel workspace, run:

```bash
npm install -g spynel
spynel
```

The npm launcher downloads the checksummed native archive for the current platform and keeps the speech runtime libraries beside the executable. On first start in a directory without `.spynel/config.yaml`, choose **Initialize Spynel**. Spynel creates the private workspace state under that directory's fixed `.spynel/` folder and continues to setup or chat.

Explicit initialization is also available:

```bash
spynel init --dir /path/to/workspace
```

Interactive `init` continues into the application. Automation can initialize without starting it:

```bash
spynel init --no-start --dir /path/to/workspace
```

Spynel detects supported coding harnesses. If none is available, setup shows installation guidance; authentication remains the responsibility of the selected harness. Run `spynel doctor` after setup to check the configured environment. See [configuration](configuration.md) and [harness compatibility](harness-compatibility.md) for the supported profiles and exact settings.

## Run from a development checkout

The development helper can download a pinned Go toolchain into the ignored repository-level `.spynel-dev/` directory when Go is unavailable:

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
"$spynel_source/bin/spynel"
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
