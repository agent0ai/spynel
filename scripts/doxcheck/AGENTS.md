# DOX Validator DOX

## Purpose

- Own the deterministic repository-wide AGENTS.md coverage and direct-child index validator.

## Local Contracts

- Every directory containing a tracked or non-ignored untracked file requires a local `AGENTS.md`; worktree-deleted paths are excluded so explicit structural deletions validate before commit, and there are no generated, testdata, vendor-like, or license-only exceptions.
- Use Git's ignore policy only to exclude disposable state before coverage evaluation. Detect missing contracts, required-heading defects, false leaf declarations, missing/extra/broken or duplicate direct-child links, and index cycles.
- Keep validation read-only, deterministic, repository-root independent, and tested with synthetic hierarchies.
- Run it through `scripts/dev.sh dox`; `scripts/smoke.sh` invokes the same gate.

## Child DOX Index

No child DOX files.
