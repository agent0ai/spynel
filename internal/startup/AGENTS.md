# Automatic Startup DOX

## Purpose

- Own reversible workspace-specific background-service registration for systemd, launchd, and Windows Task Scheduler.

## Local Contracts

- Register `spynel serve` against the absolute canonical workspace configuration and derive its workspace identifier from that fixed path.
- Escape control characters and platform metacharacters in generated service values; never invoke a shell with untrusted configuration.
- Linux `WorkingDirectory` and description use literal scalar values with escaped percent specifiers, never command-argument quotes or backslash escapes. Preserve trailing spaces/backslashes with a final directory slash; reject workspace paths containing control characters before creating startup artifacts. Keep `ExecStart` arguments quoted separately and prefix the executable with `:` to disable environment-variable substitution in literal paths. Verify generated user and system units with `systemd-analyze verify` when available, including spaces, Unicode, percent/dollar signs, quotes, and backslashes.
- Registration runs after the canonical setting is saved and reloaded; report OS registration failures explicitly. The saved setting alone does not prove that a service is registered or running.

- Register script installations through updater-resolved stable entry points rather than a resolved retained release binary. npm installations keep their supervising launcher; generated services always pass `--automatic-startup` and never perform proactive checks.

## Child DOX Index

No child DOX files.
