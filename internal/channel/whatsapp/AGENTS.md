# WhatsApp Channel DOX

## Purpose

- Own WhatsApp device pairing, live authorization, inbound/outbound messages, attachments, activity, and proactive delivery.

## Local Contracts

- Canonicalize phone-number authorization to international digits, resolve self-chat LIDs before authorization and conversation keying, and re-check the live allow-list at every pairing, event, provider, and delivery boundary.
- A changed live allow-list invalidates the old adapter at its next authorization check, including between encrypted upload and message send; the supervisor constructs the replacement. Partial native sends retain ordinary retry semantics.
- Distinguish socket pairing readiness from an authenticated connected device; expose only validated phone-number identities and retain QR data solely in the fullscreen pairing flow.
- Stream bounded encrypted media and deliver only the final response/error plus validated directives. Translate the canonical communication-agent activity lifecycle into composing for ordinary and proactively routed recovery turns, keep framework-only and eventless intake silent, and pause before terminal delivery.
- Carry the authenticated chat/stanza tuple as private stable source correlation through application acceptance so duplicate transport events cannot duplicate work.
- Proactive delivery and canonical terminal events upload validated encrypted media before text and propagate upload/send errors. Derive stable per-file IDs in a separate attachment domain from the delivery ID and pass them through `SendRequestExtra.ID`; ordinary replies keep provider-generated IDs. Notification delivery sends no composing/pause presence. Stable IDs do not prove user receipt.

## Child DOX Index

No child DOX files.
