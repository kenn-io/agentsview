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
  refresh re-derives identities against the new archive, so unchanged
  documents keep their vectors and only genuinely changed content re-embeds.
- **It's self-contained.** The mirror copies message content into `vectors.db`
  rather than joining back to the archive, so the vector store never needs the
  archive open to serve a query, and `vectors.db` can be deleted and rebuilt
  (`embeddings build --full-rebuild`) without touching `sessions.db`.

Tables inside the archive DB were rejected: that would tie vector writes to the
archive's write path and lock, and complicate the resync-swap story with
special-casing during the swap instead of a plain re-scan afterward.

### Embedding stores and `IndexSpec`

`vectors.db` can host more than one embedding store. Each store is bound by an
`IndexSpec` (internal/vector): its documents table, its own key/value metadata
table, and a table prefix for the kit-managed generation bookkeeping.
Schema-version resets, metadata (watermarks, scope, generation model names), and
generation lifecycles are all scoped per store, so resetting or rebuilding one
store never touches another. The conversation message store described in this
document is the spec returned by `MessageIndexSpec()`;
`embeddings list/activate/retire` accept `--store` (default `messages`) because
generation IDs are only unique within a store.

## Unit model: user documents and runs

The index does not embed every message individually. The embeddable universe —
`role IN ('user','assistant')`, non-system, non-system-prefixed (per
`SystemPrefixSQL`), from non-trashed sessions — is reduced by
`db.ScanEmbeddableUnits` into **unit documents**:

- **User documents**: one document per embeddable user message.
- **Run documents**: a maximal sequence of contiguous embeddable assistant
  messages within one session, bounded by embeddable user rows, session edges,
  and `is_sidechain` transitions (a contiguous sidechain block forms its own
  run and never mixes with non-sidechain messages). Member texts are joined in
  ordinal order with a single blank line (`"\n\n"`); no role labels, markers,
  or metadata are injected into the embedded text.

The boundary term is deliberately "embeddable user row", not "human turn": user
rows the system-prefix filter excludes (interruptions, task notifications,
command wrappers, continuation banners) are invisible to the reducer and do not
split runs — there is no separate "does this row split" detector to get wrong. A
run of one message degenerates to per-message behavior.

Run grouping exists because per-message embedding diluted retrieval: most
assistant messages are short, procedural narration ("Let me check the file")
that is context-poor as a standalone semantic unit, so per-message vectors were
mostly near-duplicate fragments of long work stretches. Grouped between human
turns, roughly 1.1M assistant messages collapse into ~44k runs (~25x fewer
assistant-side documents), so a "reconstruct this design decision" query matches
narrative, not fragments.

### Subordinate classification

A unit is **subordinate** when any of the following holds:

- its members have `is_sidechain = 1` (a sidechain transition always closes the
  run first, so every member of one run shares a single value);
- its session's `relationship_type` is `subagent` or `fork`;
- its session is parent-linked (`parent_session_id <> ''`) with any relationship
  type other than `continuation` — defensive, covering empty or unknown types.

Continuations are deliberately top-level, deviating from the sidebar's
child-session convention: embedding cares about content provenance, a
continuation is the same human-driven conversation, its replayed banner is
already excluded as system-prefixed, and its new content is unique. Forks follow
the existing child convention because their prefix replays parent content —
deduplication happens by downranking, not exclusion. Subordinate units stay in
the index and stay searchable; they are penalized and annotated at search time
(see [Search path](#search-path)), never hidden by default.

### The `vector_messages` mirror table

One row per unit. Columns: `doc_key` (primary key), `session_id`, `source_uuid`
(the unit's first member's), `ordinal` (the unit's first member's ordinal — the
retained unique `(session_id, ordinal)` index makes this the slot invariant: one
unit per starting ordinal), `ordinal_end` (the last member's ordinal; equal to
`ordinal` for user documents), `subordinate`, `offsets`, `content` (the unit's
joined text), `content_hash` (sha256 of content, kit's revision column),
`embed_gen`.

`offsets` is a JSON array with one entry per member message in ordinal order —
`[{"o": <ordinal>, "r": <rune_start>, "b": <byte_start>}, ...]` — ends implied
by the next entry or the content length. Rune offsets map kit chunk windows back
to member messages (anchoring); byte offsets slice snippets without re-decoding.
User documents store `[]`, so consumers parse one shape unconditionally.

### `doc_key` scheme

`internal/vector/mirror.go` builds `doc_key` from the unit's first member:

- `u:<session_id>:<source_uuid>` (user document) or
  `r:<session_id>:<source_uuid>` (run document) when the first member has a
  `source_uuid` — with a `#<n>` occurrence suffix when more than one message
  in a session shares the same `source_uuid`. `n` is a 1-based counter
  assigned in `(session_id, ordinal)` scan order and shared across unit kinds,
  so it's deterministic across resyncs.
- `o:<session_id>:<ordinal>` or `ro:<session_id>:<ordinal>` otherwise (legacy
  parsed data with no per-message UUID).

`session_id` and `source_uuid` are percent-escaped before joining — a custom
escape that only encodes `%`, `:`, and `#` as `%XX`, not a general URL-encoder —
so a literal colon, hash, or percent sign inside either component can't be
mistaken for one of the key's own delimiters, and an occurrence-suffix-shaped
`source_uuid` can't collide with a real occurrence suffix.

Keying a run on its *first* member makes run identity stable at the active tail:
a run that grows a trailing message keeps its `doc_key` — its `content_hash`
changes and the run re-embeds, which is the intended cost. A new user turn
landing mid-run after a resync splits the run: the second half becomes a new
document and the old one shrinks; the mirror's reconciliation and two-phase
eviction handle both.

UUID-keyed rows survive ordinal renumbering (e.g. from a resync) as a cheap
`ordinal`/`content_hash` update with no re-embed. Ordinal-keyed rows become a
new document whenever their ordinal shifts, and re-embed — an accepted cost that
only affects data parsed before per-message UUIDs existed.

## Mirror schema versioning

`vector_meta` carries a `mirror_schema_version` key (currently `"3"`). It covers
both the mirror's DDL shape and its document-identity scheme — what one
`vector_messages` row *means* — and is bumped whenever either changes in a way
old rows cannot simply be read as-is. History: `"2"` added the
`ordinal_end`/`subordinate`/`offsets` columns while still holding one row per
message; `"3"` switched document identity to run-grouped units with no DDL
change.

On a mismatch — including the key being absent while any mirror state already
exists:

- **Write path** (daemon, CLI build): `Open` drops every mirror-state table in
  `vectors.db` — `vector_messages`, `vector_meta`, and every kit-owned
  `message_vectors*` table, including vec0 tables left behind by retired or
  abandoned generations — recreates the current schema, and restamps the
  version, so the next build takes the existing first-ever full-build path.
  `embeddings activate` and `retire` also open read-write on their direct
  (no-daemon) path (`directGenerationAction` in
  `cmd/agentsview/embeddings.go`), so against a mismatched `vectors.db` they
  trigger the same reset and then fail with "generation not found", the reset
  having removed every generation. `vectors.db` is disposable by design;
  `sessions.db` is never reset this way.
- **Read path** (read-only `Open`: CLI reads, direct-install search): `Open`
  succeeds without touching any table, but every subsequent `Search`,
  `StaleActive`, `Generations`, or `ResolveMessageUnits` call fails closed
  with the typed sentinel `vector.ErrMirrorVersionMismatch` ("vector index was
  built by an incompatible version: run `agentsview embeddings build`") rather
  than risk misreading rows shaped by a different scheme. The search wiring
  maps the sentinel onto `ErrSemanticUnavailable`, so it surfaces exactly like
  the stale-fingerprint gate — semantic search stays wired and returns
  rebuild-required (HTTP 501) instead of silently unwiring.

The mirror version and the generation fingerprint (next section) are two
independent gates, and both are required: the version resets incompatible mirror
*state*, while the fingerprint cuts a new generation when the embedding
*configuration or scheme* changes even if the mirror were somehow current.

## Generations and fingerprints

The vector index moves through kit's generation lifecycle: **building → active →
retired**. A generation's fingerprint is derived from `model` + `dimension` +
the params map
`{max_input_chars, doc_unit_scheme: "run_v1", chunk_overlap_chars}`
(`vectorGeneration` in `cmd/agentsview/embeddings.go`), plus `input_suffix` when
configured — an empty suffix is omitted from the map rather than included as
`""`, so configs written before the key existed keep their fingerprints.
`chunk_overlap_chars` is computed by `vector.ChunkOverlap` —
`max_input_chars * 15 / 100` — the same function `Open` uses for kit's
`SplitOptions`, so the split behavior and its fingerprint can never drift apart.
Changing any input — the model, the dimension, the chunking cap, the input
suffix, the overlap formula, or the document-unit scheme — produces a different
fingerprint and cuts a new generation. Which
`[vector.embeddings.servers.<name>]` entry encoded a document is deliberately
*not* a fingerprint input: every server serves the same globally-configured
model, so their vectors are interchangeable and a build may switch servers
(`embeddings build --using <name>`) without invalidating the generation.

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

## Chunking and anchoring

Unit content is chunked by kit's `Split` with `MaxRunes = max_input_chars`
(default 8192) and `Overlap = ChunkOverlap(max_input_chars)` — 15% of the cap:
1228 runes at the default, 375 at a 2500 cap.

A hit on a run document is **anchored** to one member message: the member whose
rune span contains the matched chunk's center rune,
`chunk_start + len(chunk_runes)/2`. The center uses the chunk's *actual* rune
length, not `MaxRunes`, so a short final chunk anchors at its true center. Each
member owns only its own text span — the `"\n\n"` separator before the next
member belongs to the gap between spans — so a center falling inside a separator
anchors the earlier member, while a center exactly at a member's first rune
anchors that member. The chunk window is reproduced deterministically from the
mirrored content via kit's `Hit.ChunkIndex` plus the same `SplitOptions`
(`chunkWindow` in `internal/vector/search.go` mirrors `kitvec.Split`'s
arithmetic and is cross-checked against it in tests).

A run hit's snippet is the intersection of the chunk's rune window with the
anchor member's span — always a substring of the anchor message's own text, so
the db layer's snippet centering can locate it inside the anchor message's
content. A stale `ChunkIndex` whose re-split window misses the member entirely
falls back to the anchor member's whole span; user documents snippet the whole
matched chunk, which is already message-local.

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
active generation). Within each scan page, up to `concurrency` (the building
server's config, default 4) documents are split and encoded in parallel; saves
into `vectors.db` stay serialized on one goroutine, preserving the single-writer
model. Requests ask for `encoding_format: "base64"` (raw little-endian float32
bytes, ~4x smaller than JSON float arrays); the encoder accepts either response
shape, and a server that rejects the field downgrades the encoder to plain float
requests for its lifetime. A document whose encode call fails with a permanent
error — a 400, 413, or 422 whose error body describes the input itself, e.g. a
token/context-length overflow or a content-policy rejection — is not retried in
that fill or the next one: it's stamped for the generation with no vectors at
its current `content_hash`, which marks it non-pending. It's logged (doc key
plus the underlying error) and counted in the build summary's skipped count, but
there is no separate poison list or periodic retry — the only way it embeds
again is if the document's content itself changes later (a new `content_hash`,
so a new pending row). Every other failure — 5xx, network errors, timeouts, 429,
and any 4xx that looks like an auth, route, model, or media-type problem rather
than a rejection of this document — aborts the fill and is retried on the next
scheduled build, so a config mistake can't silently stamp the whole corpus as
embedded-with-no-vectors.

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
- **Hits are unit-level, anchored to a message.** A semantic hit resolves to
  session + `ordinal_range` (the matched unit's span) + an anchor ordinal + a
  snippet. The existing required `ordinal` field is kept and redefined as the
  anchor ordinal — backward compatible, since for user documents and
  one-message runs it is exactly the old per-message value. `ordinal_range`,
  `subordinate`, and the lineage fields (`relationship`, `parent_session_id`,
  `is_sidechain`) are carried by every mode: semantic/hybrid unit rows take
  theirs from the mirror unit, lexical rows and hybrid unit-less rows from the
  structural derivation described in
  [Conversation-unit citations](#conversation-unit-citations). `--around` and
  the context-cursor flow anchor on the anchor ordinal — the message-window
  APIs are unchanged on every backend.
- **`scope` governs unit visibility and supersedes `include_children`.**
  `scope=top|all|subordinate` (default `all`) filters each leg's hits before
  the RRF merge and before the limit. The hybrid FTS leg fetches additional
  rank-ordered batches until it holds the fusion depth `k` of surviving
  entries (capped at `maxHybridFTSBatches`), so scope discards and same-unit
  collapse do not starve it; the semantic (KNN) leg cannot page, so scoped or
  collapse-heavy searches can still under-fill past those caps even when more
  matches exist deeper in the ranking. In semantic/hybrid modes the
  sidebar-child session exclusion is lifted (`semanticSessionScopeSubquery`) —
  both hybrid legs must see the same universe for fusion to be sound — and an
  explicit `include_children` is accepted but superseded. Subagent/fork-typed
  and parent-linked sessions are also exempted from the one-shot
  (`user_message_count <= 1`) exclusion in these modes, because a delegated
  session structurally has exactly one "user" message (the task prompt) and
  the default gate would silently exclude ~98% of subagent sessions, hollowing
  out `scope=all`; root sessions with no parent link keep the one-shot
  exclusion. All other session filters (project, agent, machine, dates,
  automated) apply in every mode, and FTS-only, substring, and regex modes
  keep today's `include_children` and one-shot semantics unchanged.
- **The subordinate penalty has exactly one implementation.** `rrfMerge` in
  `internal/db` fuses rank-ordered legs with reciprocal rank fusion (rank
  constant 60) and shifts subordinate units' effective rank by +5 — a
  rank-based adjustment, not a hard tier or score multiplier, since RRF ranks
  are the only scale comparable across legs. The merge is a local
  implementation rather than kit's `Merge` because kit has no per-hit
  rank-offset hook for the subordinate penalty (upstreamable later).
  Semantic-only search routes its single ranked list through the same merge as
  a one-leg fusion, so `--semantic` downranks subordinate hits identically to
  `--hybrid` (matches still carry the searcher's own cosine scores; only the
  order changes).
- **Hybrid fuses at unit granularity, with an FTS anchor override.** The FTS leg
  stays message-granularity (exact strings, commands, filenames) over the same
  embeddable-universe predicate `ScanEmbeddableUnits` uses. Each FTS message
  hit is resolved to its containing unit via
  `VectorSearcher.ResolveMessageUnits` — a point lookup on the mirror's unique
  `(session_id, ordinal)` index: seek the greatest unit `ordinal <= x` for the
  session, verify `x <= ordinal_end`. Units within a session never overlap, so
  no extra index is needed. Hits on the same unit fuse under one key; when the
  FTS leg contributes, the exact matched message becomes the hit's anchor
  regardless of chunk center. An FTS hit with no containing unit keeps a
  message-granularity fusion key and survives fusion on its own, carrying the
  structurally derived subordinate flag through scope filtering and the fusion
  penalty — the same classification lexical mode gives the anchor (see
  [Conversation-unit citations](#conversation-unit-citations)).
- **Metadata filters post-filter the vector leg, with over-fetch.** Vector KNN
  doesn't know about `--project`/`--agent`/`--date*`, so the vector leg
  over-fetches `max(limit × 4, 200)` candidates, then filters and truncates to
  the requested limit. At small corpora or narrow filters this can return
  fewer than `--limit` results even though more exist — a known v1 tradeoff
  (see [Limitations](/semantic-search/#limitations)).

## Conversation-unit citations

Every content-search match — every mode, every backend — carries a
conversation-unit citation: `OrdinalRange [2]int` with `json:"ordinal_range"`,
always present, never omitempty (`[ordinal, ordinal]` when the anchor is its own
unit; the array form deliberately avoids the omitempty-integer trap, so a unit
starting at ordinal 0 still serializes its start), plus the
`subordinate`/`relationship`/`parent_session_id`/`is_sidechain` lineage fields
(which keep `omitempty` — false/empty means top-level/no-lineage,
unambiguously). Row cardinality stays mode-specific — lexical
(substring/regex/FTS) returns one row per matching source row with unchanged
snippets and pagination, semantic one row per embedded unit, hybrid one row per
unit with the FTS-anchor override untouched — only the citation metadata is
uniform. The HTTP response serializes `db.ContentMatch` directly; the MCP
`contentMatch` mirror struct carries the same fields; the CLI renders
`#start-end @anchor` for multi-message ranges plus a `sub` marker, in any mode.

### Per-mode provenance

`ordinal_range` always means "conversation unit", not always "embedding unit":

- **Semantic and hybrid unit rows** carry the embedded unit's span from the
  vectors.db mirror — embedding identity, including build scope.
- **Lexical rows and hybrid unit-less rows** carry a structurally derived unit
  computed from the messages/sessions tables only. Deterministic, never
  depends on whether a vector index exists or is fresh — lexical output must
  not flicker with index state.

Derived and embedded spans coincide except where embedding scope diverges from
structure: sessions excluded from the build (`include_automated = false`) and
messages newer than the last mirror refresh. There is deliberately no provenance
discriminator field on the wire — the mode implies it.

Hybrid unit-less rows (FTS hits whose message resolves to no mirror unit) are
classified **before** scope filtering and the RRF merge, so `scope` exclusion,
the subordinate rank penalty, and annotation treat a unit-less sidechain hit
exactly as lexical mode classifies the same anchor, instead of always passing it
as top-level.

### Derived-unit rules

The structural rules mirror `ScanEmbeddableUnits`'s reducer, so derived spans
equal embedding-unit spans on in-scope data. An **embeddable user row** is
`role = 'user' AND is_system = 0` with content not system-prefixed (the dialect
`SystemPrefixSQL` predicate); an **embeddable assistant row** is the same with
`role = 'assistant'` — the prefix predicate constrains only user rows, so a
system-prefixed assistant row stays embeddable and derives its run span. For an
anchor message row at ordinal `o` (`tool_input`/`tool_result` matches anchor on
the tool call's message row):

1. Embeddable user row → `[o, o]` (user messages are their own units).
1. Embeddable assistant row → the maximal stretch of embeddable assistant rows
   containing `o`, bounded exclusively by the nearest embeddable user row on
   either side, the session edges, and the nearest embeddable assistant row
   whose `is_sidechain` differs (runs never mix sidechain values). The
   endpoints are the first and last **member** ordinals — the span may cover
   non-member ordinals in between (system rows inside a run), exactly like the
   reducer's `runUnit`.
1. Anything else (system rows, system-prefixed user rows, other roles) →
   `[o, o]`: the row belongs to no conversation unit, so the citation is the
   message itself.

`tool_result_events` matches locate their anchor message row with a post-scan
secondary lookup, never a join — the events branches join only `sessions`, and
an inner join would drop matches whose anchor row is missing, changing
cardinality. An orphan event with no locatable anchor row falls back to `[o, o]`
with `is_sidechain` false; session lineage still applies.

Automation gating is deliberately ignored: derivation is structural, so matches
inside automated sessions still get real ranges. The invariant, pinned by the
reducer-equivalence test in `internal/db/unit_range_test.go`: derivation at any
member ordinal of any unit produced by
`ScanEmbeddableUnits(include_automated = true)` returns exactly that unit's
`[Ordinal, OrdinalEnd]`.

`subordinate` for derived rows uses the reducer's formula: session-subordinate
(`relationship_type IN ('subagent','fork')`, or parent-linked with
`relationship_type <> 'continuation'`) OR the anchor row's `is_sidechain`.

### Seam architecture and batching

Derivation is a pure Go pass in `internal/db` (`DeriveUnitRanges`) over a
backend-neutral seam, `UnitBoundsQuerier`, with two batched methods:
`NearestUserBoundaries` (the nearest exclusive embeddable-user boundaries around
each probe) and `RunExtents` (the first/last member ordinals of the anchor's
same-sidechain run within an exclusive interval). `internal/db`,
`internal/postgres`, and `internal/duckdb` each implement the seam with their
own dialect SQL; `internal/db` never references the other stores, and co-located
parity and reducer-parity tests in each package pin identical observable output
across backends.

- **Shared resolvers, SQL-only backends.** `ResolveUserBoundaries` and
  `ResolveRunExtents` own the orchestration — probe dedup, chunking at the
  dialect's bind-variable limit, boundary resolution, alignment and invariant
  checks — so each backend supplies only batched SQL (one statement per chunk
  of batched correlated point lookups, never one per probe).
- **Post-scan, O(page).** The lexical search SQL before the LIMIT is untouched;
  anchor classification and session lineage are fetched after truncation, and
  derivation runs over the returned page only. Rule-1, rule-3, and missing
  anchors resolve locally with no query.
- **Dense/sparse boundary flows.** Rule-2 (embeddable assistant) anchors are
  deduplicated by `(session, ordinal, sidechain)`. `NearestUserBoundaries`
  runs only on session-dense pages — at least `UnitBoundsFlowFactor` probes
  per distinct session on average — where pre-fetched user bounds pay for
  themselves by pruning stop scans and splitting probe groups at unit
  boundaries; sparse pages probe with sentinel bounds and lean on
  `RunExtents`' built-in user-row stops (real bounds are an optimization,
  never a correctness requirement).
- **Median-representative run sharing.** Probes with the same
  `(session, bounds, sidechain)` group key that land in the same run have
  identical extents, so the first `RunExtents` round queries one
  representative per group — the group's ordinal median, since page anchors
  cluster in hot runs — and hands its extent to every group sibling the extent
  covers (sound because a same-sidechain member inside the extent provably
  belongs to that run). Siblings in other runs resolve in one second batch, so
  a page costs at most one boundary statement and two `RunExtents` rounds
  regardless of how its anchors spread. Twenty hits in one monologue cost one
  probe.
- **Cost.** Measured overhead on the gated content-search benchmarks is
  sub-millisecond-scale on a 50-hit page; the benchmarks live in `internal/db`
  and are CI-gated. If real-corpus profiling ever shows meaningful cost, the
  remedy is an explicit opt-out — citations must never silently self-disable
  based on index or corpus state.

## PostgreSQL replica

`internal/postgres` implements the same `db.VectorSearcher` seam over
[pgvector](https://github.com/pgvector/pgvector), making PostgreSQL a passive
replica of the local `vectors.db`: embeddings are never computed server-side,
and `pg push` copies the active generation's documents and chunks. The fusion
machinery — `RRFMerge`, unit fusion keys, the subordinate penalty, snippet
construction — is exported from `internal/db` and reused rather than duplicated,
so semantic/hybrid parity between backends holds by construction. The
user-facing behavior (fingerprint gate, graceful 501 degradation, shared
generations) is described in
[Semantic Search — PostgreSQL](/semantic-search/#postgresql); this section
covers the invariants.

### Schema shape

- `vector_generations` is keyed by the **same config fingerprint** as local
  generations, so machines with identical `[vector.embeddings]` configs share
  one generation; `vector_generation_machines` records contributors for
  observability.
- `vector_documents` mirrors the local `vector_messages` table: `doc_key`
  primary key plus a `UNIQUE (session_id, ordinal)` index, with full document
  content duplicated into PG so snippets and anchoring reuse the exact local
  mirror semantics (rebuilding content from `messages` at query time would
  need duplicate extraction logic that can drift).
- `vector_chunks_g<id>` is one table per generation with a `halfvec(N)` column
  and an HNSW cosine index. Per-generation tables are required because a
  pgvector column has a fixed typed dimension; `halfvec` stays under HNSW's
  dimension ceiling (covering 2560-dim models that plain `vector` cannot
  index) and halves storage. Index DDL schema-qualifies the type and operator
  (`OPERATOR(schema.<=>)`) because the session `search_path` is
  target-schema-only.
- **`vector_documents` is deliberately shared across generations.** A newer push
  can overwrite content an older generation's chunks hydrate against — the
  same transient skew the local `vectors.db` has with its single shared mirror
  table. Serve only ever targets one fingerprint-matched generation, the skew
  self-heals on the next re-push, and generation-scoping the documents would
  duplicate full content per generation for no read-path benefit. Do not "fix"
  this by forking the mirror shapes.

### Push invariants

- **Delta state lives in PG, not the local sync-state store.**
  `vector_push_state (generation_id, session_id, doc_agg_hash)` is written in
  the same transaction as the doc/chunk upserts; `doc_agg_hash` is a sha256
  over each embedded doc's full row identity (`doc_key`, `source_uuid`,
  ordinals, `subordinate`, offsets, `content_hash`), so metadata-only changes
  re-push, not just content changes. State living with the data makes a PG
  reset self-healing — state and vectors vanish together.
- **Doc replacement is park-to-sentinel.** `doc_key` is stable but ordinals
  shift, so plain `ON CONFLICT (doc_key)` upserts collide with
  `UNIQUE (session_id, ordinal)`. Each changed session first parks its
  existing rows at unique negative ordinals (seeded below `MIN(ordinal)`, like
  the local mirror's parking floor), upserts current docs onto the freed
  slots, then deletes rows still parked — docs that vanished locally —
  together with every generation's chunks for them. The cascade matters: local
  kit removal deletes a vanished doc's vectors from all generations, and
  preserving another generation's chunks in PG would leave them referencing a
  row hidden behind the read path's `ordinal >= 0` tombstone guard — dead KNN
  slots that never hydrate. Whole-session eviction, by contrast, preserves doc
  rows another generation still references (they stay at non-negative,
  hydratable ordinals), because an evicted session may merely have left this
  pusher's project filter while its docs still exist locally.
- **Changed documents leave older generations' chunks in place — by design.**
  `vector_documents` is shared across generations while chunk tables are
  per-generation, mirroring the local mirror + per-generation vec0 layout.
  When content changes, the pusher re-embeds only its active generation, so
  another generation's chunks for the same doc now pair an older embedding
  with the current shared metadata. This is a deliberate staleness trade-off,
  not a race: deleting those chunks would permanently blind readers still on
  that generation (only a pusher running that embedding config can rebuild
  them, and per-generation `vector_push_state` means such a pusher's next push
  re-sends exactly the changed docs). The skew is bounded to ranking —
  hydration always reads the current shared row, and anchor/snippet resolution
  clamps a stale chunk index (`vector.DocAnchor`): a hit anchors to a real
  current member and snippets that member's own text, never another member's
  and never a panic. Vanished docs are the opposite case and DO cascade across
  every generation, per the previous bullet.
- **Eviction is scoped by push ownership, not machine names.** Deletions apply
  only to sessions this pusher owns per its owner marker, because machine
  names are aliasable. Both the per-session push transaction and each eviction
  transaction re-probe the `sessions` row `FOR UPDATE` and re-adjudicate
  ownership inside the transaction — the delta scan's ownership read can be
  minutes stale, and without the re-probe one pusher could delete chunks a
  concurrent pusher claimed and pushed after the scan. Legacy rows with an
  empty `owner_marker` fall back to the `sessions.machine` column checked
  against this pusher's machine name and marker aliases — a legacy row naming
  another machine is a conflict on both the push and evict paths, never
  adopted. Filtered pushes additionally scope by *local* project membership,
  so a session that moved out of the project filter locally is not evicted
  based on a stale PG-side project value. Sessions whose session-phase push
  failed are never eviction candidates in the same run — a failed session
  whose embedded docs all vanished locally is counted as deferred, keeping
  vector state from running ahead of the sessions rows that failed to write.
- **A partially embedded local index defers the whole phase.** A
  same-fingerprint full rebuild clears and refills the active generation in
  place, so a push running concurrently would read partial session coverage as
  truth and evict valid PG vectors. The push source refuses to export a
  generation with missing embeddings (`ErrVectorSourceNotReady`, a clean phase
  skip), and the push re-checks the source immediately before running its
  eviction list in case a rebuild started mid-push.
- **A session is replaced only when its export matches the delta scan.**
  `ExportSessionDocs` reads docs, chunks, and the session's aggregate hash in
  one local read snapshot and returns the hash of exactly the exported set;
  the push defers the session (writing nothing) when that hash differs from
  the hash its delta scan read. Without this, a rebuild starting between the
  scan and the export could replace valid PG vectors with a partial view and
  record the scan's hash as current — a state a same-fingerprint rebuild (same
  content, same hash) would never repair. On the first deferral the push also
  re-checks the source; an unready or swapped generation stops the phase,
  since every remaining session would diverge the same way.

### Read path

- Startup wiring verifies **both** the fingerprint-matched generation row and
  its chunk table. Registering a generation and creating its chunk table are
  separate statements on the push side, so an interrupted push can leave the
  row without the table; wiring a searcher against it would fail every query
  with a missing-relation error instead of degrading to
  `ErrSemanticUnavailable`.
- The KNN query fetches **exactly k chunks** — no over-fetch multiplier — to
  match the SQLite searcher's brute-force contract; the shared caller already
  over-fetches for metadata post-filtering.
- HNSW's default candidate pool (`hnsw.ef_search = 40`) silently caps results
  below k for large fetches, so each search runs
  `SET LOCAL hnsw.ef_search = clamp(k, 40, 1000)` in the query transaction.
  For k past pgvector's 1000 ceiling, `hnsw.iterative_scan = 'relaxed_order'`
  (pgvector >= 0.8) lets the scan continue; the GUC is probed with
  `current_setting(..., missing_ok)` first so older pgvector skips it instead
  of aborting the transaction.
- The hybrid keyword leg selects ILIKE candidates in recency order over the
  embedded-universe predicates, resolves them to units through
  `ResolveMessageUnits` point lookups against `vector_documents`, and feeds
  the shared RRF merge — everything after candidate selection matches the
  SQLite contract, with the BM25-vs-recency leg-ranking difference documented
  in the [user-facing docs](/semantic-search/#hybrid-keyword-leg).

## Error taxonomy

Two sentinel errors carry every semantic/hybrid failure across CLI, HTTP, and
MCP:

| Sentinel                 | Meaning                                                                                                                                        | HTTP |
| ------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------- | ---- |
| `ErrSemanticUnavailable` | Not enabled/configured, index never finished a build, still building, stale (fingerprint mismatch), or built by an incompatible mirror version | 501  |
| `ErrSemanticTransient`   | Embeddings endpoint unreachable or timed out at query time — retryable                                                                         | 503  |

`vector.ErrMirrorVersionMismatch` is not a third sentinel at this layer: the
search wiring translates it onto `ErrSemanticUnavailable`, preserving its
rebuild-required message, so callers see the same 501 family as the
stale-fingerprint case.

The distinction matters for callers: 501 means the feature will not work until
something is configured or built; 503 means it should work and is worth
retrying. CLI and MCP surface the same cause-specific remediation text described
in the [user-facing error taxonomy](/semantic-search/#error-taxonomy).

## Skill generation

`internal/skills` renders the `agentsview-finding-history` skill (see
[Skills for coding agents](/semantic-search/#skills-for-coding-agents)) from a
single embedded template, `internal/skills/templates/finding-history.md.tmpl`
via `go:embed` — the same pattern `internal/web` uses for the frontend — with no
per-harness copies checked in. `Render` fills in a harness-specific delegation
phrase (whether the harness can dispatch a search subagent or must run the
bounded probes itself) and inserts a `generated-by` header — carrying the CLI
version and a sha256 hash of the pure template render — as a YAML comment on
line two, just inside the frontmatter fence, so the file still begins with `---`
and frontmatter-based skill discovery keeps working. Staleness and tamper
detection are hash-authoritative, not version-authoritative: `Classify` compares
a file's recorded hash against its own body hash to detect modification, and
against a fresh render's hash to detect staleness, and never consults the
version string, because dev builds all report version `"dev"` and would
otherwise be indistinguishable from one another. There is deliberately no Claude
Code plugin/marketplace packaging: that would tie distribution to one harness's
install mechanism, whereas the goal is a single `SKILL.md` artifact that any
`.agents/skills`-reading harness can consume the same way, installed directly by
the `agentsview` binary rather than a separate package manager.
