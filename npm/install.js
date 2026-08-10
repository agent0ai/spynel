"use strict";

const fs = require("fs");
const path = require("path");
const https = require("https");
const http = require("http");
const childProcess = require("child_process");
const crypto = require("crypto");
const { current } = require("./platform");
const pkg = require("../package.json");

const version = pkg.version;
const vendor = path.join(__dirname, "vendor");
const marker = path.join(vendor, ".installed.json");
const MAX_ARCHIVE_BYTES = 512 * 1024 * 1024;
const MAX_CHECKSUM_BYTES = 1024 * 1024;
const MAX_ARCHIVE_ENTRIES = 4096;
const MAX_ENTRY_PATH_BYTES = 1024;
const MAX_EXPANDED_BYTES = 2 * 1024 * 1024 * 1024;

function download(url, file, maxBytes, redirects = 0, secureRequired = new URL(url).protocol === "https:") {
  return new Promise((resolve, reject) => {
    const parsed = new URL(url);
    if (parsed.protocol !== "https:" && parsed.protocol !== "http:") {
      return reject(new Error(`unsupported download protocol: ${parsed.protocol}`));
    }
    if (secureRequired && parsed.protocol !== "https:") {
      return reject(new Error("release download refused an HTTPS-to-HTTP redirect"));
    }
    const transport = parsed.protocol === "https:" ? https : http;
    const request = transport.get(parsed, response => {
      if (response.statusCode >= 300 && response.statusCode < 400 && response.headers.location) {
        response.resume();
        if (redirects >= 5) return reject(new Error("too many redirects"));
        return resolve(download(new URL(response.headers.location, parsed).toString(), file, maxBytes, redirects + 1, secureRequired));
      }
      if (response.statusCode !== 200) {
        response.resume();
        return reject(new Error(`download failed (${response.statusCode})`));
      }
      const declared = Number(response.headers["content-length"]);
      if (Number.isFinite(declared) && declared > maxBytes) {
        response.resume();
        return reject(new Error(`release download exceeds the ${maxBytes} byte limit`));
      }
      let received = 0;
      const output = fs.createWriteStream(file, { flags: "wx", mode: 0o600 });
      response.on("data", chunk => {
        received += chunk.length;
        if (received > maxBytes) request.destroy(new Error(`release download exceeds the ${maxBytes} byte limit`));
      });
      response.pipe(output);
      output.on("finish", () => output.close(resolve));
      output.on("error", reject);
      response.on("error", reject);
    });
    request.setTimeout(120_000, function () {
      this.destroy(new Error("release download timed out"));
    });
    request.on("error", reject);
  });
}

function sha256File(file) {
  return new Promise((resolve, reject) => {
    const hash = crypto.createHash("sha256");
    const input = fs.createReadStream(file);
    input.on("data", chunk => hash.update(chunk));
    input.on("end", () => resolve(hash.digest("hex")));
    input.on("error", reject);
  });
}

function validateArchiveEntries(entries) {
  if (!Array.isArray(entries) || entries.length === 0) throw new Error("release archive is empty");
  if (entries.length > MAX_ARCHIVE_ENTRIES) throw new Error("release archive contains too many entries");
  for (const original of entries) {
    if (typeof original !== "string" || Buffer.byteLength(original) > MAX_ENTRY_PATH_BYTES) {
      throw new Error("release archive contains an invalid entry path");
    }
    if (/[\0-\x1f\x7f]/.test(original)) throw new Error("release archive entry contains control characters");
    const entry = original.replaceAll("\\", "/").replace(/\/+$/, "");
    if (entry === "" || entry === ".") continue;
    if (entry.startsWith("/") || entry.startsWith("//") || /^[A-Za-z]:/.test(entry)) {
      throw new Error(`release archive contains an absolute path: ${original}`);
    }
    if (entry.split("/").some(segment => segment === "..")) {
      throw new Error(`release archive escapes its staging directory: ${original}`);
    }
  }
}

function listArchiveEntries(file) {
  const output = childProcess.execFileSync("tar", ["-tzf", file], { encoding: "utf8", maxBuffer: 4 * 1024 * 1024 });
  const entries = output.split(/\r?\n/).filter(Boolean);
  validateArchiveEntries(entries);
  return entries;
}

function validateExtractedTree(root) {
  let entries = 0;
  let bytes = 0;
  const visit = directory => {
    for (const name of fs.readdirSync(directory)) {
      entries += 1;
      if (entries > MAX_ARCHIVE_ENTRIES) throw new Error("release archive contains too many extracted entries");
      const item = path.join(directory, name);
      const info = fs.lstatSync(item);
      if (info.isSymbolicLink()) throw new Error(`release archive contains a symbolic link: ${name}`);
      if (info.isDirectory()) {
        visit(item);
      } else if (info.isFile()) {
        bytes += info.size;
        if (bytes > MAX_EXPANDED_BYTES) throw new Error("release archive expands beyond its byte limit");
      } else {
        throw new Error(`release archive contains an unsupported entry: ${name}`);
      }
    }
  };
  visit(root);
}

async function install() {
  const target = current();
  const archive = `spynel_${version}_${target.os}_${target.arch}.${target.ext}`;
  const base = process.env.SPYNEL_DOWNLOAD_BASE || `https://github.com/agent0ai/spynel/releases/download/v${version}`;
  const destination = path.join(vendor, "spynel");
  if (fs.existsSync(destination) && fs.existsSync(marker)) {
    try {
      const installed = JSON.parse(fs.readFileSync(marker, "utf8"));
      if (installed.version === version && installed.os === target.os && installed.arch === target.arch) return;
    } catch (_) {}
  }
  const staging = fs.mkdtempSync(path.join(__dirname, ".install-"));
  try {
    const temp = path.join(staging, archive);
    await download(`${base}/${archive}`, temp, MAX_ARCHIVE_BYTES);
    const checksums = path.join(staging, "checksums.txt");
    await download(`${base}/checksums.txt`, checksums, MAX_CHECKSUM_BYTES);
    const expectedLine = fs.readFileSync(checksums, "utf8").split(/\r?\n/).find(line => line.trim().endsWith(`  ${archive}`));
    if (!expectedLine) throw new Error(`release checksum is missing for ${archive}`);
    const expected = expectedLine.trim().split(/\s+/)[0].toLowerCase();
    if (!/^[a-f0-9]{64}$/.test(expected)) throw new Error(`release checksum is invalid for ${archive}`);
    const actual = await sha256File(temp);
    if (actual !== expected) throw new Error(`checksum mismatch for ${archive}`);
    fs.rmSync(checksums);
    listArchiveEntries(temp);
    childProcess.execFileSync("tar", ["-xzf", temp, "-C", staging, "--no-same-owner", "--no-same-permissions"]);
    fs.rmSync(temp);
    validateExtractedTree(staging);
    const stagedBinary = path.join(staging, "spynel");
    if (!fs.lstatSync(stagedBinary).isFile()) throw new Error(`release archive does not contain ${path.basename(stagedBinary)}`);
    fs.chmodSync(stagedBinary, 0o755);
    fs.writeFileSync(path.join(staging, ".installed.json"), JSON.stringify({ version, os: target.os, arch: target.arch }) + "\n", { mode: 0o600 });
    const backup = path.join(__dirname, `.vendor-backup-${process.pid}`);
    fs.rmSync(backup, { recursive: true, force: true, maxRetries: 3, retryDelay: 100 });
    if (fs.existsSync(vendor)) fs.renameSync(vendor, backup);
    try {
      fs.renameSync(staging, vendor);
    } catch (error) {
      if (fs.existsSync(backup)) fs.renameSync(backup, vendor);
      throw error;
    }
    try {
      fs.rmSync(backup, { recursive: true, force: true, maxRetries: 3, retryDelay: 100 });
    } catch (error) {
      console.warn(`Spynel was installed but its previous vendor backup could not be removed: ${error.message}`);
    }
  } catch (error) {
    fs.rmSync(staging, { recursive: true, force: true });
    throw error;
  }
}

if (require.main === module) {
  install().catch(error => {
    console.error(`Unable to install Spynel: ${error.message}`);
    process.exitCode = 1;
  });
}

module.exports = { install, validateArchiveEntries, validateExtractedTree };
