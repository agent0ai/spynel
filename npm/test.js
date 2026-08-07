"use strict";

const assert = require("assert");
const { resolve } = require("./platform");

assert.deepStrictEqual(resolve("linux", "x64"), { os: "linux", arch: "amd64", ext: "tar.gz" });
assert.deepStrictEqual(resolve("linux", "arm64"), { os: "linux", arch: "arm64", ext: "tar.gz" });
assert.deepStrictEqual(resolve("darwin", "x64"), { os: "darwin", arch: "amd64", ext: "tar.gz" });
assert.deepStrictEqual(resolve("darwin", "arm64"), { os: "darwin", arch: "arm64", ext: "tar.gz" });
assert.deepStrictEqual(resolve("win32", "x64"), { os: "windows", arch: "amd64", ext: "zip" });
assert.deepStrictEqual(resolve("win32", "arm64"), { os: "windows", arch: "arm64", ext: "zip" });
assert.throws(() => resolve("freebsd", "x64"), /does not publish/);

console.log("npm platform mapping matches GoReleaser targets");

