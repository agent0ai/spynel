# Telegram Channel DOX

## Purpose

- Own Telegram polling/webhook intake, live authorization, media handling, activity, identity mapping, and outbound delivery.

## Local Contracts

- Re-resolve and validate the current allow-list at startup and every inbound/outbound/provider boundary; stale identity mappings or credentials never grant access, and authorization loss terminates useful traffic.
- A changed live allow-list invalidates the old adapter at its next authorization check, including between native files and captions; the supervisor constructs the replacement. Partial native sends retain ordinary retry semantics.
- Persist only minimal identity learned from an authenticated private update, keep webhook secrets and URLs out of status, and permit only teardown `deleteWebhook` after revocation.
- Stream bounded media privately, deliver only the final response/error plus validated directives, and keep typing references per chat, best-effort, serialized, and bounded. Translate proactively routed recovery activity through the same refresher and stop it before recovered terminal delivery.
- Carry the authenticated chat/message tuple as private stable source correlation through application acceptance so polling/webhook redelivery cannot duplicate work.
- Proactive delivery and canonical terminal events upload every validated attachment through the ordinary bounded multipart sender before sending text. Return HTTP/API/upload errors; partial sends retain Telegram's at-least-once whole-message retry semantics and may duplicate earlier files.

## Child DOX Index

No child DOX files.
