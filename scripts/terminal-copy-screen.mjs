// Replay the real synthetic Go PTY through the small, test-only @xterm/headless
// package. See docs/tui-editing.md for the pinned install and capture commands.
import assert from "node:assert/strict";
import fs from "node:fs";
import { createRequire } from "node:module";
import path from "node:path";

const { Terminal } = createRequire(import.meta.url)(path.resolve(process.argv[2]));
const events = JSON.parse(fs.readFileSync(process.argv[3], "utf8"));
const makeTerminal = () => new Terminal({ cols: 60, rows: 24, scrollback: 10000, allowProposedApi: true });
const write = (term, text) => new Promise(resolve => term.write(text, resolve));
const lines = buffer => Array.from({ length: buffer.length }, (_, i) => buffer.getLine(i).translateToString(true));
const state = term => {
  const b = term.buffer.normal;
  return { lines: lines(b), base: b.baseY, x: b.cursorX, y: b.cursorY };
};

for (const archiveOnED2 of [false, true]) {
  const term = makeTerminal();
  await write(term, "unrelated shell history\r\n".repeat(30) + "\x1b[H\x1b[J");
  const shellHistory = state(term).lines.slice(0, term.buffer.normal.baseY);
  let before, returned, copies = 0;
  // Model the normal-screen ED 2 archive policy separately from xterm's native
  // in-place policy. Warp's published clear_screen(All)/clear_viewport has this
  // distinction; this is a policy model, NOT execution of Warp or its UI.
  // https://github.com/warpdotdev/warp/blob/b61e936f40ce766d3321aa4b7a2c064f001df582/crates/warp_terminal/src/model/grid/ansi_handler.rs#L799-L860
  const feed = async output => {
    const parts = output.split("\x1b[2J");
    for (let i = 0; i < parts.length; i++) {
      if (i) {
        if (archiveOnED2 && term.buffer.active.type === "normal") {
          const b = term.buffer.normal;
          const visible = lines(b).slice(b.baseY);
          const used = visible.findLastIndex(line => line !== "") + 1;
          const cursor = `\x1b[${b.cursorY + 1};${b.cursorX + 1}H`;
          await write(term, `\x1b[${term.rows};1H` + "\r\n".repeat(used) + cursor);
        }
        await write(term, "\x1b[2J");
      }
      await write(term, parts[i]);
    }
  };
  for (const event of events) {
    await feed(event.output);
    if (event.phase === "resize") {
      term.resize(event.cols, event.rows);
      if (term.buffer.active.type === "normal") returned = state(term);
    } else if (event.phase === "enter") {
      assert.equal(term.buffer.active.type, "alternate");
      before = state(term);
    } else if (event.phase === "copy") {
      copies++;
      assert.equal(term.buffer.active.type, "normal");
      const reference = makeTerminal();
      reference.resize(term.cols, term.rows);
      await write(reference, event.selection.replaceAll("\n", "\r\n"));
      const expected = state(reference), actual = state(term);
      const label = `copy ${copies}, archiveOnED2=${archiveOnED2}`;
      assert.deepEqual(actual.lines.slice(actual.base), expected.lines.slice(expected.base), `${label}: visible rows`);
      assert.deepEqual([actual.x, actual.y], [expected.x, expected.y], `${label}: cursor`);
      assert.equal(actual.base - before.base, expected.base, `${label}: accumulated history`);
      assert.deepEqual(actual.lines.slice(before.base), expected.lines, `${label}: complete long text`);
      assert.deepEqual(actual.lines.slice(0, shellHistory.length), shellHistory, `${label}: unrelated history`);
      reference.dispose();
      returned = actual;
    } else if (event.phase === "return") {
      assert.equal(term.buffer.active.type, "alternate");
      assert.deepEqual(state(term), returned, "TUI return wrote into the ordinary copy area");
    }
  }
  assert.equal(copies, 11);
  console.log(JSON.stringify({ emulator: "xterm-headless 5.5.0", archiveOnED2, copies, result: "passed" }));
  term.dispose();
}
