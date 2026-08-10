"use strict";

const mapping = {
  "linux-x64": ["linux", "amd64", "tar.gz"],
  "linux-arm64": ["linux", "arm64", "tar.gz"],
  "darwin-x64": ["darwin", "amd64", "tar.gz"],
  "darwin-arm64": ["darwin", "arm64", "tar.gz"]
};

function resolve(platform, arch) {
  const key = `${platform}-${arch}`;
  if (platform === "win32") {
    throw new Error("Spynel does not currently support Windows; use Linux or macOS");
  }
  const target = mapping[key];
  if (!target) throw new Error(`Spynel does not publish a binary for ${key}`);
  return { os: target[0], arch: target[1], ext: target[2] };
}

function current() {
  return resolve(process.platform, process.arch);
}

module.exports = { current, resolve };
