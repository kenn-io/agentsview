---
title: Semantic Search Internals
description: Architecture and invariants behind the vector index — storage, generations, build pipeline, concurrency, and search path
---

This page documents the internal design of [Semantic Search](/semantic-search/)
for maintainers extending or debugging the vector index. It assumes the
user-facing behavior described there and does not repeat configuration or CLI
usage.

## Storage layout

`vectors.db` is a separate SQLite database beside the main archive
(`sessions.db`), not a set of tables inside it. Two things follow from that:

- **It survives a parser-change resync.** A resync rebuilds and atomically swaps
  `sessions.db`; `vectors.db` is untouched by the swap. The next mirror
  refresh re-derives identities against the new archive, so unchanged messages
  keep their vectors and only genuinely changed content re-embeds.
- **It's self-contained.** The mirror copies message content into `vectors.db`
  rather than joining back to the archive, so the vector store never needs the
  archive open to serve a query, and `vectors.db` can be deleted and rebuilt
  (`embeddings build --full-rebuild`) without touching `sessions.db`.

Tables inside the archive DB were rejected: that would tie vector writes to the
archive's write path and lock, and complicate the resync-swap story with
special-casing during the swap instead of a plain re-scan afterward.

### The `vector_messages` mirror table

One row per embeddable message (`role IN ('user','assistant')`, non-system,
non-system-prefixed — the same universe FTS uses before `--exclude-system`).
Columns: `doc_key` (primary key), `session_id`, `source_uuid`, `ordinal`,
`content` (copied text), `content_hash` (sha256 of content, kit's revision
column), `embed_gen`.

### `doc_key` scheme

`internal/vector/mirror.go` builds `doc_key` as:

- `u:<session_id>:<source_uuid>` when the message has a `source_uuid` (with a
  `#<n>` occurrence suffix when more than one message in a session shares the
  same `source_uuid` — `n` is a 1-based counter assigned in
  `(session_id, ordinal)` scan order, so it's deterministic across resyncs)
- `o:<session_id>:<ordinal>` otherwise (legacy parsed data with no per-message
  UUID)

`session_id` and `source_uuid` are percent-escaped before joining — a custom
escape that only encodes `%`, `:`, and `#` as `%XX`, not a general URL-encoder —
so a literal colon, hash, or percent sign inside either component can't be
mistaken for one of the key's own delimiters, and an occurrence-suffix-shaped
`source_uuid` can't collide with a real occurrence suffix.

UUID-keyed rows survive ordinal renumbering (e.g. from a resync) as a cheap
`ordinal`/`content_hash` update with no re-embed. Ordinal-keyed rows become a
new document whenever their ordinal shifts, and re-embed — an accepted cost that
only affects data parsed before per-message UUIDs existed.

## Generations and fingerprints

The vector index moves through kit's generation lifecycle: **building → active →
retired**. A generation's fingerprint is derived from `model` + `dimension` +
`max_input_chars` — changing any of them, including the chunking cap, produces a
different fingerprint.

- `embeddings build` (incremental): mirror refresh, then fill whatever the
  active generation is missing.
- `embeddings build --full-rebuild`: if the target fingerprint differs from the
  active generation's, cuts a new generation and fills it fully, activating on
  clean completion; if the fingerprint is unchanged (e.g. rebuilding after a
  content-only change), it resets and refills the *existing* active generation
  in place — clearing its vectors, chunks, and stamps but keeping the
  generation row — rather than cutting a new one.
- The staleness gate checked at query time is exactly this: the active
  generation's stored fingerprint no longer matches the fingerprint computed
  from the current `[vector.embeddings]` config.

## Build pipeline

### Mirror refresh (scan)

Before a fill, the mirror is reconciled against the archive's embeddable
universe: new identities are inserted, `ordinal`/`content_hash` updated on
existing ones, and identities no longer present removed.

Removal is two-phase, and the ordering matters: deleting only the
`vector_messages` row would leave its vectors occupying KNN slots (the query
path filters them from hits, but the slots themselves are never reclaimed).
Removal always deletes the document's vectors first, then the mirror row — and,
within a single scan, a row that's merely displaced (for example a duplicate
`source_uuid` shifting occurrence) is parked at a negative sentinel ordinal
instead of being deleted outright, so a same-scan reinsert under the same
`doc_key` survives via upsert and keeps its `embed_gen` rather than
re-embedding.

### Fill and skip-and-stamp

Fill embeds every pending document (content changed, or never embedded, for the
active generation). A document whose encode call fails with a permanent error —
any 4xx except 429, e.g. token-window overflow or a content-policy rejection —
is not retried in that fill or the next one: it's stamped for the generation
with no vectors at its current `content_hash`, which marks it non-pending. It's
logged (doc key plus the underlying error) and counted in the build summary's
skipped count, but there is no separate poison list or periodic retry — the only
way it embeds again is if the message's content itself changes later (a new
`content_hash`, so a new pending row). All other failures (5xx, network errors,
timeouts, 429) abort the fill and are retried on the next scheduled build.

### Scope (`include_automated`)

Whether automated sessions are in the embeddable universe is stored in
`vector_meta` (`scope_include_automated`). Changing it — in config or via the
one-off `--include-automated` flag — forces a full mirror *reconciliation*, not
a re-embed: it inserts or removes rows to match the new scope, but documents
that stay in scope and are unchanged keep their existing stamps.

## Concurrency and locking

`vectors.db` follows the archive's single-writer model, with its own lock file
(`vectors.write.lock`) separate from the archive's `db.write.lock` so fills
never contend with archive writes:

- With a writable daemon running, `embeddings build`/`activate`/`retire` proxy
  to it over HTTP; the daemon holds `vectors.write.lock` for its lifetime and
  serializes all builds through one in-process `Manager`.
- Without a daemon, the CLI takes the same `vectors.write.lock` itself and runs
  the build in-process.

The after-sync scheduler debounces sync-completion signals about 30s before
triggering a build, and never blocks sync on embedding. A build already in
progress causes a new trigger to be dropped, not queued — the pending state is
left set so the next debounce or backstop tick picks it up. A periodic backstop
(`backstop_interval`, default 24h) runs a full reconciliation independent of
sync activity, to catch stragglers from crashes or transient encode failures; if
a backstop tick lands while a build is already running, it's remembered so the
*next* build, not the next 24h tick, carries the full-reconciliation flag.

Generation activation always happens under the single writer. Search opens
`vectors.db` read-only from any process, with no locking.

## Search path

- **Active generation only.** Search never falls back to a building or retired
  generation — if only a building generation exists, it hard-errors with a
  progress percentage rather than silently querying partial data.
- **Hybrid is RRF over two legs, filtered before merge.** `--hybrid` runs the
  vector leg and the FTS leg over the same corpus (embeddable messages only),
  applies session and metadata filtering to each leg independently, and only
  then fuses them with reciprocal rank fusion — so RRF never spends its rank
  budget on candidates a filter would have dropped anyway.
- **Metadata filters post-filter the vector leg, with over-fetch.** Vector KNN
  doesn't know about `--project`/`--agent`/`--date*`, so the vector leg
  over-fetches `max(limit × 4, 200)` candidates, then filters and truncates to
  the requested limit. At small corpora or narrow filters this can return
  fewer than `--limit` results even though more exist — a known v1 tradeoff
  (see [Limitations](/semantic-search/#limitations)).

## Error taxonomy

Two sentinel errors carry every semantic/hybrid failure across CLI, HTTP, and
MCP:

| Sentinel                 | Meaning                                                                                               | HTTP |
| ------------------------ | ----------------------------------------------------------------------------------------------------- | ---- |
| `ErrSemanticUnavailable` | Not enabled/configured, index never finished a build, still building, or stale (fingerprint mismatch) | 501  |
| `ErrSemanticTransient`   | Embeddings endpoint unreachable or timed out at query time — retryable                                | 503  |

The distinction matters for callers: 501 means the feature will not work until
something is configured or built; 503 means it should work and is worth
retrying. CLI and MCP surface the same cause-specific remediation text described
in the [user-facing error taxonomy](/semantic-search/#error-taxonomy).
