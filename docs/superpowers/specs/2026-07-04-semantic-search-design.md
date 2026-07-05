# Semantic Search over User/Assistant Messages

Date: 2026-07-04 Status: approved design, pending implementation plan

## Goal

Add semantic (vector) search over user and assistant message content, alongside
the existing FTS5/regex/substring content search, with the same cursor contract:
every match returns `(session_id, ordinal)` and the CLI/API can efficiently
retrieve a window of surrounding messages (or the full user/assistant history of
a session) as human text or JSON.

## Non-goals (v1)

- No frontend/SPA changes. The web UI keeps FTS-only search.
- No semantic mode on the session-level search surface (`/api/v1/search`, MCP
  `search_sessions`). Semantic lands on the content-search surface only.
- No PostgreSQL/DuckDB vector backends. Both return a capability error (pgvector
  is the natural follow-up; msgvault proves the shape).
- No embedding of thinking text, tool inputs/outputs, or system messages.
- No bundled embedding model.

## Decisions (summary)

| Decision   | Choice                                                      |
| ---------- | ----------------------------------------------------------- |
| Embedder   | OpenAI-compatible `/v1/embeddings` endpoint (Ollama/hosted) |
| Reuse      | `go.kenn.io/kit v0.2.0` `vector` + `vector/sqlitevec`       |
| Lifecycle  | msgvault precedent: explicit build + daemon incremental     |
| CLI search | `session search --semantic` / `--hybrid` mode flags         |
| Context    | `session messages --around` + `session search --context N`  |
| Storage    | Separate `vectors.db`, resync-stable keys                   |
| v1 surface | CLI + HTTP + MCP `search_content`; hybrid RRF included      |

## Architecture

```text
session search --semantic ──► service.SessionService ──► db.Store (SQLite)
        (or HTTP daemon)                                     │
                                                             ▼
                                              vectors.db (kit vector/sqlitevec)
                                                             ▲
sync complete ──► debounced embed scheduler ──► mirror refresh + kit Fill
                                                             ▲
                                        OpenAI-compatible EncodeFunc (HTTP)
```

agentsview supplies the embeddings HTTP client (`EncodeFunc`), the `vectors.db`
mirror/refresh layer, and the CLI/HTTP/MCP surface. kit supplies chunking,
batched encoding, generation lifecycle/fingerprints, `Fill` orchestration,
cosine KNN (sqlite-vec `vec0`), and rank merging.

### Embedded universe

`messages.content` where `role IN ('user','assistant')`, `is_system = 0`, and
the content is not system-prefixed (reuse the existing system-prefix filter
shared with FTS search). Nothing else is embedded.

## Configuration

```toml
[vector]
enabled = true                    # default false; everything is opt-in
# db_path defaults to <data_dir>/vectors.db

[vector.embeddings]
endpoint = "http://localhost:11434/v1"  # OpenAI-compatible; /embeddings appended
model = "nomic-embed-text"
dimension = 768                   # responses verified against this
api_key_env = "OPENAI_API_KEY"    # NAME of env var holding the key; empty = anonymous
batch_size = 32                   # inputs per HTTP call
timeout = "30s"
max_retries = 3                   # 429/5xx/network retried; 4xx fails fast
max_input_chars = 8192            # per-chunk rune cap

[vector.embed]
run_after_sync = true             # daemon embeds deltas after sync (debounced)
backstop_interval = "24h"         # periodic full scan; negative disables
```

No cron scheduling in v1; the daemon's periodic sync plus `run_after_sync`
covers freshness. `endpoint`, `model`, and `dimension` are required when
`enabled = true`; validation fails fast with actionable messages.

## Storage: vectors.db

A separate SQLite database beside the archive. It is regenerable state — the
main DB remains the persistent archive and is never modified by this feature.

Contents:

- `vector_messages` mirror table — the "documents" table kit's sqlitevec store
  co-locates with. kit's `sqlitevec.Schema` names a single `IDColumn`, so the
  identity is a synthetic single-column key. One row per embeddable message:
    - `doc_key TEXT PRIMARY KEY` — `u:<session_id>:<source_uuid>` when
      `source_uuid` is non-empty, else `o:<session_id>:<ordinal>` (same
      precedence as pin re-attachment in `internal/db/messages.go`). The schema
      permits several messages in one session to share a `source_uuid`, so
      uuid-backed keys carry an occurrence disambiguator: the first occurrence
      (in ordinal order) keeps the bare form, later ones append `#<occurrence>`
      (`u:<session_id>:<source_uuid>#2`, ...). Occurrence assignment follows
      scan order (session_id, ordinal), so it is deterministic and stable across
      resyncs; if an earlier duplicate disappears, later ones shift and re-embed —
      accepted, rare. Maps to kit `Schema.IDColumn`.
    - `session_id`, `source_uuid`, `ordinal` — payload/index columns; `ordinal` is
      the message's current ordinal, refreshed on every scan so hits always map
      to live cursors
    - `content` — the embeddable text, copied in at refresh time (kit
      `Schema.ContentColumn`; `PendingForGeneration` reads it from the documents
      table). Duplicates user/assistant text into regenerable state, keeping
      vectors.db self-contained with no ATTACH to the archive.
    - `content_hash` — revision for staleness (kit `Schema.RevisionColumn`,
      optimistic stamps)
    - `embed_gen` — nullable generation stamp (kit `Schema.EmbedGenColumn`)
- kit-managed tables, named from `Schema.VectorsPrefix` (e.g. prefix
  `message_vectors` gives `message_vectors_generations`,
  `message_vectors_chunks`, `message_vectors_stamps`, and per-generation
  `vec0` tables).

### Mirror refresh (scan)

Before each fill, scan the main DB's embeddable universe and reconcile the
mirror: insert new identities, update `ordinal`/`content_hash` on existing ones,
delete identities no longer present — via
`sqlitevec.Store.DeleteVectors(ctx, doc)` per removed identity, then the mirror
row; deleting only the row would leave orphaned vectors occupying KNN slots (kit
filters them from hits but does not reclaim the slots). Content change bumps the
revision, so kit re-embeds it; an ordinal shift on a uuid-keyed row is a cheap
mirror update with no re-embed. Legacy rows without `source_uuid` re-embed when
their ordinal shifts — accepted cost, documented.

### Resync survival

A parser-change resync rebuilds and swaps the main DB. `vectors.db` is not
touched by the swap; the next mirror refresh re-derives identities, so unchanged
messages keep their vectors and only genuinely changed content re-embeds.

### Generations

Model/dimension/chunking changes are a new kit generation (fingerprinted).
`--full-rebuild` builds the new generation alongside the frozen active one and
activates atomically on clean completion. States: building/active/retired.

## Indexing lifecycle

### CLI: `agentsview embeddings ...`

- `embeddings build` — incremental by default (mirror refresh + fill of whatever
  the active generation is missing). `--full-rebuild` cuts a new generation.
  `--backstop` forces a full reconciliation scan. `--yes` skips confirmation.
  Progress line (throttled ~2s): scanned/total, rate, ETA; final summary with
  succeeded/failed/skipped counts.
- `embeddings list` — generations table: ID, STATE, MODEL, DIM, coverage counts,
  FINGERPRINT, timestamps.
- `embeddings activate <id>` / `embeddings retire <id>` — lifecycle escape
  hatches with `--force` variants (activate with missing coverage; retire the
  active generation).

### Writer ownership

vectors.db writes follow the archive's single-writer model:

- Writable daemon running: `embeddings build/activate/retire` route to the
  daemon over HTTP — POST triggers the job, the CLI polls a status endpoint
  for progress. The daemon owns all vectors.db writes.
- No daemon: the CLI takes a dedicated `vectors.write.lock` flock in the data
  dir (same mechanism as `db.write.lock`, separate file so fills never contend
  with archive writes).

Generation activation always happens under the single writer. Search opens
vectors.db read-only from any process.

### Daemon incremental fill

`serve` wires an embed scheduler (only when `[vector].enabled`):

- Listens for sync-completion via the existing non-blocking `Emitter`. Enqueue =
  set a dirty flag and reset a ~30s debounce timer. Sync completion never
  waits on embedding.
- Fill runs in a background goroutine, low priority, overlap-guarded with
  `TryLock` (drop, not queue). Signals arriving mid-fill leave the dirty flag
  set for one follow-up pass.
- A periodic backstop (default 24h) runs a full reconciliation scan to catch
  stragglers (crashes, transient encode failures).

## Search surface

Semantic and hybrid are new **content search** modes beside
`substring|regex|fts` — CLI `session search`, HTTP `GET /api/v1/search/content`,
MCP `search_content`. The session-level search surface is unchanged.

- CLI: `--semantic` and `--hybrid` join `--regex`/`--fts` as mutually exclusive
  mode flags. All existing filters apply (`--project`, `--agent`, `--machine`,
  `--date*`, session-type flags, `--limit`).
- Like `fts`, the new modes are restricted to the `messages` source, and further
  to the embedded universe (user/assistant, non-system). Hybrid's FTS leg
  applies the same restriction so both legs rank the same corpus.
- Vector leg: embed the query per live generation, cosine KNN via kit, roll up
  chunks to one hit per message (best chunk wins). Metadata filters are
  applied after over-fetching (KNN k x factor), then trimmed to the requested
  limit — a documented v1 limitation, fine at current corpus sizes.
- Hybrid: reciprocal rank fusion of the FTS and vector legs (kit
  `MergeReciprocalRank`, k = 60).
- Result shape: existing `ContentMatch` plus `score` (cosine similarity for
  semantic, RRF score for hybrid; null for existing modes). Snippets for
  semantic hits are excerpts centered on the best-matching chunk (chunk
  offsets are deterministic from kit's split). Secret redaction and terminal
  sanitization behave exactly as today.
- Pagination: semantic/hybrid results are a single ranked page (limit-capped,
  offset cursor rejected with a clear message, msgvault precedent). FTS,
  regex, and substring pagination are untouched.

### Capability gating

- `db.Store` gains `HasSemantic() bool` and the semantic/hybrid mode is threaded
  through the existing `SearchContent` request struct.
- `service.SessionService` mirrors it; new `service.ErrSemanticUnavailable` maps
  to HTTP 501 in the huma layer and back again in the HTTP client backend.
- PostgreSQL and DuckDB stores get compile-time-forced stubs returning
  `false`/unavailable — explicitly not the `HasFTS()` emulation pattern, since
  there is nothing to emulate with. This is a documented backend parity
  divergence; pgvector is the follow-up path.
- MCP tools surface `ErrSemanticUnavailable` (and the taxonomy below) as tool
  errors carrying the same remediation text as the CLI/HTTP messages.

### Errors

Cause-specific messages, msgvault-style taxonomy:

- not enabled / not configured — "set [vector] in config.toml and run
  'agentsview embeddings build'"
- index building — includes progress percentage
- index stale — fingerprint mismatch, "run 'agentsview embeddings build
  --full-rebuild'"
- embedding endpoint unreachable / timed out
- empty free-text query rejected for semantic/hybrid

## Context retrieval (cursor follow)

The cursor is `(session_id, ordinal)` — what every content match already
returns.

### `session messages --around`

`MessageFilter` gains `Around *int`, `Before`/`After int` (defaults 5/5),
`Roles []string`, implemented in both the direct and HTTP service backends and
exposed on `GET /api/v1/sessions/{id}/messages`.

- `--around <ordinal>` is mutually exclusive with `--from`/`--direction`
  (validation error). The CLI sends `direction` only when the flag was
  explicitly set, so the flag default (`asc`) never trips the validation.
  `--before`/`--after` require `--around`.
- `--role user,assistant` composes with all retrieval modes; the full
  user/assistant history of a session is retrieved by paging
  `session messages <id> --role user,assistant` with `from = last_ordinal + 1`
  until a short page — retrieval stays paginated; there is no unpaginated
  full-history mode. Wire spelling: CLI flag `--role` takes comma-separated
  values (matching `--in` on `session search`); the HTTP query param is
  `roles`, also comma-separated (`roles=user,assistant`, matching the `in`
  param convention); MCP `get_messages` keeps its existing plural `roles`
  array parameter.
- With a role filter, before/after count **filtered** messages: two indexed
  queries on `(session_id, ordinal)` with `role IN (...)`, DESC/ASC LIMIT N.
  The anchor message is always included.
- Responses report the window's first/last ordinals so callers continue paging
  with `from = last + 1`.

### `session search --context N`

grep-style inline windows: each match carries N messages before/after in the
same response (JSON: `context_before`/`context_after` arrays; human output:
indented role-tagged lines around the highlighted match). Works with every
search mode. Costs one windowed query per hit, so it pairs with modest limits;
`--context` caps at 10.

### MCP

- `search_content` gains `mode` (`semantic`/`hybrid`) and `context`.
- `get_messages` gains `around`/`before`/`after`, implemented server-side via
  the same service call (replacing post-fetch role trimming on this path)
  while keeping its `filtered` accounting and `next_from` semantics.
  Interaction with existing fields: `around` is mutually exclusive with both
  `from` and `direction` (tool error if either is combined with `around`);
  `before`/`after` require `around` and replace `limit` on that path;
  `next_from` is the window's last ordinal + 1.
- MCP preserves its existing filtering contract on the around path: an empty
  `roles` parameter means the MCP default (user and assistant only), never
  "all roles", and system/system-prefixed messages are always excluded — the
  anchor included by the window is suppressed from the output (still counted
  in `filtered`) when it violates the role/system contract.

## Dependencies

- `go.kenn.io/kit` bumped from v0.1.7 to **v0.2.0** (first tag shipping the
  `vector` and `vector/sqlitevec` packages). The kit APIs this spec relies on:

    - `vector.Split(content string, o SplitOptions) []Chunk` — rune windowing with
      overlap
    - `type EncodeFunc func(ctx context.Context, texts []string) ([][]float32, error)`
      and
      `vector.EncodeBatched(ctx context.Context, enc EncodeFunc, chunks []Chunk, o BatchOptions) ([]Vector, error)`
    - `vector.Generation{Model, Dimensions, Params}` with `Fingerprint() string`
    - `vector.Store[K, G]` (`PendingForGeneration`, `SaveVectors`/`ErrStale`,
      `LiveGenerations`, `QueryGeneration`)
    - `vector.Fill[K, G](ctx context.Context, store Store[K, G], gen G, enc EncodeFunc, o FillOptions[K]) (FillStats, error)`
    - `vector.Search[K, G](ctx context.Context, store Store[K, G], queryText string, encFor func(gen G) EncodeFunc, o SearchOptions) ([]Hit[K], error)`
    - `vector.RollupByDocument[K](hits []Hit[K]) []Hit[K]` and
      `vector.Merge[K](perGeneration [][]Hit[K], o MergeOptions) []Hit[K]` with
      `MergeReciprocalRank`
    - `vector/sqlitevec.Store[K, G]` configured via `sqlitevec.Schema`
      (`DocsTable`, `IDColumn`, `ContentColumn`, `EmbedGenColumn`,
      `RevisionColumn`, `VectorsPrefix`) — vec0 tables, cosine KNN, revision
      stamps, generation states

    The implementation plan's first task is the pin bump plus an API-reality check
    against the shipped tag. Gaps kit does not cover (mirror refresh,
    watermark/backstop semantics) live in agentsview's mirror layer and are
    flagged as candidate kit feedback.

- sqlite-vec arrives transitively via kit's sqlitevec backend (cgo bindings;
  agentsview already requires `CGO_ENABLED=1`).

## Testing

- Unit: fake deterministic `EncodeFunc` (no network) driving build → search →
  context round-trips; mirror-refresh staleness (content change, ordinal shift
  with/without `source_uuid`, deleted message); resync-swap survival;
  generation rotation and fingerprint mismatch.
- Embeddings client: httptest-backed retry behavior (429/5xx vs 4xx), dimension
  verification, batch splitting, api_key_env resolution.
- Service parity: table-driven tests over both direct and HTTP backends for the
  new search mode and `--around`/`--role` semantics; 501 mapping.
- Store contract: PG/DuckDB return `ErrSemanticUnavailable` (compile-time forced
  by the interface).
- CLI: golden human output for search-with-context and around windows.
- No e2e/Playwright changes (no UI in v1).
