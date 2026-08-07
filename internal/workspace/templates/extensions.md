# Spynel Extensions

Each installed Git repository may expose `.spynel-extension.yaml`:

```yaml
name: example
hooks:
  message.received: ["./bin/example-hook", "message.received"]
  harness.before: ["./bin/example-hook", "harness.before"]
```

Hook executables receive one JSON object on stdin and may return:

```json
{"payload": {"text": "rewritten"}, "cancel": false, "message": "optional note"}
```

Available hooks are `message.received`, `harness.before`, `harness.after`, `task.claimed`, and `task.completed`. Hook commands execute with the extension repository as their working directory. Installing an extension grants it local code execution privileges; review it first.

