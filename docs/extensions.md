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

Empty stdout preserves the payload. A JSON result can replace it, cancel the operation, or provide a local message:

```json
{"payload":{"text":"rewritten"},"cancel":false,"message":"optional explanation"}
```

Hooks run sequentially in sorted extension-name order and each receives the previous hook's payload. Nonzero exit, timeout, or invalid JSON fails the surrounding operation instead of silently ignoring enforcement.

The canonical lifecycle names are `harness.before` and `harness.after`. Version-one manifests using `recipient.before` or `recipient.after` remain supported as compatibility aliases.

The current hook model changes messages and lifecycle behavior. Adding a future compiled-in channel or harness still requires implementing the corresponding Go interface; a future extension RPC protocol can expose those registries without weakening hook portability.
