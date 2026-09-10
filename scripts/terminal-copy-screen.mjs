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
const lines = (buffer, cols) => Array.from({ length: buffer.length }, (_, i) => buffer.getLine(i).translateToString(true, 0, cols));
const state = term => {
  const b = term.buffer.normal;
  return { lines: lines(b), base: b.baseY, x: b.cursorX, y: b.cursorY };
};

for (const archiveOnED2 of [false, true]) {
  const term = makeTerminal();
  const expectedExit = makeTerminal();
  const seed = "unrelated shell history\r\n".repeat(30) + "\x1b[H\x1b[J" + "shell before TUI\r\n$ spynel\r\n";
  await write(term, seed);
  await write(expectedExit, seed);
  const shellHistory = state(term).lines.slice(0, term.buffer.normal.baseY);
  let before, returned, copies = 0;
  // Model the normal-screen ED 2 archive policy separately from xterm's native
  // in-place policy. Warp's published clear_screen(All)/clear_viewport has this
  // distinction; this is a policy model, NOT execution of Warp or its UI.
  // https://github.com/warpdotdev/warp/blob/b61e936f40ce766d3321aa4b7a2c064f001df582/crates/warp_terminal/src/model/grid/ansi_handler.rs#L799-L860
  const feed = async (term, output) => {
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
    if (event.phase === "exit") {
      const boundary = "\x1b[?1049l";
      const parts = event.output.split(boundary);
      assert.equal(parts.length, 2, "one final alternate-screen exit");
      await feed(term, parts[0]);
      assert.equal(term.buffer.active.type, "alternate");
      assert.ok(lines(term.buffer.active, term.cols).every(line => line.trimEnd() === ""),
        "populated final alternate screen could be retained by the emulator");
      for (let y = 0; y < term.rows; y++) {
        for (let x = 0; x < term.cols; x++) {
          const cell = term.buffer.active.getLine(y).getCell(x);
          assert.ok(cell.isFgDefault() && cell.isBgDefault(), "erased alternate cell retained TUI colors");
        }
      }
      await feed(term, boundary + parts[1]);
      await feed(expectedExit, boundary + "\x1b[0m\x1b[?25h\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1004l\x1b[?2004l" +
        "shutdown diagnostic\r\nshell-ready> shell input\r\nshell-read: shell input\r\nPASS\r\n");
      assert.equal(term.buffer.active.type, "normal");
      assert.deepEqual(state(term), state(expectedExit), "exit changed shell history/cursor, lost diagnostics, or rendered after cleanup");
      assert.deepEqual(term.modes, expectedExit.modes, "exit did not restore terminal modes");
      const core = term._core; // Test-only inspection: public xterm API omits these states.
      assert.equal(core.coreService.isCursorHidden, false, "cursor remained hidden");
      assert.equal(core.coreService.decPrivateModes.sendFocus, false, "focus reporting remained enabled");
      assert.equal(core._inputHandler._curAttrData.fg, 0, "foreground/style leaked into shell");
      assert.equal(core._inputHandler._curAttrData.bg, 0, "background/style leaked into shell");
      continue;
    }
    await feed(term, event.output);
    await feed(expectedExit, event.output);
    if (event.phase === "resize") {
      term.resize(event.cols, event.rows);
      expectedExit.resize(event.cols, event.rows);
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
  assert.ok(copies === 0 || copies === 11);
  assert.equal(events.at(-1).phase, "exit", "capture must include actual process exit");
  console.log(JSON.stringify({ emulator: "xterm-headless 5.5.0", archiveOnED2, copies, result: "passed" }));
  term.dispose();
  expectedExit.dispose();
}
