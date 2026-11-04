# Spynel 0.16.4

`/models` now aliases `/model`: it shows the current configured model alongside available choices and uses the same selection path. The configured value remains visible when model discovery is unavailable.

This patch includes the Telegram streaming behavior published in 0.16.3: accepted user messages freeze the prior preview, subsequent output starts a new message, and successful completion sends no auxiliary notice. Worker protocol remains 19.
