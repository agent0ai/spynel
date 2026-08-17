# Updater DOX

## Purpose

- Own npm-install detection and bounded registry update checks for launcher-managed updates.

## Local Contracts

- Perform proactive ten-second npm checks only for interactive TUI starts; automatic services and noninteractive commands must not make immediate or periodic checks. Require the supervising launcher's explicit current-launch eligibility signal, accept its bounded initial-attempt timestamp and semantic latest-version snapshot, then let the eligible TUI refresh through the same checker no more than hourly.
- `/update install` coordinates the dedicated process exit so the supervising npm launcher replaces and restarts the native binary.

## Child DOX Index

No child DOX files.
