# Media DOX

## Purpose

- Own bounded attachment storage and outbound directives plus local speech decoding, model acquisition, and transcription.

## Local Contracts

- Stream media under configured limits into private files, revalidate opened outbound files against symlink, type, readability, and concurrent-growth constraints, and keep ordinary links inert.
- Decode supported audio to mono 16 kHz float PCM and serialize transcription through one process-wide worker; preserve originals and make failures visible without invoking Python, FFmpeg, or external ASR tools.
- Coordinate model cache installation across processes, enforce pinned size/hash/archive safety and compatibility markers, and atomically publish only complete private model directories.

## Child DOX Index

Direct child DOX files:

| Child | Scope |
| --- | --- |
| [miniaudio/AGENTS.md](miniaudio/AGENTS.md) | Pinned miniaudio decoder bridge and license. |
