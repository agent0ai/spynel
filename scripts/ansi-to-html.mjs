import fs from "node:fs";

if (process.argv.length !== 4) {
  throw new Error("usage: node ansi-to-html.mjs INPUT.ansi OUTPUT.html");
}

const source = fs.readFileSync(process.argv[2], "utf8")
  .replace(/\x1b\]8;;.*?\x1b\\/gs, "");

const escapeHTML = (value) => value
  .replaceAll("&", "&amp;")
  .replaceAll("<", "&lt;")
  .replaceAll(">", "&gt;");

let foreground = null;
let background = null;
let bold = false;
let italic = false;
let underline = false;
let inverse = false;

const css = () => {
  const values = [];
  const effectiveForeground = inverse ? background : foreground;
  const effectiveBackground = inverse ? foreground : background;
  if (effectiveForeground) values.push(`color:${effectiveForeground}`);
  if (effectiveBackground) values.push(`background:${effectiveBackground}`);
  if (bold) values.push("font-weight:700");
  if (italic) values.push("font-style:italic");
  if (underline) values.push("text-decoration:underline");
  return values.join(";");
};

const applySGR = (raw) => {
  const values = raw === "" ? [0] : raw.split(";").map((value) => Number(value));
  for (let index = 0; index < values.length; index += 1) {
    const value = values[index];
    if (value === 0) {
      foreground = null;
      background = null;
      bold = false;
      italic = false;
      underline = false;
	  inverse = false;
    } else if (value === 1) bold = true;
    else if (value === 3) italic = true;
    else if (value === 4) underline = true;
	else if (value === 7) inverse = true;
    else if (value === 22) bold = false;
    else if (value === 23) italic = false;
    else if (value === 24) underline = false;
	else if (value === 27) inverse = false;
    else if (value === 39) foreground = null;
    else if (value === 49) background = null;
    else if ((value === 38 || value === 48) && values[index + 1] === 2 && index + 4 < values.length) {
      const color = `rgb(${values[index + 2]},${values[index + 3]},${values[index + 4]})`;
      if (value === 38) foreground = color;
      else background = color;
      index += 4;
    }
  }
};

let body = "";
let cursor = 0;
const matcher = /\x1b\[([0-9;]*)m/g;
for (const match of source.matchAll(matcher)) {
  const text = source.slice(cursor, match.index);
  if (text) body += `<span style="${css()}">${escapeHTML(text)}</span>`;
  applySGR(match[1]);
  cursor = match.index + match[0].length;
}
const tail = source.slice(cursor).replace(/\x1b\[[0-9;?]*[A-Za-z]/g, "");
if (tail) body += `<span style="${css()}">${escapeHTML(tail)}</span>`;

const html = `<!doctype html>
<meta charset="utf-8">
<style>
  html, body { margin: 0; background: #080d17; }
  body { padding: 18px; width: max-content; }
  pre {
    margin: 0;
    font-family: "DejaVu Sans Mono", "Liberation Mono", monospace;
    font-size: 15px;
    line-height: 1;
    font-variant-ligatures: none;
    color: #d4dceb;
  }
</style>
<pre>${body}</pre>`;

fs.writeFileSync(process.argv[3], html);
