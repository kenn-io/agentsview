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
  `vectors.db` is disposable by design; `sessions.db` is never reset this way.
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
(`vectorGeneration` in `cmd/agentsview/embeddings.go`). `chunk_overlap_chars` is
computed by `vector.ChunkOverlap` — `max_input_chars * 15 / 100` — the same
function `Open` uses for kit's `SplitOptions`, so the split behavior and its
fingerprint can never drift apart. Changing any input — the model, the
dimension, the chunking cap, the overlap formula, or the document-unit scheme —
produces a different fingerprint and cuts a new generation.

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
active generation). A document whose encode call fails with a permanent error —
a 400, 413, or 422 whose error body describes the input itself, e.g. a
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
  session + `ordinal_start`..`ordinal_end` + an anchor ordinal + a snippet.
  The existing required `ordinal` field is kept and redefined as the anchor
  ordinal — backward compatible, since for user documents and one-message runs
  it is exactly the old per-message value. `ordinal_start`, `ordinal_end`,
  `subordinate`, and the lineage fields (`relationship`, `parent_session_id`,
  `is_sidechain`) are additive, populated only by the semantic/hybrid modes;
  lexical modes emit byte-identical JSON to before. `--around` and the
  context-cursor flow anchor on the anchor ordinal — the message-window APIs
  are unchanged on every backend.
- **`scope` governs unit visibility and supersedes `include_children`.**
  `scope=top|all|subordinate` (default `all`) filters each leg's hits before
  the RRF merge and before the limit, so a scoped search still fills up to the
  limit from the over-fetched candidates. In semantic/hybrid modes the
  sidebar-child session exclusion is lifted (`semanticSessionScopeSubquery`) —
  both hybrid legs must see the same universe for fusion to be sound — and an
  explicit `include_children` is accepted but superseded. Subagent/fork-typed
  and parent-linked sessions are also exempted from the one-shot
  (`user_message_count <= 1`) exclusion in these modes, because a delegated
  session structurally has exactly one "user" message (the task prompt) and
  the default gate would silently exclude ~98% of subagent sessions, hollowing
  out `scope=all`; top-level sessions keep the one-shot exclusion. All other
  session filters (project, agent, machine, dates, automated) apply in every
  mode, and FTS-only, substring, and regex modes keep today's
  `include_children` and one-shot semantics unchanged.
- **The subordinate penalty has exactly one implementation.** `rrfMerge` in
  `internal/db` fuses rank-ordered legs with reciprocal rank fusion (rank
  constant 60) and shifts subordinate units' effective rank by +5 — a
  rank-based adjustment, not a hard tier or score multiplier, since RRF ranks
  are the only scale comparable across legs. Semantic-only search routes its
  single ranked list through the same merge as a one-leg fusion, so
  `--semantic` downranks subordinate hits identically to `--hybrid` (matches
  still carry the searcher's own cosine scores; only the order changes).
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
  message-granularity fusion key, is never subordinate-penalized, and survives
  fusion on its own.
- **Metadata filters post-filter the vector leg, with over-fetch.** Vector KNN
  doesn't know about `--project`/`--agent`/`--date*`, so the vector leg
  over-fetches `max(limit × 4, 200)` candidates, then filters and truncates to
  the requested limit. At small corpora or narrow filters this can return
  fewer than `--limit` results even though more exist — a known v1 tradeoff
  (see [Limitations](/semantic-search/#limitations)).

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
