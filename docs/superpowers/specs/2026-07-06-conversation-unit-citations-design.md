# Conversation-Unit Citations for Content Search — Design

Decision record for extending the content-search wire contract so every match,
in every mode, carries a conversation-unit citation. Follows the run-grouped
embeddings work (docs/semantic-search-internals.md); supersedes the branch-local
`ordinal_start`/`ordinal_end` fields, which never shipped to main.

## Problem

Semantic and hybrid search return one row per embedding unit with a span
(`ordinal_start`..`ordinal_end`); substring, regex, and FTS return one row per
matching message with no unit information. Agent consumers therefore get N thin
rows for one assistant monologue in lexical modes, with no signal that the rows
share a conversational unit and no handle to recover the surrounding run.

## Contract

Row cardinality stays mode-specific; citation metadata becomes uniform:

- **Lexical (substring/regex/fts): one row per occurrence.** Grep semantics are
  the contract; no default collapse. A future opt-in `group=unit` is out of
  scope.
- **Semantic: one row per embedded unit.** `ordinal` is the chunk-center anchor,
  as today.
- **Hybrid: one row per unit** (FTS-anchor override unchanged); rows whose
  message has no embedded unit stay message-granularity.
- **Every row carries `ordinal_range`** — the conversation unit enclosing the
  anchor — plus the subordinate/lineage fields, in all modes.

### Wire format

`db.ContentMatch` changes (the HTTP response serializes this struct directly;
huma schema and the generated frontend client follow):

```go
// Replaces OrdinalStart/OrdinalEnd (branch-only fields, safe to remove).
// Always present, never omitempty: [start, end] of the conversation unit
// containing the anchor; [ordinal, ordinal] when the anchor is its own
// unit. A range starting at ordinal 0 serializes as [0, N].
OrdinalRange [2]int `json:"ordinal_range"`
```

- `ordinal` is unchanged: the anchor / exact matched message in every mode.
- `subordinate`, `relationship`, `parent_session_id`, `is_sidechain` are now
  populated in **all** modes (they were semantic/hybrid-only). Their
  `omitempty` stays: false/empty means top-level/no-lineage, unambiguously.
- `score` remains semantic/hybrid-only. Snippets are unchanged in every mode
  (lexical snippets still come from the matched message).
- The array form deliberately avoids the omitempty-integer trap: a unit starting
  at ordinal 0 still serializes its start.

### What the range means (per-mode provenance)

`ordinal_range` always means "conversation unit", not always "embedding unit":

- **semantic / hybrid unit rows**: the embedded vector unit's span, from the
  vectors.db mirror (embedding identity, including build scope).
- **lexical rows and hybrid unit-less rows**: a structurally derived unit
  computed from the messages/sessions tables only. Deterministic, never
  depends on whether a vector index exists or is fresh — lexical output must
  not flicker with index state.

Derived and embedded spans coincide except where embedding scope diverges from
structure: sessions excluded from the build (`include_automated = false`), and
messages newer than the last mirror refresh. Docs state this explicitly; no
provenance discriminator field is added (mode implies it).

### MCP

`internal/mcp/tools.go` `contentMatch` (its own mirror struct) gains the same
citation fields: `ordinal_range`, `subordinate`, `relationship`,
`parent_session_id`, `is_sidechain`. Agents are the primary consumers of the
top-level-vs-subordinate evidence signal.

### Scope filter

`scope=top|all|subordinate` stays semantic/hybrid-only. Filtering grep results
by unit scope is a separate semantic change, deferred.

## Derived-unit definition

Structural rules, mirroring `ScanEmbeddableUnits`'s reducer
(internal/db/messages.go) so derived spans equal embedding-unit spans on
in-scope data. Terminology: a row is an **embeddable user row** when
`role = 'user' AND is_system = 0` and the content is not system-prefixed (the
dialect `SystemPrefixSQL` predicate); an **embeddable assistant row** is the
same with `role = 'assistant'`.

For an anchor message row `m` at ordinal `o` (for `tool_input` / `tool_result`
locations the anchor is the tool call's message row, whose ordinal the match
already carries):

1. `m` is an embeddable user row → `[o, o]` (user messages are their own units).
1. `m` is an embeddable assistant row → the maximal stretch of embeddable
   assistant rows containing `o`, bounded exclusively by: the nearest
   embeddable user row on either side, the session edges, and the nearest
   embeddable assistant row whose `is_sidechain` differs from `m`'s (runs
   never mix sidechain values). The range endpoints are the first and last
   member ordinals — the span may cover non-member ordinals in between (system
   rows inside a run), exactly like `runUnit`.
1. Anything else (system rows, system-prefixed rows, other roles) → `[o, o]`.
   The row belongs to no conversation unit; the citation is the message
   itself.

Automation gating is deliberately ignored: derivation is structural, so matches
inside automated sessions still get real ranges. The invariant is: **derivation
at any member ordinal of any unit produced by
`ScanEmbeddableUnits(include_automated = true)` returns exactly that unit's
`[Ordinal, OrdinalEnd]`.**

`subordinate` for derived rows uses the same formula as the reducer:
session-subordinate (`relationship_type IN ('subagent','fork')`, or
parent-linked with `relationship_type <> 'continuation'`) OR the anchor row's
`is_sidechain`. All run members share one sidechain value, so the anchor's flag
equals the run's.

## Derivation mechanics and performance

Performance is a hard constraint: content search is a hot path and the citation
must not meaningfully slow it.

- **Post-scan enrichment, not query rewrites.** The lexical search SQL keeps its
  shape; the only in-query change is selecting a few extra columns that are
  already one join away (`s.relationship_type`, `s.parent_session_id`,
  `m.is_sidechain`, plus an `embeddable`/role classification boolean computed
  from columns already read). Derivation runs as a second pass over the
  returned page only.
- **O(page), never O(corpus).** Pages are small: db default 50, max 500; MCP
  caps at 30. Per anchor the derivation needs at most four boundary lookups
  (nearest embeddable user row before/after; nearest sidechain-flip assistant
  row within those bounds), each an index-served `(session_id, ordinal)` walk
  (`ORDER BY ordinal DESC/ASC LIMIT 1` with the boundary predicate) — the same
  pattern `getMessagesAroundAnchor` already uses on both backends. Lookups for
  a page are batched into one statement per backend where the dialect allows
  (the `enrichSemanticHits` VALUES-CTE precedent), else prepared point
  queries.
- **Page-level memoization makes the monologue case the cheapest case.** Hits
  are grouped by session; once a range is derived, any other anchor in the
  same session that falls inside it (and is an embeddable assistant row)
  reuses it. Twenty hits in one run cost one derivation.
- **Worst case is bounded by run length.** A boundary walk reads rows until the
  predicate matches; the pathological anchor deep inside a
  multi-thousand-message run costs a scan of that run — the same order as
  fetching the run for display. No walk cap initially; add one only if the
  benchmark demands it.
- **Benchmark gate.** Add a content-search benchmark (seeded corpus including
  long runs and many-hits-per-run pages) to `internal/db`, which is already in
  `BENCH_GATE_PACKAGES`, so CI's bench.yml gates the PR against its merge
  base. Acceptance: no significant regression beyond ~10% on a 50-hit
  substring/FTS page; measure before tuning further.
- **Contingency (documented, not built):** if profiling on real corpora shows
  meaningful cost, add an explicit `citations=off` opt-out. Never auto-disable
  based on index or corpus state.

## Population points

- **SQLite lexical** (`searchContentSubstring`, `searchContentRegex`,
  `searchContentFTS`): scan the extra columns, then the shared derivation
  pass. Row cardinality, ordering, snippets, `LIMIT/OFFSET` pagination, and
  `NextCursor` behavior are unchanged.
- **PostgreSQL** (`internal/postgres/search_content.go`): same columns, same
  derivation, using `PostgresSystemPrefixSQL`. `fts` mode's ILIKE fallback
  behaves like substring.
- **DuckDB** (`internal/duckdb/store.go` `SearchContent`): same treatment with
  the DuckDB dialect predicate. All three backends must produce identical
  observable output (backend-parity rule); parity tests are co-located with
  the feature.
- **Semantic**: `OrdinalRange` is populated from the mirror unit span (a rename
  of today's two fields).
- **Hybrid**: mirror-unit rows as today; unit-less FTS rows (resolver returns no
  `DocKey`) get a derived range instead of the current `[o, o]` self-range.
  Fusion keys, the subordinate penalty, and the FTS-anchor override are
  untouched.
- **CLI** (`session_search.go`): `formatMatchOrdinal` reads the range —
  `#start-end @anchor` when end > start, `#ordinal` otherwise; the `sub`
  marker can now appear on lexical rows too.
- **Shared implementation**: one Go derivation helper in `internal/db` drives
  all three backends through their boundary-lookup queries;
  `internal/postgres` and `internal/duckdb` already depend on `internal/db`
  for dialect predicates.

## Testing

- **Equivalence property test**: seed a corpus with runs, sidechain flips,
  system rows inside runs, session-edge runs, and automated sessions; assert
  the invariant above for every unit and member, plus `[o, o]` for every
  embeddable user row and non-embeddable anchor.
- **Wire pins**: `ordinal_range` present on every match in every mode, including
  `[0, N]`; the two existing tests asserting lexical omission of the old keys
  are updated to assert the new always-present contract.
- **Cross-backend parity tests** co-located with the feature (SQLite / PG /
  DuckDB produce identical matches for the same corpus).
- **Hybrid unit-less injection**: an FTS hit outside the mirror gets the derived
  range, not a self-range.
- **Benchmark**: the gated content-search benchmark above.

## Out of scope

- `group=unit` collapse for lexical modes (future opt-in).
- `scope` filtering for lexical modes.
- A provenance discriminator on the wire.
- Frontend UI rendering of ranges (the generated client picks up the schema;
  consuming it is separate work).
