# npm Executable Shim DOX

## Purpose

- Own the installed `spynel` Node executable shim.

## Local Contracts

- Resolve the platform package through the npm wrapper modules, forward arguments and signals to the native executable, and preserve native process exit behavior.
- Keep native execution asynchronous so SIGINT, SIGTERM and SIGHUP sent to the launcher PID reach its child. Wait for native exit/cleanup before returning or replacing the package; retain a stop latch across launches and remove listeners on final exit.
- Inherit terminal stdin/stdout/stderr. Receive bounded native diagnostic identity on a separate descriptor, then invoke the native harness-free log sink with the actual PID, status or signal after close, before an update replaces the executable. Keep payloads free of command arguments and user content. A failed/timed-out recorder warns without replacing the native result; signal-only native exits re-signal the launcher rather than becoming status 1. Clear inherited diagnostic protocol markers on each launch and retain one launch ID across native in-place restarts.
- Validate platform support before update checks or native launch so the Windows stub fails immediately and never searches for a dormant executable.
- Interactive TUI launches may perform the bounded update flow; noninteractive commands and generated automatic-startup services must not.
- Keep this file dependency-light and compatible with the Node version declared by the root package manifest.

- Use native check/restart coordination for both startup-offer and explicit-update paths, with bounded subprocess execution. Preserve normalized requester arguments and propagate coordination failures; never report success merely because npm exited successfully.

- The current npm wrapper explicitly advertises coordinated-update protocol 2 for native supervision and latched asynchronous shutdown; older protocol 1 is not sufficient. Bind that capability to validated launcher ownership and include it in process registration; an older wrapper supervising a newer native executable must fail preflight with one-time stop/relaunch guidance instead of silently using the old single-process update protocol.

- Keep one shutdown latch/listener set across the whole launcher lifetime, including update prompts and asynchronous package/coordination work. A stopped launcher never starts another native generation. Native supervises executors; Node owns only npm replacement and its explicit update loop.

- Export `main(handoff, lifetime)` for package-replacement handoff. Only after all prior native/helper children exit, reload package-local modules and call the installed implementation in the same Node process with the same lifetime controller/listeners. The new implementation coordinates other instances before launching the requester and does not repeat the startup offer. Already-current restart needs no reload. Failed updates/handoffs stop rather than reusing stale modules; immutable older wrappers require an operator fresh launch.

- Code 75 authorizes replacement only with its current private, bounded structured request (normalized arguments, install boolean and semantic version). Missing or invalid intent preserves status 75 and stops without an update; never infer intent from the exit code alone.

- Startup offers pass advertised target worker compatibility into native preflight; structured code-75 requests also require a nonnegative integer worker_protocol. Zero is explicitly unknown, and the native owner decides whether live workers permit it. Preserve refusal before npm and pass the exact preflighted version to package installation. Post-publication coordination uses the installed native protocol before application signals.

## Child DOX Index

No child DOX files.
