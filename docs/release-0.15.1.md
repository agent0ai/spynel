# Spynel 0.15.1

Telegram and WhatsApp now keep recognized text commands such as `/status` and `/restart` responsive while an earlier message is downloading media, transcribing speech or being processed. Ordinary messages remain ordered, and commands retain their existing authorization, routing and behavior.

Restart and shutdown no longer wait for an in-flight native transcription to finish. Cancelled channel generations discard late results; native resources remain owned until inference returns or the process exits.

Both intake lanes are bounded. A command's own extension, provider or filesystem work can still take time. This release preserves the forwarding attribution introduced in 0.15.0 and worker protocol 17.
