# Updater DOX

## Purpose

- Own npm-install detection and bounded registry update checks for launcher-managed updates.

## Local Contracts

- Perform the ten-second npm check only for interactive TUI starts; automatic services and noninteractive commands must not make it.
- `/update install` coordinates the dedicated process exit so the supervising npm launcher replaces and restarts the native binary.

## Child DOX Index

No child DOX files.
