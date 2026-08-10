# Filesystem Publication DOX

## Purpose

- Own cross-platform atomic replacement and no-clobber publication primitives.

## Local Contracts

- Write complete temporary content in the destination directory, apply requested private permissions, synchronize as required, and rename only after successful preparation.
- No-clobber publication must never overwrite a concurrently created target; platform-specific replacement files preserve equivalent observable semantics.
- Callers own content validation and higher-level rollback; this package owns filesystem publication integrity.

## Child DOX Index

No child DOX files.
