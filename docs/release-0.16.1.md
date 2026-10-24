# Spynel 0.16.1 candidate

Ordinary updates can replace an idle worker when its compatibility is unknown or differs from the target, then reconnect through the existing application launcher. This includes standalone updates whose GitHub release metadata does not advertise a worker protocol. Known-compatible workers keep running.

Replacement waits for active work and retained delivery to finish. An installation-wide admission fence prevents new work or worker launch from racing replacement across workspaces. A failed installation resumes an unchanged worker; the npm launcher restores service only after verifying the installed package and executable. Identity, ownership and ready-generation checks remain mandatory.

There is a one-time bootstrap limit: publication cannot change an old updater's preflight or add atomic pause support to an already-running old worker. Affected installations must first drain work and pending delivery, deliberately stop, externally install the complete release through their original installation method and relaunch. A compatible npm upgrade may retain an old worker until that fresh launch. See [the bootstrap instructions](getting-started.md#first-upgrade-from-an-older-worker), including the cross-installation scope of `killall`.

Worker protocol remains 18; the installation-control endpoint is additive. This candidate has not been published or deployed. Native qualification and independent acceptance must finish before release; local synthetic tests do not establish real Telegram/WhatsApp delivery or repair of existing VPSs.
