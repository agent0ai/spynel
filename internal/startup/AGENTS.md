# Automatic Startup DOX

## Purpose

- Own reversible workspace-specific background-service registration for systemd, launchd, and Windows Task Scheduler.

## Local Contracts

- Register `spynel serve` against the absolute canonical workspace configuration and derive its workspace identifier from that fixed path.
- Escape control characters and platform metacharacters in generated service values; never invoke a shell with untrusted configuration.
- Linux `WorkingDirectory` and description use literal scalar values with escaped percent specifiers, never command-argument quotes or backslash escapes. Preserve trailing spaces/backslashes with a final directory slash; reject workspace paths containing control characters before creating startup artifacts. Keep `ExecStart` arguments quoted separately and prefix the executable with `:` to disable environment-variable substitution in literal paths.
- Systemd system services explicitly declare `User=root`. Systemd and launchd registrations carry the registering process's `HOME` and any explicit `XDG_CONFIG_HOME`/`XDG_CACHE_HOME`, so headless startup can find the same per-user identity, harness homes, and speech cache as interactive launches. Do not copy arbitrary environment variables or credentials into service files.
- Every enable request validates the home/workspace directories, canonical configuration, executable, and optional npm launcher. Stage native syntax validation before replacing existing registrations: `systemd-analyze verify` in the matching scope on Linux and `plutil -lint` on macOS. Reload systemd after every registration/removal, including unchanged retries, and query the exact unit through bounded `list-unit-files` output; only persistent `enabled` proves enablement, and an absent/disabled entry proves removal. macOS also applies the launchctl enable/disable override for the matching system/GUI domain; Windows queries a newly created scheduled task. Native helper work has a 30-second total deadline and bounded diagnostics.
- Registration runs after the canonical preference is saved and reloaded; report native validation, access, reload, and verification failures explicitly. The saved preference alone does not prove registration succeeded or the service is running. Enable/disable actions affect future automatic startup without starting or stopping current Spynel processes.

- Register script installations through updater-resolved stable entry points rather than a resolved retained release binary. npm installations keep their supervising launcher; generated services always pass `--automatic-startup` and never perform proactive checks.

- Explicit installation removal scans bounded generated registrations for the exact stable executable or npm launcher, including quoted special paths. Stop/unload matching services and remove their future startup registrations before deleting binaries; ordinary preference changes retain their existing lifecycle. An absent Linux user manager allows offline removal, but an active service that cannot be stopped fails closed. Elevated removal retains the original user scope alongside system registrations.

- `Enabled` inspects persistent native registration without changing it or trusting the saved preference. Linux queries the exact unit file; macOS checks the launchd override and validated LaunchAgent/LaunchDaemon file. Failed or unrecognized queries return an error, never an assumed disabled state.

## Child DOX Index

No child DOX files.
