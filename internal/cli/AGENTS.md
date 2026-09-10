# CLI DOX

## Purpose

- Own public command parsing, process lifecycle, plain automation commands, static docs dispatch, and server-runtime composition.

## Local Contracts

- Route shared behavior through the same application service and local API as interactive channels; deterministic commands must not start a harness merely because no owner exists.
- Before an ownerless plain-CLI `/cleanup`, hold the shared election mutation boundary through the destructive operation and recheck that the primary lease is absent and the durable clean-release grace period has expired. If ownership appeared or failover is still fenced, join the healthy owner or fail closed during the transition; never run cleanup from a separate process-local service beside an owner or an already-open successor awaiting promotion.
- While running a workspace server/election, absorb SIGTSTP from startup through shutdown so terminal job control cannot suspend a shared primary. Unregister the signal channel afterward. Ctrl+Z has no shell-suspend action; explicit SIGSTOP cannot be prevented and still requires operator recovery. Never steal a fresh lease because readiness fails.
- Preserve bounded stdin, attachments, final/stream/NDJSON output contracts, strict active-turn follow-up checks, and platform-specific restart behavior.
- Keep `model`, `effort`, and `speed` aliases on the shared command path so noninteractive clients inspect, set, reset, and validate the same model properties as the TUI.
- Generate one private stable source-message identity before each CLI submission and retain it across loopback dispatch so retries cannot duplicate work.
- Keep `docs` offline and workspace-independent. `spynel notify` requires exactly one of explicit `--origin ORIGIN` or `--recent-authorized`, independently revalidates workspace/origin authorization through the application service, and remains one ordinary delivery command with no task-transition flags or task-log side effects. Recent routing exposes no resolved destination and fails closed on ambiguous authority/activity. Operator positional input may remain compatible, while generated transition-notification guidance still uses concrete `--workdir`, `--origin`, and `--message`. A notification agent records success, skip, or command failure by editing the task itself.
- Canonicalize bare interactive launch context before configuration discovery. An uninitialized child may enter its discovered parent or initialize locally only through the required pre-election choice; explicit and noninteractive commands retain deterministic non-prompting discovery.
- Poll owner shared state for TUI runtime, durable-work, and caller-scoped selected-conversation activity changes; translate latest activity counts through a bounded nonblocking bridge into balanced canonical TUI events, including startup and overlap counts larger than the output buffer, and leave bounded work diagnostics off the visual header.
- Keep asynchronous local recovery results durable in the named CLI/TUI conversation and surface explicitly marked recovery terminals to an already-open TUI through its exact startup-snapshot history boundary, preserving durable order and error role without treating them as acknowledged task notifications.
- Admit each TUI's selected conversation into the owner's renewable live-conversation lease boundary before reading its startup history, seed startup from the caller-scoped state returned by that registration rather than an earlier readiness snapshot, retain that lease for its complete interactive lifetime, and renew the displayed identity after switches.
- When an interactive TUI start observed an existing fresh primary, print one pre-alternate-screen connecting line followed by success or a sanitized actionable failure. Keep first-owner, headless, remote-channel, redirected, and automation startup output unchanged.

- Expose `events --conversation NAME [--after CURSOR]` as a long-lived single-attempt NDJSON subscription to an existing primary. `send`/`followup --request-id` retain client correlation; `send`, `followup` and `events --socket` explicitly select one private socket without local configuration/election discovery. JSON dispatched errors stay on protocol stdout with generic stderr failure, and continuing finals/errors do not end a request.
- `serve --socket PATH` adds the optional Unix listener only when this process acquires a new primary. Keep the loopback listener, all TUI attachment semantics and election fences. Headless primary terms opt into the Runtime operational stderr projection; TUI-hosting terms do not.

- Keep standalone bootstrap `install-bundle` independent of workspace initialization. Reuse updater ownership to route explicit update completion and ordinary restart through the stable installed entry point, preserving primary/job shutdown and init continuation arguments. Authorize proactive standalone checks only in an actual interactive TUI launch without automatic-startup or skip-check flags.
- An ownerless standalone update restarts into `version`; for NDJSON output, use `version --quiet` so the new executable runs successfully without appending plain text or another terminal event after the correlated update acknowledgment.

## Child DOX Index

No child DOX files.
