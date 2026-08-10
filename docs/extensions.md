# Extensions and hooks

Spynel uses portable executable hooks instead of Go's platform/toolchain-coupled plugin mechanism. Install repositories only after review:

```bash
spynel extension install GIT_URL [NAME]
spynel extension list
spynel extension remove NAME
```

Each repository declares `.spynel-extension.yaml`:

```yaml
name: redact-and-audit
hooks:
  message.received: ["./bin/hook", "message.received"]
  harness.before: ["./bin/hook", "harness.before"]
  harness.after: ["./bin/hook", "harness.after"]
  task.claimed: ["./bin/hook", "task.claimed"]
  task.completed: ["./bin/hook", "task.completed"]
```

Spynel runs commands from the extension root with `SPYNEL_HOOK` and `SPYNEL_EXTENSION` environment variables. One JSON line arrives on stdin:

```json
{"hook":"message.received","payload":{"channel":"telegram","text":"hello"}}
```

`task.completed` payloads also carry a stable `event_id`. Delivery is at least once: Spynel durably records each extension only after the hook exits successfully, and retries an unrecorded hook after failure or restart with the same event ID. A hook can therefore run more than once even when an earlier process already produced effects. Consumers of `task.completed` must persistently deduplicate every externally visible effect by `event_id`; an in-memory check is not sufficient. Spynel does not claim exactly-once execution of arbitrary extension side effects.

Empty stdout preserves the payload. A JSON result can replace it, cancel the operation, or provide a local message:

```json
{"payload":{"text":"rewritten"},"cancel":false,"message":"optional explanation"}
```

Hooks run sequentially in sorted extension-name order and each receives the previous hook's payload. Nonzero exit, timeout, invalid JSON, or protocol stdout beyond 1 MiB fails the surrounding operation instead of silently ignoring enforcement. Stderr is diagnostic rather than protocol data: Spynel retains at most 64 KiB for a failed hook, sends it through the runtime log's redaction and entry-boundary controls, and never includes it in the user-facing hook error.

The supported lifecycle names are exactly `message.received`, `harness.before`, `harness.after`, `task.claimed`, and `task.completed`. Discovery rejects any other hook name so a typo or removed hook cannot remain silently inactive.

The current hook model changes messages and lifecycle behavior. Adding a future compiled-in channel or harness still requires implementing the corresponding Go interface; a future extension RPC protocol can expose those registries without weakening hook portability.
