# Telegram Channel DOX

## Purpose

- Own Telegram polling/webhook intake, live authorization, media handling, activity, identity mapping, and outbound delivery.

## Local Contracts

- Polling and webhook intake enqueue authorized/addressed updates into the common bounded ordinary/command lanes before media preparation. Webhook overflow returns 503 for provider retry; polling overflow returns an explicit retry response. Recheck sender/live authorization on dequeue and after preparation. Keep native source identity immutable and reply delivery state local; generation cancellation suppresses late preparation and responses.

- Re-resolve and validate the current allow-list at startup and every inbound/outbound/provider boundary; stale identity mappings or credentials never grant access, and authorization loss terminates useful traffic.
- A changed live allow-list invalidates the old adapter at its next authorization check, including between native files and captions; the supervisor constructs the replacement. Partial native sends retain ordinary retry semantics.
- Persist only minimal identity learned from an authenticated private update, keep webhook secrets and URLs out of status, and permit only teardown `deleteWebhook` after revocation.
- Stream bounded media privately, deliver only the final response/error plus validated directives, and keep typing references per chat, best-effort, serialized, and bounded. Translate proactively routed recovery activity through the same refresher and stop it before recovered terminal delivery.
- Carry the authenticated chat/message tuple as private stable source correlation through application acceptance so polling/webhook redelivery cannot duplicate work.
- Derive compact forwarding attribution only from top-level `forward_origin` (user, hidden user, chat or channel), or mark an automatic forward with missing origin as `unknown`. Prefer the source handle, then available first name/chat title, then native identifier. Never attribute a quoted/replied message's origin to the new message or use signatures as identities. Media, captions and transcriptions retain this display-only value after ordinary sender authorization.
- Proactive delivery and canonical terminal events upload every validated attachment through the ordinary bounded multipart sender before sending text. Return HTTP/API/upload errors; partial sends retain Telegram's at-least-once whole-message retry semantics and may duplicate earlier files.

## Child DOX Index

No child DOX files.
