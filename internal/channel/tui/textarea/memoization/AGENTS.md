# Textarea Memoization DOX

## Purpose

- Own the small generic cache helper used by textarea rendering and layout calculations.

## Local Contracts

- Cache keys must completely describe the computed value; callers must invalidate or replace caches when dependent editor state changes.
- Keep the helper deterministic, allocation-conscious, and free of TUI or application dependencies.

## Child DOX Index

No child DOX files.
