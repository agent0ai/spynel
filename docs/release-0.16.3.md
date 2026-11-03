# Spynel 0.16.3

Telegram assistant progress moves to a new message after an accepted user message, a command response, or a notification interrupts the current preview. Final delivery retains text that had not reached the preview; Telegram does not send a separate completion notice.

`/model` and `/harness` show their current configured selections alongside the available choices. `/model` also reports the configured value when model discovery is unavailable.

This patch retains worker protocol 19. Telegram delivery remains subject to the provider's existing at-least-once retry behavior; a crash between delivery and its local receipt can duplicate a final response.
