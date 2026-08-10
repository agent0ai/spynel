"use strict";

const fs = require("fs");
const path = require("path");

const SEMVER = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*))*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/;
const REPOSITORY = "agent0ai/spynel";

function releaseMetadata(tag, prereleaseValue) {
  const match = typeof tag === "string" && tag.startsWith("v") ? SEMVER.exec(tag.slice(1)) : null;
  if (!match) {
    throw new Error(`release tag must be v followed by a semantic version, got ${JSON.stringify(tag)}`);
  }
  if (prereleaseValue !== "true" && prereleaseValue !== "false") {
    throw new Error("GitHub prerelease state must be true or false");
  }

  const version = tag.slice(1);
  const prerelease = Boolean(match[4]);
  if (prerelease !== (prereleaseValue === "true")) {
    throw new Error(`release tag ${tag} and GitHub prerelease state do not agree`);
  }
  return { tag, version, prerelease };
}

function relativeTargetURL(target, tag, image) {
  if (/^(?:[A-Za-z][A-Za-z\d+.-]*:|\/\/|\/|#)/.test(target)) {
    return target;
  }

  const suffixAt = target.search(/[?#]/);
  const file = suffixAt === -1 ? target : target.slice(0, suffixAt);
  const suffix = suffixAt === -1 ? "" : target.slice(suffixAt);
  const normalized = path.posix.normalize(file.replace(/^\.\//, ""));
  if (!normalized || normalized === "." || normalized === ".." || normalized.startsWith("../")) {
    return target;
  }

  const encodedTag = encodeURIComponent(tag);
  const encodedPath = normalized.split("/").map(encodeURIComponent).join("/");
  const base = image
    ? `https://raw.githubusercontent.com/${REPOSITORY}/${encodedTag}`
    : `https://github.com/${REPOSITORY}/blob/${encodedTag}`;
  return `${base}/${encodedPath}${suffix}`;
}

function rewriteReadme(markdown, tag) {
  const inlineLinks = /(!?\[[^\]\r\n]*\]\()\s*(<[^>\r\n]+>|[^)\s]+)([^)\r\n]*\))/g;
  let rewritten = markdown.replace(inlineLinks, (match, prefix, wrappedTarget, suffix) => {
    const angled = wrappedTarget.startsWith("<") && wrappedTarget.endsWith(">");
    const target = angled ? wrappedTarget.slice(1, -1) : wrappedTarget;
    const replacement = relativeTargetURL(target, tag, prefix.startsWith("!"));
    if (replacement === target) {
      return match;
    }
    return `${prefix}${angled ? `<${replacement}>` : replacement}${suffix}`;
  });

  const htmlImageSources = /(<img\b[^>]*?\ssrc=)(["'])([^"']+)(\2)/gi;
  rewritten = rewritten.replace(htmlImageSources, (match, prefix, quote, target) => {
    const replacement = relativeTargetURL(target, tag, true);
    return replacement === target ? match : `${prefix}${quote}${replacement}${quote}`;
  });
  return rewritten;
}

function prepareRelease(root, tag, prereleaseValue) {
  const metadata = releaseMetadata(tag, prereleaseValue);
  const packagePath = path.join(root, "package.json");
  const readmePath = path.join(root, "README.md");
  const pkg = JSON.parse(fs.readFileSync(packagePath, "utf8"));

  pkg.version = metadata.version;
  fs.writeFileSync(packagePath, `${JSON.stringify(pkg, null, 2)}\n`);

  const readme = fs.readFileSync(readmePath, "utf8");
  fs.writeFileSync(readmePath, rewriteReadme(readme, metadata.tag));
  return metadata;
}

if (require.main === module) {
  try {
    const metadata = prepareRelease(path.resolve(__dirname, ".."), process.argv[2], process.argv[3]);
    console.log(`prepared npm package ${metadata.version} from ${metadata.tag}`);
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}

module.exports = { prepareRelease, releaseMetadata, rewriteReadme };
