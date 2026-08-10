# npm Executable Shim DOX

## Purpose

- Own the installed `spynel` Node executable shim.

## Local Contracts

- Resolve the platform package through the npm wrapper modules, forward arguments and signals to the native executable, and preserve native process exit behavior.
- Validate platform support before update checks or native launch so the Windows stub fails immediately and never searches for a dormant executable.
- Interactive TUI launches may perform the bounded update flow; noninteractive commands and generated automatic-startup services must not.
- Keep this file dependency-light and compatible with the Node version declared by the root package manifest.

## Child DOX Index

No child DOX files.
