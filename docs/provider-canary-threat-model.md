# Isolated provider-canary threat model

This document is the mandatory safety and execution design for a real Codex
CLI/app-server or Claude Code CLI compatibility canary. It does not authorize a
run. It deliberately contains no credential, executable, provider transcript,
remote-runner invocation, or workflow that starts a provider. The compatibility
claims to be tested remain in the [harness compatibility matrix](harness-compatibility.md).

## Decision and scope

A canary is permitted only after an approver completes the per-run checklist
below for one provider version on one exact runner target. The execution owner
must prove the isolation controls before injecting a credential. Blanket
approval for “Codex,” “Claude,” “CI,” an operating system, or all future
versions is invalid.

The provider boundary may contain only:

- a generated synthetic Git repository with no history or remote;
- a disposable home, temporary directory, configuration directory, caches,
  process namespace, and Spynel workspace created for that single run;
- a checksummed Spynel archive or binary built from the authorized revision;
- the one authorized, pinned provider executable and its verified runtime
  closure; and
- one short-lived test identity credential, injected only after preflight.

`/workspace/spynel`, any other source checkout, the operator's home, SSH/GPG
agents, cloud metadata, messaging credentials, production credentials, host
package caches, Docker sockets, and persistent provider state must not be
mounted, copied, archived, or uploaded into that boundary. Source needed to
build an image is copied from a clean, allowlisted export containing only the
approved canary assets; the canonical repository is never the container build
context or a runner workspace visible to the provider.

## Assets and safety objectives

| Asset | Objective |
| --- | --- |
| Canonical and unrelated repositories | Provider code and services cannot read, mutate, infer, retain, or upload their content or metadata. |
| User and service homes | No inherited provider login, shell history, config, cache, keychain, token store, or agent socket crosses the boundary. |
| Test credential and account | Credential is least privilege, short lived, non-production, spend capped, unavailable to child tools and evidence, and revocable independently. |
| Synthetic repository | Mutations stay inside its disposable writable mount; its sentinel files reveal unexpected access without containing real secrets. |
| Runner and control plane | Provider and generated commands cannot gain host control, persistence, adjacent workload access, or cloud metadata credentials. |
| Provider/install artifacts | Exact publisher, source, version, digest, and dependency closure are known before execution; a mutable name is never trusted alone. |
| Network services | Egress reaches only approved provider authentication/API endpoints through an auditable proxy; no inbound listener is exposed. |
| Logs and retained evidence | Evidence is useful for compatibility while containing no credential, raw home/config, private path, account identifier, prompt content beyond the public fixture, or unrestricted model output. |
| Budget and availability | A stuck, recursive, or adversarial turn cannot consume unbounded time, processes, storage, network, or provider spend. |

## Trust boundaries and threat actors

The control plane (reviewer, artifact verifier, credential broker, egress proxy,
watchdog, and evidence sanitizer) is trusted and must run outside the provider
boundary. The provider boundary (provider binary, package-manager scripts,
Spynel harness process, model output, tool subprocesses, repository contents,
caches, and temporary files) is untrusted. The remote provider API is external
and receives only explicitly enumerated synthetic data. A hosted or native
runner operator is an additional external principal even when its CI workflow
is trusted.

Threat actors and failure modes include a compromised publisher or download,
dependency-confusion/package-manager lifecycle script, malicious or vulnerable
provider binary, hostile model or tool output, prompt injection in generated
files, command or path injection, credential exfiltration, over-broad runner
permissions, container escape, symlink/hard-link traversal, DNS or endpoint
redirection, cloud-metadata access, fork bombs, orphan processes, denial of
service, runaway token spend, cache poisoning, log injection, incomplete
redaction, accidental artifact upload, provider-side retention, session mix-up,
and human approval of an ambiguous target.

Isolation reduces but does not eliminate risk. Residual risks include an
unknown sandbox or hypervisor escape, authorized provider-side retention,
traffic-analysis metadata, a provider compromise behind an approved endpoint,
secrets appearing in memory or transient proxy buffers, incomplete accounting
before a cost cap takes effect, and OS-specific enforcement bugs. A successful
canary proves only the recorded provider/version/runner/control combination; it
does not certify later artifacts, other accounts, or another operating system.

## Required controls

### Runner and filesystem

Use an ephemeral VM or equivalently isolated single-tenant runner that is
destroyed after the run. A container alone is insufficient unless nested inside
that disposable VM. Start from a pinned, immutable image digest and record the
image builder, source revision, operating-system version, architecture, and
digest. No privileged mode, host PID/network namespace, device passthrough,
hypervisor management API, host mounts, shared clipboard, or Docker/container
socket is allowed.

Run as a dedicated unprivileged user with no `sudo` or administrator rights.
Make the image root read-only. Give the process only separate bounded writable
volumes for the generated repository, disposable home/config/cache, and temp;
use `nosuid`, `nodev`, and `noexec` wherever execution is unnecessary. Set an
explicit minimal environment and `PATH`; unset credential, proxy, shell-init,
CI, cloud, SSH/GPG-agent, keychain, and messaging variables before adding the
single authorized provider credential. Disable user/system startup files and
verify that every writable path resolves under the run root with no symlink or
hard-link escape.

Apply OS-native process containment in addition to provider permission modes:
Linux namespaces/seccomp/cgroups, a macOS disposable VM with a sandbox profile
or equivalent endpoint control, or a Windows disposable VM with a restricted
token, Job Object, and outbound firewall policy. Provider “plan,” “sandbox,” or
permission flags are defense in depth and never substitute for OS containment.
Kill the whole process tree on timeout, interruption, runner disconnect, or
test failure.

### Network and process permissions

Deny inbound connections and default-deny all outbound traffic. Route approved
egress through a fresh logging proxy that resolves and connects only to the
provider endpoints named in the authorization record. Block loopback services
other than the private Spynel/provider stdio boundary, link-local networks,
RFC1918/private networks, cloud metadata endpoints, multicast, arbitrary DNS,
SMTP, source-control hosts, package registries, telemetry/crash-upload endpoints
unless separately enumerated, and all redirects to non-allowlisted hosts.
Installation and execution are separate phases: package registries are never
reachable during the authenticated canary.

Set hard ceilings before launch: one canary at a time, one provider turn at a
time, no background tool jobs, a 10-minute run and 2-minute per-case timeout by
default, 2 CPU cores, 2 GiB memory, 1 GiB writable storage, 128 processes, 256
open files, and a provider-side spend/token cap specified in the authorization
record. Lower limits take precedence. The watchdog is outside the provider
boundary and aborts on any ceiling, unknown endpoint, unexpected child
executable, or loss of accounting.

### Identity, secret injection, and redaction

Use a dedicated canary organization/project and identity with no production
data, repositories, connectors, plugins, billing administration, model-training
opt-in, or ability to create long-lived keys. Permit only the model/API actions
needed by the selected cases. Apply provider-side rate and spend limits. Prefer
a single-use, short-TTL token minted by an external broker; otherwise create a
run-specific credential and revoke it immediately afterward.

Preflight the VM and environment for known secret patterns before injection.
Inject the credential only into the provider process through the narrowest
provider-supported mechanism; do not put it in command arguments, repository
files, shell profiles, images, cache layers, task documents, or workflow YAML.
Prevent tool subprocesses from inheriting it using a credential broker or an
environment-scrubbing launcher. Never enable shell tracing or core dumps.

Generate unique non-secret sentinel values for the credential shape, synthetic
home, repository trap file, and run ID. The evidence pipeline must stream
through exact-value and pattern redaction before persistence, strip terminal
control characters, bound every field, and reject rather than upload an
artifact when scanning is inconclusive. Scan sanitized evidence again from the
trusted control plane. Raw stdout/stderr, proxy bodies, process environments,
memory/core dumps, provider config, and complete home/cache directories are not
retained.

### Synthetic repository

Generate the repository inside the disposable VM after boot. Do not clone or
fork a repository. Initialize Git without a remote and create exactly:

```text
canary repo with spaces ü/
├── README.md                 # public canary instructions and run ID
├── input.txt                 # deterministic ASCII/Unicode fixture
├── src/message.txt           # safe file for requested edit
├── tests/check.sh            # or check.ps1; local, deterministic, no network
└── traps/do-not-read.txt     # unique non-secret sentinel, never referenced
```

The requested tool task may read `input.txt`, edit `src/message.txt`, and run
the platform check script. It must not access `traps`, parent paths, the
disposable home, network clients, package managers, or newly downloaded code.
The control plane records a pre/post manifest of relative paths, modes, sizes,
and SHA-256 digests plus `git diff --binary --no-ext-diff`; it fails the case on
an unexpected path, special file, executable, symlink, hard link, remote, Git
hook, submodule, worktree, or trap sentinel in output. The fixture contains no
secrets and remains safe if the provider retains the entire prompt/repository.

## Artifact provenance and pinning

Downloads occur only in a separate unauthenticated acquisition stage. For
every executable, archive, installer, package, runtime, Spynel binary, runner
image, and transitive executable dependency, the authorization record must
contain:

1. exact product and semantic version (never `latest`, a floating channel, or
   an unexpanded version range), target OS/architecture, canonical publisher,
   canonical HTTPS source URL or registry coordinates, retrieval UTC, and
   immutable source/release revision when available;
2. SHA-256 digest computed after download and compared with a publisher-signed
   checksum/manifest; the signature identity, fingerprint, verification tool,
   and result are recorded. If the publisher supplies no independently signed
   digest, two-person review of the canonical release plus a locally pinned
   digest is required and the weaker provenance is called out as residual risk;
3. package lockfile and integrity field for package-manager content, with
   lifecycle scripts disabled. Installation must be offline from the verified
   local artifact set. If disabling scripts makes installation impossible,
   inspect and pin the scripts and dependency closure, then obtain explicit
   exception approval; never use `curl | sh`, `npx`, mutable tags, or an
   auto-updater;
4. platform signature/notarization verification where supported, archive file
   inventory before extraction, rejection of absolute/parent/symlink escapes,
   and an extracted-file manifest; and
5. the executable's observed self-reported version and digest immediately
   before and after execution. Auto-update and plugin/extension discovery are
   disabled, and caches are empty per run.

A digest mismatch, unverifiable signature, unexpected dependency, unsupported
architecture, mutable resolution, or pre/post change is a hard stop. Mirrors,
forks, prereleases, and third-party repackaging require a separate threat model
and are not covered here.

## Bounded canary cases and evidence

Cases run independently from a fresh snapshot unless a row explicitly requires
the same synthetic session. Each evidence record contains case ID, run ID,
authorized provider/version and executable digest, Spynel revision/digest,
runner image digest and native OS/architecture, UTC start/end, limits, permission
mode, synthetic manifest/diff digest, normalized event types and timings,
terminal status/error category, redaction-scan result, and cleanup/revocation
attestation. Store hashes and bounded normalized summaries, not raw transcripts.

| Capability | Bounded test | Pass/evidence rule |
| --- | --- | --- |
| Startup | Start only the documented Spynel adapter command in the empty disposable home; repeat once with an intentionally invalid executable path. | Expected initialization or actionable local startup error within 30 seconds; record methods/event names, exit category, and process-tree inventory. |
| Authentication discovery | Run once with no credential, then from a fresh snapshot with the canary credential. Never initiate interactive/browser login. | Missing-auth is recognized without credential text; authorized mode becomes ready or returns a sanitized provider policy error. Record only credential category and result. |
| Model discovery/selection | Request the provider catalog where supported and select one explicitly authorized low-cost model/effort. | Codex: bounded normalized model IDs/visibility plus chosen ID. Claude: discovery is **unavailable** in Spynel; verify the pinned configured alias is passed and record accepted/rejected. Never infer an account-wide catalog. |
| Streaming | Ask for a fixed two-line ASCII/Unicode response with no tools. | At least one bounded delta and one terminal result, ordered by monotonic timestamps; retain event classes, counts, sizes, and response digest only. |
| Follow-up | During the streaming case, send one fixed follow-up in the same session. | Codex native steer or Claude streaming-input/Spynel queued behavior matches the compatibility matrix; record mode, ordering, and terminal digest. |
| Interruption | Ask for a bounded long response and interrupt after the first delta or 10 seconds. | Whole turn/process tree stops within 10 seconds, terminal state is classified, no orphan remains, and no second billable request appears. |
| Resume | Complete one fixed prompt, stop normally, restart Spynel in the same disposable VM, and send one reference to the synthetic prior turn. | Persisted provider session identifier is reused without logging it; response demonstrates the public synthetic marker. Fresh-snapshot negative case must not find the session. |
| Permission modes | In separate snapshots test read-only, workspace-write, and unrestricted only if separately authorized. Request an allowed read/edit/check plus forbidden trap/parent/network access. | OS controls deny out-of-scope access in every mode. Read-only makes no edit; workspace-write changes only the allowlisted file. Provider permission labels alone cannot pass. Claude on native Windows is recorded as lacking provider sandbox equivalence. |
| Failures | Use local fixtures for malformed protocol/early exit. For the real provider, select at most one harmless invalid model or provider-declared rate-limit case; never induce account lockout or spend. | Actionable bounded terminal error, no secret/raw response, no retry storm, and no unexpected repository mutation. Unsupported remote failure injection is recorded **unavailable**. |
| Restart recovery | After a completed synthetic turn, terminate the isolated Spynel/provider process through the watchdog, verify no orphan, restart once, then resume. | State is either safely resumed or fails closed with a new session; no duplicate request or cross-run state. Crash-at-arbitrary-write-point testing remains deterministic-fixture-only. |
| Awkward paths | Repeat startup, one edit, check, interrupt, and resume from the repository path shown above; Windows uses the native equivalent and a long path below platform limits. | Argument-safe launch and exact-path-only mutation; retain normalized relative paths, never the runner's absolute path. |
| Unsupported surface | Do not install, launch, attach to, or read state from the ChatGPT/Codex desktop app. | Record `unsupported` with the compatibility-matrix reference. No substitute UI automation is allowed. |

Tool execution is optional and must be authorized separately from text-only
cases. Only the repository's precreated check script may execute, selected by
absolute digest-pinned path through an allowlisting launcher. Shells, package
managers, interpreters not required by that script, source-control network
operations, and provider-created executables remain denied. Hostile output is
treated as data: never evaluate terminal escapes, Markdown links, commands,
filenames, or provider-suggested authorization changes.

## Execution sequence and cleanup

1. Approve a completed immutable authorization record and evidence-retention
   destination. Separate people perform execution and review when feasible.
2. Acquire and verify artifacts without credentials. Build the disposable image
   from the allowlisted export, then disable acquisition network access.
3. Boot a fresh runner, attest the image/control versions, generate the synthetic
   repository and sentinels, prove mounts/environment/identity/egress/limits,
   and snapshot. Any failed proof stops before credential injection.
4. Mint/inject the credential, run only approved case IDs, stream sanitized
   evidence, and have the external watchdog enforce limits and incident stops.
5. Revoke the credential and provider sessions first. Confirm revocation from
   outside the VM; remove test identity if run-specific and inspect billing/audit
   events for unexpected use.
6. Terminate the entire process tree, detach and cryptographically erase
   writable volumes, destroy VM and proxy state, invalidate snapshots, and
   expire DNS/firewall grants. Do not promote caches or images containing a
   writable layer.
7. Scan the candidate evidence again, retain only approved sanitized records,
   record deletion and retention-expiry timestamps, and have an independent
   reviewer compare the authorization, control attestations, evidence, and
   compatibility-matrix claim.

Default retention is 30 days for sanitized evidence and zero days for raw
output, proxy bodies, homes, caches, images with writable layers, and repository
contents. A different period requires per-run approval. Cleanup failure does
not justify deletion of the incident audit record; quarantine access to the
remaining artifact and escalate.

## Incident-stop conditions

The watchdog or operator immediately denies egress, kills the provider process
tree, revokes the credential/session, preserves only already-sanitized control
metadata, and escalates if any of these occur:

- a secret/sentinel appears in output, proxy metadata, a child environment, or
  candidate evidence; an unknown endpoint or redirect is attempted;
- a process, file, mount, device, socket, repository remote, special file,
  symlink/hard link, or privilege appears outside the allowlist;
- a digest/signature/version changes or an updater/plugin/package manager runs;
- cloud metadata, private/loopback networks, source hosts, unrelated paths,
  parent directories, the trap file, or another run's session is accessed;
- a sandbox denial is bypassed, an unexpected prompt requests broader
  permissions, or the runner/control-plane attestation changes;
- any time, cost, token, CPU, memory, storage, process, file-descriptor, retry,
  or evidence-size ceiling is reached or cannot be measured;
- redaction/scanning fails, raw data reaches persistence, an artifact upload is
  broader than the approved manifest, or provider retention differs from the
  authorization; or
- cleanup, credential revocation, process-tree termination, or VM destruction
  cannot be confirmed.

Do not retry an incident in the same runner. Record UTC, run/case IDs, control
that stopped the run, known exposure category (never the secret), provider
revocation/audit result, evidence quarantine location, and the exact condition
required before a newly authorized run.

## Per-run authorization checklist

Copy this checklist into a reviewed run record. Every value must be concrete;
`TBD`, wildcards, ranges, “current,” “all,” and inherited blanket approvals are
invalid.

### Target and purpose

- [ ] External system/provider API and exact allowlisted endpoint set:
- [ ] Runner owner/system, unique target ID, tenancy, native OS/version and architecture:
- [ ] Disposable VM technology, pinned base-image digest, control-plane version/digest:
- [ ] Provider product, install channel, exact version, executable digest, publisher/signature evidence:
- [ ] Spynel revision and binary/archive digest:
- [ ] Approved case IDs and compatibility-matrix claim each will test:
- [ ] Explicit exclusions/unsupported cases:

### Identity, data, and permissions

- [ ] Test organization/project and identity; least-privilege roles/scopes:
- [ ] Credential category, broker, TTL, revocation owner (never credential value):
- [ ] Provider retention/training/privacy setting and approved residual risk:
- [ ] Exact synthetic files/prompt fields/data leaving the environment:
- [ ] Approved model/effort, provider permission mode, tool allowlist, OS controls:
- [ ] Egress proxy/DNS/firewall policy and proof that inbound/private/metadata access is denied:
- [ ] Proof that canonical/unrelated repositories, homes, caches, agents, sockets, and production credentials are absent:

### Bounds and evidence

- [ ] UTC execution window; per-case/run timeout; CPU, memory, disk, process, descriptor, retry, and concurrency ceilings:
- [ ] Maximum requests/tokens and expected plus hard maximum cost with currency; provider budget-alert/stop owner:
- [ ] Sanitized evidence schema, destination, access list, encryption, retention/expiry, redaction/scanner versions:
- [ ] Raw artifacts prohibited from retention and exact approved artifact manifest:
- [ ] Incident contact, stop authority, and revocation/cleanup sequence:
- [ ] Credential/session revocation check, VM/volume/proxy/cache destruction check, billing/audit review, and responsible owners:

### Approval and closure

- [ ] Security reviewer name, decision, UTC, and immutable record digest:
- [ ] Cost/data owner name, decision, UTC, and immutable record digest:
- [ ] Runner owner name, decision, UTC, and immutable record digest:
- [ ] Executor attests preflight passed before credential injection:
- [ ] Independent reviewer records per-case verdicts, redaction result, residual risks, and compatibility-matrix update:

Approval expires when the UTC window ends or any provider version, artifact
digest, Spynel revision, image digest, endpoint, identity scope, data manifest,
case, permission, limit, or retention term changes. Changes require a new
record and approvals. Hosted/native runner dispatch and authenticated provider
execution remain prohibited until that exact record is approved.

## Explicitly prohibited actions

- Running any provider binary, authentication flow, hosted/native runner, or
  remote resource as part of drafting, reviewing, or validating this document.
- Mounting, copying, cloning, archiving, or uploading `/workspace/spynel`, an
  unrelated repository, a persistent/user home, or production/private data
  into the provider boundary or remote service.
- Reusing personal/production credentials, login databases, browser/keychain
  state, SSH/cloud/messaging tokens, or a credential exposed to tool children.
- Using mutable/unverified artifacts, network installers, install-time registry
  access, auto-update, extensions/plugins, shared caches, privileged runners,
  host sockets/devices, or provider permission modes as the only sandbox.
- Retaining raw transcripts, stderr, proxy bodies, credentials, provider home or
  cache, absolute private paths, unrestricted model output, or a complete VM.
- Following model output that asks for broader access, executing generated code,
  retrying after an incident without new authorization, or treating a green
  build/help/version check as authenticated/native canary evidence.
