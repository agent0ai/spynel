# Agent-readable documentation

`spynel docs` is the stable offline interface for product behavior. It is compiled into the binary, never invokes Codex or Claude Code, and does not require an initialized workspace, primary server, local API, or network connection.

## Interface and schema

The index, topic lookup, and search forms are:

```text
spynel docs [page NUMBER] [--format text|json]
spynel docs TOPIC [page NUMBER] [--format text|json]
spynel docs search QUERY [page NUMBER] [--format text|json]
```

Ordinary documents render whole. Oversized documents split only at topic/section/result boundaries under a deterministic 10,000-token page budget, conservatively estimated as one token per three Unicode runes. A record larger than that budget remains intact rather than splitting its paragraph, code block, table, references, or JSON object. Split pages report the applied budget and page estimate. A separate maximum of 128 entries, 64 KiB, and 48 Ki-runes bounds every representation. Text is deterministic Markdown without terminal escapes. JSON uses `schema_version: spynel.docs/v1` and returns `kind`, stable topic/section `id` values, `title`, `summary` or `content`, `related` references, and page number/total/entry/byte/rune metadata. Error documents use `kind: error` with a stable code, actionable message, optional suggestion, and valid topic IDs. Invalid requests exit nonzero.

Static content is classified as `user-command`, `workflow-contract`, `implementation-architecture`, or `runtime-state`. The last classification documents how to query live state; it never embeds current values.

## Ownership and safe content

The canonical catalog and schema live in `internal/agentdocs`. Content is curated Go data rather than a runtime scan of repository Markdown. Never add histories, recipient/session identifiers, notification origins, leases containing private identifiers, tokens, credentials, arbitrary environment values, or unreviewed workspace files. Link readers to typed `status`, `jobs`, `logs`, or durable task/goal files when current state is required.

To add or change a topic:

1. Use a lowercase stable topic ID and lowercase stable section IDs; IDs are public references and must not be renamed casually.
2. Classify the topic, keep sections concise, and add only resolvable topic or `topic#section` references.
3. If the topic belongs in concise channel help, assign its short help ID/summary and keep `/help` focused.
4. Update the matching README and CLI/configuration/architecture documentation and the nearest DOX contracts when ownership or behavior changes.
5. Add or update catalog, output, prompt, and command-catalog tests. Run `go test ./...`, `go vet ./...`, `go build ./cmd/spynel`, `scripts/smoke.sh`, and `git diff --check` before release.

`Validate` rejects duplicate/invalid IDs and broken references. CLI tests must also ensure every documented command is present in the executable or shared slash-command catalog. Release review compares behavior, `/help`, prompts, repository documentation, and the embedded catalog so none becomes an isolated source of truth.
