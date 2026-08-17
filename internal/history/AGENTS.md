# Conversation History DOX

## Purpose

- Own append-oriented per-channel conversation history, bounded context rendering, discovery, clearing, and point-in-time branching.

## Local Contracts

- Keep each channel/conversation independent, store complete durable JSONL with private permissions, and tolerate only explicitly handled tail corruption.
- Build prompts backward from disk under both message and character limits; never load an unbounded history to compute bounded context.
- Branch into a new conversation without mutating its source, retain bounded reply references, and exclude private attachment contents from records.
- Append a recovery baseline to every branch so copied source messages remain context only and can never become stalled-message candidates in the new conversation.
- Retention uses the last durable entry timestamp, falling back to file modification time only for empty histories; delete only strict pre-cutoff regular files and preserve explicitly protected live conversations while reporting per-item failures.
- Recent-authorized proactive routing reads a content-free durable latest-`user` activity sidecar per conversation, ignores assistant, notification, and delivery-ledger entries so delivery cannot select itself, and ignores sidecars without a corresponding history. This sidecar is only the minimal channel-resolution primitive; it carries no reminder policy or state.
- Keep source-message identity, local acceptance time, provider-neutral admission, logical execution, exact terminal/cancellation coverage, retrigger reservation, and local recovery-display facts append-only and private. The recovery activation marker is forward-only: create it without reading or rewriting existing histories, and compare it to local acceptance rather than transport-provided message time so delayed newly admitted messages remain eligible; missing or pre-activation correlation is categorically ineligible. Recovery and duplicate-identity reads are strict and bounded; classify the complete admitted bound and fail closed on corruption or overflow rather than dispatching from a partial tail.

## Child DOX Index

No child DOX files.
