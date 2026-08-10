# Textarea Derivative DOX

## Purpose

- Own the narrowly scoped MIT-licensed derivative of Bubbles v0.21 textarea used by the TUI composer.

## Local Contracts

- Preserve the included license and upstream editor semantics except for maintained local fixes.
- Wrapping, cursor navigation, deletion, selection, and transposition must operate on extended grapheme clusters without splitting or losing characters.
- Rebind cached focused/blurred styles after theme changes so copied models do not retain obsolete palette pointers.

## Child DOX Index

Direct child DOX files:

| Child | Scope |
| --- | --- |
| [memoization/AGENTS.md](memoization/AGENTS.md) | Small generic memoization helper used by textarea layout. |
