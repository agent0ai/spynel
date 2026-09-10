# Programmatic integration (v1)

Spynel can be the communication control plane for a development sandbox. Its
existing application service owns conversations, harness dispatch, jobs, history,
and ordinary task/goal notification delivery. The interface is provider-neutral;
it does not add a second agent engine or sandbox-specific policy.

## Start headless; attach a terminal later

Use a locally built binary (`scripts/dev.sh build`) for final testing. Substitute
your **test workspace** below; this does not replace an installed binary.

```sh
BIN=/absolute/path/to/spynel/.tmp-bin/spynel
WORK=/absolute/path/to/test-workspace
"$BIN" init --no-start --dir "$WORK"
"$BIN" serve --config "$WORK/.spynel/config.yaml"
```

`serve` prints ongoing UTC, level, component and event names to stderr (for
example `info harness turn_started`, `info jobs job_finished`). This deliberately
omits diagnostic bodies, prompts, provider payloads, source identities and
credentials. Full bounded, redacted diagnostics remain available through
`spynel log`. Consume or redirect stderr normally; a blocked stderr destination
can apply backpressure. Protocol/JSON stdout is separate. `serve --tui` suppresses
this mirror so it cannot corrupt that process's alternate screen.

From another terminal in the same workspace/environment, run bare `spynel`
(or `spynel serve --tui --config "$WORK/.spynel/config.yaml"`). It attaches to
the same primary; only that primary runs channels and orchestration. An additional
TUI has an independent conversation. It does **not** mirror `cli/adapter` or take
over its response stream. `/resume` branches saved history into an independent
TUI conversation. A headless server's separate stderr stays on its own terminal.

## Send, receive, reconnect

In a separate shell, start the subscription first and wait for its initial
checkpoint line before submitting a message:

```sh
"$BIN" events --config "$WORK/.spynel/config.yaml" --conversation adapter
"$BIN" send --config "$WORK/.spynel/config.yaml" --conversation adapter \
  --request-id request-001 --json 'Inspect the sandbox status'
"$BIN" followup --config "$WORK/.spynel/config.yaml" --conversation adapter \
  --request-id request-002 --json 'Also include current blockers'
"$BIN" stop --config "$WORK/.spynel/config.yaml" --conversation adapter
"$BIN" events --config "$WORK/.spynel/config.yaml" --conversation adapter \
  --after "$LAST_PROCESSED_CURSOR"
```

Flags precede positional text. `send --stdin` accepts bounded stdin; `--stream`
prints deltas, `--json` prints NDJSON events, and default output is final-only.
`followup` requires an active turn and uses the harness's existing native-steer
or queued continuation behavior. A handoff ends the older request stream with a
done **status**, not a final result. `continues:true` final/error events are
intermediate; wait for a non-continuing final/error. `/stop` targets the named
conversation; acceptance of interruption is not proof of provider termination.
An interruption attempt discards ordinary follow-ups that have not reached the
provider, including a batch awaiting handoff. Each discarded request receives a
non-continuing terminal error, also retained in conversation events. These
requests stay cancelled even if interrupting the active provider fails; their
errors do not prove that the active provider has stopped.

Ordinary later notifications addressed to `cli/adapter` arrive on `events` even
after `send` exits. In a synthetic workspace, test the ordinary outbox path with:

```sh
"$BIN" notify --workdir "$WORK" --origin cli/adapter --message 'Synthetic task result'
```

This queues an ordinary local notification; it does not claim a task completed
or replace task/goal notification policy. Notification agents still decide
whether to send according to the existing workflow contracts.

## HTTP contract

The supported integration surface is `/v1/message`, `/v1/events`,
`/v1/conversation`, `/v1/status`, `/v1/notify`, and authenticated health.
All operations use the existing elected service. Other TUI-specific routes are
not an integration SDK. JSON requests reject unknown fields, extra JSON values
and bodies over 1 MiB. Local message input permits only `cli` or `tui`; external
adapters use `cli`. It cannot impersonate authenticated remote-channel intake.
Text must be nonempty valid UTF-8, at most 512 KiB (the encoded-body limit also
applies). Conversation names are 1–120 ASCII letters/digits/dots/underscores/
hyphens, start with a letter/digit, and cannot end with a dot or underscore.

`POST /v1/message` accepts the core message fields:

```json
{"channel":"cli","conversation":"adapter","source_message_id":"request-001","text":"Inspect the sandbox status","followup_only":false}
```

`source_message_id` is an opaque client-owned request identity: 1–128 ASCII
letters/digits/dots/underscores/colons/hyphens, beginning with a letter/digit.
The server generates one when omitted. Keep explicit IDs if retry correlation
matters. Each streamed HTTP line wraps a core response in `event`, with
`request_id` on the envelope and event. CLI JSON exposes the core event directly.
Provider thread/turn fields are opaque optional diagnostic data, not stable
adapter identifiers. HTTP envelopes can instead contain `error`, `code` and
`request_id`; codes include `request_failed`, `duplicate_request` and
`admission_busy`. An eventless framework handler ends with `handled:true`;
that means it returned without a provider response, not that work completed.
Pre-stream validation/authentication failures use non-2xx HTTP responses.

Provider start/steering statuses mean admission, **not completion**. Request
completion requires a non-continuing final/error; a done status only releases
that request's emitter. No response or unexpected EOF is an unknown outcome,
never success. HTTP/client disconnect cancels that request's context, using the
existing provider cancellation boundary. It cannot undo side effects and may
race completion. Closing a subscription never cancels a conversation.
Accepted queued followups belong to the durable conversation and run under the
primary's context even if their submitting stream disconnects; use `/stop` to
interrupt that conversation's logical work.

### Retry boundary

There is no durable idempotency-key service or exactly-once side-effect guarantee.
Within one elected service, up to 64 concurrent admission calls are fenced by
source identity before hooks/provider dispatch. A repeated **recorded** source ID
is rejected without dispatch, including after restart while its original history
is intact. Same ID with different input is still a duplicate, never a new request.
The existing strict duplicate scan fails closed on corrupt histories or histories
over 2,000 raw records. Clear/deletion/branching and an ownerless one-shot process
are outside the retry guarantee. Extension cancellation before the user record
exists is also outside durable duplicate suppression.

After a transport failure, inspect correlated events/history first; never silently
retry with a fresh ID. If retrying, retain the same ID and exact input. A duplicate
response proves only a prior reservation/record exists, not provider completion.
If the result remains ambiguous, reconcile it instead of replaying side effects.
The client makes no automatic message retry. CLI `--json` puts dispatched handler
and provider errors on stdout as JSON and returns nonzero with a generic stderr
failure. Argument/discovery failures can occur before any JSON stream exists.

### Conversation events and resynchronization

`GET /v1/events?conversation=adapter&after=CURSOR` streams NDJSON using
`schema:"spynel.events/v1"`. Omit `after` to begin at the current tail.
Each event envelope contains `event` with `id`, `cursor`, `at`, `kind`, `text`,
optional `request_id`, `notification_id`, `final_text`, `done` and `continues`.
Checkpoint envelopes contain just schema and `cursor`, initially, after a batch,
and at least every 15 seconds while idle. A new empty conversation gets a private
history baseline so its cursor is restart-stable. Ledger records are not exposed.

Events are committed user records, assistant replies, errors and ordinary
notifications, in durable append order. All survive a normal primary restart
while history remains intact. Deltas, activity, provider statuses, screens,
admission errors and unfinished partial output do **not** survive via this feed;
use the request stream for live deltas. Committed terminal text includes the full
turn. A followup's final carries the responding emitter's request ID; it can cover
earlier requests in that same turn and is not one-final-per-input delivery.

Cursor identity binds the workspace/conversation, history generation and preceding
record. Keep it opaque. Reconnect after the last **processed** event/checkpoint.
Persist the cursor only after applying prior events; repeat delivery is possible
if processing and cursor persistence are interrupted. Deduplicate by event `id`;
also deduplicate ordinary notifications by `notification_id`, which survives an
outbox delivery retry even if another history record was appended. Receipt does
not acknowledge a notification or mutate task state.

Retention is the existing history lifetime: explicit clear/removal and ordinary
history cleanup (default 30 days, live configurable) end replay. Subscribers do
not pin histories. A subscription reads every 250 ms, allows at most 256 raw
records and 4 MiB of unread history per read, and shares a 32-subscriber server
limit across loopback and socket. It owns no event queue; history is its buffer.
Writes have a five-second deadline. Lag beyond either replay bound closes with
`replay_limit`; malformed/expired/changed cursors fail with `invalid_cursor` or
`cursor_expired`. Initial errors are HTTP 409 JSON, later errors are NDJSON error
envelopes followed by EOF. Capacity rejection is HTTP 429. No gap is silently
skipped, no background replay is executed, and reconnect is caller-controlled.

`GET /v1/conversation?conversation=adapter` returns the same schema, `events`,
`cursor`, and `bounded:true`. This is an atomic snapshot of the newest 100 visible
history entries under a 2 Mi-rune limit paired with its resume cursor. Snapshot
entries have no delivery IDs/cursors, but retain request/notification correlation.
It is a bounded resynchronization view, not proof all historical side effects were
observed. Resume `events` from its cursor; use ordinary history inspection if older
records are needed. A renamed workspace changes cursors and requires resync.

## Optional Unix socket

Keep the loopback default for ordinary local CLI/TUI clients. To add a socket to
a **new** primary, create a dedicated private directory and pass one launch flag:

```sh
mkdir -m 700 /absolute/private-spynel-control
"$BIN" serve --config "$WORK/.spynel/config.yaml" \
  --socket /absolute/private-spynel-control/api.sock
"$BIN" events --socket /absolute/private-spynel-control/api.sock --conversation adapter
"$BIN" send --socket /absolute/private-spynel-control/api.sock \
  --conversation adapter --request-id socket-001 --json 'Inspect status'
```

`send`, `followup`, and `events` accept explicit `--socket`; it selects the
server's workspace without local election/configuration discovery. It cannot be
combined with `--config` or attachment copying. A direct HTTP adapter can use
all the supported routes, including sending `/stop` as ordinary message text.
Adding `--socket` beside an existing owner fails explicitly; it does not restart
that owner. TUI attachment continues using the original loopback API.

The absolute socket path is limited to 100 bytes. The existing parent directory
must be owned by the current user, mode 0700, with no symlinks. Both the socket
and sibling `api.sock.json` descriptor are mode 0600. The descriptor has schema
`spynel.socket/v1`, a non-secret SHA-256 `workspace_id`, and the private bearer
token. Never log/copy that token into arguments or mount the entire `.spynel`
directory for integration. The same bearer authentication protects both
listeners, with workspace-wide operator access; it is not a restricted capability
for a single conversation. The client pins the descriptor's workspace identity.
Both listeners stop with the primary. Cleanup removes only the exact files it
created. Existing files, symlinks, descriptors and stale sockets are **refused**;
after a crash, confirm no owner is using them before manually removing the pair.

Unix sockets are supported on Linux/macOS; Windows returns an explicit unsupported
error. A bind-mounted dedicated socket directory can connect same-kernel Linux
containers with compatible numeric UID ownership and filesystem/socket access.
Remount the directory rather than only a socket inode if reconnect after restart
is required. Host/VM kernels, Docker Desktop boundaries, UID mapping and mount
permissions may prevent this. Some shared/FUSE workspace mounts also reject
socket chmod; choose a private native-filesystem runtime directory instead.
Spynel fails closed if it cannot establish private socket permissions. A socket alone is not container isolation: its
bearer grants operator control over that workspace and its trusted harness.
The loopback client's foreign-environment and primary-election fences are unchanged.

## Small Python HTTP example (standard library only)

This example assumes the directory/descriptor above has been privately provisioned
and trusted. It prints protocol records without exposing credentials. Run it once
to snapshot/send, then run it with the returned processed cursor to receive/replay.

```python
import http.client, json, socket, sys, urllib.parse
from pathlib import Path

path, conversation, mode = sys.argv[1:4]  # absolute socket, name, snapshot/send/events
descriptor = json.loads(Path(path + ".json").read_text())
assert descriptor["schema"] == "spynel.socket/v1"
# Pin descriptor["workspace_id"] to your provisioned workspace in a real adapter.
class Connection(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.connect(path)

connection = Connection("spynel")
query = urllib.parse.urlencode({"conversation": conversation})
body = None
if mode == "send":
    body = json.dumps({"channel": "cli", "conversation": conversation,
                       "source_message_id": sys.argv[4], "text": sys.argv[5]})
    method, route = "POST", "/v1/message"
else:
    method, route = "GET", "/v1/" + ("conversation" if mode == "snapshot" else "events")
    if mode == "events" and len(sys.argv) > 4:
        query += "&" + urllib.parse.urlencode({"after": sys.argv[4]})
    route += "?" + query
connection.request(method, route, body, {"Authorization": "Bearer " + descriptor["token"],
                                        "Content-Type": "application/json"})
response = connection.getresponse()
if response.status != 200:
    raise SystemExit(f"HTTP {response.status}: {response.read().decode()}")
for line in response:
    record = json.loads(line)
    print(json.dumps(record), flush=True)  # Apply effects before saving its cursor.
connection.close()  # EOF alone never proves a provider turn completed.
```

Final local test: use a disposable workspace with your intended harness, start
`serve --socket`, subscribe, send and follow up, verify a later local notification,
reconnect with its prior cursor, and attach a separate terminal. Controlled
fixtures verify the interface; real provider, terminal emulator and 80|20 sandbox
topology still require your local test.
