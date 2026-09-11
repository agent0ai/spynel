# Textarea Derivative DOX

## Purpose

- Own the narrowly scoped MIT-licensed derivative of Bubbles v0.21 textarea used by the TUI composer and form fields.

## Local Contracts

- Preserve the included license and upstream editor semantics except for maintained local fixes.
- Wrapping, cursor navigation, deletion, selection, and transposition must operate on extended grapheme clusters without splitting or losing characters.
- Own logical selection ranges, selected-text replacement/deletion, visual-row hit testing, viewport offsets, and selected-character rendering in this shared widget; TUI callers must not recreate its wrap or word-boundary algorithms. Preserve tabs in the value (displayed as four spaces), and reject oversized edits without truncating clusters or deleting an existing selection. Whole-value replacement validates before clearing the draft; range replacement preserves and remaps the current caret/selection for asynchronous edits.
- Own shared rune-indexed `WordRange`/`LineRange` helpers for both TUI panes. Words use the existing whitespace-delimited grapheme convention, retaining punctuation; whitespace runs stay within a logical line. Logical lines exclude their newline. `CharacterAt` resolves the actual grapheme under either half of a wide cell and excludes synthetic cursor/row-end fill; `Hit` remains the nearest insertion boundary.
- Consuming a selection for an edit also clears an empty click/Shift anchor. Rejected or empty insertions retain that anchor for subsequent Shift extension; accepted insertion, newline and deletion cannot select their own result. Newline uses the shared transactional insertion path. Transpose/case shortcuts clear selection before their ordinary upstream operation.
- Rebind cached focused/blurred styles after theme changes so copied models do not retain obsolete palette pointers.
- Accepted insertion and selection deletion reveal the caret at the widget boundary, including direct clipboard calls. Replacement validation isolates mutable viewport state so rejection preserves scrolling as well as text/selection. Asynchronous range replacement keeps a previously visible caret visible after reflow; preserve intentional wheel scrolling when the caret was already offscreen. Rendering and non-edit events must not undo wheel scrolling.
- Width and height changes also keep a previously visible caret in view without changing logical selection. When the caret was deliberately scrolled offscreen, clamp the existing viewport to the new layout instead of jumping to the caret. Callers must not synthesize key events to repair widget geometry.

- Own bounded draft-local undo/redo (100 full-text snapshots, 4 MiB including state overhead) with immutable text/caret/selection snapshots. Group adjacent non-whitespace typing and repeated same-kind/direction character or word deletions within 500 ms, up to 64 changed runes per group; a larger single input event remains atomic. Unicode whitespace uses `unicode.IsSpace`; whitespace/newline insertion, navigation, selection, and action/direction changes separate groups. Paste, cut and selection/range replacements are isolated actions. Capture positions at action entry, but serialize text only before an accepted text mutation starts a new group; nested and joined mutations reuse that snapshot. No-op/rejected edits preserve history and redo. Undo/redo reveals the restored caret after reflow. `Reset` and accepted `SetValue` clear history; `ReplaceRange` remains an undoable edit preserving current caret/selection. A history generation changes on resets and undo/redo, including empty-stack presses, to fence delayed clipboard/attachment results. Callers may inspect retained text solely to keep draft metadata restorable without maintaining another history stack, and use the widget's key-boundary classification even for keys consumed by the surrounding UI.

- Masked field presentation hides text cells while retaining the same source offsets, grapheme boundaries, selection, and hit testing. Rendering never exposes secret text, including selected and cursor cells.

## Child DOX Index

Direct child DOX files:

| Child | Scope |
| --- | --- |
| [memoization/AGENTS.md](memoization/AGENTS.md) | Small generic memoization helper used by textarea layout. |
