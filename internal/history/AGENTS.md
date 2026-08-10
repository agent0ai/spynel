# Conversation History DOX

## Purpose

- Own append-oriented per-channel conversation history, bounded context rendering, discovery, clearing, and point-in-time branching.

## Local Contracts

- Keep each channel/conversation independent, store complete durable JSONL with private permissions, and tolerate only explicitly handled tail corruption.
- Build prompts backward from disk under both message and character limits; never load an unbounded history to compute bounded context.
- Branch into a new conversation without mutating its source, retain bounded reply references, and exclude private attachment contents from records.

## Child DOX Index

No child DOX files.
