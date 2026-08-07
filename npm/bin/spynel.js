#!/usr/bin/env node
"use strict";

const path = require("path");
const childProcess = require("child_process");
const binary = path.join(__dirname, "..", "vendor", process.platform === "win32" ? "spynel.exe" : "spynel");
const result = childProcess.spawnSync(binary, process.argv.slice(2), { stdio: "inherit" });
if (result.error) {
  console.error(`Unable to run Spynel: ${result.error.message}`);
  process.exit(1);
}
process.exit(result.status === null ? 1 : result.status);

