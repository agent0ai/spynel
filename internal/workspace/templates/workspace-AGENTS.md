# Spynel Workspace DOX

This directory is durable state owned jointly by the user, Spynel, and dispatched agents.

## Contracts

- Markdown documents are the source of truth. Keep their body human-readable and front matter machine-readable.
- Do not edit `.spynel/runtime/leases/`; Spynel owns processing leases and recovery bookkeeping.
- Do not edit or publish `.spynel/history/`, `.spynel/attachments/`, `whatsapp.db`, or harness session data; they may contain secrets, remote media, and private conversations.
- `.spynel/models/whisper/` contains downloaded local speech-model weights and the verified whisper.cpp runtime cache. `.spynel/runtime/speech/` is bounded transient processing space owned by Spynel; agents must not use it for durable artifacts.
- Prompts in `.spynel/prompts/` are user-overridable runtime contracts.
- Extensions in `.spynel/extensions/` are trusted executable code. Review repositories before installing them.
- Agents processing a file must record progress in the file, choose a configured next status, update `status` and `updated_at`, and move the file into the matching folder.

## Structure

- `tasks/` owns short-lived work items.
- `goals/` owns recurring or long-lived objectives.
- `prompts/` owns channel, task, goal, and recovery lead messages.
- `extensions/` owns installed Git extensions and hook manifests.
- `history/` owns independent append-only histories per channel and conversation.
- `attachments/` owns private TUI, Telegram, and WhatsApp media referenced from messages.
- `models/` owns reusable local model weights downloaded by configured processors.
- `runtime/` owns Spynel leases, harness session mappings, and transient bounded work files.
