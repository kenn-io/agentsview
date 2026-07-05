---
title: Semantic Search
description: Vector (semantic) search over session messages, plus hybrid search and cursor-based context retrieval
---

AgentsView can index user and assistant message content into a local vector
store and search it by meaning instead of exact terms, alongside the existing
substring/regex/FTS5 content search. This is an opt-in feature backed by an
OpenAI-compatible embeddings endpoint — a local [Ollama](https://ollama.com)
model or a hosted API.

!!! note "SQLite only"

    Semantic and hybrid search require the local SQLite archive.
    [PostgreSQL sync](/pg-sync/) and the [DuckDB mirror](/duckdb/) do not support a
    vector backend yet, so `--semantic`/`--hybrid` against `--pg` or a DuckDB-backed
    server return the same "not available" error described below.

## Enabling `[vector]`

Semantic search is disabled by default. Add a `[vector]` section to
`~/.agentsview/config.toml`:

```toml
[vector]
enabled = true                    # default false; everything below is opt-in
# db_path defaults to <data_dir>/vectors.db

[vector.embeddings]
endpoint = "http://localhost:11434/v1"  # OpenAI-compatible base URL; "/embeddings" is appended
model = "nomic-embed-text"
dimension = 768                   # every returned vector must have this length
api_key_env = "OPENAI_API_KEY"    # name of an env var holding the key; omit for anonymous access
batch_size = 32                   # inputs per HTTP call (default 32)
timeout = "30s"                   # per-HTTP-call timeout (default "30s")
max_retries = 3                   # attempts on 429/5xx/network errors; 4xx fails fast (default 3)
max_input_chars = 8192            # per-chunk rune cap (default 8192)

[vector.embed]
run_after_sync = true             # daemon embeds deltas after each sync, debounced ~30s (default true)
backstop_interval = "24h"         # periodic full reconciliation scan; negative disables (default "24h")
```

`endpoint`, `model`, and `dimension` are required once `enabled = true`;
`agentsview` fails fast with an actionable message if any is missing or a
duration field doesn't parse. Restart the daemon (or run a CLI command) after
editing the file.

The first scheduled build that `run_after_sync` triggers after enabling
`[vector]` embeds the entire existing archive, not just deltas, since the mirror
starts out empty and every message counts as pending. For a hosted embeddings
API that is a real cost event, so run `agentsview embeddings build` directly at
a time of your choosing if you want to control when that initial cost lands,
rather than letting the debounced after-sync scheduler trigger it on its own.

### Ollama quickstart

```bash
# Pull an embeddings model once.
ollama pull nomic-embed-text

# Ollama serves an OpenAI-compatible endpoint at /v1; no API key needed.
```

```toml
[vector]
enabled = true

[vector.embeddings]
endpoint = "http://localhost:11434/v1"
model = "nomic-embed-text"
dimension = 768
```

The encoder POSTs to `<endpoint>/embeddings` with an OpenAI-style
`{"model": ..., "input": [...]}` body and expects
`{"data": [{"index": 0, "embedding": [...]}]}` back — this matches Ollama's
`/v1/embeddings` route as well as OpenAI and most self-hosted OpenAI-compatible
servers. A response whose embedding length doesn't match `dimension` is
rejected.

## Building the index

```bash
agentsview embeddings build            # incremental: refresh + fill whatever's missing
agentsview embeddings build --yes      # skip confirmation prompts
agentsview embeddings build --full-rebuild --yes  # new generation, re-embeds every message
agentsview embeddings build --backstop # force a full mirror reconciliation scan, no re-embed
```

`embeddings build` mirrors the embeddable universe (every non-system
user/assistant message) into `vectors.db`, then fills whatever the active
generation is missing. `--full-rebuild` cuts a new **generation** — useful after
changing `model`, `dimension`, or `max_input_chars` — and prompts for
confirmation with a live count of embeddable messages unless `--yes` is passed.
Progress prints every ~2 seconds while a build runs, and a summary line reports
documents embedded, chunks, skipped, and stale counts on completion.

When a writable local daemon is running, `build`/`activate`/`retire` proxy to it
over HTTP so the daemon remains the sole writer of `vectors.db`; without a
daemon, the CLI takes a dedicated `vectors.write.lock` in the data directory and
runs the build in-process. If [`run_after_sync`](#enabling-vector) is enabled,
the daemon also embeds sync deltas automatically on a debounce, so a manual
`build` is mainly for the initial index or a `--full-rebuild`.

```bash
agentsview embeddings list
```

```text
ID  STATE     MODEL              DIM  EMBEDDED  MISSING  FINGERPRINT
1   active    nomic-embed-text   768  482       0        3f2a9c1e0b7d
```

Generations move through **building → active → retired**. A first build
activates automatically once it reaches full coverage.

```bash
agentsview embeddings activate <id> [--force]
agentsview embeddings retire <id> [--force]
```

`activate` on a generation with incomplete coverage, or `retire` on the
currently active generation, is refused unless `--force` is passed.

## Searching: `session search --semantic` / `--hybrid`

`--semantic` and `--hybrid` are new content-search modes alongside
`--regex`/`--fts`, mutually exclusive with each other and with the substring
default:

```bash
agentsview session search "database connection pooling" --semantic --limit 10
agentsview session search "flaky test" --hybrid --project myapp
```

- `--semantic` ranks by cosine similarity against the query's embedding.
- `--hybrid` fuses the semantic ranking with an FTS5 ranking of the same corpus
  using reciprocal rank fusion, so exact-term matches and meaning-based
  matches both surface.
- Both modes are restricted to the `messages` source — the same restriction
  `--fts` already has — since only user/assistant message content is embedded
  (never raw tool_input/tool_result rows or system messages). Passing `--in`
  with any other source is rejected.
- All the usual filters apply: `--project`, `--agent`, `--machine`, `--date*`,
  `--include-children`, etc. Metadata filters are applied *after* the vector
  leg over-fetches candidates (4x the requested limit) — see
  [Limitations](#limitations).
- Results are a single ranked page: `--cursor` is rejected for
  `--semantic`/`--hybrid` with a clear error, since RRF and cosine ranking
  don't have a stable offset to page from. Every match carries a `score` field
  (cosine similarity, or the RRF score for hybrid); substring/regex/fts
  matches leave `score` unset.
- An empty query pattern (`""`) returns no matches rather than an error, on
  every mode.

Human output shows the score inline:

```text
abc123  #42 score=0.87  myapp  message
    ...ideas for pooling database connections across worker threads...
```

### Inline context: `--context N`

```bash
agentsview session search "database connection pooling" --semantic --context 2
```

Every match gets `N` messages of context before and after it in the same
response — `context_before`/`context_after` arrays in JSON, indented
`role: content` lines around the match in human output. This works with every
search mode and costs one extra windowed query per hit. Values above 10 are
rejected with `context: maximum is 10` rather than silently clamped. Context
messages are secret-redacted by default, same as `--reveal` governs for the
match snippet itself.

## Cursor-follow: from a hit to its surrounding conversation

Every content-search match — regardless of mode — returns a
`(session_id, ordinal)` cursor. Use `session messages --around` to pull a window
of the conversation around that ordinal without re-running the search:

```bash
agentsview session messages <session-id> --around 42 --before 5 --after 5
agentsview session messages <session-id> --around 42 --role user,assistant
```

- `--around <ordinal>` centers a window on that message; `--before`/`--after`
  default to 5 and require `--around`. `--around` is mutually exclusive with
  `--from`/`--direction`.
- `--role` filters to a comma-separated role list (e.g. `user,assistant`). With
  a role filter, `--before`/`--after` count *filtered* messages, not raw
  ordinals — the anchor message is always included regardless of its role.
- The response reports the window's first/last ordinals, so you can keep paging
  forward with
  `agentsview session messages <id> --from <last+1> --role user,assistant` to
  walk the rest of the session's user/assistant history. There is no
  unpaginated "give me everything" mode.
- `--before`/`--after` are clamped so the total window never exceeds the
  server's message-page limit (1000 messages); an oversized request is
  silently capped rather than rejected.

The typical workflow: run `session search --semantic "<query>"`, take the
`session_id`/`ordinal` off a hit, then
`session messages <session-id> --around <ordinal>` to read what led up to it and
what followed.

## Error taxonomy

| Situation                                                                                            | Message                                                                                                                                                    |
| ---------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `[vector]` not enabled                                                                               | `vector search is not enabled: set [vector] enabled = true in config.toml` (from `agentsview embeddings ...`)                                              |
| No `VectorSearcher` wired (index never built, or PG/DuckDB backend)                                  | `semantic search not available: enable [vector] in config.toml and run 'agentsview embeddings build'`                                                      |
| Only a building generation exists                                                                    | same message, plus `: index is building: N% complete`                                                                                                      |
| Active generation's fingerprint no longer matches config (model/dimension/`max_input_chars` changed) | same message, plus `: index is stale (embedding config changed): run 'agentsview embeddings build --full-rebuild'`                                         |
| Embeddings endpoint unreachable or timed out                                                         | `[vector.embeddings] request: ...` (the underlying transport error)                                                                                        |
| Embeddings endpoint returned non-200                                                                 | `[vector.embeddings] status <code>: <body>`                                                                                                                |
| `--in` names a source other than `messages` with `--semantic`/`--hybrid`                             | CLI: `--semantic searches messages only; drop --in` (or `--hybrid ...`); HTTP/MCP: `search: semantic search only supports the messages source (got "...")` |
| `--cursor` with `--semantic`/`--hybrid`                                                              | `semantic search returns a single ranked page; cursor pagination is not supported`                                                                         |

Over HTTP (`GET /api/v1/search/content`) and MCP (`search_content`), the "not
available" family of errors maps to HTTP `501 Not Implemented` and the matching
MCP tool error, carrying the same remediation text.

## Limitations

- **Metadata filters post-filter the vector leg.** `--semantic`/`--hybrid`
  over-fetch candidates from the vector index (4x the requested limit, or a
  fixed minimum if that's larger), then drop hits whose session fails
  `--project`/`--agent`/`--date*`/etc., then truncate to the requested limit.
  At small corpus sizes or with a narrow filter, this can return fewer than
  `--limit` results even though more exist. This is a known v1 tradeoff, not a
  bug.
- **Legacy no-`source_uuid` rows re-embed on ordinal shifts.** Each embedded
  message is keyed by its stable per-message UUID when the parser recorded
  one, or by `(session_id, ordinal)` when it didn't. UUID-keyed rows survive
  ordinal renumbering (e.g. from a resync) as a cheap metadata update with no
  re-embed; ordinal-keyed rows are treated as a new document and re-embedded
  when their ordinal shifts. This only affects older parsed data predating
  per-message UUIDs and is an accepted cost rather than a bug.
- **SQLite only.** PostgreSQL sync and the DuckDB mirror have no vector backend;
  `--semantic`/`--hybrid` against `--pg` or a DuckDB-backed server return the
  "not available" error (HTTP 501) described above. `pgvector` support is a
  possible follow-up.
- **No frontend integration.** The web UI's command palette and in-session
  search remain FTS-only; semantic and hybrid search are CLI/HTTP/MCP-only in
  this release.
- **The index embeds message `content` verbatim.** Like `--fts`, it only draws
  from the `messages` source, so raw tool_input/tool_result rows are never
  candidates. System messages are handled more strictly, though: `--fts` still
  includes them unless the caller passes `--exclude-system`, while
  `--semantic`/`--hybrid` always exclude system messages from the index with
  no flag to opt back in. But anything a parser rendered *into* a
  user/assistant message's content is embedded with it: thinking text
  flattened inline as `[Thinking]...[/Thinking]` markers, and tool-call
  summaries some parsers render into assistant content, are all ordinary message
  text to the index.
