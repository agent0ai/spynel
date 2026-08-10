# Updater DOX

## Purpose

- Own npm-install detection, bounded registry update checks, semantic application-version state, and retry-safe version transitions.

## Local Contracts

- Perform the ten-second npm check only for interactive TUI starts; automatic services and noninteractive commands must not make it.
- `/update install` coordinates the dedicated process exit so the supervising npm launcher replaces and restarts the native binary.
- Run compiled and trusted extension migrations with exact from/to versions before committing the new version, retaining retry safety after failure.

## Child DOX Index

No child DOX files.
