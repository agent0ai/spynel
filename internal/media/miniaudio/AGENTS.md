# Miniaudio Bridge DOX

## Purpose

- Own the pinned miniaudio C source, license, and CGO decoder bridge for WAV, FLAC, and MP3 speech inputs.

## Local Contracts

- Preserve upstream license and source provenance; update the C source, header, bridge, and supported-format tests together.
- Validate decoder lengths and channel/rate conversions before exposing samples to Go; do not permit unbounded allocation from media metadata.
- This bridge produces PCM for the parent media package and does not own model loading or transcription.

## Child DOX Index

No child DOX files.
