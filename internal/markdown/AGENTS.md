# Markdown Rendering DOX

## Purpose

- Own GitHub-flavored Markdown conversion for ANSI terminals, Telegram HTML, and WhatsApp-native formatting.

## Local Contracts

- Sanitize terminal control behavior while preserving supported formatting, compact code-block boundaries, and explicit absolute-file or URI hyperlinks.
- Do not invent bases for relative links or let one theme's syntax palette leak into another render.
- Keep transport escaping and length behavior deterministic; rendering must not mutate persisted Markdown source.

## Child DOX Index

No child DOX files.
