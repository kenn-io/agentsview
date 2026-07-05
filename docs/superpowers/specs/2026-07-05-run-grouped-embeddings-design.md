# Run-Grouped Embeddings Design

Status: approved design, not yet implemented. Supersedes the per-message
assistant embedding scheme currently on this branch (nothing has shipped; the
index is rebuildable).

## Problem

The current vector index embeds every embeddable message individually. Corpus
analysis of the live archive (non-automated sessions, 2026-07-05) shows why that
dilutes retrieval quality:

- ~49.7k user messages vs ~1.095M assistant messages.
- 909k of those assistant messages carry tool use with p50 120 chars — short,
  procedural narration ("Let me check the file") that is context-poor as a
  standalone semantic unit.
- Grouped between human turns, assistant messages form ~44k runs (avg 24.7
  messages/run, p50 10, p99 184, max 14,659). Per-message embedding spends
  most vectors on fragments of long work stretches.
- 31% of assistant volume is subordinate material: sidechain messages (21.8%)
  and subagent sessions (5.0%) are embedded today with no marker, blended into
  ranking alongside top-level human-driven work.

Goal: embed coherent semantic units so "reconstruct this design decision"
queries match narrative, not fragments — while keeping subordinate evidence
searchable but ranked below top-level intent.

## Unit model

**User documents are unchanged**: one document per embeddable user row, with
today's `u:`/`o:` doc keys, occurrence suffixes, and escaping.

**Assistant content becomes run documents.** A run is a maximal sequence of
embeddable assistant messages within one session, bounded by:

- embeddable user rows (`role = 'user' AND is_system = 0 AND` not
  system-prefixed per `SystemPrefixSQL`) — the same predicate that defines
  user documents, so boundaries need no new classification;
- session start/end;
- transitions of `is_sidechain`: contiguous sidechain blocks form their own runs
  and never mix with non-sidechain messages.

The boundary term is deliberately "embeddable user row", not "human turn": rows
excluded by the system-prefix filter (interruptions, task notifications, command
wrappers, continuation banners) do not split runs.

A run of one message degenerates to today's behavior: p50 exchanges look the
same as the current scheme.

### Run identity

`doc_key = r:<session_id>:<identity of the run's first message>` where the
first-message identity reuses the existing machinery: percent-escaped
`source_uuid` with `#<n>` occurrence suffix, ordinal fallback (`ro:` prefix) for
legacy data without UUIDs. Properties:

- A run that grows a trailing message keeps its doc_key; `content_hash` changes
  and the run re-embeds (the active tail re-embeds per build, which is the
  intended cost).
- A new user turn landing mid-run after a resync splits the run: the second half
  becomes a new document, the old one shrinks; kit's reconciliation and
  two-phase eviction handle both, unchanged.

### Run content

Message texts joined in ordinal order with a single blank line (`\n\n`). No
structural markers, role labels, or metadata are injected into the embedded text
— lineage, ordinals, and sidechain/subagent status live in mirror columns and
hit metadata only.

## Mirror schema v2

`vector_messages` becomes a unit mirror (name kept for continuity):

```sql
CREATE TABLE IF NOT EXISTS vector_messages (
    doc_key       TEXT PRIMARY KEY,
    session_id    TEXT NOT NULL,
    source_uuid   TEXT NOT NULL DEFAULT '',  -- first message of the unit
    ordinal       INTEGER NOT NULL,          -- ordinal_start (index compat)
    ordinal_end   INTEGER NOT NULL,          -- == ordinal for user docs
    subordinate   INTEGER NOT NULL DEFAULT 0,
    offsets       TEXT NOT NULL DEFAULT '',  -- JSON, see below
    content       TEXT NOT NULL,
    content_hash  TEXT NOT NULL,
    embed_gen     TEXT
);
```

`offsets` is a JSON array, one entry per member message, in ordinal order:
`[{"o": ordinal, "r": rune_start, "b": byte_start}, ...]` (ends implied by the
next entry / content length). Rune offsets map kit chunk windows to messages;
byte offsets slice snippets without re-decoding. Empty for user docs.

### Subordinate classification

A unit is subordinate when any of:

- its messages have `is_sidechain = 1` (sidechain runs);
- the session's `relationship_type` is `subagent` or `fork`;
- the session is parent-linked (`parent_session_id <> ''`) with any relationship
  type other than `continuation` (defensive: covers empty or unknown types).

Continuations are deliberately top-level, deviating from
`canonicalChildRelationships` (which includes `continuation` to dedupe session
identity in lists): embedding cares about content provenance, a continuation is
the same human-driven conversation, its replayed banner is already excluded as
system-prefixed, and its new content is unique. Forks follow the existing child
convention because their prefix replays parent content (dedup by downranking;
fork volume is 0.56% of assistant messages, so index bloat is negligible).

## Versioning and migration

Two independent mechanisms, both required:

1. **Mirror schema version.** New `vector_meta` key `mirror_schema_version`
   (this scheme writes `2`). The write-path `Open` compares it against the
   binary's version: on mismatch — including the key being absent while any
   mirror state exists — it drops and recreates all mirror-owned tables and
   clears the refresh watermark and scope keys, so the next build takes the
   existing first-ever full path. `vectors.db` is disposable by design;
   `sessions.db` is never touched. The read path treats a version mismatch as
   `ErrStale`-equivalent (semantic search reports the index must be rebuilt)
   rather than misreading v1 rows.
1. **Generation fingerprint.** `vectorGeneration` Params gain
   `doc_unit_scheme = "run_v1"` and `chunk_overlap_chars = <n>` alongside
   `max_input_chars`. The scheme change therefore cuts a new generation
   through the existing building → active → retired lifecycle even if the
   mirror were somehow current, and future overlap tuning is a fingerprint
   change, not a silent drift.

## Chunking

Runs are chunked by the existing `kitvec.Split`. Overlap changes from the
implicit `maxInputChars / 30` to an explicit `maxInputChars * 15 / 100` (1228
chars at the default `max_input_chars = 8192`; 375 at a 2500 cap), recorded in
the fingerprint as `chunk_overlap_chars`. No kit changes required.

**Anchor policy:** a hit's anchor is the message whose rune span contains the
matched chunk's center rune, `chunk_start + len(chunk_runes)/2` — the chunk's
actual rune length, not `max_runes`, so short final chunks anchor at their true
center; if the center falls on a boundary, the earlier message wins. kit's
`Hit.ChunkIndex` plus `SplitOptions` reproduce the chunk window
deterministically from the mirrored content.

## Search, ranking, citation

- **Hit shape.** A semantic hit resolves to session + `ordinal_start`..
  `ordinal_end` + anchor ordinal + a snippet sliced from the matched chunk
  (byte offsets). Hits carry lineage: `subordinate`, `relationship_type`,
  `parent_session_id`, and whether the unit is a sidechain run. User-doc hits
  keep today's single-ordinal shape (range collapses to one ordinal).
- **`--around` and the context cursor flow anchor on the anchor ordinal** — no
  changes to the message-window APIs on any backend.
- **Scope filter.** `--scope top|all|subordinate` on semantic/hybrid search
  (API: `scope` param). Default `all`: subordinate content stays discoverable
  but penalized.
- **Subordinate penalty.** Applied at the merge step as a rank-based adjustment
  (subordinate hits' RRF contributions use `rank + P`, with `P` a small
  constant, initial value 5), not a hard tier and not a score multiplier — RRF
  ranks are the only scale that is comparable across legs.
- **Hybrid fusion.** The FTS leg stays message-granularity (exact strings,
  commands, filenames). Before RRF, each FTS message hit maps to its
  containing unit (its user doc, or the run whose ordinal range contains it);
  fusion happens at unit granularity. When the FTS side contributes, the exact
  matched message becomes the hit's anchor regardless of chunk center.
- **FTS-only search is untouched** at message granularity.

## Surface changes

- Search API/CLI hit fields: `ordinal_range`, `anchor`, `subordinate`,
  `relationship` (client regenerated).
- `embeddings status`/build summaries count units, not messages; docs updated
  (semantic-search.md, semantic-search-internals.md — corpus, doc_key scheme,
  migration, anchor policy sections).
- finding-history skill template: cite session + ordinal range, use
  `--scope top` when reconstructing decisions, treat subordinate hits as
  supporting evidence requiring parent corroboration.

## Cost estimate

~47k user docs + ~42k run docs ≈ 90k documents, ~250k chunks (15% overlap
included) vs ~1.14M single-message docs today: roughly 5x fewer encode units for
the initial build, with long work stretches contributing a handful of narrative
chunks instead of hundreds of near-duplicate procedural vectors.

## Testing

- Reducer: boundary cases (system-prefixed user rows do not split; sidechain
  transitions do; session-edge runs; single-message runs; empty-content
  messages), offset correctness (rune and byte, multi-byte content).
- Identity: stable doc_key across appends; split/merge on mid-run user-turn
  insertion; resync ordinal renumbering with UUID keys.
- Migration: v1 mirror with data → version bump drops/recreates and next build
  is full; read path on mismatched version reports rebuild-required;
  fingerprint change cuts a new generation.
- Anchoring: chunk-center mapping across message boundaries; FTS-anchored hybrid
  hits.
- Ranking: subordinate penalty ordering; `--scope` filters; continuation
  classified top-level, fork/subagent/sidechain subordinate; parent-linked
  empty-type subordinate.
- Backend parity: message-window and FTS behavior unchanged on SQLite/PG/DuckDB
  (vector search itself remains SQLite-local by design).

## Out of scope

- Contextual prefixing (prepending the triggering user turn to run text) — a
  content-synthesis change adoptable later; changes only content_hash.
- Cross-session dedup of fork-replayed content beyond downranking.
- Mirror Refresh write-amplification work (tracked separately).
