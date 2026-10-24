# Spynel 0.15.0 release notes

Forwarded Telegram and WhatsApp messages now retain their attribution in agent context, using `[timestamp] user (@alice) forwarded from (@bob): message`. The actual sender remains distinct from the forwarded source, including for media, captions, voice transcription, saved history, queued follow-ups and recovery.

Telegram uses the origin supplied by the platform, including hidden-user names and chat/channel sources. Ordinary WhatsApp forwards do not expose their original author and show `(unknown)`; explicit newsletter metadata can supply a channel name. Attribution never changes sender authorization or reply routing, and existing history is not backfilled.

This release preserves the configurable speech idle timeout introduced in 0.14.2 and its worker protocol 17. Upgrades from older incompatible workers retain the existing requirement to drain jobs and pending delivery, stop the older installation, then update and relaunch.

Private Unix socket cleanup now retains file ownership through shutdown so inode reuse cannot cause it to delete a replacement authentication descriptor.

Native packages support Linux and macOS on amd64 and arm64. Automated forwarding checks use synthetic provider events; live Telegram/WhatsApp delivery and actual speech-model processing were not exercised for this change.
