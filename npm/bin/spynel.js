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

async function main() {
  current();
  if (shouldCheckAtStartup(args)) {
    try {
      const update = await checkForUpdate();
      if (update.available && await promptForStartupUpdate(update, { packageRoot })) {
        try {
          runNPMUpdate({ packageRoot });
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

  const environment = {
    ...process.env,
    SPYNEL_NPM_PACKAGE_ROOT: packageRoot,
    SPYNEL_NPM_LAUNCHER_MANAGED: "1",
    SPYNEL_NPM_LAUNCHER: __filename,
    SPYNEL_NPM_NODE: process.execPath,
    SPYNEL_NPM_UPDATE_STATE: path.join(os.tmpdir(), `spynel-update-${process.pid}-${crypto.randomBytes(8).toString("hex")}.json`)
  };
  for (;;) {
    const binary = path.join(__dirname, "..", "vendor", "spynel");
    const result = childProcess.spawnSync(binary, args, { stdio: "inherit", env: environment });
    if (result.error) {
      fs.rmSync(environment.SPYNEL_NPM_UPDATE_STATE, { force: true });
      console.error(`Unable to run Spynel: ${result.error.message}`);
      return 1;
    }
    if (result.status !== UPDATE_EXIT_CODE) {
      fs.rmSync(environment.SPYNEL_NPM_UPDATE_STATE, { force: true });
      return result.status === null ? 1 : result.status;
    }
    try {
      const request = JSON.parse(fs.readFileSync(environment.SPYNEL_NPM_UPDATE_STATE, "utf8"));
      if (Array.isArray(request.args)) args = request.args;
    } catch (_) {
      // Older binaries do not publish restart arguments; reuse this launch.
    }
    fs.rmSync(environment.SPYNEL_NPM_UPDATE_STATE, { force: true });
    try {
      runNPMUpdate({ packageRoot });
    } catch (error) {
      console.error(`Unable to update Spynel: ${error.message}`);
      if (args.length === 0 || args[0] === "serve") continue;
      return 1;
    }
  }
}

main().then(code => process.exit(code), error => {
  console.error(`Unable to run Spynel: ${error.message}`);
  process.exit(1);
});
