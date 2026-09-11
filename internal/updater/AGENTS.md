# Updater DOX

## Purpose

- Own npm and standalone installation detection, bounded release checks, verified bundle installation, and cross-workspace process restart/shutdown coordination.

## Local Contracts

- Bind npm detection to the running executable in the validated package vendor tree. Script ownership comes only from the installation-local `.spynel-install` and immutable release `.bundle.json`, never workspace state or inherited npm metadata. Unmanaged archive/development executables remain unmanaged.
- Resolve and retain the process executable during package initialization, before an update can switch an invoked symlink. Every later detection validates that retained bundle and its running version; never re-detect the old process against the new launcher target or relax version validation to recover ownership.
- npm proactive checks require the supervising launcher's explicit interactive eligibility and reuse its bounded timestamp/version snapshot. A script-installed TUI receives eligibility from the actual interactive CLI launch, ignores inherited npm snapshots, and uses the same asynchronous ten-second check and at-most-hourly refresh. Noninteractive, automatic-startup, and explicitly suppressed launches never check proactively.
- GitHub checks use the stable latest-release endpoint; reject drafts, prereleases and invalid versions. Trusted mirror overrides are `SPYNEL_GITHUB_API_URL` for discovery and `SPYNEL_DOWNLOAD_BASE` for matching assets. Production defaults use the canonical repository over HTTPS. Bound download time, bytes and redirects; refuse HTTPS downgrades.
- `InstallArchive` is the shared native boundary for `install.sh` and explicit GitHub updates. Verify the exact checksum and reject unsafe/duplicate paths, links, special files, oversized archives/trees, missing libraries/licenses, compression errors, or a candidate whose bounded `--version` execution fails. Never execute an unvalidated candidate during an update.
- The POSIX bootstrap first verifies checksums and archive paths/types, then streams only exact executable/library members to fixed temporary paths. That verified runtime invokes `install-bundle` to perform complete validation and installation. Its first supported release must include this entry point.
- Install only into a new/empty or marked per-user root. Serialize publication with a nonblocking OS file lock that releases on crash. Interrupted staging/downloads are inert. Publish a new immutable `releases/<version>-<unique>` directory and atomically replace `current`; never overwrite existing bundles or unrelated launchers, and reject downgrades even after a concurrent update.
- Keep `<root>/spynel -> current/spynel` stable. Resolve restart and automatic-startup through it even when `os.Executable` names an older bundle. Retain old bundles and inert interrupted stages; explicit uninstall stops every process using the installation. Do not add speculative process garbage collection.
- `/update` downloads/validates/switches script bundles before requesting the existing graceful shutdown/restart path. Failed validation leaves the active bundle usable. npm replacement remains exclusively in the supervising launcher after Go exits. Workspace election/job shutdown and saved state retain their existing owners.

- `Uninstall` validates installation ownership, serializes against publication, asks startup ownership to remove matching restart registrations, then terminates processes by their actual executable path (TERM followed by bounded KILL). Remove only marked runtime content and exact launcher links; preserve workspace data and unrelated siblings. npm removal accepts a validated global package even after failed postinstall and delegates package/link removal to `npm uninstall --global --prefix` for that exact prefix. Use native Linux/macOS process inspection; never kill by process name or command-text substring.

- Own the per-user OS-config `spynel/processes` registry of private PID/generation/executable/installation/version records for live servers and TUIs. Verify native executable identity before signals; use a new registration generation at the target version with application readiness as restart evidence. Coordinate every registered instance of the selected installation across workspaces, preserving terminal descriptors through exec. Reject pre-coordination processes explicitly. `KillAll` uses the same verified discovery across installations and shares bounded TERM/KILL termination with uninstall; startup service shutdown is a supplied owning-layer callback.

- The current npm wrapper explicitly advertises coordinated-update support. Bind that capability to validated launcher ownership and include it in process registration; an older wrapper supervising a newer native executable must fail preflight with one-time stop/relaunch guidance instead of silently using the old single-process update protocol.

- Native process discovery must retain visibility after npm unlinks a running image. On macOS, use the retained executable-path field from `kern.procargs2` when `proc_pidpath` loses the vnode path; never expose its arguments or environment. An unreadable live registered process is an explicit error, never an omitted restart target.

## Child DOX Index

No child DOX files.
