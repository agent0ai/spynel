# Media DOX

## Purpose

- Own bounded attachment storage and outbound directives plus local speech decoding, model acquisition, and transcription.

## Local Contracts

- The pinned native runtime initializes before Go. On Linux ARM64, a native ELF `fwrite` interposition filters only the exact three-chunk zero-vendor startup warning, passes changed/unrelated diagnostics through, and disables filtering in the executable constructor. Do not redirect stderr, alter CPU detection, or mutate shared module-cache libraries. Keep native constructor and real recognizer-failure subprocess checks; remove this workaround when the pinned runtime no longer emits the warning.
- Stream media under configured limits into private files, revalidate opened outbound files against symlink, type, readability, and concurrent-growth constraints, and keep ordinary links inert.
- Decode supported audio to mono 16 kHz float PCM and serialize transcription through one process-wide worker; preserve originals and make failures visible without invoking Python, FFmpeg, or external ASR tools.
- Coordinate model cache installation across processes, enforce pinned size/hash/archive safety and compatibility markers, and atomically publish only complete private model directories.

## Child DOX Index

Direct child DOX files:

| Child | Scope |
| --- | --- |
| [miniaudio/AGENTS.md](miniaudio/AGENTS.md) | Pinned miniaudio decoder bridge and license. |
