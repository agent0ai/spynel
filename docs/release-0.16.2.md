# Spynel 0.16.2

Assistant progress is now on by default in the TUI, Telegram and WhatsApp. `/verbose` reports the shared setting; `/verbose on` and `/verbose off` apply live across conversations and instances using the same configuration.

The automatic option is removed. Existing saved `channels.verbose: auto` is read as `on` without rewriting the file; the next ordinary settings save writes the normalized value. An explicit `off` remains off, and unrelated settings are preserved.

Telegram and WhatsApp previews no longer add a Spy heading. Model-authored wording and native formatting remain intact. Final replies, errors, attachments, notifications, throttling, retained history and explicit CLI/API streams keep their existing behavior. Only eligible assistant progress is shown; private reasoning, tool payloads and background jobs remain excluded.

Worker protocol is 19 because old workers retain the obsolete auto representation in canonical settings digests. Existing update coordination replaces compatible-installation idle workers after work and pending delivery drain; active work blocks incompatible replacement. Older installations without that capability retain the [one-time bootstrap instructions](getting-started.md#first-upgrade-from-an-older-worker).

Publication does not update or restart an existing installation. Local and native synthetic checks do not establish real Telegram/WhatsApp delivery.
