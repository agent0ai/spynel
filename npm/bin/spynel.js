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
  shouldCheckAtStartup
} = require("../update");
const { current } = require("../platform");

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
    SPYNEL_NPM_LAUNCHER: __filename,
    SPYNEL_NPM_NODE: process.execPath,
    SPYNEL_NPM_UPDATE_STATE: path.join(os.tmpdir(), `spynel-update-${process.pid}-${crypto.randomBytes(8).toString("hex")}.json`)
  };
  delete environment.SPYNEL_NPM_PERIODIC_UPDATE_CHECKS;
  delete environment.SPYNEL_NPM_UPDATE_CHECKED_AT;
  delete environment.SPYNEL_NPM_UPDATE_LATEST;
  if (periodicChecks) environment.SPYNEL_NPM_PERIODIC_UPDATE_CHECKS = "1";
  if (checkedAt) environment.SPYNEL_NPM_UPDATE_CHECKED_AT = checkedAt;
  if (update) environment.SPYNEL_NPM_UPDATE_LATEST = update.latest;
  return environment;
}

function coordinateInstances(command, environment) {
  const result = childProcess.spawnSync(path.join(packageRoot, "npm", "vendor", "spynel"), [command], {
    stdio: ["ignore", "ignore", "inherit"], env: environment, timeout: 40_000
  });
  if (result.error || result.status !== 0) {
    throw result.error || new Error("Spynel instance coordination failed; review the error above");
  }
}

async function main() {
  current();
  const periodicChecks = shouldCheckAtStartup(args);
  if (periodicChecks) {
    startupUpdateCheckedAt = new Date().toISOString();
    try {
      const update = await checkForUpdate();
      startupUpdate = update;
      if (update.available && await promptForStartupUpdate(update, { packageRoot })) {
        try {
          const environment = createLaunchEnvironment(process.env, false);
          coordinateInstances("check-restartable", environment);
          runNPMUpdate({ packageRoot, expectedVersion: update.latest });
          coordinateInstances("restart-instances", environment);
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
    const binary = path.join(__dirname, "..", "vendor", "spynel");
    const result = await new Promise(resolve => {
      const child = childProcess.spawn(binary, args, { stdio: "inherit", env: environment });
      const signals = ["SIGINT", "SIGTERM", "SIGHUP"].map(signal => [signal, () => child.kill(signal)]);
      const finish = result => {
        for (const [signal, forward] of signals) process.removeListener(signal, forward);
        resolve(result);
      };
      for (const [signal, forward] of signals) process.on(signal, forward);
      child.once("error", error => finish({ error }));
      child.once("close", (status, signal) => finish({ status, signal }));
    });
    if (result.error) {
      fs.rmSync(environment.SPYNEL_NPM_UPDATE_STATE, { force: true });
      console.error(`Unable to run Spynel: ${result.error.message}`);
      return 1;
    }
    if (result.status !== UPDATE_EXIT_CODE) {
      fs.rmSync(environment.SPYNEL_NPM_UPDATE_STATE, { force: true });
      return result.status === null ? 1 : result.status;
    }
    let request = {};
    try {
      request = JSON.parse(fs.readFileSync(environment.SPYNEL_NPM_UPDATE_STATE, "utf8"));
      if (Array.isArray(request.args)) args = request.args;
    } catch (_) {
      // Older binaries do not publish restart arguments; reuse this launch.
    }
    fs.rmSync(environment.SPYNEL_NPM_UPDATE_STATE, { force: true });
    try {
      coordinateInstances("check-restartable", environment);
      if (request.install !== false) runNPMUpdate({ packageRoot, expectedVersion: request.version });
      coordinateInstances("restart-instances", environment);
    } catch (error) {
      console.error(`Unable to update Spynel: ${error.message}`);
      if (args.length === 0 || args[0] === "serve") continue;
      return 1;
    }
  }
}

if (require.main === module) {
  main().then(code => process.exit(code), error => {
    console.error(`Unable to run Spynel: ${error.message}`);
    process.exit(1);
  });
}

module.exports = { createLaunchEnvironment };
