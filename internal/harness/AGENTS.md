# Harness Adapter DOX

## Purpose

- Own the provider-neutral harness interface, catalog/factories, transactional supervisor, and Codex, Claude Code, Agent Zero CLI, Pi, and ACP process adapters.

## Local Contracts

- Keep application and orchestration code harness-neutral; adapters map native events into the common response and follow-up contract.
- Declare native-steer versus queued follow-up behavior explicitly, fence active executions, and invoke durable iteration reservation exactly once immediately before accepted provider delivery.
- Expose the provider-neutral new/steered/queued conversation admission classification at the same session fence so application correlation can cover consolidated terminal responses exactly.
- Order forward-looking model commits and dispatch snapshots under one supervisor fence. Never mutate an already-admitted provider turn; native steering remains part of that turn, while queued continuations and later top-level turns snapshot the newest model at their own provider-dispatch admission.
- Extend that fence to reasoning effort and service mode. Codex reads per-model effort/service tiers and passes `effort`/`serviceTier`; Claude uses only verified `--effort`; Pi identifies its current/default model and current thinking level from `get_state`, queries each catalog model's exact RPC thinking levels in a separate no-session process selected by the non-persistent `--model` runtime override, labels a default effort only for the authoritative current model, and passes `--thinking`; never use Pi's persistent `set_model` RPC for capability inspection. Preserve Pi's historical acceptance/clamping only for the provenance-marked legacy omitted effort while validating explicit values strictly. ACP model selection remains supported, but reasoning and speed are unsupported because ACP advertises optional thought choices only after session creation and defines no standard speed category.
- Build commands without shell interpolation, bound scanner/event payloads, surface startup and negotiation diagnostics, and keep portable fixture providers synthetic.
- Keep Agent Zero CLI as the fixed `agent-zero` profile backed by the shared ACP adapter, launch `a0 acp`, and require a bounded successful `a0 acp --check` before reporting it available.

## Child DOX Index

No child DOX files.
