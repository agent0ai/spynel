# Telegram Channel DOX

## Purpose

- Own Telegram polling/webhook intake, live authorization, media handling, activity, identity mapping, and outbound delivery.

## Local Contracts

- Re-resolve and validate the current allow-list at startup and every inbound/outbound/provider boundary; stale identity mappings or credentials never grant access, and authorization loss terminates useful traffic.
- Persist only minimal identity learned from an authenticated private update, keep webhook secrets and URLs out of status, and permit only teardown `deleteWebhook` after revocation.
- Stream bounded media privately, deliver only the final response/error plus validated directives, and keep typing references per chat, best-effort, serialized, and bounded. Translate proactively routed recovery activity through the same refresher and stop it before recovered terminal delivery.
- Carry the authenticated chat/message tuple as private stable source correlation through application acceptance so polling/webhook redelivery cannot duplicate work.

## Child DOX Index

No child DOX files.
