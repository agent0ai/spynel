"use strict";

const childProcess = require("child_process");
const fs = require("fs");
const http = require("http");
const https = require("https");
const path = require("path");
const readline = require("readline");
const pkg = require("../package.json");

const DEFAULT_TIMEOUT_MS = 10_000;
const STARTUP_PROMPT_TIMEOUT_MS = 10_000;
const DEFAULT_REGISTRY_URL = "https://registry.npmjs.org/spynel/latest";
const MAX_RESPONSE_BYTES = 64 * 1024;

function parseVersion(value) {
  const match = String(value).trim().replace(/^v/, "").match(/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$/);
  if (!match) return null;
  return {
    numbers: [Number(match[1]), Number(match[2]), Number(match[3])],
    prerelease: match[4] ? match[4].split(".") : []
  };
}

function compareVersions(left, right) {
  const a = parseVersion(left);
  const b = parseVersion(right);
  if (!a || !b) throw new Error(`invalid semantic version: ${!a ? left : right}`);
  for (let index = 0; index < 3; index += 1) {
    if (a.numbers[index] !== b.numbers[index]) return a.numbers[index] < b.numbers[index] ? -1 : 1;
  }
  if (a.prerelease.length === 0 || b.prerelease.length === 0) {
    if (a.prerelease.length === b.prerelease.length) return 0;
    return a.prerelease.length === 0 ? 1 : -1;
  }
  const limit = Math.min(a.prerelease.length, b.prerelease.length);
  for (let index = 0; index < limit; index += 1) {
    const aValue = a.prerelease[index];
    const bValue = b.prerelease[index];
    if (aValue === bValue) continue;
    const aNumeric = /^(0|[1-9]\d*)$/.test(aValue);
    const bNumeric = /^(0|[1-9]\d*)$/.test(bValue);
    if (aNumeric && bNumeric) return Number(aValue) < Number(bValue) ? -1 : 1;
    if (aNumeric !== bNumeric) return aNumeric ? -1 : 1;
    return aValue < bValue ? -1 : 1;
  }
  if (a.prerelease.length === b.prerelease.length) return 0;
  return a.prerelease.length < b.prerelease.length ? -1 : 1;
}

function requestJSON(url, timeoutMs, redirects = 0, deadline = Date.now() + timeoutMs) {
  return new Promise((resolve, reject) => {
    const parsed = new URL(url);
    const transport = parsed.protocol === "https:" ? https : http;
    const remaining = deadline - Date.now();
    if (remaining <= 0) return reject(new Error(`npm update check timed out after ${timeoutMs}ms`));
    const request = transport.get(parsed, {
      headers: {
        Accept: "application/json",
        "User-Agent": `spynel/${pkg.version}`
      }
    }, response => {
      if (response.statusCode >= 300 && response.statusCode < 400 && response.headers.location) {
        response.resume();
        if (redirects >= 5) {
          clearTimeout(timer);
          return reject(new Error("npm registry returned too many redirects"));
        }
        clearTimeout(timer);
        return resolve(requestJSON(new URL(response.headers.location, parsed).toString(), timeoutMs, redirects + 1, deadline));
      }
      if (response.statusCode !== 200) {
        response.resume();
        clearTimeout(timer);
        return reject(new Error(`npm registry returned ${response.statusCode}`));
      }
      const chunks = [];
      let size = 0;
      response.on("data", chunk => {
        size += chunk.length;
        if (size > MAX_RESPONSE_BYTES) {
          request.destroy(new Error("npm registry response is too large"));
          return;
        }
        chunks.push(chunk);
      });
      response.on("end", () => {
        clearTimeout(timer);
        try {
          resolve(JSON.parse(Buffer.concat(chunks).toString("utf8")));
        } catch (error) {
          reject(new Error(`invalid npm registry response: ${error.message}`));
        }
      });
    });
    const timer = setTimeout(() => request.destroy(new Error(`npm update check timed out after ${timeoutMs}ms`)), remaining);
    request.on("error", error => {
      clearTimeout(timer);
      reject(error);
    });
  });
}

async function checkForUpdate(options = {}) {
  const current = options.current || pkg.version;
  const registryURL = options.registryURL || process.env.SPYNEL_NPM_REGISTRY_URL || DEFAULT_REGISTRY_URL;
  const timeoutMs = options.timeoutMs || DEFAULT_TIMEOUT_MS;
  const metadata = await requestJSON(registryURL, timeoutMs);
  if (!metadata || typeof metadata.version !== "string" || !parseVersion(metadata.version)) {
    throw new Error("npm registry did not return a semantic Spynel version");
  }
  return {
    current,
    latest: metadata.version,
    available: compareVersions(metadata.version, current) > 0
  };
}

function shouldCheckAtStartup(args, input = process.stdin, output = process.stdout, environment = process.env) {
  const interactiveStart = args.length === 0 || args[0] === "init" || (args[0] === "serve" && args.includes("--tui"));
  return interactiveStart && !args.includes("--automatic-startup") &&
    environment.SPYNEL_SKIP_UPDATE_CHECK !== "1" &&
    Boolean(input.isTTY && output.isTTY);
}

function samePath(left, right) {
  try {
    return fs.realpathSync(left) === fs.realpathSync(right);
  } catch (_) {
    return path.resolve(left) === path.resolve(right);
  }
}

function npmInvocation(packageRoot = path.resolve(__dirname, "..")) {
  const npm = process.platform === "win32" ? "npm.cmd" : "npm";
  const rootResult = childProcess.spawnSync(npm, ["root", "--global"], { encoding: "utf8", windowsHide: true, timeout: DEFAULT_TIMEOUT_MS });
  const globalRoot = rootResult.status === 0 ? rootResult.stdout.trim() : "";
  if (globalRoot && samePath(packageRoot, path.join(globalRoot, pkg.name))) {
    return { command: npm, args: ["update", "--global", pkg.name], display: `npm update --global ${pkg.name}` };
  }
  const nodeModules = path.dirname(packageRoot);
  const prefix = path.basename(nodeModules) === "node_modules" ? path.dirname(nodeModules) : process.cwd();
  return { command: npm, args: ["update", pkg.name, "--prefix", prefix], display: `npm update ${pkg.name}` };
}

function runNPMUpdate(options = {}) {
  const packageRoot = options.packageRoot || path.resolve(__dirname, "..");
  const before = JSON.parse(fs.readFileSync(path.join(packageRoot, "package.json"), "utf8")).version;
  const invocation = npmInvocation(packageRoot);
  const result = childProcess.spawnSync(invocation.command, invocation.args, {
    cwd: packageRoot,
    stdio: options.stdio || "inherit",
    windowsHide: false
  });
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`${invocation.display} exited with status ${result.status}`);
  const after = JSON.parse(fs.readFileSync(path.join(packageRoot, "package.json"), "utf8")).version;
  if (compareVersions(after, before) <= 0) {
    throw new Error(`${invocation.display} completed but Spynel remained at ${after}; check the project's dependency range or update it manually`);
  }
  return { ...invocation, before, after };
}

async function promptForStartupUpdate(result, options = {}) {
  const input = options.input || process.stdin;
  const output = options.output || process.stdout;
  if (!input.isTTY || !output.isTTY) return false;

  const invocation = npmInvocation(options.packageRoot);
  const environment = options.environment || process.env;
  const timeoutMs = options.timeoutMs || STARTUP_PROMPT_TIMEOUT_MS;
  const tickMs = options.tickMs || 1_000;
  const countdownUnitMs = options.countdownUnitMs || 1_000;
  const styled = environment.NO_COLOR === undefined && environment.TERM !== "dumb";
  const cursorControl = options.cursorControl !== false && environment.TERM !== "dumb";
  const bold = value => styled ? `\x1b[1m${value}\x1b[22m` : value;
  const muted = value => styled ? `\x1b[2m${value}\x1b[22m` : value;
  const terminal = readline.createInterface({ input, output, terminal: true });
  const deadline = Date.now() + timeoutMs;
  let lastRemaining = Math.ceil(timeoutMs / countdownUnitMs);
  let firstPrompt = true;
  let settled = false;

  output.write(`\n⬆️  New version ${result.latest} available — current is ${result.current}\n`);
  const renderPrompt = (remaining, ending = "") => {
    const suffix = ending || `skipping in ${remaining}…`;
    const line = `${bold("Update now")}? [Y]es / [N]o  ${muted(suffix)}`;
    if (cursorControl) {
      terminal.setPrompt(line);
      terminal.prompt(!firstPrompt);
    } else {
      if (!firstPrompt) output.write("\n");
      output.write(line);
    }
    firstPrompt = false;
  };
  renderPrompt(lastRemaining);

  return new Promise(resolve => {
    let interval;
    let timeout;
    const finish = (update, ending) => {
      if (settled) return;
      settled = true;
      clearInterval(interval);
      clearTimeout(timeout);
      terminal.close();
      if (cursorControl) {
        readline.cursorTo(output, 0);
        readline.clearLine(output, 0);
        output.write(`${bold("Update now")}? [Y]es / [N]o  ${muted(ending)}`);
      } else {
        renderPrompt(0, ending);
      }
      output.write("\n");
      resolve(update);
    };
    terminal.once("line", answer => {
      const normalized = answer.trim().toLowerCase();
      finish(normalized === "y" || normalized === "yes", normalized === "y" || normalized === "yes" ? `running ${invocation.display}…` : "skipping.");
    });
    terminal.once("SIGINT", () => finish(false, "skipping."));
    terminal.once("close", () => finish(false, "skipping."));
    interval = setInterval(() => {
      const remaining = Math.max(0, Math.ceil((deadline - Date.now()) / countdownUnitMs));
      if (remaining !== lastRemaining && remaining > 0) {
        lastRemaining = remaining;
        renderPrompt(remaining);
      }
    }, tickMs);
    timeout = setTimeout(() => finish(false, "timed out; skipping."), timeoutMs);
  });
}

module.exports = {
  DEFAULT_TIMEOUT_MS,
  STARTUP_PROMPT_TIMEOUT_MS,
  checkForUpdate,
  compareVersions,
  npmInvocation,
  parseVersion,
  promptForStartupUpdate,
  runNPMUpdate,
  shouldCheckAtStartup
};
