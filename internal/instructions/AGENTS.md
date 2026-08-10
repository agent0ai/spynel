# Persistent Instruction Loader DOX

## Purpose

- Own fixed-role loading and prompt-section rendering for workspace-local persistent agent instructions.

## Local Contracts

- Resolve only the five canonical role files, load them fresh for each matching invocation, cap them at 64 KiB, require valid UTF-8 and safe regular-file boundaries, and reject symlinks or unsafe permissions.
- Missing files produce explicit empty sections; read or validation failure affects only the relevant session and never yields partial instructions.
- Never derive paths from messages, workflow evidence, or user content, and never log instruction bodies.

## Child DOX Index

No child DOX files.
