"use strict";

const assert = require("assert");
const fs = require("fs");
const http = require("http");
const path = require("path");
const stream = require("stream");
const { spawn, spawnSync } = require("child_process");
const { once } = require("events");
const { resolve } = require("./platform");
const { install, validateArchiveEntries, validateExtractedTree } = require("./install");
const { prepareRelease, releaseMetadata, rewriteReadme } = require("./prepare-release");
const { runCommand, STARTUP_PROMPT_TIMEOUT_MS, checkForUpdate, compareVersions, promptForStartupUpdate, shouldCheckAtStartup } = require("./update");
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
assert.strictEqual(skippedSnapshot.SPYNEL_NPM_COORDINATED_UPDATES, "2");
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

async function checkLauncherSignals() {
  const directory = fs.mkdtempSync(path.join(require("os").tmpdir(), "spynel-launcher-"));
  try {
    fs.mkdirSync(path.join(directory, "npm", "bin"), { recursive: true });
    fs.mkdirSync(path.join(directory, "npm", "vendor"));
    for (const file of ["bin/spynel.js", "platform.js", "update.js"]) {
      fs.copyFileSync(path.join(__dirname, file), path.join(directory, "npm", file));
    }
    fs.copyFileSync(path.join(__dirname, "..", "package.json"), path.join(directory, "package.json"));
    const marker = path.join(directory, "stopped");
    fs.writeFileSync(path.join(directory, "npm", "vendor", "spynel"), `#!/usr/bin/env node
for (const signal of ["SIGINT", "SIGTERM", "SIGHUP"]) process.on(signal, () => {
  setTimeout(() => { require("fs").writeFileSync(${JSON.stringify(marker)}, signal); process.exit(0); }, 30);
});
console.log(process.pid);
setInterval(() => {}, 1000);
`, { mode: 0o700 });
    for (const signal of ["SIGINT", "SIGTERM", "SIGHUP"]) {
      const launcher = spawn(process.execPath, [path.join(directory, "npm", "bin", "spynel.js"), "serve"], { stdio: ["ignore", "pipe", "pipe"] });
      let nativePID;
      const exited = once(launcher, "exit");
      const timeout = setTimeout(() => launcher.kill("SIGKILL"), 3000);
      try {
        const [ready] = await once(launcher.stdout, "data", { signal: AbortSignal.timeout(3000) });
        nativePID = Number(ready.toString().trim());
        assert(Number.isInteger(nativePID) && nativePID > 0);
        launcher.kill(signal);
        assert.deepStrictEqual(await exited, [0, null], "launcher must wait for graceful native shutdown");
        assert.strictEqual(fs.readFileSync(marker, "utf8"), signal);
      } finally {
        clearTimeout(timeout);
        launcher.kill("SIGKILL");
        if (nativePID) {
          try { process.kill(nativePID, "SIGKILL"); } catch (error) { if (error.code !== "ESRCH") throw error; }
        }
      }
    }
  } finally {
    fs.rmSync(directory, { recursive: true, force: true });
  }
}

function checkLauncherDiagnostics() {
  const directory = fs.mkdtempSync(path.join(__dirname, ".tmp-diagnostics-"));
  try {
    fs.mkdirSync(path.join(directory, "npm", "bin"), { recursive: true });
    fs.mkdirSync(path.join(directory, "npm", "vendor"));
    for (const file of ["bin/spynel.js", "platform.js", "update.js"]) {
      fs.copyFileSync(path.join(__dirname, file), path.join(directory, "npm", file));
    }
    fs.copyFileSync(path.join(__dirname, "..", "package.json"), path.join(directory, "package.json"));
    const record = path.join(directory, "observed.json");
    fs.writeFileSync(path.join(directory, "npm", "vendor", "spynel"), `#!${process.execPath}
const fs = require("fs");
if (process.argv[2] === "__diagnostics") {
  fs.writeFileSync(${JSON.stringify(record)}, fs.readFileSync(0), {mode: 0o600});
  process.exit(process.env.DIAGNOSTIC_TEST_MODE === "recorder-failure" ? 1 : 0);
}
fs.writeSync(3, JSON.stringify({directory: "unused", instance: "synthetic-run", version: "test", revision: "test", pid: process.pid}));
console.log("intentional-stdout");
process.stderr.write("panic: synthetic-native-cause\\n" + "x".repeat(200000), () => {
  const mode = process.env.DIAGNOSTIC_TEST_MODE;
  if (mode.startsWith("SIG")) process.kill(process.pid, mode);
  else process.exit(mode === "normal" ? 0 : 7);
});
`, { mode: 0o700 });
    for (const mode of ["normal", "error", "SIGTERM", "SIGKILL", "recorder-failure"]) {
      const result = spawnSync(process.execPath, [path.join(directory, "npm", "bin", "spynel.js"), "serve"], {
        encoding: "utf8", timeout: 5000, maxBuffer: 1 << 20,
        env: { ...process.env, DIAGNOSTIC_TEST_MODE: mode }
      });
      assert.ifError(result.error);
      assert.strictEqual(result.signal, mode.startsWith("SIG") ? mode : null);
      assert.strictEqual(result.status, mode.startsWith("SIG") ? null : mode === "normal" ? 0 : 7);
      assert.strictEqual(result.stdout, "intentional-stdout\n");
      assert(result.stderr.includes("synthetic-native-cause"));
      assert(result.stderr.length >= 200000, "stderr must continue reaching its original destination");
      const observed = JSON.parse(fs.readFileSync(record, "utf8"));
      assert.strictEqual(observed.signal, mode.startsWith("SIG") ? mode : "");
      assert.strictEqual(observed.status, mode.startsWith("SIG") ? null : mode === "normal" ? 0 : 7);
      assert.strictEqual(observed.stderr, undefined, "exit metadata must not contain application output");
      if (mode === "recorder-failure") assert(result.stderr.includes("native exit diagnostics unavailable"));
    }
  } finally {
    fs.rmSync(directory, { recursive: true, force: true });
  }
}

function checkLauncherUpdates() {
  const directory = fs.mkdtempSync(path.join(require("os").tmpdir(), "spynel-update-"));
  try {
    fs.mkdirSync(path.join(directory, "npm", "bin"), { recursive: true });
    fs.mkdirSync(path.join(directory, "npm", "vendor"));
    fs.mkdirSync(path.join(directory, "tools"));
    for (const file of ["bin/spynel.js", "platform.js", "update.js"]) {
      fs.copyFileSync(path.join(__dirname, file), path.join(directory, "npm", file));
    }
    const native = `#!${process.execPath}
const fs = require("fs");
const action = process.argv[2];
fs.appendFileSync(process.env.UPDATE_TEST_LOG, action + "\\n");
if (action === process.env.UPDATE_TEST_FAIL) process.exit(1);
if (action === "hold-worker-update") {
  let input = "";
  process.stdout.write("ready\\n");
  process.stdin.on("data", chunk => input += chunk);
  process.stdin.on("end", () => { if (input !== (process.env.UPDATE_TEST_FAIL === "npm" ? "" : "commit\\n")) process.exit(2); });
}
if (action === "update") {
  if (process.env.UPDATE_TEST_FAIL === "raw75") process.exit(75);
  if (process.env.UPDATE_TEST_FAIL === "invalid75") { fs.writeFileSync(process.env.SPYNEL_NPM_UPDATE_STATE, "{}", {mode: 0o600}); process.exit(75); }
  fs.writeFileSync(process.env.SPYNEL_NPM_UPDATE_STATE, JSON.stringify({args: ["version"], install: process.env.UPDATE_TEST_CURRENT !== "1", version: "2.0.0", worker_protocol: 14}), {mode: 0o600});
  process.exit(75);
}
`;
    fs.writeFileSync(path.join(directory, "npm", "vendor", "spynel"), native, { mode: 0o700 });
    fs.writeFileSync(path.join(directory, "tools", "npm"), `#!${process.execPath}
const fs = require("fs");
if (process.argv[2] === "root") process.exit(1);
fs.appendFileSync(process.env.UPDATE_TEST_LOG, "npm\\n");
if (process.env.UPDATE_TEST_FAIL === "npm") process.exit(1);
fs.writeFileSync("package.json", JSON.stringify({name: "spynel", version: "2.0.0"}));
`, { mode: 0o700 });
    for (const failure of ["", "check-restartable", "hold-worker-update", "npm", "restart-instances", "current", "raw75", "invalid75"]) {
      const log = path.join(directory, "calls");
      fs.writeFileSync(log, "");
      fs.writeFileSync(path.join(directory, "package.json"), JSON.stringify({ name: "spynel", version: "1.0.0" }));
      const result = spawnSync(process.execPath, [path.join(directory, "npm", "bin", "spynel.js"), "update"], {
        encoding: "utf8", timeout: 5000,
        env: { ...process.env, PATH: path.join(directory, "tools") + path.delimiter + process.env.PATH, UPDATE_TEST_LOG: log, UPDATE_TEST_FAIL: failure, UPDATE_TEST_CURRENT: failure === "current" ? "1" : "0" }
      });
      assert.ifError(result.error);
      const expected = ["raw75", "invalid75"].includes(failure) ? ["update"] : ["update", "check-restartable", ...(failure === "current" ? [] : ["hold-worker-update", "npm"]), "restart-instances", "version"];
      const failed = expected.indexOf(failure);
      assert.deepStrictEqual(fs.readFileSync(log, "utf8").trim().split("\n"), failed >= 0 ? expected.slice(0, failed + 1) : expected);
      assert.strictEqual(result.status, ["raw75", "invalid75"].includes(failure) ? 75 : failed >= 0 ? 1 : 0, result.stderr);
    }
  } finally {
    fs.rmSync(directory, { recursive: true, force: true });
  }
}

async function checkLauncherHandoff() {
  const directory = fs.mkdtempSync(path.join(__dirname, ".tmp-handoff-"));
  try {
    fs.mkdirSync(path.join(directory, "npm", "bin"), { recursive: true });
    fs.mkdirSync(path.join(directory, "npm", "vendor"));
    fs.mkdirSync(path.join(directory, "tools"));
    const launcherSource = fs.readFileSync(path.join(__dirname, "bin/spynel.js"), "utf8");
    const updateSource = fs.readFileSync(path.join(__dirname, "update.js"), "utf8");
    fs.writeFileSync(path.join(directory, "fresh-update.js"), updateSource);
    fs.copyFileSync(path.join(__dirname, "platform.js"), path.join(directory, "npm", "platform.js"));
    fs.writeFileSync(path.join(directory, "fresh-launcher.js"), launcherSource.replace(
      'SPYNEL_NPM_COORDINATED_UPDATES: "2",',
      'SPYNEL_NPM_COORDINATED_UPDATES: "2", SPYNEL_TEST_FRESH_LAUNCHER: "1",'
    ));
    fs.writeFileSync(path.join(directory, "tty.js"), "process.stdin.isTTY = process.stdout.isTTY = true;");
    fs.writeFileSync(path.join(directory, "npm", "vendor", "spynel"), `#!${process.execPath}
const fs = require("fs");
const action = process.argv[2];
fs.appendFileSync(process.env.HANDOFF_LOG, JSON.stringify({action, args: process.argv.slice(2), parent: process.ppid, fresh: process.env.SPYNEL_TEST_FRESH_LAUNCHER, protocol: process.env.SPYNEL_NPM_COORDINATED_UPDATES, periodic: process.env.SPYNEL_NPM_PERIODIC_UPDATE_CHECKS, checkedAt: process.env.SPYNEL_NPM_UPDATE_CHECKED_AT, latest: process.env.SPYNEL_NPM_UPDATE_LATEST}) + "\\n");
if (action === "--version") console.log("spynel " + require("../../package.json").version);
if (action === "restart-instances" && process.env.HANDOFF_MODE === "coordination-failure") process.exit(1);
if (action === "hold-worker-update") { process.stdout.write("ready\\n"); process.stdin.resume(); }
if (action === "restart-instances" && process.env.HANDOFF_MODE === "stop-coordination") {
  console.error("ready:" + process.pid);
  setInterval(() => {}, 1000);
}
if (action === "update" || action === "serve" && process.env.HANDOFF_MODE === "repeat" && require("../../package.json").version === "0.13.1") {
  fs.writeFileSync(process.env.SPYNEL_NPM_UPDATE_STATE, JSON.stringify({args: ["serve", "--config", "space ü/.spynel/config.yaml"], install: true, version: action === "serve" ? "0.13.2" : "0.13.1", worker_protocol: 14}), {mode: 0o600});
  process.exit(75);
}
if (action === "serve" && process.env.HANDOFF_MODE === "stop-native") {
  process.on("SIGTERM", () => setTimeout(() => process.exit(0), 30));
  console.log("ready:" + process.pid);
  setInterval(() => {}, 1000);
}
`, { mode: 0o700 });
    fs.writeFileSync(path.join(directory, "tools", "npm"), `#!${process.execPath}
const fs = require("fs");
if (process.argv[2] === "root") process.exit(1);
fs.appendFileSync(process.env.HANDOFF_LOG, JSON.stringify({action: "npm", pid: process.pid, args: process.argv.slice(2)}) + "\\n");
if (process.env.HANDOFF_MODE === "startup-failure") process.exit(1);
if (process.env.HANDOFF_MODE === "package-only") process.exit(0);
const before = require(process.cwd() + "/package.json").version;
fs.writeFileSync("package.json", JSON.stringify({name: "spynel", version: before === "0.13.1" || process.env.HANDOFF_MODE === "unexpected-version" ? "0.13.2" : "0.13.1"}));
fs.copyFileSync("fresh-launcher.js", "npm/bin/spynel.js");
fs.copyFileSync("fresh-update.js", "npm/update.js");
fs.appendFileSync("npm/update.js", '\\nrequire("fs").appendFileSync(process.env.HANDOFF_LOG, JSON.stringify({action: "fresh-helper", version: require("../package.json").version, parent: process.pid, listeners: process.listenerCount("SIGTERM")}) + "\\\\n");\\n');
if (process.env.HANDOFF_MODE === "unsupported") fs.writeFileSync("npm/bin/spynel.js", "module.exports = {};");
if (process.env.HANDOFF_MODE.endsWith("stop-update")) {
  console.log("ready:" + process.pid);
  setInterval(() => {}, 1000);
}
`, { mode: 0o700 });
    for (const mode of ["repeat", "startup", "startup-failure", "startup-stop-update", "unsupported", "coordination-failure", "package-only", "unexpected-version", "stop-update", "stop-coordination", "stop-native"]) {
      const startup = mode.startsWith("startup");
      const log = path.join(directory, "calls");
      fs.writeFileSync(log, "");
      fs.writeFileSync(path.join(directory, "package.json"), JSON.stringify({ name: "spynel", version: "0.13.0" }));
      fs.writeFileSync(path.join(directory, "npm", "bin", "spynel.js"), launcherSource);
      fs.writeFileSync(path.join(directory, "npm", "update.js"), updateSource + (startup ? `
module.exports.checkForUpdate = async () => ({available: true, latest: "0.13.1", workerProtocol: 14});
module.exports.promptForStartupUpdate = async () => true;
` : ""));
      const command = [...(startup ? ["-r", path.join(directory, "tty.js")] : []), path.join(directory, "npm", "bin", "spynel.js"), ...(startup ? ["serve", "--tui"] : ["update"])];
      const environment = { ...process.env, SPYNEL_SKIP_UPDATE_CHECK: "0", PATH: path.join(directory, "tools") + path.delimiter + process.env.PATH, HANDOFF_LOG: log, HANDOFF_MODE: mode };
      let result;
      if (mode.includes("stop-")) {
        const launcher = spawn(process.execPath, command, { env: environment, stdio: ["ignore", "pipe", "pipe"] });
        const exited = once(launcher, "exit");
        let childPID;
        let stderr = "";
        launcher.stderr.on("data", chunk => { stderr += chunk; });
        const timeout = setTimeout(() => launcher.kill("SIGKILL"), 5000);
        try {
          const [ready] = await once(mode === "stop-coordination" ? launcher.stderr : launcher.stdout, "data", { signal: AbortSignal.timeout(5000) });
          childPID = Number(ready.toString().trim().split(":")[1]);
          assert(Number.isInteger(childPID) && childPID > 0);
          launcher.kill("SIGTERM");
          assert.deepStrictEqual(await exited, [0, null], stderr);
          assert.throws(() => process.kill(childPID, 0), {code: "ESRCH"}, "launcher must reap its helper/native child");
          result = {status: 0, stderr};
        } finally {
          clearTimeout(timeout);
          launcher.kill("SIGKILL");
          if (childPID) {
            try { process.kill(childPID, "SIGKILL"); } catch (error) { if (error.code !== "ESRCH") throw error; }
          }
        }
      } else {
        result = spawnSync(process.execPath, command, { encoding: "utf8", timeout: 5000, env: environment });
        assert.ifError(result.error);
      }
      const calls = fs.readFileSync(log, "utf8").trim().split("\n").map(line => JSON.parse(line));
      const actions = calls.map(call => call.action);
      assert.strictEqual(result.status, ["unsupported", "coordination-failure", "unexpected-version"].includes(mode) ? 1 : 0, result.stderr);
      const expected = [...(startup ? [] : ["update"]), "check-restartable", "hold-worker-update", "npm"];
      if (!["unsupported", "package-only", "unexpected-version", "stop-update", "startup-stop-update", "startup-failure"].includes(mode)) {
        expected.push("fresh-helper", "restart-instances");
        if (!["coordination-failure", "stop-coordination"].includes(mode)) expected.push("serve");
        if (mode === "repeat") expected.push("check-restartable", "hold-worker-update", "npm", "fresh-helper", "restart-instances", "serve");
      }
      if (mode === "startup-failure") expected.push("--version", "restart-instances", "serve");
      if (mode === "package-only") expected.push("--version", "restart-instances", "serve");
      assert.deepStrictEqual(actions, expected, mode);
      for (const call of calls.filter(call => call.action === "check-restartable")) {
        assert.deepStrictEqual(call.args, ["check-restartable", "--worker-protocol", "14"]);
      }
      const installs = calls.filter(call => call.action === "npm");
      for (let index = 0; index < installs.length; index++) {
        const version = index === 0 ? "0.13.1" : "0.13.2";
        assert(installs[index].args.includes(`spynel@${version}`), "install must pin the preflighted target");
        assert.deepStrictEqual(installs[index].args.slice(0, 3), ["install", "--no-save", "--package-lock=false"]);
      }
      const nativeCalls = calls.filter(call => ["update", "serve", "check-restartable", "restart-instances"].includes(call.action));
      assert.strictEqual(new Set(nativeCalls.map(call => call.parent)).size, 1, "handoff must retain a single Node process");
      for (const call of calls.filter(call => call.action === "fresh-helper")) {
        assert.strictEqual(call.listeners, 1, "handoff must retain one lifetime signal handler");
        assert.strictEqual(call.parent, nativeCalls[0].parent);
      }
      assert.deepStrictEqual(calls.filter(call => call.action === "fresh-helper").map(call => call.version), mode === "repeat" ? ["0.13.1", "0.13.2"] : expected.includes("fresh-helper") ? ["0.13.1"] : []);
      for (const call of calls.filter(call => ["restart-instances", "serve"].includes(call.action))) {
        assert.strictEqual(call.fresh, ["startup-failure", "package-only"].includes(mode) ? undefined : "1", "post-publication work must use the installed launcher implementation");
        assert.strictEqual(call.protocol, "2");
        const snapshot = startup && !(mode === "startup-failure" && call.action === "restart-instances");
        assert.strictEqual(call.periodic, snapshot ? "1" : undefined);
        assert.strictEqual(call.latest, snapshot ? "0.13.1" : undefined);
        if (snapshot) assert(Number.isFinite(Date.parse(call.checkedAt)), "handoff must preserve the startup snapshot");
        if (call.action === "serve") assert.deepStrictEqual(call.args, startup ? ["serve", "--tui"] : ["serve", "--config", "space ü/.spynel/config.yaml"]);
      }
      if (mode === "unsupported") assert.match(result.stderr, /cannot accept the update handoff/);
      if (mode === "package-only") assert.match(result.stderr, /remained at 0.13.0/);
      if (mode === "unexpected-version") assert.match(result.stderr, /remained at 0.13.2/);
    }
  } finally {
    fs.rmSync(directory, { recursive: true, force: true });
  }
}

async function checkUpdateHelperCancellation() {
  const controller = new AbortController();
  const pending = runCommand(process.execPath, ["-e", "setInterval(() => {}, 1000)"], { signal: controller.signal, stdio: "ignore", timeout: 2000 });
  controller.abort();
  const result = await pending;
  assert(result.error && result.error.code === "ABORT_ERR");
  assert.strictEqual(result.signal, "SIGTERM");
  const stopped = new AbortController();
  stopped.abort();
  const never = await runCommand(process.execPath, ["-e", "process.exit(99)"], { signal: stopped.signal, stdio: "ignore" });
  assert(never.error && never.error.code === "ABORT_ERR");
}

async function main() {
  await checkUpdateHelperCancellation();
  await checkLauncherHandoff();
  checkLauncherUpdates();
  checkLauncherDiagnostics();
  await checkLauncherSignals();
  const platformDescriptor = Object.getOwnPropertyDescriptor(process, "platform");
  try {
    Object.defineProperty(process, "platform", { value: "win32", configurable: true });
    await assert.rejects(install(), /does not currently support Windows/);
  } finally {
    Object.defineProperty(process, "platform", platformDescriptor);
  }

  const registry = http.createServer((_, response) => {
    response.setHeader("content-type", "application/json");
    response.end(JSON.stringify({ name: "spynel", version: "0.3.0", workerProtocol: 14 }));
  });
  const registryURL = await listen(registry);
  try {
    const result = await checkForUpdate({ current: "0.2.0", registryURL, timeoutMs: 1_000 });
    assert.deepStrictEqual(result, { current: "0.2.0", latest: "0.3.0", available: true, workerProtocol: 14 });
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
  // Busy event loops may skip countdown values; require a live update.
  assert.match(timedOut.rendered, /skipping in [1-9]…/);
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
  assert.match(partialRendered, /skipping in [1-9]…/);
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
