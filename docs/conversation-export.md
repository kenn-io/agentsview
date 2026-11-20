---
title: Conversation Export
description: Incremental exports of stored user and assistant messages
---

Conversation exports let you keep a lightweight copy of stored user and
assistant messages for reporting or analysis. You start by reading a text-free
changes listing, pick the sessions you want, and then fetch only the new or
changed message text. Activity exports remain separate. In conversation exports,
messages from an ongoing session do not wait for a reporting hour to close.

The export commands read normalized messages straight from your local SQLite
archive. They cover every supported agent and imported archive, including
sessions whose original source files are gone. They never contact an agent,
reparse transcripts, start a server, or read a replica.

The first `changes` call on an archive takes longer than later calls because it
builds the conversation change index from existing database records; normal
syncing never pays that cost. If a daemon owns the archive, the build runs
through the daemon; otherwise it runs directly. If you try to read a message
before that first `changes` call, the command fails without returning a
checkpoint. Existing session and reporting exports keep their current formats.

Version 0.44.0 also fixes how Codex prompts are stored. Before you run
read-only exports on an existing archive, let the normal startup resync refresh
those records. Sessions whose source files are missing keep their stored
content. See the [changelog](/docs/changelog/#0440) for details on the parser
fix.

## Start a copy and keep it current

```bash
agentsview export conversations changes --limit 100
agentsview export conversations changes --cursor PAGE_CURSOR
agentsview export conversations changes --checkpoint SAVED_CHECKPOINT
```

When you run `changes` without a cursor or checkpoint, the walk starts from the
beginning of the retained changes. Each JSON page returns:

- `schema_version`, `archive_id`, and `database_id`, which identify the contract
  and source.
- `changes`, which holds message or session metadata without message text.
- `next_cursor` when more pages remain, or `checkpoint` on the final page.

Read through every page before you save the new checkpoint, and store it
together with your consumer's durable progress so an interrupted import can
replay the same work. Pass that checkpoint on your next poll. If nothing has
changed in the archive, the command returns no changes.

The listing reflects compact current state rather than an event history or a
globally frozen snapshot. If a message changes again while you paginate, it
appears in the next cycle, and you can keep polling to catch up. Do not treat a
message's absence from a page, or an interrupted walk, as a deletion.

Each message change identifies the session and stable message along with its
revision, order, role, optional source timestamp, text digest, and byte count.
Session changes refresh project evidence independently of message text. Resolve
project sharing before you fetch message bodies. A display label alone does not
establish repository identity. The project reference follows the evidence rules
in [Session Export](/docs/session-export/#project-evidence).

## Follow changes live

If you are building a sync client or dashboard, you can follow archive changes
as they happen and resume after a disconnect without gaps or repeats. Open the
server-sent events stream on the local daemon:

```http
GET /api/v1/changes/stream?after=CURSOR
```

Leave out `after` on your first connection. On later connections, pass the `to`
value from the last `changes` event you finished processing, and treat cursors
as opaque strings. The stream uses the same authentication as `/api/v1/events`,
including the `?token=` fallback for native browser `EventSource` clients
described in [SSE Endpoints](/docs/remote-access/#sse-endpoints). A server
without a local SQLite archive returns HTTP 501.

The stream reads the archive and never writes your sessions or messages.
If the archive has never had a conversation export, the daemon first builds the
conversation change index, the same one-time step that
`export conversations changes` runs. The stream sends `ready` first, sends
heartbeats while the index builds, and then starts the replay. That build runs
in one archive write transaction, so sync waits for it, and it finishes even if
the subscriber disconnects. A read-only daemon cannot build the index and
streams session records only.

Each sync pass that writes archive data produces a `changes` event right away,
without the `events_coalesce_interval` delay that `/api/v1/events` applies.
When nobody is watching the archive, watcher-triggered syncs start at most once
per `background_sync_interval`, which defaults to `"10m"`. When at least one
subscriber is connected, the daemon shortens that interval to one second, or
keeps a shorter configured `sync_interval`, so a session file change usually
reaches the stream within about a second. Once the last subscriber disconnects,
the background interval applies again.

The stream sends these events, with one JSON object per `data` line:

| Event       | Data                                                                                                                                       |
| ----------- | ------------------------------------------------------------------------------------------------------------------------------------------ |
| `ready`     | `schema_version`, `archive_id`, `database_id`, `head`, the cursor of the newest change when the connection opened, and `local_machine_id`. |
| `changes`   | `schema_version`, `archive_id`, `database_id`, `from`, `to`, `reset`, `sessions`, and `messages`.                                          |
| `heartbeat` | A timestamp, sent on the same interval as `/api/v1/events`.                                                                                |
| `error`     | `{"error": "..."}` when a read fails. The stream then closes; reconnect with the last `to` you processed.                                  |

How `changes` events fit together:

- The first event starts at your `after` cursor, and each later event sets
  `from` to the previous event's `to`. If your cursor is already current, no
  `changes` event arrives until something changes.
- The stream first replays everything after the cursor in pages of at most 500
  message and session changes, then stays open and pushes new changes as the
  daemon publishes them. Changes that arrive together are grouped into one
  event.
- When `after` is empty, belongs to another database generation, predates
  removed deleted-message records, or is not a cursor this archive issued, the
  stream sets `reset` to `true` with an empty `from` and replays from the
  beginning of the archive. Replace your local copy with what follows instead
  of merging into it. A full resync or compaction during a connection triggers
  the same reset. Because the daemon removes deleted-message records after one
  to two weeks, a reader that reads at least weekly is not reset by that
  cleanup.
- Like the changes listing, the stream delivers compact current state. If a
  record changes twice before you read it, you receive it once in its newest
  state.

Every event includes the current state of each session touched by a change.
When a message changes, the event also includes its session, so session totals
stay current. A session object contains:

- `session_id` and `deleted`. A trashed or deleted session has `deleted: true`,
  a `null` project, `null` usage, and empty fields other than `machine_id`.
- `machine_id`, identifying the machine where the session originated.
- `agent` and `automation`, where `automation` is `interactive`, `automated`,
  or `subagent`.
- `project`, using the same project reference as the changes listing.
- `models`, `started_at`, `last_activity_at`, and `message_count`.
- `usage`, holding the session's token and cost totals.

A message object omits message text and text digests. It contains:

- `session_id`, `message_id`, `revision`, `deleted`, `role`, and `timestamp`,
  matching the changes listing.
- `machine_id`, matching its session's value.
- `model` and `project`.
- `usage`, or `null` when the message recorded none.

An archive can hold copies of sessions synced from other machines. To find
which sessions started on this machine, compare each record's `machine_id` with
the `local_machine_id` from `ready`. Matching strings belong to this machine's
sessions. Both fields use the originating machine identities described in
[per-machine reporting exports](reporting-export.md#report-an-archive-that-contains-several-machines):

- `local_machine_id` is this installation's identity, recorded when the daemon
  starts. It is `null` only when the archive has no recorded installation
  identity.
- A copy from another machine carries that machine's identity, and a recorded
  machine alias resolves to the identity it names.
- A session stored without a machine carries `archive:<archive_id>`.
- A trashed session and its messages keep their `machine_id`. A permanently
  deleted session no longer has a record, so its `deleted` session and message
  records carry `machine_id: null`.
- A message keeps the `machine_id` it was sent with until the message changes.
  If a session's machine changes, the daemon sends the session again with the
  new value; treat the session's value as current.

A usage object contains `input_tokens`, `output_tokens`,
`cache_creation_tokens`, `cache_read_tokens`, `cost_microdollars`, and
`priced`. Cost is an exact integer number of microdollars calculated from
AgentsView's model pricing using the same rules as session summaries. `priced`
is `true` only when every counted usage has a price. Otherwise
`cost_microdollars` covers only the priced portion, and a session with no usage
reports zeros with `priced: false`.

If a message's usage changes while its text stays the same, the stream sends
that message again with the same `revision` and its updated `usage`. This
happens, for example, when a later sync adds token usage to an earlier reply or
attaches a usage record to that message. Only messages whose stored usage
changed are sent again, and each change is sent once. Rewriting a session's
usage records can also re-send messages after the first changed record even if
their own usage did not change. Some updates alter a message's computed `usage`
without changing its stored usage, so they update only the session totals or
send nothing:

- Model pricing updates change computed costs without sending any message or
  session. To recompute every value, reconnect without a cursor.
- When two sessions store the same Claude response, such as a resumed or forked
  transcript, writing to one can move that usage between them. The daemon sends
  the written session, while the other session's message and totals keep their
  earlier usage until it changes again.
- When a session-level reported cost is spread across messages, changing any of
  its usage changes their shares, and only the changed messages are sent again.

## Read only the text you need

```bash
agentsview export conversations message SESSION_ID MESSAGE_ID \
  --database-id DATABASE_ID --revision REVISION --max-bytes 65536

agentsview export conversations message SESSION_ID MESSAGE_ID \
  --database-id DATABASE_ID --revision REVISION --offset NEXT_OFFSET
```

Use the identifiers and revision from the changes listing. The response
includes the current project reference from the same read snapshot as the text.
Check it again before you disclose the body to another system.

The `text` field holds a UTF-8-safe chunk, while `offset` and `next_offset` are
byte offsets. In the body response, `text_bytes` gives the size of the complete
message as this command returns it. Keep requesting chunks with the same
generation and revision until `next_offset` reaches `text_bytes`. `--max-bytes`
defaults to 65,536 and must allow at least four bytes, which is the maximum
length of one UTF-8 character. A stale revision fails instead of returning a
different version of the message, and text is never silently truncated.

If a revision has been superseded, the command leaves stdout empty, writes
`revision_changed` JSON to stderr, and exits with code 5. Discard any partial
body for that revision and finish the changes walk; the next checkpoint cycle
supplies the updated version. Other read failures do not authorize advancing
durable progress.

A digest identifies content equality rather than chronology or message
identity. Two identical utterances remain distinct messages. A pricing update
does not invalidate unchanged visible text. When reconciling a rebuilt archive,
you can use retained digests to avoid fetching bodies you already hold.

### Secrets are masked by default

Before writing message bodies, AgentsView masks detected secrets such as API
keys, tokens, and private keys. The export uses the same detector and masked
form as search results and the secrets list, for example `AKIA…PLJM` for an AWS
access key. Detection is best-effort; see
[SECURITY.md](https://github.com/kenn-io/agentsview/blob/main/SECURITY.md#secrets-subsystem)
for its limits.

AgentsView masks the full message before splitting it into chunks, so a secret
that spans a chunk boundary is still caught. When a body contains a masked
secret:

- The response sets `redacted: true`.
- `text_bytes`, `offset`, and `next_offset` count bytes of the masked text.
  They can differ from the `text_bytes` in the changes listing, which counts
  the stored message.
- `digest` still identifies the stored message, so the masked text does not
  hash to it. Keep using `digest` to check whether a message changed.

To export the stored text unchanged, pass `--reveal` on every body read for
that message:

```bash
agentsview export conversations message SESSION_ID MESSAGE_ID \
  --database-id DATABASE_ID --revision REVISION --reveal
```

With `--reveal`, `redacted` is omitted and the body's `text_bytes` matches the
changes listing. Do not mix masked and revealed reads of the same message,
because their byte offsets differ. Because the changes listing never contains
text, it has no `--reveal` flag.

## What text means

By default, `--text-mode stored` exports the `content` archived on user and
assistant records. Separate thinking and tool fields are omitted, while inline
renderings already present in that content stay intact. System messages and
tool result messages are omitted in both modes.

### Export classified dialogue

To export the archive's classified `dialogue_text`, pass `--text-mode visible`
to both the changes listing and each body read:

```bash
agentsview export conversations changes --text-mode visible
agentsview export conversations message SESSION_ID MESSAGE_ID \
  --text-mode visible --database-id DATABASE_ID --revision REVISION
```

Visible mode uses the same stored dialogue as local message search when its
parser has classified the source text. It keeps user and assistant text without
rendered tool calls or reasoning, while authored text that quotes tool markers,
code, or logs remains text. Secrets are masked the same way as in stored mode.

This mode requires stored classification. Unlike message search, it does not
fall back to rendered `content` when `dialogue_text` is missing. Those records
return `gap: "visible_text_unavailable"`, which includes older records and
formats that do not yet populate dialogue. An explicitly empty dialogue
projection returns `gap: "no_visible_text"`. Both return `text: null`, zero
text bytes, and no digest, and neither means a message was deleted. Your
consumer decides how a new gap affects a body it already retained.

Responses stay on schema 1 and include `text_mode: "visible"` when you select
visible mode. Digests, byte lengths, and UTF-8 chunks describe the selected
text. Both modes share message identities, and changes to either text
projection advance the message revision. Cursors and checkpoints are
mode-specific, so keep separate progress and body caches for stored and visible
modes. Passing a stored-mode checkpoint to visible mode, or the reverse, fails.

On the next writable archive open, AgentsView upgrades derived export metadata
from already stored dialogue. This preserves message identities and needs no
source files or parser resync. Export cannot reconstruct classification that is
missing from the archive. Normal parser upgrades can supply it when the
original sources are still available, while source-less archives keep their
stored content and report visible-mode gaps.

This release's scoped parser upgrade also preserves dialogue classification for
plain-text messages and adds it to the shared Codex parser. Normal sync
reparses affected sources in place. Older versions discarded dialogue when it
equalled displayed content, so those records cannot be classified retroactively
without their sources. Keeping the classification also means storing the
dialogue field even when its text equals displayed content.

When `archive_content=usage` is set, fetching message text fails with an error
and leaves stdout empty, even before the daemon restarts. Text-free change
listings stay available. To apply the storage policy to existing archive
content, follow the [archive content instructions](/docs/configuration/#archive-content).

Existing session summaries stay content-free. Their usage and cost are session
facts, not measured per-message costs or human working time. Join records by
archive and session identity, and do not spread session cost evenly across
messages.

### Session context in visible exports

In visible mode, every live change and text chunk includes a `session` object
with `agent` and `automation`. `automation` uses the same disjoint categories
as Activity: `interactive`, `automated`, or `subagent`. Delegation takes
precedence over automation, including for a worker without a known parent. You
do not need a separate session-summary export to interpret a prompt's context.

When these session fields change, AgentsView publishes a session change even if
message text is unchanged. Session context is read in the same SQLite snapshot
as project evidence and text, and consumers should compare it across chunked
reads the same way they compare project evidence. Deletions omit session
context; its absence does not mean a retained message came from an interactive
session.

## Session tool summaries

Exported message text omits tool calls. To see what a session's tools did
without exporting tool names, arguments, or results, add `--tool-summaries` to
the changes listing:

```bash
agentsview export conversations changes --tool-summaries
agentsview export conversations changes --tool-summaries --checkpoint SAVED_CHECKPOINT
```

With this flag, the listing also includes `tool_summary` changes, one per
session. Each carries the session's project reference and a `tool_summary`
object with the session's complete current totals:

```json
{
  "duration_ms": 70000,
  "categories": [
    {"category": "Bash", "calls": 3, "observed": 2, "errored": 1, "timed": 2, "duration_ms": 60000},
    {"category": "Read", "calls": 1, "observed": 1, "errored": 0, "timed": 1, "duration_ms": 20000}
  ],
  "skills": [{"name": "review-change", "calls": 2}]
}
```

- `categories` holds one entry per tool category, sorted by name. `calls`
  counts every call in that category, including any still running.
- `observed` counts the calls whose result AgentsView classified, and `errored`
  counts the ones classified as an error. Measure error rates against
  `observed`, not `calls`. Because classification reads result text, `errored`
  signals trouble rather than proving the task failed.
- `timed` counts the calls with a measured start and end, and `duration_ms`
  covers only those calls. An untimed call has an unknown duration, not a zero
  duration.
- The top-level `duration_ms` merges overlapping calls. Because category
  durations can overlap each other, they do not add up to that total. Durations
  follow the session's timing view and are clipped to the session's recorded
  start and end. When a session has no recorded start, the timing view clips to
  the first prompt while the summary does not.
- `skills` lists the names of the skills the session invoked, sorted by name.
- Each subagent session has its own summary. Its parent's delegation call
  already spans it, so do not add a parent's totals to its subagents'.

AgentsView sends a summary again whenever the session's transcript, start, or
end changes, when a call's result is classified, when a delegated subagent
session arrives, is deleted, or changes its start or end, when its storage
policy gap changes, or when the session is trashed or restored. Replace the
summary you hold; consecutive summaries can be identical. Additional rules:

- A trashed session's summary has `deleted: true` and no `tool_summary`. A
  permanently deleted session has no summary change; its session change reports
  the deletion.
- A session stored under `archive_content=usage` has
  `gap: "archive_content_excluded"` and no `tool_summary`. Its calls are not
  stored, so this means unknown rather than zero.
- A summary is not sent again when a session's project reference changes; its
  session change reports the new reference. A session with neither an exported
  message nor an earlier session change gets no session change, so its summary
  keeps the reference it was listed with.
- A walk lists summaries after its message and session changes.
- Summaries have their own `revision` sequence. Compare a summary's revision
  only with another summary's revision, and apply the same database generation
  rule as for messages.

A cursor or checkpoint tracks how far it has delivered summaries. If you add
`--tool-summaries` to a walk whose saved checkpoint did not include them, the
walk returns every session's current summary without listing its messages
again. Running a walk without the flag gives up that position, so passing
`--tool-summaries` again later returns every summary again. Responses remain on
schema 1. An archive last written by an older version needs one writable open,
such as a daemon restart, before summaries can be listed.

## Identity, gaps, and rebuilding

Within one source writer's lineage, the logical key is
`(archive_id, session_id, message_id)`. The physical `database_id` changes on a
full resync, and a changed generation invalidates any saved checkpoint. When
that happens, the command leaves stdout empty, writes a
`reconciliation_required` JSON error to stderr, and exits with code 4.
Reconcile the new generation instead of treating it as an empty update. The
daemon removes deleted-message records after one to two weeks, and
[`db compact`](/docs/commands/#agentsview-db-compact) removes them right away,
so a checkpoint saved before the newest removed record fails the same way.
Records of messages with a native source ID are never removed, so a returning
message keeps its `message_id`.

Native identities stored in `source_uuid` handle edits, insertions, and
deletions without turning every change into a new utterance. Some source
formats lack native message IDs. Proven append operations and unchanged
replacement snapshots can preserve their assigned identities, whereas an
arbitrary rewrite cannot be identified reliably by matching positions,
timestamps, or repeated text. That ambiguity is a coverage gap, not proof that
an earlier citation still refers to the same utterance.

A full resync counts as a replacement too. If a session's complete projection
changes, messages without native IDs can become `identity_ambiguous` even when
their own text did not change. An identical complete projection preserves those
IDs, and so does a projection that is identical except that messages gain
native IDs, each used once, such as after a parser upgrade starts keeping them;
those messages lose their `identity_unavailable` gap. Every exported message
corresponds to a normalized database message record.

Messages without stored content and content excluded by archive policy are
explicit gaps, not empty successful conversations. The `gap` values distinguish
`visible_text_unavailable`, `archive_content_excluded`, `identity_unavailable`,
and `identity_ambiguous`. An identity gap can still have readable text, while a
content gap has `text: null`. Retained orphaned sessions are not deleted merely
because their source files are unavailable. A deletion record and unavailable
source content mean different things, and each consumer decides its own
retention and erasure policy.

Usage-only replacements and rebuilds keep existing message IDs, clear their
text and digests, and mark them `archive_content_excluded`. The session gap
covers activity without retained message records. Only a complete content
replacement can establish that a message was removed; a usage-only projection
cannot.

Consumers must implement idempotent writes and handle revisions. The CLI does
not track acknowledgments for a destination or promise exactly-once network
delivery. When you analyze exported prose, treat it as untrusted source
material.

## Storage and work per poll

The local archive stores a digest of each current message in its export index
along with compact change and deletion metadata. The text itself stays in the
archived message and is read back by position. The archive does not keep every
intermediate body as an event log or track individual consumers. Changes
queries seek by publication revision; unchanged polling does not walk
transcript bodies. A text fetch returns at most the requested bytes, but it
reads and hashes the whole message to verify it against the pinned revision.
Archives written by this version store no text in the export index, so an older
exporter cannot serve their bodies. If your consumer needs past revisions,
retain them yourself.

Activity history uses `agentsview export range` to find a starting date,
followed by the existing hour, day, and digest exports. This does not make
historical activity digest calculation incremental, nor does it establish a
text-collection policy for a downstream system.
