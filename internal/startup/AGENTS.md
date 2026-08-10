# Automatic Startup DOX

## Purpose

- Own reversible workspace-specific background-service registration for systemd, launchd, and Windows Task Scheduler.

## Local Contracts

- Register `spynel serve` against the absolute canonical workspace configuration and preserve the pre-migration workspace identifier when replacing or removing prior registrations.
- Escape control characters and platform metacharacters in generated service values; never invoke a shell with untrusted configuration.
- Registration participates in configuration transactions, so failures restore previous configuration and harness state.

## Child DOX Index

No child DOX files.
