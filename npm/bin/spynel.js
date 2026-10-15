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
let args = process.argv.slice(2);
let startupUpdate;
let startupUpdateCheckedAt;

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

async function coordinateInstances(command, environment, signal) {
  const result = await runCommand(path.join(packageRoot, "npm", "vendor", "spynel"), [command], {
    stdio: ["ignore", "ignore", "inherit"], env: environment, timeout: 70_000, signal
  });
  if (result.error || result.status !== 0) {
    throw result.error || new Error("Spynel instance coordination failed; review the error above");
  }
}

async function main() {
  current();
  const controller = new AbortController();
  let native;
  const signals = ["SIGINT", "SIGTERM", "SIGHUP"].map(signal => [signal, () => {
    controller.abort();
    if (native) native.kill(signal);
  }]);
  for (const [signal, stop] of signals) process.on(signal, stop);
  try {
    const periodicChecks = shouldCheckAtStartup(args);
    if (periodicChecks) {
      startupUpdateCheckedAt = new Date().toISOString();
      try {
        const update = await checkForUpdate();
        startupUpdate = update;
        if (update.available && await promptForStartupUpdate(update, { packageRoot, signal: controller.signal })) {
          try {
            const environment = createLaunchEnvironment(process.env, false);
            await coordinateInstances("check-restartable", environment, controller.signal);
            await runNPMUpdate({ packageRoot, expectedVersion: update.latest, signal: controller.signal });
            await coordinateInstances("restart-instances", environment, controller.signal);
          } catch (error) {
            console.error(`Unable to update Spynel: ${error.message}. Starting the installed version.`);
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
    for (;;) {
      if (controller.signal.aborted) return 0;
      const binary = path.join(__dirname, "..", "vendor", "spynel");
      const result = await new Promise(resolve => {
        let metadata = "";
        const child = childProcess.spawn(binary, args, { stdio: ["inherit", "inherit", "inherit", "pipe"], env: { ...environment, SPYNEL_DIAGNOSTICS_FD: "3", SPYNEL_DIAGNOSTICS_RUN: crypto.randomBytes(8).toString("hex") } });
        child.stdio[3].on("data", chunk => { if (metadata.length <= 8192) metadata = (metadata + chunk.toString("utf8")).slice(0, 8193); });
        native = child;
        let finished = false;
        const finish = async result => {
          if (finished) return;
          finished = true;
          native = undefined;
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
            typeof request.install !== "boolean" || typeof request.version !== "string" || !parseVersion(request.version)) {
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
        await coordinateInstances("check-restartable", environment, controller.signal);
        if (request.install !== false) await runNPMUpdate({ packageRoot, expectedVersion: request.version, signal: controller.signal });
        await coordinateInstances("restart-instances", environment, controller.signal);
      } catch (error) {
        console.error(`Unable to update Spynel: ${error.message}`);
        if (controller.signal.aborted) return 0;
        if (args.length === 0 || args[0] === "serve") continue;
        return 1;
      }
    }
  } finally {
    for (const [signal, stop] of signals) process.removeListener(signal, stop);
  }
}

if (require.main === module) {
  main().then(exitResult, error => {
    console.error(`Unable to run Spynel: ${error.message}`);
    process.exit(1);
  });
}

module.exports = { createLaunchEnvironment };
