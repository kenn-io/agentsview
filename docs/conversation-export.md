---
title: Conversation Export
description: Incremental exports of stored user and assistant messages
---

Use conversation exports to maintain a lightweight copy of stored user and
assistant messages for reporting or analysis. Read a text-free changes listing
first, select the sessions you want, then fetch only new or changed text.
Activity exports remain separate: messages from an ongoing session do not wait
for a reporting hour to close.

These commands read normalized messages from the local SQLite archive. They work
across all supported agents and imported archives, including sessions whose
source files are no longer available. They do not contact an agent, reparse its
transcripts, start a server, or read the DuckDB mirror.

The first `changes` call on an archive builds the conversation change index from
existing database records, so it takes longer than later calls; syncing never
pays for it. The build runs through the daemon when one owns the archive, and
directly otherwise. Reading a message before that first call fails without
returning a checkpoint. Existing session and reporting exports keep their
current formats.

Version 0.44.0 also corrects how Codex prompts are stored. Existing archives
need the normal startup resync to refresh those records before read-only exports
can run. Sessions whose source files are gone retain their stored content. See
the [changelog](/docs/changelog/#0440) for the parser correction.

## Start a copy and keep it current

```bash
agentsview export conversations changes --limit 100
agentsview export conversations changes --cursor PAGE_CURSOR
agentsview export conversations changes --checkpoint SAVED_CHECKPOINT
```

The first call, without a cursor or checkpoint, starts from the beginning of the
retained changes. Each JSON page contains:

- `schema_version`, `archive_id` and `database_id` identifying the contract and
  source;
- `changes`, containing message or session metadata, but no message text;
- `next_cursor` for an unfinished walk, or `checkpoint` after its last page.

Finish every page before saving the new checkpoint. Save it together with the
consumer's durable progress so an interrupted import can replay the same work.
Use the checkpoint on the next poll. An unchanged archive returns no changes.

This is compact current state, not an event history or a globally frozen
snapshot. A message changed again during pagination appears in the next cycle.
Continue polling to catch up. Do not infer deletion from absence in a page or
from an interrupted walk.

Message changes identify the session and stable message, its revision, order,
role, optional source timestamp, text digest and byte count. Session changes
refresh project evidence independently of text. Resolve project sharing before
fetching bodies; a display label alone does not establish repository identity.
The project reference follows the evidence rules in
[Session Export](/docs/session-export/#project-evidence).

## Read only the text you need

```bash
agentsview export conversations message SESSION_ID MESSAGE_ID \
  --database-id DATABASE_ID --revision REVISION --max-bytes 65536

agentsview export conversations message SESSION_ID MESSAGE_ID \
  --database-id DATABASE_ID --revision REVISION --offset NEXT_OFFSET
```

Take the identifiers and revision from the changes listing. The response
includes the current project reference from the same read snapshot as the text.
Check it again before disclosing the body to another system.

`text` contains a UTF-8-safe chunk. `offset` and `next_offset` are byte offsets;
`text_bytes` describes the complete message. Continue until `next_offset`
reaches `text_bytes`, preserving the same generation and revision on every
request. `--max-bytes` defaults to 65,536 and must allow at least four bytes,
the maximum length of one UTF-8 character. A stale revision fails instead of
returning a different message version. No text is silently truncated.

A superseded revision leaves stdout empty, emits `revision_changed` JSON on
stderr and exits with code 5. Discard any partial body for that revision and
finish the changes walk; the next checkpoint cycle supplies the correction.
Other read failures do not authorize advancing durable progress.

A digest identifies content equality, not chronology or message identity. Two
identical utterances remain distinct messages. A pricing update does not
invalidate unchanged visible text. Consumers may use retained digests to avoid
fetching bodies they already hold when reconciling a rebuilt archive.

## What text means

The default `--text-mode stored` exports the `content` archived on user and
assistant records. Separate thinking and tool fields are omitted, but inline
renderings already present in that content remain. System messages and tool
result messages are omitted in both modes.

### Export classified dialogue

Use `--text-mode visible` on both the changes listing and each body read to
export the archive's classified `dialogue_text`:

```bash
agentsview export conversations changes --text-mode visible
agentsview export conversations message SESSION_ID MESSAGE_ID \
  --text-mode visible --database-id DATABASE_ID --revision REVISION
```

This uses the same stored dialogue that local message search uses, when its
parser has classified the source text. It keeps user and assistant text without
rendered tool calls or reasoning. Authored text that quotes tool markers, code
or logs remains text; this is not a secret-redaction feature.

The export requires stored classification. Unlike message search, it does not
fall back to rendered `content` when `dialogue_text` is absent. Such records
return `gap: "visible_text_unavailable"`, including older records and formats
that do not yet populate dialogue. An explicitly empty dialogue projection
returns `gap: "no_visible_text"`. Both have `text: null`, zero text bytes and no
digest. Neither case means a message was deleted. Consumers decide how a new gap
affects a previously retained body.

Responses remain schema 1 and add `text_mode: "visible"` when selected. Digests,
byte lengths and UTF-8 chunks describe the selected text. Message identities are
shared across modes; changes to either text projection advance the message
revision. Cursors and checkpoints are mode-specific, so keep separate progress
and body caches for the two selections. Passing a stored-mode checkpoint to
visible mode, or the reverse, fails.

The next writable archive open upgrades the derived export metadata from already
stored dialogue. It preserves message identities and does not require source
files or a parser resync. Export cannot reconstruct classification that is
absent from the archive. Normal parser upgrades may supply it when the original
sources remain available; source-less archives retain their stored content and
report visible-mode gaps.

This release's scoped parser upgrade preserves dialogue classification for
plain-text messages too and adds it to the shared Codex parser. Normal sync
reparses affected sources in place. Older versions discarded dialogue when it
equalled displayed content, so those records cannot be classified retroactively
without their sources. Keeping the classification also means storing the
dialogue field when its text equals displayed content.

With `archive_content=usage`, fetching message text fails with an error and
leaves stdout empty, even before the daemon restarts. Text-free change listings
remain available. To apply the storage policy to existing archive content,
follow the [archive content instructions](/docs/configuration/#archive-content).

Existing session summaries remain content-free. Their usage and cost are session
facts, not measured per-message costs or human working time. Join records
through archive and session identity; do not spread session cost evenly over
messages.

### Session context in visible exports

Each live change and text chunk in visible mode includes a `session` object with
`agent` and `automation`. Automation uses the same disjoint categories as
Activity: `interactive`, `automated`, or `subagent`. Delegation takes precedence
over automation, including a worker without a known parent. Consumers do not
need a session-summary export to interpret a prompt's context.

Changes to these session fields publish a session change even when message text
stays unchanged. Session context is read in the same SQLite snapshot as project
evidence and text. Consumers should compare it across chunked reads, as they do
project evidence. Deletions omit it; absence is not evidence that a retained
message came from an interactive session.

## Identity, gaps and rebuilding

The logical key is `(archive_id, session_id, message_id)` within one source
writer's lineage. The physical `database_id` changes on full resync. A changed
generation invalidates a saved checkpoint: the command leaves stdout empty,
writes a `reconciliation_required` JSON error to stderr, and exits with code 4.
Reconcile the new generation rather than treating it as an empty update.

Native identities stored in `source_uuid` support edits, insertion and deletion
without turning every change into another utterance. Some source formats lack
native message IDs. Proven append operations and unchanged replacement snapshots
can preserve their assigned identities; an arbitrary rewrite cannot be
identified reliably by matching positions, timestamps or repeated text. Such
ambiguity is a coverage gap, not proof that a previous citation still identifies
the same utterance.

A full resync is a replacement too. If a session's complete projection changes,
messages without native IDs can become `identity_ambiguous` even when their own
text is unchanged. An identical complete projection preserves those IDs. Each
exported message corresponds to a normalized database message record.

Messages without stored content and content excluded by archive policy are
explicit gaps. They are not empty successful conversations. The `gap` values
distinguish `visible_text_unavailable`, `archive_content_excluded`,
`identity_unavailable`, and `identity_ambiguous`. An identity gap may still have
readable text; a content gap has `text: null`. Retained orphaned sessions are
not deleted merely because their source files are unavailable. A deletion record
and unavailable source content have different meanings; consumers decide their
own retention and erasure policy.

Usage-only replacements and rebuilds keep existing message IDs, clear their text
and digests, and mark them `archive_content_excluded`. The session gap covers
activity without retained message records. Only a complete content replacement
can establish that a message was removed; a usage-only projection cannot.

Consumers must implement idempotent writes and handle revisions. The CLI does
not track acknowledgments for a particular destination or promise exactly-once
network delivery. Treat exported prose as untrusted source material when using
it for analysis.

## Storage and work per poll

The local archive keeps a digest of each current message in its export index,
plus compact change and deletion metadata; the text itself stays in the archived
message and is read back by position. It does not retain each intermediate body
as an event log or track individual consumers. Changes queries seek by
publication revision; unchanged polling does not walk transcript bodies. A text
fetch returns at most the requested bytes but reads and hashes the whole message
to verify it against the pinned revision. Archives written by this version hold
no text in the export index, so an older exporter cannot serve their bodies. A
consumer that needs past revisions must retain them itself.

Activity history uses `agentsview export range` to discover a starting date,
then the existing hour, day and digest exports. This does not make historical
activity digest calculation incremental or establish a text-collection policy
for a downstream system.
