# npm Executable Shim DOX

## Purpose

- Own the installed `spynel` Node executable shim.

## Local Contracts

- Resolve the platform package through the npm wrapper modules, forward arguments and signals to the native executable, and preserve native process exit behavior.
- Keep native execution asynchronous so SIGINT, SIGTERM and SIGHUP sent to the launcher PID reach its child. Wait for native exit/cleanup before returning or replacing the package; remove forwarding listeners between launches.
- Validate platform support before update checks or native launch so the Windows stub fails immediately and never searches for a dormant executable.
- Interactive TUI launches may perform the bounded update flow; noninteractive commands and generated automatic-startup services must not.
- Keep this file dependency-light and compatible with the Node version declared by the root package manifest.

- Use native check/restart coordination for both startup-offer and explicit-update paths, with bounded subprocess execution. Preserve normalized requester arguments and propagate coordination failures; never report success merely because npm exited successfully.

## Child DOX Index

No child DOX files.
