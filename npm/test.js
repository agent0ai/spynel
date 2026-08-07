"use strict";

const assert = require("assert");
const fs = require("fs");
const http = require("http");
const path = require("path");
const { resolve } = require("./platform");
const { validateArchiveEntries, validateExtractedTree } = require("./install");
const { checkForUpdate, compareVersions, shouldCheckAtStartup } = require("./update");
const pkg = require("../package.json");

assert.deepStrictEqual(resolve("linux", "x64"), { os: "linux", arch: "amd64", ext: "tar.gz" });
assert.deepStrictEqual(resolve("linux", "arm64"), { os: "linux", arch: "arm64", ext: "tar.gz" });
assert.deepStrictEqual(resolve("darwin", "x64"), { os: "darwin", arch: "amd64", ext: "tar.gz" });
assert.deepStrictEqual(resolve("darwin", "arm64"), { os: "darwin", arch: "arm64", ext: "tar.gz" });
assert.deepStrictEqual(resolve("win32", "x64"), { os: "windows", arch: "amd64", ext: "zip" });
assert.throws(() => resolve("win32", "arm64"), /does not publish/);
assert.throws(() => resolve("freebsd", "x64"), /does not publish/);

assert.strictEqual(pkg.name, "spynel");
assert.strictEqual(pkg.bin.spynel, "npm/bin/spynel.js");
assert.strictEqual(pkg.publishConfig.registry, "https://registry.npmjs.org");
assert.strictEqual(pkg.repository.url, "git+https://github.com/agent0ai/spynel.git");
assert(fs.readFileSync(path.join(__dirname, "install.js"), "utf8").includes("https://github.com/agent0ai/spynel/releases/download/"));
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
  console.log("npm launcher, update check, and native target mapping are valid");
}

main().catch(error => {
  console.error(error);
  process.exitCode = 1;
});
