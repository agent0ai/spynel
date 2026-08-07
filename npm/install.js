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
const target = current();
const archive = `spynel_${version}_${target.os}_${target.arch}.${target.ext}`;
const base = process.env.SPYNEL_DOWNLOAD_BASE || `https://github.com/frdel/spynel/releases/download/v${version}`;
const vendor = path.join(__dirname, "vendor");
const destination = path.join(vendor, process.platform === "win32" ? "spynel.exe" : "spynel");
const marker = path.join(vendor, ".installed.json");

function download(url, file, redirects = 0) {
  return new Promise((resolve, reject) => {
    const transport = url.startsWith("https:") ? https : http;
    transport.get(url, response => {
      if (response.statusCode >= 300 && response.statusCode < 400 && response.headers.location) {
        response.resume();
        if (redirects >= 5) return reject(new Error("too many redirects"));
        return resolve(download(response.headers.location, file, redirects + 1));
      }
      if (response.statusCode !== 200) return reject(new Error(`download failed (${response.statusCode})`));
      const output = fs.createWriteStream(file);
      response.pipe(output);
      output.on("finish", () => output.close(resolve));
      output.on("error", reject);
    }).on("error", reject);
  });
}

async function install() {
  if (fs.existsSync(destination) && fs.existsSync(marker)) {
    try {
      const installed = JSON.parse(fs.readFileSync(marker, "utf8"));
      if (installed.version === version && installed.os === target.os && installed.arch === target.arch) return;
    } catch (_) {}
  }
  fs.mkdirSync(vendor, { recursive: true });
  fs.rmSync(destination, { force: true });
  fs.rmSync(marker, { force: true });
  const temp = path.join(vendor, archive);
  await download(`${base}/${archive}`, temp);
  const checksums = path.join(vendor, "checksums.txt");
  await download(`${base}/checksums.txt`, checksums);
  const expectedLine = fs.readFileSync(checksums, "utf8").split(/\r?\n/).find(line => line.trim().endsWith(`  ${archive}`));
  if (!expectedLine) throw new Error(`release checksum is missing for ${archive}`);
  const expected = expectedLine.trim().split(/\s+/)[0].toLowerCase();
  const actual = crypto.createHash("sha256").update(fs.readFileSync(temp)).digest("hex");
  fs.rmSync(checksums);
  if (actual !== expected) throw new Error(`checksum mismatch for ${archive}`);
  if (target.ext === "zip") {
    childProcess.execFileSync("powershell", ["-NoProfile", "-Command", `Expand-Archive -Force '${temp.replaceAll("'", "''")}' '${vendor.replaceAll("'", "''")}'`]);
  } else {
    childProcess.execFileSync("tar", ["-xzf", temp, "-C", vendor]);
  }
  fs.rmSync(temp);
  if (process.platform !== "win32") fs.chmodSync(destination, 0o755);
  fs.writeFileSync(marker, JSON.stringify({ version, os: target.os, arch: target.arch }) + "\n", { mode: 0o600 });
}

install().catch(error => {
  console.error(`Unable to install Spynel: ${error.message}`);
  process.exitCode = 1;
});
