# Spynel 0.16.0

`/verbose on|off|auto` controls assistant progress across every conversation and instance sharing the same Spynel configuration. Bare `/verbose` reports the current setting.

- `auto` is the default: progress in the TUI, final responses only in Telegram and WhatsApp.
- `on` shows assistant progress and final responses in all three channels.
- `off` hides progress in all three while keeping final responses, command replies, errors and attachments visible.

The setting persists as `channels.verbose` and applies live without restarting a turn. Turning it on shows subsequent progress without replaying suppressed content. It does not change retained history, explicit CLI/API streaming, model reasoning or response length, authorization, or task notifications. Only user-facing assistant output is eligible; private reasoning, tool details and background jobs are excluded. Providers must emit intermediate output for it to appear.

Remote previews use bounded, throttled native edits. Requests already in flight can finish after a toggle, and rejected or ambiguous edits retain the transport's normal delivery limits.

This release also includes recovery when a saved Codex rollout is missing and `/new` for starting a fresh Telegram or WhatsApp conversation. It preserves the forwarding attribution and responsive commands from 0.15.x.

Worker protocol is now 18. Before upgrading an installation with an older incompatible worker, drain jobs and pending delivery, explicitly stop it, then update and relaunch. Publication does not update or restart an existing installation.

Native packages support Linux and macOS on amd64 and arm64. Channel and provider checks use synthetic fixtures; live Telegram/WhatsApp delivery and real provider sessions were not exercised for this release.
