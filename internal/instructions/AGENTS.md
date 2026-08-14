# Persistent Instruction Loader DOX

## Purpose

- Own exact-once framework guidance composition plus fixed-role loading and prompt-section rendering for workspace-local persistent agent instructions.

## Local Contracts

- Resolve only the five canonical role files, load them fresh for each matching invocation, cap them at 64 KiB, require valid UTF-8 and safe regular-file boundaries, and reject symlinks or unsafe permissions.
- Missing files produce explicit empty sections; read or validation failure affects only the relevant session and never yields partial instructions.
- Never derive paths from messages, workflow evidence, or user content, and never log instruction bodies.
- Inject the same framework-owned scope-discipline rule exactly once for every role before appending workspace-owner instructions; it governs proportionality without weakening explicit user, safety, authorization, lifecycle, review, evidence, or data-handling requirements.
- Keep framework-owned chat guidance effective for stock and preserved custom prompt templates and inject it exactly once. The evidence-grounded honesty section must forbid lying, fabricated evidence, invented causes, false inspection implications, and unverified facts; distinguish observation/evidence, inference, uncertainty, and unknowns; require proportionate authoritative checks and honest “I don't know yet” handling; and cover evidence-based corrections without claiming perfect model behavior. Require contextual confidence before interpreting speech-transcribed Spynel variants so unrelated literal meanings remain unchanged.

## Child DOX Index

No child DOX files.
