# WhatsApp Channel DOX

## Purpose

- Own WhatsApp device pairing, live authorization, inbound/outbound messages, attachments, activity, and proactive delivery.

## Local Contracts

- After actual-sender and group/self-chat validation, place recognized text-only commands in the separate bounded command lane. Both lanes recheck the generation and live authorization, preserve the authenticated stanza and provenance, and join on shutdown. Overflow retains the existing rejected-intake behavior with content-free logging; no per-message goroutine or extra harness session is created.

- Canonicalize phone-number authorization to international digits, resolve self-chat LIDs before authorization and conversation keying, and re-check the live allow-list at every pairing, event, provider, and delivery boundary.
- A changed live allow-list invalidates the old adapter at its next authorization check, including between encrypted upload and message send; the supervisor constructs the replacement. Partial native sends retain ordinary retry semantics.
- Distinguish socket pairing readiness from an authenticated connected device; expose only validated phone-number identities and retain QR data solely in the fullscreen pairing flow.
- Stream bounded encrypted media and default to final response/error plus validated directives (global verbose on also enables bounded assistant progress). Translate the canonical communication-agent activity lifecycle into composing for ordinary and proactively routed recovery turns, keep framework-only and eventless intake silent, and pause before terminal delivery.
- Carry the authenticated chat/stanza tuple as private stable source correlation through application acceptance so duplicate transport events cannot duplicate work.
- Read forwarding only from the accepted unwrapped message's own context, using `isForwarded`, positive `forwardingScore`, explicit `forwardOrigin`, or forwarded-newsletter metadata. Ordinary forwards provide no original author and display `unknown`; newsletter forwards may use their supplied channel name, otherwise `unknown` without exposing opaque newsletter JIDs. Never treat reply participant, remote JID, quoted-message context or body text as forwarding source. Preserve attribution through media preparation and voice transcription without altering sender authorization or routing.
- Proactive delivery and canonical terminal events upload validated encrypted media before text and propagate upload/send errors. Derive stable per-file IDs in a separate attachment domain from the delivery ID and pass them through `SendRequestExtra.ID`; ordinary replies keep provider-generated IDs. Notification delivery sends no composing/pause presence. Stable IDs do not prove user receipt.

- Resolve global verbosity from shared live settings without changing the adapter fingerprint. Verbose on uses one bounded native-formatted preview and the installed SDK edit builder, with live authorization on send/edit. Final/error delivery replaces its first chunk when possible and falls back to ordinary sending on edit failure; files retain stable delivery identities. Framework command replies never consume an active preview.

## Child DOX Index

No child DOX files.
