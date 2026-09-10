# TUI editing and terminal checks

The TUI selects logical text. Soft wraps, panel padding, inline-code padding,
scrollbars, and ANSI sequences do not become copied text. Source newlines,
indentation, tabs, and empty lines remain meaningful.

| Action | Binding or gesture |
| --- | --- |
| Select message content across responses | Start dragging inside the message body. Labels and the left gutter are excluded. |
| Select whole messages with labels | Start dragging in `Spy`/`You`/`Err` or their left gutter. This mode stays latched throughout the drag, including reverse drags. |
| Position composer caret / select input | Click / drag inside the composer. Shift-click extends the existing anchor. |
| Select a word in either pane | Double-click text, then hold and drag to extend or shrink by whole words. Words include punctuation; whitespace selects its run within the line. |
| Select a line in either pane | Triple-click text, then hold and drag by complete logical lines across soft wraps. The final newline and chat label are excluded. |
| Extend keyboard selection | Shift+arrows/Home/End; Ctrl+Shift+Left/Right/Home/End. |
| Select all | Ctrl+A in the focused pane. History remains read-only. |
| Copy / cut / paste | Ctrl+C / Ctrl+X / Ctrl+V. Cut changes only selected composer text; with no selection it does nothing. Typing or paste replaces composer selection. |
| Move by word | Ctrl+Left/Right; Alt+B/F or Alt+Left/Right. Words are whitespace-delimited grapheme clusters. |
| Delete a word | Ctrl+Delete / distinguishably encoded Ctrl+Backspace; Alt+D / Alt+Backspace or Ctrl+W. |
| Scroll | Wheel over the intended pane; PageUp/PageDown or Alt+Up/Down for history. Composer scrolling does not move its caret. |
| Extend beyond a viewport | Drag above/below it. Scrolling advances at most three rows per 50 ms tick. Return inside, release, or press Escape to stop. |
| Send / newline | Enter / Shift+Enter (or Ctrl+J). Alt+Enter retains its send behavior. |
| Cancel / stop / quit | Escape cancels selection. Without a focused selection, Ctrl+C clears input, stops an active turn with empty input, or quits idle chat. `/quit` exits. |
| Native terminal copy | Select text, then F6. Use ordinary terminal selection and Cmd+C / Ctrl+Shift+C; Escape, Enter, Space, Backspace, Delete, Tab or any supported F key returns. |
| Undo / redo | Ctrl+Z / Ctrl+Y in the composer, silently. These keys never suspend Spynel or edit the transcript. |
| Redraw | Ctrl+L redraws. |

Successive primary clicks must be within 500 ms, on the same screen row and
within one column in the same pane. A fourth click starts over. Moving while
pressed, scrolling, typing, focus loss, or resizing resets the sequence. Releasing
keeps the selection. Held word/line drags retain the original selected unit when
extending, shrinking, or reversing across it, including during autoscroll.
Display-only padding and empty row-end fill do not select a word or line.

New selections clear the other pane's selection. Typing returns focus from output
to the composer. Output selection pauses automatic tail following. Selected
streamed text keeps the interpretation shown when selected: appended source
continues to appear, while Markdown formatting that would change the selected
prefix is deferred until the selection is cleared. Resize reflows the retained
text; eviction from the bounded live display or a conversation switch clears
invalidated selections. Durable history is never edited by selection or cut.

Pasted text stays editable even when long or multiline. Tabs remain tabs in the
value and display as four spaces. CRLF becomes one newline. The composer rejects
an edit exceeding its input limit without deleting the previous selection;
feedback appears in the footer. Pasting a readable single-line local file path
inserts its literal text immediately, then prepares the attachment asynchronously.
Successful conversion preserves the current caret and selection, including typing
during preparation. If the pasted path was edited or occurs more than once,
Spynel keeps the literal text and reports that it could not identify the attachment
range. An oversized attachment label also leaves the literal text intact.

Undo restores text, caret and selection, revealing the caret after wrapping or
resize. Adjacent typing and repeated character/word deletions in the same direction
group within a 500 ms pause, up to 64 changed runes. A larger single input event
stays atomic. Unicode whitespace/newlines, navigation, selection and action changes
separate groups. Each paste, cut or selection replacement is one action.
Text snapshots are captured only when an accepted edit starts a group.
A new edit after undo clears redo; empty history is
silent. History is local to the draft, bounded to 100 snapshots and 4 MiB, and
clears on send, Ctrl+C clear, command-picker draft replacement and conversation
changes. It cannot restore a submitted message.

Attachment conversion is a separate undoable replacement of its pasted path:
undo first restores the literal path, then undoes the paste. Redo reuses already
prepared attachment metadata; it never copies again or removes files. Separate
copies with the same basename receive numbered composer labels so undo/redo and
submission retain the correct file reference. Undo/redo
cancels pending paste preparation and ignores late native-clipboard and attachment
results, even if redo restores identical text. Cancelled paths remain literal;
paste again to request preparation.

## Clipboard and terminal compatibility

Copy retains an internal copy, offers base64-encoded OSC 52 to the terminal, and
uses an OS clipboard helper when available outside SSH. A helper inside a
container cannot establish delivery to the host clipboard. There is no routine
copy-status notice and no acknowledgement of OSC 52 delivery. Ctrl+V pastes the
internal copy when native copy is unavailable. Terminal Cmd+V / Ctrl+Shift+V
pastes the host clipboard; Spynel does not query it remotely.

For macOS → Warp → `docker exec`, use **F6 after selecting text** if Ctrl+C does
not reach the macOS clipboard. F6 prints that logical selection once on the
ordinary terminal screen after clearing only its visible area, without bars or
instructions. Earlier scrollback is not erased. Application mouse capture, the
fullscreen renderer and focus reporting are released. Bracketed paste stays
enabled so pasted exit keys and their remaining text are discarded together.
Select the text with the terminal's ordinary mouse selection, scroll through its
normal scrollback if needed, then use Cmd+C (or Ctrl+Shift+C). Escape, Enter,
Space, Backspace, Delete, Tab or any terminal-supported F key restores the TUI,
draft, selection, focus and current dimensions. Complete key frames and queued
copy-view input are consumed before return. Tea restores the alternate screen
once before the result callback; Spynel does not leave/reenter it again with the
renderer running, which could otherwise write a TUI frame into normal history.
Incoming application work continues; its UI events resume on return. Controls in source text are
removed before printing so they cannot change terminal state. Long copy-view
text uses normal terminal scrolling, subject to the emulator's scrollback limit.
F6 uses cursor-home followed by erase-below (ED 0), which clears the visible
rows in place. Erase-all (ED 2) can instead archive those rows: Warp's
[published normal-grid implementation](https://github.com/warpdotdev/warp/blob/b61e936f40ce766d3321aa4b7a2c064f001df582/crates/warp_terminal/src/model/grid/ansi_handler.rs#L799-L860)
distinguishes these operations. Repeated short selections reuse the display;
a shorter selection removes the previous selection's visible tail. This does
not remove old rows already in scrollback, including overflow from long
selections or earlier F6 entries. Standard cursor addressing cannot selectively
overwrite historical rows. Erasing all saved lines would also destroy unrelated
shell history, so F6 never does that. Use ordinary selection copy (Ctrl+C/OSC 52)
when you want to avoid adding long text to scrollback; its delivery depends on
the host terminal. Actual Warp behavior and clipboard delivery still need local
verification.
Terminals that ignore bracketed-paste mode cannot distinguish pasted return
keys from typed keys; avoid pasting into this copy view on those terminals.

Cmd+C may be intercepted by the emulator; it need not reach Spynel. OSC 52 and
native selection depend on emulator settings. Shift+drag may bypass application
mouse capture, but copies rendered screen cells; F6 exposes logical text without
Spynel padding or scrollbars. No synthetic Linux PTY establishes real macOS
clipboard delivery.

Shell suspension is unsupported while Spynel runs a workspace server or election,
including startup: it absorbs SIGTSTP. A forced SIGSTOP cannot be prevented. If an
older instance's shell reports `Stopped`, use `fg` in that shell to resume it,
then `/quit` if it should exit. Do not delete a fresh primary lease or kill an
unrelated primary. A new TUI uses the existing primary's backend until that
primary exits; launching a new binary does not replace it.

Traditional terminals cannot always distinguish Ctrl+Backspace from Backspace
or Ctrl+H, or Shift+Enter from Enter. Spynel accepts common CSI-u/modifyOtherKeys
encodings for those keys; otherwise use Alt+Backspace/Ctrl+W and Ctrl+J. It does
not enable a terminal-wide extended keyboard protocol. Existing Bubble Tea key
encodings include rxvt Shift+Home/End, Linux-console function keys and
Escape-prefixed Alt navigation. A lone Escape or Alt+Escape is delivered after
a short delay; a subsequently arriving CSI prefix after a lone Escape is still
framed as protocol. Focus-out (`ESC [ O`) shares a prefix with DECCKM keys:
an immediate following `A`–`D` or `a`–`d` completes the key; otherwise it is a
focus report, waiting for the 60 ms input timeout if no next byte arrives.
Those identical bytes
cannot identify focus-out followed immediately by such a typed letter.
Literal mouse-looking text in bracketed paste is never parsed as mouse
input. Multiline paste requires the terminal's bracketed-paste support to prevent
unmarked Enter bytes from acting as send keys.

Drag cancellation outside the terminal window requires the emulator to deliver
a release or focus-loss report. Escape always cancels it on return. Mouse capture
is restored on return from F6 and disabled on ordinary exit. Linux
and macOS are the supported distribution targets; Windows remains unsupported.

## Local acceptance checklist

Build with the repository's supported Go toolchain:

```sh
mkdir -p .tmp-bin
go build -o .tmp-bin/spynel ./cmd/spynel
.tmp-bin/spynel --config /path/to/workspace/.spynel/config.yaml
```

Use a synthetic conversation for these checks:

1. Select from message content across two replies, then repeat starting in a
   `Spy`/`You` label. Paste each result into a plain-text editor. Confirm the two
   label modes, soft-wrap joining, real newlines, code indentation, blank lines,
   trailing spaces, and absence of scrollbar/panel cells. Repeat in reverse.
2. Include `界`, `é`, and `👩🏽‍💻`. Select on both halves of wide glyphs, resize the
   terminal, and copy again. Confirm complete characters and precise highlights.
   Double-click and triple-click text in both panes, including a wrapped
   continuation row and both halves of a wide glyph. Release and copy: expect
   the word or complete logical line, without the next line or message. Hold the
   last press and drag forward, backward, and back through the anchor: expect
   whole units and the original word/line to remain intact. Try
   punctuation, whitespace, a slow/distant click, a wheel event between clicks,
   and a drag followed by a click; verify that these reset or select as described.
3. Paste several lines into the composer. Click a caret position, Shift-select,
   drag across lines, replace by typing and by paste, cut, and paste back. Try
   Ctrl+A, Home/End, word movement/deletion, and both reverse and Shift drags.
   Replace `DEF` in `abcDEFghi` with a short paste `XY`, then type `Z`: expect
   `abcXYZghi`, including when another line follows. At the input limit, a
   rejected paste must retain the draft and selection.
   Also click without dragging in empty and nonempty input, then type `abc` or
   paste `PASTED` followed by typing `XY`: all inserted text must remain, with
   no selection until explicitly extended. Try Shift+Home, copy, replacement,
   and Alt+Left followed by typing with your emulator's actual key encodings.
   With more than ten composer rows, paste again at the end: the new text and
   caret must appear immediately. Repeat with a local attachment path. Rejected
   replacements must also retain the current scroll position.
   Narrow and widen the terminal with a long wrapped draft at the ten-row cap,
   then shorten its height. A visible caret and selected tail must remain in
   view. Wheel away from that caret and resize again: preserve the detached
   scroll position, clamping only when the new content is shorter.
4. Fill each pane beyond its visible height. Wheel over each independently. Drag
   above/below both panes, return inside, reverse direction, release at both
   boundaries, and switch window focus. Confirm scrolling stops and copied
   ranges extend beyond the original viewport.
5. Keep a response selection while it streams and finishes, then resize and
   copy. Clear it to publish deferred Markdown formatting. Confirm input remains
   independently editable and history remains unchanged by cut/delete.
6. Test native clipboard locally, terminal paste, and the actual SSH/multiplexer
   path you use. In macOS → Warp → `docker exec`, select a short line and repeat
   F6/Enter three times; the current visible copy area should stay at the top
   without new blank rows or old text. Repeat with several lines followed by
   one short line, a wrapping line, a narrower/wider window, and more than one
   screen of text. Scroll up and copy the full long selection, then reenter with
   short text: only its visible area is replaced; old overflow remains in
   scrollback. Distinguish new buildup from history left by the old binary.
   Verify OSC 52 permission and repeated return with each documented key. Paste exit-key-looking
   content in the copy view; the draft must remain unchanged.
   Paste literal `[<64;1;1M` alongside multiline text; nothing should send early.
7. Verify Enter, Shift+Enter/Ctrl+J, Ctrl+C copy, then clear/stop/quit without
   selection, `/quit`, Ctrl+L, and silent Ctrl+Z/Ctrl+Y undo/redo without suspension.
   Undo/redo a multiline selection replacement, cut and word deletion, then type
   after undo to clear redo. Verify send, clear and conversation changes reset
   history; an unfinished attachment paste must not overwrite an undone draft.
   After exit, confirm
   the shell has normal echo, cursor, scrolling, paste, and mouse behavior.

## Runnable verification boundary

```sh
go test ./internal/channel/tui/... ./internal/markdown
go test -race ./internal/channel/tui/... ./internal/markdown
go test ./internal/channel/tui -run '^TestRealPTYSemanticInputAndModeRestoration$' -count=1
scripts/capture-tui.sh .tmp-artifacts/20260909-spynel-tui-semantic-editing/captures
```

For actual screen/cursor assertions across repeated F6 cycles, reuse the same
PTY capture with a small test-only emulator (no application dependency):

```sh
mkdir -p .tmp-artifacts/f6-screen
npm install --prefix .tmp-artifacts/f6-screen/emulator --ignore-scripts --no-audit --no-fund @xterm/headless@5.5.0
SPYNEL_COPY_SCREEN_CAPTURE="$PWD/.tmp-artifacts/f6-screen/pty.json" \
  go test ./internal/channel/tui -run '^TestRealPTYSemanticInputAndModeRestoration$' -count=1
node scripts/terminal-copy-screen.mjs \
  .tmp-artifacts/f6-screen/emulator/node_modules/@xterm/headless \
  .tmp-artifacts/f6-screen/pty.json
```

Replay checks three consecutive short selections, longer-to-shorter output,
wrapping, resize, over-height text, cursor placement, preserved shell history
and return-screen isolation. It runs xterm's native erase behavior and a
separately labeled model of normal-screen ED 2 archiving. The model demonstrates
the policy difference; neither replay runs Warp or establishes its block/UI
behavior. The former ED 2 copy output fails the archival regression on the
second short entry.

The Linux PTY fixture starts with unset model dimensions and checks real startup
sizing and OS resize delivery, including selected-tail reflow and detached
composer scrolling. It drives the raw-input reader, Bubble Tea decoder,
model, and renderer with split/coalesced mouse packets, Unicode multiline paste,
click/type/paste continuation, double/triple clicks through SGR and X10 releases,
held unit drags and reversal, wrapped Unicode word/line copy and replacement,
rxvt/Alt/console-key frames, selection/replacement,
word deletion, raw Ctrl+Z/Ctrl+Y, redo invalidation, draft reset, OSC copy,
autoscroll, focus loss, and
static text-only F6 copy, fragmented return keys, coalesced input/paste isolation
and return/exit mode restoration. It uses isolated synthetic text and no application owner,
real harness, private history, or desktop clipboard. Helper/model tests exercise
additional reverse ranges, source/layout mapping, resizing, streaming, and
boundary cases. PNG captures show selection in both panes and general theme
rendering. The capture host lacks complete CJK/emoji fonts, so its images cannot
establish glyph appearance; range assertions and PTY snapshots check the text.
These checks do not establish compatibility with Jan's emulator, OS clipboard,
fonts, SSH topology, or multiplexer settings; the checklist above covers those.
