# Core Value Types DOX

## Purpose

- Own stable cross-package message, response, screen, status, and formatting value types that prevent package cycles.

## Local Contracts

- Keep types provider- and transport-neutral; implementation dependencies belong in consuming packages.
- Preserve bounded and optional serialization semantics, including reply references and attachment directives, without embedding secrets.
- Keep compact count formatting deterministic for constrained status surfaces.

## Child DOX Index

No child DOX files.
