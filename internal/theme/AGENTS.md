# Theme DOX

## Purpose

- Own semantic color-role validation, file-backed palette discovery, built-in theme assets, and editable workspace palette installation.

## Local Contracts

- Require every semantic role, a unique safe theme name, valid colors, and deterministic fallback behavior.
- Keep built-in YAML under `themes/` as the single source for runtime fallbacks and initialized workspace copies; never duplicate palette values in Go code or workspace templates.
- Theme files are user-overridable workspace assets; never replace local customizations or mutate a loaded palette in place.
- Visual changes require theme tests and the repository's TUI capture/PNG inspection path.

## Child DOX Index

Direct child DOX files:

| Child | Scope |
| --- | --- |
| [themes/AGENTS.md](themes/AGENTS.md) | Built-in semantic theme YAML assets. |
