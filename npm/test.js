"use strict";

const assert = require("assert");
const fs = require("fs");
const http = require("http");
const path = require("path");
const stream = require("stream");
const { resolve } = require("./platform");
const { install, validateArchiveEntries, validateExtractedTree } = require("./install");
const { prepareRelease, releaseMetadata, rewriteReadme } = require("./prepare-release");
const { STARTUP_PROMPT_TIMEOUT_MS, checkForUpdate, compareVersions, promptForStartupUpdate, shouldCheckAtStartup } = require("./update");
const { createLaunchEnvironment } = require("./bin/spynel");
const pkg = require("../package.json");

const staleSnapshot = {
  SPYNEL_NPM_PERIODIC_UPDATE_CHECKS: "1",
  SPYNEL_NPM_UPDATE_CHECKED_AT: "2000-01-01T00:00:00Z",
  SPYNEL_NPM_UPDATE_LATEST: "99.0.0",
};
const successfulSnapshot = createLaunchEnvironment(staleSnapshot, true, "2026-08-16T07:00:00Z", { latest: "0.3.0" });
assert.strictEqual(successfulSnapshot.SPYNEL_NPM_PERIODIC_UPDATE_CHECKS, "1");
assert.strictEqual(successfulSnapshot.SPYNEL_NPM_UPDATE_CHECKED_AT, "2026-08-16T07:00:00Z");
assert.strictEqual(successfulSnapshot.SPYNEL_NPM_UPDATE_LATEST, "0.3.0");
const failedSnapshot = createLaunchEnvironment(staleSnapshot, true, "2026-08-16T07:00:00Z");
assert.strictEqual(failedSnapshot.SPYNEL_NPM_PERIODIC_UPDATE_CHECKS, "1");
assert.strictEqual(failedSnapshot.SPYNEL_NPM_UPDATE_CHECKED_AT, "2026-08-16T07:00:00Z");
assert.strictEqual(failedSnapshot.SPYNEL_NPM_UPDATE_LATEST, undefined);
const skippedSnapshot = createLaunchEnvironment(staleSnapshot, false);
assert.strictEqual(skippedSnapshot.SPYNEL_NPM_PERIODIC_UPDATE_CHECKS, undefined);
assert.strictEqual(skippedSnapshot.SPYNEL_NPM_UPDATE_CHECKED_AT, undefined);
assert.strictEqual(skippedSnapshot.SPYNEL_NPM_UPDATE_LATEST, undefined);

assert.deepStrictEqual(resolve("linux", "x64"), { os: "linux", arch: "amd64", ext: "tar.gz" });
assert.deepStrictEqual(resolve("linux", "arm64"), { os: "linux", arch: "arm64", ext: "tar.gz" });
assert.deepStrictEqual(resolve("darwin", "x64"), { os: "darwin", arch: "amd64", ext: "tar.gz" });
assert.deepStrictEqual(resolve("darwin", "arm64"), { os: "darwin", arch: "arm64", ext: "tar.gz" });
assert.throws(() => resolve("win32", "x64"), /does not currently support Windows/);
assert.throws(() => resolve("win32", "arm64"), /does not currently support Windows/);
assert.throws(() => resolve("freebsd", "x64"), /does not publish/);

assert.strictEqual(pkg.name, "spynel");
assert.strictEqual(pkg.description, "A non-AI orchestration layer connecting one human to many coding agents");
assert.strictEqual(pkg.bin.spynel, "npm/bin/spynel.js");
assert.strictEqual(pkg.publishConfig.registry, "https://registry.npmjs.org");
assert.strictEqual(pkg.repository.url, "git+https://github.com/agent0ai/spynel.git");
assert(fs.readFileSync(path.join(__dirname, "install.js"), "utf8").includes("https://github.com/agent0ai/spynel/releases/download/"));
assert.deepStrictEqual(releaseMetadata("v1.2.3-beta.1", "true"), {
  tag: "v1.2.3-beta.1",
  version: "1.2.3-beta.1",
  prerelease: true,
});
assert.throws(() => releaseMetadata("1.2.3", "false"), /release tag/);
assert.throws(() => releaseMetadata("v1.2.3", "true"), /do not agree/);
assert.strictEqual(
  rewriteReadme(
    "![Logo](assets/logo.png) [Guide](docs/guide.md) [Section](#section) <img src=\"./assets/demo image.png\">\n",
    "v1.2.3",
  ),
  "![Logo](https://raw.githubusercontent.com/agent0ai/spynel/v1.2.3/assets/logo.png) " +
    "[Guide](https://github.com/agent0ai/spynel/blob/v1.2.3/docs/guide.md) [Section](#section) " +
    "<img src=\"https://raw.githubusercontent.com/agent0ai/spynel/v1.2.3/assets/demo%20image.png\">\n",
);
const prepared = fs.mkdtempSync(path.join(__dirname, ".test-release-"));
try {
  fs.writeFileSync(path.join(prepared, "package.json"), '{"name":"spynel","version":"0.0.0-development"}\n');
  fs.writeFileSync(path.join(prepared, "README.md"), "[Docs](docs/README.md)\n");
  assert.strictEqual(prepareRelease(prepared, "v2.0.0", "false").version, "2.0.0");
  assert.strictEqual(JSON.parse(fs.readFileSync(path.join(prepared, "package.json"), "utf8")).version, "2.0.0");
  assert.strictEqual(
    fs.readFileSync(path.join(prepared, "README.md"), "utf8"),
    "[Docs](https://github.com/agent0ai/spynel/blob/v2.0.0/docs/README.md)\n",
  );
} finally {
  fs.rmSync(prepared, { recursive: true, force: true });
}
assert.doesNotThrow(() => validateArchiveEntries(["./spynel", "./lib/runtime.so", "licenses/miniaudio/LICENSE"]));
assert.throws(() => validateArchiveEntries(["../../outside"]), /escapes/);
assert.throws(() => validateArchiveEntries(["C:\\outside\\spynel.exe"]), /absolute/);
assert.throws(() => validateArchiveEntries(["safe\nunsafe"]), /control/);
const extracted = fs.mkdtempSync(path.join(__dirname, ".test-extracted-"));
try {
  fs.mkdirSync(path.join(extracted, "lib"));
  fs.writeFileSync(path.join(extracted, "spynel"), "binary");
  fs.writeFileSync(path.join(extracted, "lib", "runtime.so"), "library");
  assert.doesNotThrow(() => validateExtractedTree(extracted));
  if (process.platform !== "win32") {
    fs.symlinkSync(path.join(extracted, "spynel"), path.join(extracted, "link"));
    assert.throws(() => validateExtractedTree(extracted), /symbolic link/);
  }
} finally {
  fs.rmSync(extracted, { recursive: true, force: true });
}
for (const required of ["bin/spynel.js", "install.js", "platform.js", "update.js"]) {
  assert(fs.existsSync(path.join(__dirname, required)), `npm package is missing ${required}`);
}
assert(compareVersions("1.3.0", "1.2.9") > 0);
assert(compareVersions("1.0.0", "1.0.0-rc.1") > 0);
assert(compareVersions("1.0.0-beta.2", "1.0.0-beta.11") < 0);
assert.strictEqual(shouldCheckAtStartup(["serve", "--automatic-startup"], { isTTY: true }, { isTTY: true }, {}), false);
assert.strictEqual(shouldCheckAtStartup(["version"], { isTTY: true }, { isTTY: true }, {}), false);
assert.strictEqual(shouldCheckAtStartup([], { isTTY: true }, { isTTY: true }, {}), true);
assert.strictEqual(shouldCheckAtStartup([], { isTTY: false }, { isTTY: true }, {}), false);
assert.strictEqual(STARTUP_PROMPT_TIMEOUT_MS, 10_000);

async function listen(server) {
  await new Promise((accept, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", accept);
  });
  return `http://127.0.0.1:${server.address().port}`;
}

async function close(server) {
  await new Promise((accept, reject) => server.close(error => error ? reject(error) : accept()));
}

async function main() {
  const platformDescriptor = Object.getOwnPropertyDescriptor(process, "platform");
  try {
    Object.defineProperty(process, "platform", { value: "win32", configurable: true });
    await assert.rejects(install(), /does not currently support Windows/);
  } finally {
    Object.defineProperty(process, "platform", platformDescriptor);
  }

  const registry = http.createServer((_, response) => {
    response.setHeader("content-type", "application/json");
    response.end(JSON.stringify({ name: "spynel", version: "0.3.0" }));
  });
  const registryURL = await listen(registry);
  try {
    const result = await checkForUpdate({ current: "0.2.0", registryURL, timeoutMs: 1_000 });
    assert.deepStrictEqual(result, { current: "0.2.0", latest: "0.3.0", available: true });
  } finally {
    await close(registry);
  }

  const stalled = http.createServer(() => {});
  const stalledURL = await listen(stalled);
  try {
    await assert.rejects(checkForUpdate({ current: "0.2.0", registryURL: stalledURL, timeoutMs: 20 }), /timed out/);
  } finally {
    await close(stalled);
  }

  const update = { current: "0.4.0", latest: "0.5.0", available: true };
  const prompt = async (answer, options = {}) => {
    const { answerDelayMs = 0, ...promptOptions } = options;
    const input = new stream.PassThrough();
    const output = new stream.PassThrough();
    input.isTTY = true;
    output.isTTY = true;
    output.columns = 100;
    let rendered = "";
    output.on("data", chunk => { rendered += chunk.toString("utf8"); });
    const result = promptForStartupUpdate(update, {
      input,
      output,
      environment: { TERM: "dumb" },
      timeoutMs: 100,
      tickMs: 5,
      countdownUnitMs: 10,
      ...promptOptions,
    });
    if (answer !== null) setTimeout(() => input.write(`${answer}\n`), answerDelayMs);
    return { accepted: await result, rendered };
  };
  assert.strictEqual((await prompt("yes")).accepted, true);
  assert.strictEqual((await prompt("n")).accepted, false);
  assert.strictEqual((await prompt("unrelated input")).accepted, false);
  const timedOut = await prompt(null);
  assert.strictEqual(timedOut.accepted, false);
  assert(timedOut.rendered.startsWith("\n⬆️  New version 0.5.0 available — current is 0.4.0\n"));
  assert(timedOut.rendered.includes("Update now? [Y]es / [N]o"));
  assert(timedOut.rendered.includes("skipping in 10…"));
  assert(timedOut.rendered.includes("skipping in 9…"));
  assert(timedOut.rendered.includes("timed out; skipping."));
  const partialInput = new stream.PassThrough();
  const partialOutput = new stream.PassThrough();
  partialInput.isTTY = true;
  partialOutput.isTTY = true;
  partialOutput.columns = 100;
  let partialRendered = "";
  partialOutput.on("data", chunk => { partialRendered += chunk.toString("utf8"); });
  const partialResult = promptForStartupUpdate(update, {
    input: partialInput,
    output: partialOutput,
    environment: { TERM: "xterm" },
    timeoutMs: 100,
    tickMs: 5,
    countdownUnitMs: 10,
  });
  setTimeout(() => partialInput.write("x"), 5);
  assert.strictEqual(await partialResult, false);
  assert(partialRendered.includes("skipping in 9…"));
  assert(partialRendered.includes("skipping in 1…"));
  assert(partialRendered.endsWith("timed out; skipping.\x1b[22m\n"));
  assert.strictEqual((await prompt("yes", { answerDelayMs: 120 })).accepted, false);
  const styledPrompt = await prompt("no", { environment: { TERM: "xterm" }, cursorControl: false });
  assert(styledPrompt.rendered.includes("\x1b[1mUpdate now\x1b[22m"));
  assert(styledPrompt.rendered.includes("\x1b[2mskipping in 10…\x1b[22m"));
  assert.strictEqual(await promptForStartupUpdate(update, {
    input: { isTTY: false },
    output: { isTTY: true },
  }), false);
  console.log("npm launcher, update check, and native target mapping are valid");
}

main().catch(error => {
  console.error(error);
  process.exitCode = 1;
});
