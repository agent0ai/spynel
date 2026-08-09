# Coding harness compatibility

This document is the compatibility baseline for Spynel's built-in coding
harnesses. It separates provider documentation, behavior exercised by Spynel's
deterministic tests, and behavior that still needs a real-provider or native-OS
canary. Codex and Claude documentation was retrieved on **2026-08-07**; Pi and
ACP documentation and registry manifests were retrieved on **2026-08-08**.

Any future authenticated or native-provider run must follow the separately
reviewed [provider-canary threat model and authorization plan](provider-canary-threat-model.md).
That plan is a gate, not evidence that a canary has run.

## Evidence labels and integration boundary

- **Documented** means the provider currently describes the public interface.
- **Tested** means a deterministic Spynel test exercises the adapter contract.
- **Observed** means a command or implementation was inspected on the audit
  host, but no authenticated provider request was made.
- **Unverified** means neither the repository nor a recorded canary establishes
  the behavior on the claimed provider version and operating system.
- **Unsupported** means Spynel deliberately has no integration for the surface.

Spynel integrates with Codex app-server, Claude Code non-interactive print
mode, Pi JSONL RPC, and stable ACP v1 JSON-RPC over stdio. The
provider-neutral contract is `Harness`, with optional model and follow-up
capabilities ([implementation](../internal/harness/harness.go)); registration,
aliases, fixed arguments, and executable discovery are centralized in the
[catalog](../internal/harness/catalog.go).

The native desktop surface that OpenAI currently documents as the **ChatGPT
desktop app**, including its Codex workspace, is not a Spynel harness. The
[desktop app documentation](https://developers.openai.com/codex/app/) describes
an interactive application and no supported local automation endpoint for
third-party clients. OpenAI separately documents
[app-server](https://developers.openai.com/codex/app-server/) for embedding rich
Codex clients and recommends the SDK for job automation. Spynel therefore does
not discover an app bundle, attach to the app, read its private data, or claim
that installing the macOS app satisfies the `codex` CLI prerequisite. Shared
branding and shared authentication options do not make those surfaces
interchangeable.

## Version baseline

| Surface | Spynel boundary | Minimum compatible version | Current audit evidence | Support statement |
| --- | --- | --- | --- | --- |
| Codex CLI/app-server | `codex app-server --stdio`; documented JSONL request/response and notification methods | **Not established.** No primary evidence establishes one numeric lower bound. Spynel instead requires the documented initialize result and validates each consumed method/result/terminal shape at its earliest safe use. | `codex-cli 0.147.0` was found at `/usr/bin/codex` on Linux arm64; `codex app-server --help` exposed stdio and schema tooling. Synthetic current and incompatible schema variants pass; no authenticated request was sent. | Supported implementation boundary with fail-closed capability diagnostics; provider-version compatibility remains provisional until versioned canaries exist. See [Codex adapter](../internal/harness/codex.go), [variant tests](../internal/harness/compatibility_test.go), and the official [app-server protocol](https://developers.openai.com/codex/app-server/). |
| Claude Code CLI | `claude -p` with `stream-json` output and either text or `stream-json` input | **Not established.** No primary evidence establishes one numeric lower bound. At startup Spynel requires every CLI flag needed by the configured mode from `claude --help`; the first turn must negotiate the documented `system/init` event with `session_id`. | Claude Code `2.1.224` was found at `/root/.local/bin/claude` on Linux arm64; `claude --help` exposed every flag Spynel uses. Synthetic current and incompatible help/stream variants pass; no authenticated request was sent. | Supported implementation boundary with fail-closed flag and stream-initialization diagnostics; provider-version compatibility remains provisional until versioned canaries exist. See [Claude adapter](../internal/harness/claude.go), [variant tests](../internal/harness/compatibility_test.go), and Anthropic's [programmatic-use documentation](https://code.claude.com/docs/en/headless). |
| Pi coding agent | `pi --mode rpc`; documented JSONL commands and events | **Not established.** Spynel requires a non-empty `--version`, successful `get_state`, and both queue-mode configuration commands. | `@earendil-works/pi-coding-agent` `0.84.1` was installed on Linux arm64. Keyless `PI_OFFLINE=1` RPC negotiation succeeded; synthetic lifecycle, steering, cancellation, model, and resume tests pass. No model request was sent. | Supported native RPC boundary; authenticated model/tool behavior remains unverified. See the [Pi adapter](../internal/harness/pi.go), [tests](../internal/harness/pi_test.go), and official [RPC documentation](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/rpc.md). |
| ACP v1 agents | Newline-delimited JSON-RPC 2.0 over a launched command's stdin/stdout | Protocol version **1**, negotiated by `initialize`; there is no numeric minimum agent release. | Synthetic initialize, session setup/resume, config-option, streaming, permission, cancellation, persistence, and shell-free argument tests pass. Alias commands were checked against official ACP registry manifests; no alias executable or authenticated provider was run. | Supported shared adapter for catalog aliases and custom commands. Stable v1 stdio is supported; draft HTTP transport is deliberately unsupported. See the [ACP adapter](../internal/harness/acp.go), [tests](../internal/harness/acp_test.go), official [transport](https://agentclientprotocol.com/protocol/v1/transports), and [initialization](https://agentclientprotocol.com/protocol/v1/initialization) specifications. |
| ChatGPT desktop app / Codex workspace | None | Not applicable | The app was not available on the Linux audit host. Official app documentation was reviewed; it does not document an app-local automation interface usable by Spynel. | **Unsupported as an integration surface.** Users need a separately discoverable `codex` CLI. See the [catalog](../internal/harness/catalog.go) and official [desktop app page](https://developers.openai.com/codex/app/). |

“Current audit evidence” is deliberately narrower than “certified provider
version.” Capability checks prove only the surfaces they exercise; they do not
prove authentication, tool execution, or provider-side session recovery. An
incompatible executable now fails at startup or the earliest documented
negotiation point, before a new or resumed session identifier is overwritten.

## Pi and ACP lifecycle coverage

The older matrix below remains the detailed Codex/Claude/desktop comparison.
The newer adapters have this narrower evidence:

| Capability | Pi RPC | ACP v1 and aliases |
| --- | --- | --- |
| Startup and transport | **Tested:** exact executable/cwd launch, bounded `--version`, strict JSONL, `get_state`, and queue modes. Non-JSON stdout fails closed. | **Tested:** exact command plus argument vector, absolute cwd, JSON-RPC 2.0 framing, and protocol-version-1 initialize. ACP stable transport is stdio; custom URL configuration is intentionally absent. |
| Streaming and completion | **Tested:** text deltas and authoritative assistant-message endings are aggregated; only `agent_settled` is terminal because `agent_end` may precede retry or continuation. | **Tested:** `session/update` agent chunks and tool status stream until the `session/prompt` response supplies a stop reason. |
| Follow-up and interruption | **Tested native steer:** `steer` transfers output to the newest emitter; `abort` interrupts. | **Tested queue/cancel:** ACP v1 has no prompt-steer method. Accumulated adjacent chat messages form one ordered next prompt; `/stop` sends `session/cancel`. |
| Resume and settings | **Tested:** session-file path and policy fingerprint persist; model/thought/tool choices become RPC CLI flags. | **Tested:** opaque session IDs persist; advertised `session/resume` is preferred over `session/load`; model/thought overrides use category-matched config options. |
| Permissions | **Tested mapping:** read-only enables only Pi read/search tools. Pi provides no OS sandbox through RPC. | **Tested mapping:** Spynel advertises no filesystem/terminal client callbacks. Read-only auto-rejects unsafe permission requests; broader modes choose one-time allowance. Agents that skip permission requests remain trusted local processes. |

## Synthetic protocol variants

The portable fixture process and compatibility suite are labelled
`codex-app-server-public-schema-retrieved-2026-08-07`,
`claude-code-stream-json-docs-retrieved-2026-08-07`,
`pi-jsonl-rpc-docs-retrieved-2026-08-08`, and
`acp-stable-v1-schema-retrieved-2026-08-08`. They exercise supported lifecycle
shapes plus representative missing methods/flags/identity fields, renamed
fields, changed initialization events/versions, and changed terminal statuses.
Incompatible session and initialization cases assert that no invalid mapping
is persisted.

These are synthetic public-interface snapshots, not captured authenticated
transcripts. Update both labels and affected variants whenever the retrieval
date or a consumed public shape changes; add a failing variant before accepting
a new shape. See the [fixture process](../internal/harness/process_fixture_test.go)
and [compatibility decisions](../internal/harness/compatibility_test.go).

## Lifecycle matrix

Every row below links both the Spynel implementation or test evidence and the
provider's public documentation. “Tested” refers to deterministic fixtures on
the audit host unless stated otherwise.

| Lifecycle capability | Codex CLI/app-server | Claude Code CLI print mode | ChatGPT desktop app / Codex workspace |
| --- | --- | --- | --- |
| Startup | **Tested:** Spynel starts `codex app-server --stdio`, requires a documented object result from `initialize`, then sends `initialized`; missing methods and changed fields fail with executable/surface diagnostics ([adapter](../internal/harness/codex.go), [variant tests](../internal/harness/compatibility_test.go)). Stdio JSONL and initialization are documented by [app-server](https://developers.openai.com/codex/app-server/). | **Tested:** Spynel validates the executable and cwd, performs a bounded `--help` capability check for flags consumed by the configured mode, then requires `system/init` with `session_id` before persisting the first turn ([adapter](../internal/harness/claude.go), [variant tests](../internal/harness/compatibility_test.go)). Print mode and its flags are documented in the [CLI reference](https://code.claude.com/docs/en/cli-reference). | **Unsupported:** the catalog contains only CLI executables ([catalog](../internal/harness/catalog.go)); the official [desktop app page](https://developers.openai.com/codex/app/) documents interactive launch, not an attach/start API. |
| Authentication discovery | **Implementation assumption:** Spynel supplies no credential and relies on the child CLI's normal environment and stored sign-in. Startup errors are surfaced, but login readiness is not probed ([adapter](../internal/harness/codex.go)). OpenAI documents ChatGPT and API-key sign-in for the CLI in [authentication](https://developers.openai.com/codex/auth/). **Unverified** in authenticated canaries. | **Implementation assumption:** Spynel supplies no credential and relies on Claude Code's precedence and inherited environment ([adapter](../internal/harness/claude.go)). Anthropic documents browser login, environment credentials, cloud providers, storage, and precedence in [authentication](https://code.claude.com/docs/en/authentication). **Unverified** in authenticated canaries and background-service environments. | **Unsupported:** Spynel never reads or reuses desktop credentials ([catalog](../internal/harness/catalog.go)). OpenAI documents that the app and CLI support sign-in, but does not document the app as a credential broker for third parties ([authentication](https://developers.openai.com/codex/auth/)). |
| Model discovery and selection | **Tested:** Spynel pages `model/list`, excludes hidden entries, preserves exact model IDs and effort values, and sends configured model/effort on thread or turn start ([adapter](../internal/harness/codex.go), [tests](../internal/harness/codex_test.go)). `model/list` is in the official [app-server protocol](https://developers.openai.com/codex/app-server/). Account-specific catalogs are **unverified** live. | **Partially tested:** exact configured values become `--model` and `--effort`; the displayed catalog is a static alias list because Claude exposes no picker API ([adapter](../internal/harness/claude.go), [tests](../internal/harness/claude_test.go)). The flags and aliases are documented in the [CLI reference](https://code.claude.com/docs/en/cli-reference). Alias availability is account/provider dependent and may drift. | **Unsupported:** no desktop model list is consumed ([catalog](../internal/harness/catalog.go)); model choice is an interactive app feature in the [desktop documentation](https://developers.openai.com/codex/app/), not a documented third-party API. |
| Streaming | **Tested:** app-server agent-message deltas are forwarded and multiple assistant items are retained; the last assistant item is separately identified ([adapter](../internal/harness/codex.go), [tests](../internal/harness/codex_test.go)). Streamed agent events are a documented [app-server](https://developers.openai.com/codex/app-server/) use case. | **Tested:** Spynel requests `--output-format stream-json --verbose --include-partial-messages`, emits text deltas, and consumes the terminal result ([adapter](../internal/harness/claude.go), [tests](../internal/harness/claude_test.go)). Anthropic documents this event stream and terminal result in [programmatic use](https://code.claude.com/docs/en/headless). | **Unsupported:** no desktop event stream is consumed ([catalog](../internal/harness/catalog.go)); the [desktop app page](https://developers.openai.com/codex/app/) documents UI output only. |
| Active-turn follow-up | **Tested native steer:** Spynel sends `turn/steer` with the active turn ID and hands subsequent output to the newest emitter ([adapter](../internal/harness/codex.go), [tests](../internal/harness/codex_test.go)). Expected-turn semantics are documented by [app-server](https://developers.openai.com/codex/app-server/). | **Tested with two modes:** stream-json input writes another user event to the live process; permission configurations requiring ordinary text input use the supervisor's ordered same-session queue ([adapter](../internal/harness/claude.go), [supervisor](../internal/harness/supervisor.go), [tests](../internal/harness/claude_test.go)). Streaming input is documented by the [CLI reference](https://code.claude.com/docs/en/cli-reference); queued follow-up is a Spynel behavior, not a provider primitive. | **Unsupported:** no app turn is addressable by Spynel ([catalog](../internal/harness/catalog.go)); the [desktop documentation](https://developers.openai.com/codex/app/) exposes no third-party turn-steering method. |
| Interruption | **Tested:** `turn/interrupt` targets the stored active thread and turn; terminal interrupted status clears activity ([adapter](../internal/harness/codex.go), [tests](../internal/harness/codex_test.go)). The method and interrupted terminal state are documented by [app-server](https://developers.openai.com/codex/app-server/). | **Tested adapter behavior:** Spynel cancels the process context and additionally sends `os.Interrupt` on non-Windows systems ([adapter](../internal/harness/claude.go), [tests](../internal/harness/claude_test.go)). Anthropic's streaming protocol now advertises interrupt capabilities in [programmatic use](https://code.claude.com/docs/en/headless), but Spynel does not negotiate or use those receipts. Native Windows interruption is **unverified**. | **Unsupported:** Spynel owns no desktop process or turn ([catalog](../internal/harness/catalog.go)); the [desktop app page](https://developers.openai.com/codex/app/) documents user interaction only. |
| Resume and session persistence | **Tested:** Spynel atomically persists conversation-to-thread IDs, calls `thread/resume` after restart, and starts a fresh thread if resume fails ([adapter](../internal/harness/codex.go), [tests](../internal/harness/codex_test.go)). Conversation history and thread resume are documented by [app-server](https://developers.openai.com/codex/app-server/). Live restart recovery is **unverified**. | **Tested:** Spynel atomically persists session ID plus a policy fingerprint and passes `--resume`; policy changes intentionally start a fresh provider session ([adapter](../internal/harness/claude.go), [tests](../internal/harness/claude_test.go)). Print sessions remain resumable by ID per [session documentation](https://code.claude.com/docs/en/sessions). Live restart recovery is **unverified**. | **Unsupported:** desktop histories are not imported or resumed ([catalog](../internal/harness/catalog.go)). OpenAI documents desktop project/chat continuity in the [desktop app](https://developers.openai.com/codex/app/), not a supported external session API. |
| Permission and sandbox mapping | **Tested mapping:** `read-only`, `workspace-write`, and `danger-full-access` become app-server thread/turn sandbox values; workspace-write carries cwd and configured network access ([adapter](../internal/harness/codex.go), [tests](../internal/harness/codex_test.go)). Codex documents sandbox/approval configuration in [config basics](https://developers.openai.com/codex/config-basic/) and native Windows differences in [Windows sandbox](https://developers.openai.com/codex/windows/). App-server approval callbacks are not implemented because Spynel currently uses a non-interactive approval policy. | **Tested mapping:** read-only uses `plan`; workspace-write uses `acceptEdits` plus `Bash(*)`; unrestricted non-root uses bypass; root falls back to `acceptEdits` with allowed tools ([adapter](../internal/harness/claude.go), [tests](../internal/harness/claude_test.go)). Anthropic defines these controls in [permission modes](https://code.claude.com/docs/en/permission-modes). These are Claude permission modes, **not OS sandbox equivalence**; native Windows sandbox protection is unavailable according to [setup](https://code.claude.com/docs/en/setup). | **Unsupported:** desktop permission UI is not controlled ([catalog](../internal/harness/catalog.go)); the [desktop app](https://developers.openai.com/codex/app/) is a separate interactive surface. |
| Failure behavior | **Tested:** JSON-RPC errors, invalid start responses, stream closure, process exit, and active-turn failures become terminal errors; no automatic app-server restart occurs ([adapter](../internal/harness/codex.go), [tests](../internal/harness/codex_test.go)). Official error categories and JSON-RPC errors are documented by [app-server](https://developers.openai.com/codex/app-server/). Provider-specific retry semantics are **unverified**. | **Tested:** nonzero/early exit emits a bounded stderr tail; malformed JSON lines are ignored; a provider `result` with `is_error` becomes a terminal error ([adapter](../internal/harness/claude.go), [tests](../internal/harness/claude_test.go)). Anthropic documents terminal results and retry events in [programmatic use](https://code.claude.com/docs/en/headless). Spynel currently ignores structured retry events and capability announcements. | **Unsupported:** desktop failures are outside the harness supervisor ([catalog](../internal/harness/catalog.go)); the [desktop app page](https://developers.openai.com/codex/app/) provides no automation failure contract. |
| Restart recovery | **Partially tested:** session maps reload and a new app-server can resume a saved thread, while the supervisor can retry an unavailable harness ([adapter](../internal/harness/codex.go), [supervisor](../internal/harness/supervisor.go), [tests](../internal/harness/codex_test.go)). Provider persistence is documented by [app-server](https://developers.openai.com/codex/app-server/). Process-crash recovery against a real current CLI is **unverified**. | **Partially tested:** persisted IDs reload and subsequent bounded processes use `--resume`; policy mismatches avoid unsafe reuse ([adapter](../internal/harness/claude.go), [tests](../internal/harness/claude_test.go)). Anthropic documents durable local sessions and `--resume` in [sessions](https://code.claude.com/docs/en/sessions). Process-crash recovery against a real current CLI is **unverified**. | **Unsupported:** Spynel restart state has no desktop identifier ([catalog](../internal/harness/catalog.go)); desktop continuity described by the [desktop app](https://developers.openai.com/codex/app/) remains app-owned. |

## Operating-system and architecture evidence

Build availability, provider availability, deterministic native tests, and live
provider canaries are distinct claims:

| Target | Spynel release artifact | Provider documentation | Harness behavior evidence |
| --- | --- | --- | --- |
| Linux amd64 | Built on an amd64 Linux runner ([release workflow](../.github/workflows/release.yml)). | Codex documents a Linux installer ([CLI](https://developers.openai.com/codex/cli/)); Claude documents Ubuntu, Debian, and Alpine on x64/arm64 ([setup](https://code.claude.com/docs/en/setup)). | Native synthetic-contract/archive evidence is configured, but no retained amd64 record or artifact-linked provider canary has been inspected. The 2026-08-07 local audit was arm64, not amd64. |
| Linux arm64 | Built on an arm64 Linux runner ([release workflow](../.github/workflows/release.yml)). | Both providers document Linux installation; Claude explicitly documents ARM64 ([Codex CLI](https://developers.openai.com/codex/cli/), [Claude setup](https://code.claude.com/docs/en/setup)). | The synthetic harness suite and packaged-archive smoke passed locally on Linux arm64 on 2026-08-07 and produced an `observed-native` record. Authenticated canaries were not run. |
| macOS amd64 | Built on an Intel macOS runner ([release workflow](../.github/workflows/release.yml)). | Both providers document macOS CLI installation ([Codex CLI](https://developers.openai.com/codex/cli/), [Claude setup](https://code.claude.com/docs/en/setup)). | Native synthetic-contract/archive evidence is configured, but no retained macOS amd64 result or real-provider canary is recorded. |
| macOS arm64 | Built on an Apple Silicon runner ([release workflow](../.github/workflows/release.yml)). | Both providers document macOS; the separately documented desktop app does not replace either CLI ([Codex CLI](https://developers.openai.com/codex/cli/), [desktop app](https://developers.openai.com/codex/app/), [Claude setup](https://code.claude.com/docs/en/setup)). | Native synthetic-contract/archive evidence is configured, but no retained macOS arm64 result or real-provider canary is recorded. |
| Windows amd64 | Built on a native Windows runner ([release workflow](../.github/workflows/release.yml)). | Codex documents native Windows with Windows 11 recommended and recent Windows 10 best-effort ([Windows sandbox](https://developers.openai.com/codex/windows/)); Claude documents native Windows 10 1809+/Server 2019+ x64/ARM64, with no native sandbox ([setup](https://code.claude.com/docs/en/setup)). | Portable fixtures and native synthetic-contract/archive evidence are configured, but no retained native Windows result or real-provider canary has been inspected. Cross-compilation is not treated as native evidence. |
| Windows arm64 | No Spynel artifact is published because the bundled speech runtime has no supported target ([release workflow](../.github/workflows/release.yml), [release documentation](releasing.md)). | Claude documents ARM64 hardware and Windows; Codex documents native Windows but the reviewed page does not make an architecture-specific CLI guarantee ([Claude setup](https://code.claude.com/docs/en/setup), [Codex Windows](https://developers.openai.com/codex/windows/)). | **Unsupported Spynel release target.** Provider availability does not create a Spynel support claim. |

The release workflow's verification suite runs on Ubuntu before five native
packaging jobs. Each native job is now configured to run the synthetic
`internal/harness` contract suite and smoke its extracted archive from a path
containing spaces and non-ASCII characters. It retains a separate bounded JSON
artifact named `spynel-evidence-<os>-<arch>` with the commit/tag, native target,
Go version, archive checksum, commands, results, and `observed-native`
classification. The smoke uses an isolated home and allowlisted environment;
it exercises version, help, provider-free initialization, and clean missing-
harness guidance without installing, authenticating, or launching Codex or
Claude Code. This is a configured evidence design, not proof that the workflow
has run. Only an actually retained record is native evidence, and authenticated
provider canaries remain a separate requirement.

## Known gaps and release language

- There is deliberately no numeric minimum-version claim: the reviewed public
  evidence does not establish one. Codex validates initialization and each
  required method/result at its earliest safe use; Claude validates configured
  CLI flags at startup and stream initialization on the first turn. A later,
  previously unused provider surface can still reveal drift only when reached.
- The Codex adapter handles the stable methods it uses but does not respond to
  interactive approval requests. Its derived `never` policy is part of the
  supported current configuration, and administrator-enforced requirements may
  reject it.
- Claude model choices are maintained aliases, not a machine-readable account
  catalog. New, retired, unavailable, or organization-restricted aliases can
  make the UI differ from the actual account.
- Claude streaming-input follow-ups and ordinary-text tool permissions are
  selected from current implementation assumptions. They need versioned
  fixtures and authenticated canaries before being called provider guarantees.
- No real-provider run in this audit verifies authentication, model catalogs,
  token streaming, tool execution, interruption, or recovery. Such a run could
  incur cost or mutate a repository and requires a controlled canary account.
- Paths containing spaces and non-ASCII characters have no retained native
  provider evidence on every OS. Executable launch uses Go argument arrays, but
  that implementation property is not sufficient cross-platform proof.
- The macOS desktop app is explicitly outside the Spynel support boundary. Do
  not write “supports the Codex app”; say “supports the Codex CLI app-server”
  until OpenAI publishes and Spynel implements a distinct app integration.

Release notes may say that Spynel **includes adapters** for Codex app-server and
Claude Code print mode on the listed Spynel build targets. They must not say a
provider/OS combination is “tested,” “certified,” or “fully supported” without
a retained native test and versioned real-provider canary for that combination.

## Drift maintenance

Refresh this document and its fixtures under any of these conditions:

1. Before a Spynel release that changes `internal/harness`, discovery, session
   storage, sandbox mapping, or supported OS/architecture claims.
2. When a provider changes the app-server schema, CLI flags, stream event
   shapes, session behavior, authentication precedence, permission modes,
   platform support, or desktop automation documentation.
3. At least once per planned compatibility-goal review while that goal is
   active, and before making a new compatibility claim after a provider update.

For each refresh:

- record retrieval date, exact CLI version, install channel, OS, architecture,
  authentication type category (never the secret), and whether the evidence is
  help-only, deterministic, or an authenticated canary;
- regenerate or capture protocol fixtures from public schemas/events without
  storing credentials, prompts with private data, or raw provider transcripts;
- run the provider-neutral supervisor tests and each adapter contract suite on
  every claimed native target, including spaces/non-ASCII paths and clean
  interruption/restart cases;
- add a fixture before accepting a changed method, event, or error shape;
- update this matrix and release language whenever a row changes support level;
  and
- require independent review for every compatibility-changing task.

A new provider version requires fixture or release-claim changes when it removes
or alters a consumed flag/method/field, changes terminal-event semantics,
changes permission effects, invalidates saved session IDs, changes executable
placement, or drops a claimed OS/architecture. A documentation-only wording
change that leaves the contract intact still updates the retrieval date and
source, but does not by itself establish a new tested version.
