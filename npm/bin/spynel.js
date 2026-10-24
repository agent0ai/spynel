#!/usr/bin/env node
"use strict";

const path = require("path");
const os = require("os");
const crypto = require("crypto");
const fs = require("fs");
const childProcess = require("child_process");
const {
  checkForUpdate,
  promptForStartupUpdate,
  runNPMUpdate,
  runCommand,
  parseVersion,
  shouldCheckAtStartup
} = require("../update");
const { current } = require("../platform");

async function retainExit(binary, environment, metadata, result) {
  if (!metadata) return;
  let identity;
  try {
    identity = JSON.parse(metadata);
    if (identity.pid !== result.pid) throw new Error("invalid native identity");
  } catch (_) {
    console.error("spynel: native exit diagnostics unavailable; retain terminal stderr and host exit records");
    return;
  }
  const recorded = await runCommand(binary, ["__diagnostics", "exit"], {
    env: environment, stdio: ["pipe", "ignore", "inherit"], timeout: 8000,
    input: JSON.stringify({ ...identity, status: result.status, signal: result.signal || "" })
  });
  if (recorded.error || recorded.status !== 0) {
    console.error("spynel: native exit diagnostics unavailable; retain terminal stderr and host exit records");
  }
}

function exitResult(result) {
  if (typeof result === "number") {
    process.exitCode = result;
  } else {
    // Forward the child's actual signal after pending terminal output drains.
    process.stderr.write("", () => process.kill(process.pid, result.signal));
  }
}

const UPDATE_EXIT_CODE = 75;
const packageRoot = path.resolve(__dirname, "..", "..");

function createLaunchEnvironment(parentEnvironment = process.env, periodicChecks, checkedAt, update) {
  const environment = {
    ...parentEnvironment,
    SPYNEL_NPM_PACKAGE_ROOT: packageRoot,
    SPYNEL_NPM_LAUNCHER_MANAGED: "1",
    SPYNEL_NPM_COORDINATED_UPDATES: "2",
    SPYNEL_NPM_LAUNCHER: __filename,
    SPYNEL_NPM_NODE: process.execPath,
    SPYNEL_NPM_UPDATE_STATE: path.join(os.tmpdir(), `spynel-update-${process.pid}-${crypto.randomBytes(8).toString("hex")}.json`)
  };
  delete environment.SPYNEL_NPM_PERIODIC_UPDATE_CHECKS;
  delete environment.SPYNEL_NPM_UPDATE_CHECKED_AT;
  delete environment.SPYNEL_NPM_UPDATE_LATEST;
  delete environment.SPYNEL_DIAGNOSTICS_FD;
  delete environment.SPYNEL_DIAGNOSTICS_RUN;
  if (periodicChecks) environment.SPYNEL_NPM_PERIODIC_UPDATE_CHECKS = "1";
  if (checkedAt) environment.SPYNEL_NPM_UPDATE_CHECKED_AT = checkedAt;
  if (update) environment.SPYNEL_NPM_UPDATE_LATEST = update.latest;
  return environment;
}

async function coordinateInstances(command, environment, signal, workerProtocol) {
  const args = [command];
  if (workerProtocol !== undefined) args.push("--worker-protocol", String(workerProtocol));
  const result = await runCommand(path.join(packageRoot, "npm", "vendor", "spynel"), args, {
    stdio: ["ignore", "ignore", "inherit"], env: environment, timeout: 70_000, signal
  });
  if (result.error || result.status !== 0) {
    throw result.error || new Error("Spynel instance coordination failed; review the error above");
  }
}

async function replacePackage(environment, signal, version, workerProtocol) {
  const operation = new AbortController();
  const abort = () => operation.abort();
  signal.addEventListener("abort", abort, { once: true });
  if (signal.aborted) abort();
  const child = childProcess.spawn(path.join(packageRoot, "npm", "vendor", "spynel"),
    ["hold-worker-update", "--worker-protocol", String(workerProtocol)],
    { env: environment, stdio: ["pipe", "pipe", "inherit"], signal: operation.signal });
  child.stdin.on("error", () => {});
  let observedError;
  child.on("error", error => { observedError = error; });
  let finishing = false;
  const closed = new Promise(resolve => child.once("close", status => { if (!finishing) abort(); resolve(status); }));
  let timer;
  try {
    await new Promise((resolve, reject) => {
      let output = "";
      timer = setTimeout(() => reject(new Error("worker update preparation timed out")), 70_000);
      child.stdout.on("data", chunk => {
        output = (output + chunk.toString("utf8")).slice(0, 7);
        if (output === "ready\n") resolve();
        else if (output.length >= 6) reject(new Error("invalid worker update readiness"));
      });
      closed.then(() => reject(observedError || new Error("worker update preparation failed")));
    });
    clearTimeout(timer);
    await runNPMUpdate({ packageRoot, expectedVersion: version, signal: operation.signal });
    finishing = true;
    timer = setTimeout(() => child.kill("SIGKILL"), 20_000);
    child.stdin.end("commit\n");
    if (await closed !== 0) throw observedError || new Error("updated worker shutdown was not confirmed");
  } finally {
    clearTimeout(timer);
    child.stdin.end();
    // EOF aborts a prepared update; native owns resumption and the OS lock.
    const kill = setTimeout(() => child.kill("SIGKILL"), 20_000);
    await closed;
    clearTimeout(kill);
    signal.removeEventListener("abort", abort);
  }
}

async function verifiedInstallation(version, environment, signal) {
  try {
    if (JSON.parse(fs.readFileSync(path.join(packageRoot, "package.json"), "utf8")).version !== version) return false;
    const result = await runCommand(path.join(packageRoot, "npm", "vendor", "spynel"), ["--version"], {
      env: environment, stdio: ["ignore", "pipe", "inherit"], timeout: 10_000, signal
    });
    return !result.error && result.status === 0 && result.stdout.trim() === `spynel ${version}`;
  } catch (_) { return false; }
}

async function main(handoff, lifetime) {
  current();
  let args = handoff ? handoff.args : process.argv.slice(2);
  let startupUpdate = handoff && handoff.startupUpdate;
  let startupUpdateCheckedAt = handoff && handoff.startupUpdateCheckedAt;
  const { controller } = lifetime;
  const launchVersion = JSON.parse(fs.readFileSync(path.join(packageRoot, "package.json"), "utf8")).version;
  const periodicChecks = handoff ? handoff.periodicChecks : shouldCheckAtStartup(args);
  const nextLauncher = () => ({ handoff: { args, periodicChecks, startupUpdate, startupUpdateCheckedAt } });
  if (!handoff && periodicChecks) {
    startupUpdateCheckedAt = new Date().toISOString();
    try {
      const update = await checkForUpdate();
      startupUpdate = update;
      if (update.available && await promptForStartupUpdate(update, { packageRoot, signal: controller.signal })) {
        try {
          const environment = createLaunchEnvironment(process.env, false);
          await coordinateInstances("check-restartable", environment, controller.signal, update.workerProtocol);
          await replacePackage(environment, controller.signal, update.latest, update.workerProtocol);
          return controller.signal.aborted ? 0 : nextLauncher();
        } catch (error) {
          console.error(`Unable to update Spynel: ${error.message}.`);
          if (controller.signal.aborted) return 0;
          if (!await verifiedInstallation(launchVersion, createLaunchEnvironment(process.env, false), controller.signal)) return 1;
          await coordinateInstances("restart-instances", createLaunchEnvironment(process.env, false), controller.signal);
          console.error(`Starting the unchanged Spynel ${launchVersion} installation.`);
        }
      }
    } catch (error) {
      // Startup must remain available when npm or the network is slow. The
      // explicit /update command reports errors in chat when the user asks.
      if (process.env.SPYNEL_DEBUG_UPDATE === "1") {
        console.warn(`Spynel update check skipped: ${error.message}`);
      }
    }
  }

  const environment = createLaunchEnvironment(process.env, periodicChecks, startupUpdateCheckedAt, startupUpdate);
  if (handoff) await coordinateInstances("restart-instances", environment, controller.signal);
  for (;;) {
    if (controller.signal.aborted) return 0;
    const binary = path.join(__dirname, "..", "vendor", "spynel");
    const result = await new Promise(resolve => {
      let metadata = "";
      const child = childProcess.spawn(binary, args, { stdio: ["inherit", "inherit", "inherit", "pipe"], env: { ...environment, SPYNEL_DIAGNOSTICS_FD: "3", SPYNEL_DIAGNOSTICS_RUN: crypto.randomBytes(8).toString("hex") } });
      child.stdio[3].on("data", chunk => { if (metadata.length <= 8192) metadata = (metadata + chunk.toString("utf8")).slice(0, 8193); });
      lifetime.native = child;
      let finished = false;
      const finish = async result => {
        if (finished) return;
        finished = true;
        lifetime.native = undefined;
        result.pid = child.pid;
        await retainExit(binary, environment, metadata, result);
        resolve(result);
      };

      child.once("error", error => finish({ error }));
      child.once("close", (status, signal) => finish({ status, signal }));
    });
    if (controller.signal.aborted) { fs.rmSync(environment.SPYNEL_NPM_UPDATE_STATE, { force: true }); return result.signal ? {signal: result.signal} : result.status || 0; }
    if (result.error) {
      fs.rmSync(environment.SPYNEL_NPM_UPDATE_STATE, { force: true });
      console.error(`Unable to run Spynel: ${result.error.message}`);
      return 1;
    }
    if (result.status !== UPDATE_EXIT_CODE) {
      fs.rmSync(environment.SPYNEL_NPM_UPDATE_STATE, { force: true });
      return result.signal ? { signal: result.signal } : result.status;
    }
    let request;
    try {
      const info = fs.lstatSync(environment.SPYNEL_NPM_UPDATE_STATE);
      if (!info.isFile() || info.size > 32768 || (info.mode & 0o077) !== 0) throw new Error("invalid update request");
      request = JSON.parse(fs.readFileSync(environment.SPYNEL_NPM_UPDATE_STATE, "utf8"));
      if (!Array.isArray(request.args) || request.args.length < 1 || request.args.length > 128 ||
          !request.args.every(arg => typeof arg === "string" && arg.length <= 4096) ||
          typeof request.install !== "boolean" || typeof request.version !== "string" || !parseVersion(request.version) ||
          !Number.isSafeInteger(request.worker_protocol) || request.worker_protocol < 0) {
        throw new Error("invalid update request");
      }
      args = request.args;
    } catch (_) {
      fs.rmSync(environment.SPYNEL_NPM_UPDATE_STATE, { force: true });
      console.error("spynel: native update request missing or invalid; stopped without changing the package");
      return UPDATE_EXIT_CODE;
    }
    fs.rmSync(environment.SPYNEL_NPM_UPDATE_STATE, { force: true });
    try {
      await coordinateInstances("check-restartable", environment, controller.signal, request.worker_protocol);
      if (request.install) {
        await replacePackage(environment, controller.signal, request.version, request.worker_protocol);
        return controller.signal.aborted ? 0 : nextLauncher();
      }
      await coordinateInstances("restart-instances", environment, controller.signal);
    } catch (error) {
      console.error(`Unable to update Spynel: ${error.message}`);
      if (controller.signal.aborted) return 0;
      if (request.install && await verifiedInstallation(request.version, environment, controller.signal)) return nextLauncher();
      if (args[0] !== "version" && await verifiedInstallation(launchVersion, environment, controller.signal)) {
        await coordinateInstances("restart-instances", environment, controller.signal);
        console.error(`Restarting the unchanged Spynel ${launchVersion} installation after the failed update.`);
        continue;
      }
      return 1;
    }
  }
}

async function runLauncher() {
  let launch = main;
  let handoff;
  const lifetime = { controller: new AbortController() };
  const signals = ["SIGINT", "SIGTERM", "SIGHUP"].map(signal => [signal, () => {
    lifetime.controller.abort();
    if (lifetime.native) lifetime.native.kill(signal);
  }]);
  for (const [signal, stop] of signals) process.on(signal, stop);
  try {
    for (;;) {
      if (lifetime.controller.signal.aborted) return 0;
      const result = await launch(handoff, lifetime);
      if (!result || !result.handoff) return result;
      handoff = result.handoff;
      // npm replaced these files; all old children/helpers have exited before reload.
      for (const filename of Object.keys(require.cache)) {
        if (filename.startsWith(packageRoot + path.sep)) delete require.cache[filename];
      }
      launch = require(__filename).main;
      if (typeof launch !== "function") throw new Error("installed npm launcher cannot accept the update handoff; relaunch Spynel with the installed command");
    }
  } catch (error) {
    if (lifetime.controller.signal.aborted) return 0;
    throw error;
  } finally {
    for (const [signal, stop] of signals) process.removeListener(signal, stop);
  }
}

if (require.main === module) {
  runLauncher().then(exitResult, error => {
    console.error(`Unable to run Spynel: ${error.message}`);
    process.exit(1);
  });
}

module.exports = { createLaunchEnvironment, main };
