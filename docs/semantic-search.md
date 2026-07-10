---
title: Semantic Search
description: Vector (semantic) search over session messages, plus hybrid search and cursor-based context retrieval
---

AgentsView can index user and assistant message content into a local vector
store and search it by meaning instead of exact terms, alongside the existing
substring/regex/FTS5 content search. This is an opt-in feature backed by an
OpenAI-compatible embeddings endpoint — a local [Ollama](https://ollama.com)
model or a hosted API.

For the architecture behind this page — storage layout, generations,
concurrency, and the search path — see
[Semantic Search Internals](/semantic-search-internals/).

!!! note "Backends"

    Semantic and hybrid search run on the local SQLite archive and on
    [PostgreSQL](#postgresql) via pgvector. The [DuckDB mirror](/duckdb/) has no
    vector backend, so `--semantic`/`--hybrid` against a DuckDB-backed server return
    the "not available" error described below.

## Enabling `[vector]`

Semantic search is disabled by default. Add a `[vector]` section to
`~/.agentsview/config.toml`:

```toml
[vector]
enabled = true                    # default false; everything below is opt-in
# db_path defaults to <data_dir>/vectors.db
include_automated = false         # default; automated sessions (e.g. roborev) are not embedded -- set true to include

[vector.embeddings]
model = "nomic-embed-text"
dimension = 768                   # every returned vector must have this length
max_input_chars = 8192            # per-chunk rune cap (default 8192)
# input_suffix = "<|endoftext|>"  # appended to every embedded text; default empty (see below)
default_server = "local"          # server used for query encoding and unnamed builds

[vector.embeddings.servers.local]
endpoint = "http://localhost:11434/v1"  # OpenAI-compatible base URL; "/embeddings" is appended
api_key_env = "OPENAI_API_KEY"    # name of an env var holding the key; omit for anonymous access
batch_size = 32                   # inputs per HTTP call (default 32)
concurrency = 4                   # documents embedded in parallel during a build (default 4)
timeout = "30s"                   # per-HTTP-call timeout (default "30s")
max_retries = 3                   # attempts on 429/5xx/network errors; 4xx fails fast (default 3)

[vector.embed]
run_after_sync = true             # daemon embeds deltas after each sync, debounced ~30s (default true)
backstop_interval = "24h"         # periodic full reconciliation scan; negative disables (default "24h")
```

`model`, `dimension`, and at least one `[vector.embeddings.servers.<name>]`
entry with an `endpoint` are required once `enabled = true`; `agentsview` fails
fast with an actionable message if any is missing or a duration field doesn't
parse. Restart the daemon (or run a CLI command) after editing the file.

### Named embeddings servers

Model identity — `model`, `dimension`, `max_input_chars`, `input_suffix` — is
global: every server in the `servers` table must serve that same model, so
vectors produced by any of them are interchangeable and land in the same
generation. What varies per server is transport and capacity: `endpoint`,
`api_key_env`, `timeout`, `max_retries`, `batch_size`, and `concurrency`.

This split exists so you can encode search queries against a fast local server
while offloading bulk index builds to a bigger remote machine:

```toml
[vector.embeddings]
model = "qwen3-embedding-4b"
dimension = 2560
input_suffix = "<|endoftext|>"
default_server = "local"

[vector.embeddings.servers.local]      # laptop llama.cpp: low latency for queries
endpoint = "http://127.0.0.1:30000/v1"

[vector.embeddings.servers.build-box]  # remote GPU box: high throughput for builds
endpoint = "http://build-box:30000/v1"
timeout = "300s"
concurrency = 6
```

`default_server` names the server used for search-time query encoding and for
any build that doesn't select one; with a single server defined it is implicit,
with more than one it is required.
`agentsview embeddings build --using build-box` runs one build against a
different server without touching the default. Because the model identity is
global, the server choice is not part of the generation fingerprint — a build
started on one server can be topped up incrementally from another.

One caveat: the same model served at different quantizations (say F16 on one
box, Q8 on another) produces slightly different vectors for the same text. They
live in the same embedding space and search still works, but for bit-identical
vectors serve the same weights everywhere.

`concurrency` bounds how many documents a build embeds in parallel. Builds are
usually round-trip-bound rather than compute-bound — especially against a remote
endpoint — so a few requests in flight at once multiply throughput. Servers that
process one request at a time simply queue the extras; raise the value if your
endpoint has spare parallel capacity, or set it to 1 to send one request at a
time. Responses are requested in the compact base64 encoding automatically (with
a transparent fallback for servers that reject or ignore `encoding_format`),
which cuts response transfer roughly 4x on slow links.

`input_suffix` is appended verbatim to every text sent to the endpoint —
documents at build time and queries at search time — for models that expect a
terminator the serving layer does not add. The main example is Qwen3-Embedding
served by llama.cpp, which is benchmarked with `<|endoftext|>` appended to each
input. The suffix is part of the generation fingerprint, so changing it
(including setting it for the first time) re-embeds the whole archive on the
next build.

The first scheduled build that `run_after_sync` triggers after enabling
`[vector]` embeds the entire existing archive, not just deltas, since the mirror
starts out empty and every document counts as pending. For a hosted embeddings
API that is a real cost event, so run `agentsview embeddings build` directly at
a time of your choosing if you want to control when that initial cost lands,
rather than letting the debounced after-sync scheduler trigger it on its own.
The same cost event can recur on upgrade: when a new agentsview version changes
the index's internal mirror schema or document-identity scheme, the next
writable open resets the mirror, and with `run_after_sync = true` the next sync
automatically re-embeds the entire archive against the configured endpoint.

By default, `include_automated = false` keeps automated sessions (e.g. roborev)
out of the embedding index entirely, mirroring session search's default
exclusion of those sessions from results. This matters most for a large archive
dominated by automated sessions: embedding content that search already hides by
default just adds embedding API cost and dilutes semantic ranking with results
nobody is searching for. Because a session that was never embedded has no vector
to match, `session search --semantic --include-automated` still returns no
semantic hits for automated sessions unless the index was built with
`include_automated = true` (or a one-off `embeddings build --include-automated`,
see below). Changing `include_automated` between builds — in config or via the
flag — triggers a full mirror reconciliation on the next build: it removes
now-out-of-scope rows (and their vectors) or picks up newly-in-scope sessions,
without re-embedding documents that were already in scope and unchanged.

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
model = "nomic-embed-text"
dimension = 768

[vector.embeddings.servers.local]
endpoint = "http://localhost:11434/v1"
```

The encoder POSTs to `<endpoint>/embeddings` with an OpenAI-style
`{"model": ..., "input": [...]}` body and expects
`{"data": [{"index": 0, "embedding": [...]}]}` back — this matches Ollama's
`/v1/embeddings` route as well as OpenAI and most self-hosted OpenAI-compatible
servers. A response whose embedding length doesn't match `dimension` is
rejected.

## What gets embedded: units, not messages

The index embeds **unit documents**, not individual messages:

- Every embeddable user message (non-system, not system-prefixed) is its own
  document.
- Assistant messages between those user messages are concatenated — in order,
  separated by blank lines — into one **run** document per stretch of work. A
  run captures a whole narrative arc (analysis, tool narration, conclusions)
  instead of scattering it across hundreds of short fragments.

This matters for both quality and cost. Most assistant messages are short,
procedural narration that is meaningless as a standalone search hit; grouped
into runs, roughly 1.1 million assistant messages collapse into ~44k documents —
about 25x fewer assistant-side documents to embed and rank. Long documents are
chunked at `max_input_chars` runes (default 8192) with a 15% overlap between
consecutive chunks (1228 runes at the default; 375 at a 2500 cap), so an initial
build sends several times fewer encode requests than a per-message scheme would.

Content from sidechains and delegated (subagent/fork) sessions is embedded too,
but classified **subordinate**: still searchable, annotated in results, and
ranked below top-level human-driven work. The
[`--scope` flag](#scoping-results-scope) controls whether you see it.

## Building the index

```bash
agentsview embeddings build            # incremental: refresh + fill whatever's missing
agentsview embeddings build --yes      # skip confirmation prompts
agentsview embeddings build --full-rebuild --yes  # re-embeds every document
agentsview embeddings build --backstop # force a full mirror reconciliation scan
agentsview embeddings build --include-automated  # embed automated sessions for this build only
agentsview embeddings build --using build-box    # encode against a named server instead of the default
```

`--using <name>` selects which `[vector.embeddings.servers.<name>]` entry the
build encodes against; without it the build uses `default_server`. A mistyped
name fails immediately, before anything starts.

`--include-automated` overrides `[vector].include_automated` for this one build;
it does not change the config file. Bare `--include-automated` embeds automated
sessions, and `--include-automated=false` force-excludes them even if the config
default is `true`. It is meant for a one-off build, not scheduled ones: the
after-sync scheduler and periodic backstop always build from the config value,
so mixing the flag with a different config default flips the index's scope back
and forth on every other build, forcing a full mirror reconciliation each time.
Set `include_automated = true` in `config.toml` instead if you want automated
sessions embedded on every build.

`embeddings build` mirrors the embeddable universe (user documents and assistant
runs, see [What gets embedded](#what-gets-embedded-units-not-messages)) into
`vectors.db`, then fills whatever the active generation is missing.
`--full-rebuild` re-embeds every document: if the target fingerprint (derived
from `model`, `dimension`, `max_input_chars`, `input_suffix` when set, the
document-unit scheme, and the derived chunk overlap) differs from the active
generation, it cuts a new **generation**; if the fingerprint is unchanged, it
instead resets and refills the active generation in place rather than cutting a
new one. It prompts for confirmation with a live count of embeddable unit
documents unless `--yes` is passed. Progress prints every ~2 seconds while a
build runs, and a summary line reports documents embedded, chunks, skipped, and
stale counts on completion.

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

### Command Palette

The web command palette offers **Full text**, **Semantic**, and **Hybrid**
modes. Semantic ranks message content by meaning, while Hybrid combines semantic
and FTS5 rankings. Both present the highest-ranked match from each session, with
at most one result per session, and remember the selected mode across palette
openings and browser sessions.

Semantic and Hybrid depend on the same enabled `[vector]` configuration and
active embeddings index described above. Configuration and index builds remain
CLI/config-file operations; the palette surfaces actionable setup or rebuild
errors and stays in the selected mode. To continue with Full text after an
error, choose it explicitly—the UI never falls back automatically. The
in-session find bar remains unchanged and does not support semantic search.

Palette searches run after a 300ms typing pause. Each Semantic or Hybrid query
must be encoded, so a remote embeddings server can add latency and per-request
cost compared with a local encoder.

### Command-line search

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
  etc. Metadata filters are applied *after* the vector leg over-fetches
  candidates (4x the requested limit) — see [Limitations](#limitations). In
  these two modes [`--scope`](#scoping-results-scope) replaces
  `--include-children` for deciding whether delegated-session content appears.
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

### Hit shape: ranges and anchors

Every content-search match, in every mode, cites a *conversation unit* — a user
message, or a run of assistant messages between user turns — anchored to one
specific message inside it:

- `ordinal` is the **anchor**: the exact matched message, same as every other
  release. For user messages and single-message units it's the message's own
  ordinal.
- `ordinal_range` is `[start, end]` — the conversation unit containing the
  anchor. It is always present, never omitted: a single-message unit
  serializes `[ordinal, ordinal]`, and a unit starting at ordinal 0 still
  serializes its start.
- `subordinate`, `relationship`, `parent_session_id`, and `is_sidechain` carry
  the hit's lineage in every mode: whether it came from a sidechain or a
  delegated (subagent/fork) session, and which parent session to corroborate
  against. These stay `omitempty`; a missing key unambiguously means top-level
  / no lineage.

What the range *means* depends on the mode:

- **Semantic hits and hybrid unit hits** carry the embedded unit's span from the
  vector index — the identity of the document that actually matched.
- **Substring, regex, and FTS matches** (and hybrid hits whose message has no
  embedded unit) carry a **structurally derived** unit computed from the
  archive's messages alone, using the same user-message/assistant-run rules
  the index uses. Lexical citations therefore need no vector index and never
  change with index state. Derived and embedded spans coincide except where
  the index's scope diverges from structure: sessions excluded from the build
  (`include_automated = false`) and messages newer than the last index
  refresh.

Lexical row cardinality is unchanged: substring/regex/FTS still return one row
per matching source row, with the same snippets — the range and lineage fields
are additive metadata on each row.

Human output renders a multi-message unit as `#<start>-<end> @<anchor>` and
marks subordinate hits with `sub`; both can appear in any mode:

```text
def456  #12-40 @19 sub score=0.71  myapp  message
    ...decided to key runs on the first member so tail growth is cheap...
```

### Scoping results: `--scope`

`--scope top|all|subordinate` (HTTP/MCP: `scope`) controls whether subordinate
content — sidechain runs and subagent/fork session content — appears in semantic
and hybrid results:

- `all` (default): everything is searchable; subordinate hits are downranked
  below top-level hits of similar relevance and annotated, never hidden.
- `top`: only top-level, human-driven conversation. Use this when reconstructing
  decisions — delegated sessions repeat their parent's instructions and can
  drown out the conversation where the decision was actually made.
- `subordinate`: only sidechain/delegated content, e.g. to find what a subagent
  actually did.

`--scope` is only valid with `--semantic`/`--hybrid` (other modes reject it) and
supersedes `--include-children` there: child sessions are always visible to
these modes so that `scope` alone governs what you see. Subagent/fork-typed and
parent-linked sessions are also exempt from the default one-shot exclusion in
these modes — a subagent session structurally has exactly one "user" message
(its task prompt), so the one-shot gate would otherwise hide nearly all of them.
Substring, regex, and FTS modes keep the existing `--include-children` and
one-shot behavior unchanged.

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
what followed. For a hit whose unit spans a multi-message run, `ordinal` is the
anchor — the member the matched text belongs to — so centering `--around` on it
lands in the right part of the run; widen `--before`/`--after` toward the ends
of `ordinal_range` to read the whole stretch.

## Error taxonomy

| Situation                                                                                               | Message                                                                                                                                                    |
| ------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `[vector]` not enabled                                                                                  | `vector search is not enabled: set [vector] enabled = true in config.toml` (from `agentsview embeddings ...`)                                              |
| No `VectorSearcher` wired (index never built, DuckDB backend, or PG with no matching pushed generation) | `semantic search not available: enable [vector] in config.toml and run 'agentsview embeddings build'`                                                      |
| Only a building generation exists                                                                       | same message, plus `: index is building: N% complete`                                                                                                      |
| Active generation's fingerprint no longer matches config (model, dimension, or chunking changed)        | same message, plus `: index is stale (embedding config changed): run 'agentsview embeddings build --full-rebuild'`                                         |
| Index was built by an incompatible agentsview version (mirror schema mismatch)                          | same message, plus `` : vector index was built by an incompatible version: run `agentsview embeddings build` ``                                            |
| `--scope` with a lexical mode (or without `--semantic`/`--hybrid`)                                      | CLI: `--scope requires --semantic or --hybrid`; HTTP/MCP: `scope is only supported for semantic and hybrid search modes`                                   |
| Embeddings endpoint unreachable or timed out                                                            | `[vector.embeddings] request: ...` (the underlying transport error)                                                                                        |
| Embeddings endpoint returned non-200                                                                    | `[vector.embeddings] status <code>: <body>`                                                                                                                |
| `--in` names a source other than `messages` with `--semantic`/`--hybrid`                                | CLI: `--semantic searches messages only; drop --in` (or `--hybrid ...`); HTTP/MCP: `search: semantic search only supports the messages source (got "...")` |
| `--cursor` with `--semantic`/`--hybrid`                                                                 | `semantic search returns a single ranked page; cursor pagination is not supported`                                                                         |

Over HTTP (`GET /api/v1/search/content`) and MCP (`search_content`), the "not
available" family of errors maps to HTTP `501 Not Implemented` and the matching
MCP tool error, carrying the same remediation text.

## PostgreSQL

The `--pg` read path and `agentsview pg serve` support semantic and hybrid
search backed by [pgvector](https://github.com/pgvector/pgvector), so a shared
PostgreSQL deployment answers `--semantic`/`--hybrid` the same way a local
SQLite index does. Only the DuckDB mirror lacks a vector backend and still
returns the "not available" error (HTTP 501).

### Pushing embeddings

`agentsview pg push` runs a vector phase after the session and message phases:
it copies the machine's active generation from the local `vectors.db` mirror
into PostgreSQL as per-generation `halfvec` chunk tables. Only the active
generation is pushed, and only sessions whose document set changed since the
last push are re-sent, mirroring the incremental session push (`pg push --full`
bypasses the change detection and re-sends every session's vectors). The
`pg push` summary reports the phase as
`Vectors: N session(s) pushed, ... docs, ... chunks`, or
`Vectors: skipped (<reason>)` when it does not run.

Skip the phase for a single run with `--no-vectors`, or disable it persistently:

```toml
[pg]
push_vectors = false
```

`push_vectors` defaults to true, so a machine with `[vector]` enabled pushes its
embeddings automatically. A machine without `[vector]` enabled has no generation
to push and skips the phase regardless.

### Serving semantic search

`pg serve` — and every `--pg` direct-read command — wires PG-backed semantic
search at startup when three conditions hold on the serving host:

1. `[vector]` is enabled in that host's `config.toml`.
1. The host's embedding config fingerprint (model, dimension, chunking, prompt
   affixes) matches a generation already pushed to PostgreSQL. The fingerprint
   is immutable, so a startup match cannot go stale while the process runs;
   changing the local embeddings config changes the fingerprint and requires a
   restart to pick up.
1. The configured embeddings server is reachable from the serving host, because
   query text is embedded at search time with the same encoder the index was
   built with.

If no generation matches, `pg serve` starts normally but semantic and hybrid
search return the 501 "not available" error carrying the mismatch reason, which
lists the fingerprints PostgreSQL does have so an operator can tell a "wrong
config" miss from a "never pushed" one. A missing pgvector extension or vector
tables degrade the same way — see
[Backends without pgvector](#backends-without-pgvector).

### Shared generations across machines

A generation is keyed by its config fingerprint, so every machine with an
identical `[vector.embeddings]` config pushes into the *same* PostgreSQL
generation. Their documents and chunks accumulate side by side, and any serving
host with the matching fingerprint searches the union. Coverage is partial by
construction: a session becomes semantically searchable only once the machine
that owns it has pushed its embeddings, so a freshly pushed session's text can
match lexically (through the hybrid keyword leg) before its vectors arrive.

### Storage and indexing

Embeddings are stored as pgvector `halfvec` (16-bit) columns, one chunk table
per generation, each indexed with an HNSW cosine index. `halfvec` halves storage
versus 32-bit `vector` and stays under HNSW's dimension ceiling, so models up to
2560 dimensions index cleanly where plain `vector` cannot. Per-generation tables
are required because a pgvector column has a fixed typed dimension.

### Backends without pgvector

Semantic search degrades gracefully when the extension is absent or too old.
`pg push` best-effort runs `CREATE EXTENSION IF NOT EXISTS vector`; if that
fails — CockroachDB, which has no pgvector; a database where the extension
package is not installed; or a role lacking `CREATE` privilege — schema setup
logs a one-line notice and continues, and the vector phase is skipped. `halfvec`
needs pgvector 0.7.0 or newer: `CREATE EXTENSION IF NOT EXISTS` never upgrades
an existing extension object, so when an older version is installed `pg push`
attempts `ALTER EXTENSION vector UPDATE` (healing servers whose pgvector package
was upgraded in place) and otherwise skips the vector phase, reporting the
installed version. Session and message sync are unaffected in every case. On the
read side a missing vector table is treated as "no generation found", so
`pg serve` starts and only `--semantic`/`--hybrid` return 501 while lexical
search keeps working.

### Maintenance

`agentsview pg vectors list` prints every generation with its model, dimension,
document and chunk counts, contributing machines, and creation time.
`agentsview pg vectors drop <id>` removes a generation and all of its embeddings
(prompted unless `--yes`); use it to reclaim space after retiring an embedding
model. Both accept `--target` to select a non-default PG target.

### Hybrid keyword leg

Both backends fuse the same two legs with the same reciprocal-rank merge, but
their keyword legs rank differently. SQLite's hybrid keyword leg is BM25-ranked
through FTS5; PostgreSQL's is an `ILIKE` scan ordered by recency (newest first).
The vector leg, the RRF fusion, the subordinate-unit penalty, scope filtering,
and hit anchoring are identical, so top results usually agree — but the
keyword-leg input order, and therefore fusion ties broken by keyword rank, can
differ between the backends.

## Limitations

- **Metadata filters post-filter the vector leg.** `--semantic`/`--hybrid`
  over-fetch candidates from the vector index (4x the requested limit, or a
  fixed minimum if that's larger), then drop hits whose session fails
  `--project`/`--agent`/`--date*`/etc., then truncate to the requested limit.
  At small corpus sizes or with a narrow filter, this can return fewer than
  `--limit` results even though more exist. A narrow `--scope` (and, in
  hybrid, matches concentrated in one long run) can likewise return fewer than
  `--limit` even when more matches exist deeper in the ranking. This is a
  known v1 tradeoff, not a bug.
- **Legacy no-`source_uuid` rows re-embed on ordinal shifts.** Each embedded
  document is keyed by its first message's stable per-message UUID when the
  parser recorded one, or by `(session_id, ordinal)` when it didn't.
  UUID-keyed documents survive ordinal renumbering (e.g. from a resync) as a
  cheap metadata update with no re-embed; ordinal-keyed documents are treated
  as new and re-embedded when their ordinal shifts. This only affects older
  parsed data predating per-message UUIDs and is an accepted cost rather than
  a bug.
- **The active run re-embeds as it grows.** A run document is keyed on its first
  message, so a session's trailing run keeps its identity as new assistant
  messages append — but its content changes, so each build re-embeds the
  current tail. That is the intended cost of grouping; finished runs never
  re-embed.
- **DuckDB mirror has no vector backend.** `--semantic`/`--hybrid` against a
  DuckDB-backed server return the "not available" error (HTTP 501) described
  above. PostgreSQL is supported through pgvector — see
  [PostgreSQL](#postgresql).
- **The index embeds message `content` verbatim.** Like `--fts`, it only draws
  from the `messages` source, so raw tool_input/tool_result rows are never
  candidates. System messages are handled more strictly, though: `--fts` still
  includes them unless the caller passes `--exclude-system`, while
  `--semantic`/`--hybrid` always exclude system messages from the index with
  no flag to opt back in. But anything a parser rendered *into* a
  user/assistant message's content is embedded with it: thinking text
  flattened inline as `[Thinking]...[/Thinking]` markers, and tool-call
  summaries some parsers render into assistant content, are all ordinary message
  text to the index. Run documents concatenate that per-message text unchanged
  — no role labels or markers are injected between members.

## Skills for coding agents

`agentsview skills install` writes a bundled skill file that teaches a
coding-agent harness the search workflow described on this page: when to reach
for `--hybrid` versus `--fts`, how to react to the
[error taxonomy](#error-taxonomy), and how to walk from a hit into its
surrounding conversation with
[`session messages --around`](#cursor-follow-from-a-hit-to-its-surrounding-conversation).

```bash
agentsview skills install                    # both harnesses, user level
agentsview skills install --harness claude   # one harness only
agentsview skills install --project          # install under the current git root
agentsview skills list                       # show install state per harness
```

| `--harness` | Target                                                                                                                     |
| ----------- | -------------------------------------------------------------------------------------------------------------------------- |
| `claude`    | `~/.claude/skills/agentsview-finding-history/SKILL.md`                                                                     |
| `agents`    | `$HOME/.agents/skills/agentsview-finding-history/SKILL.md` — the open convention Codex reads (per Codex's own skills docs) |

`--project` swaps the base from the home directory to the current git root (or
the working directory itself outside a repo), writing to `.claude/skills/...`
and `.agents/skills/...` instead.

Every rendered file carries a `generated-by` header with a content hash, written
as a YAML comment just inside the frontmatter fence so the file still starts
with `---` and harnesses keep discovering it. `install` overwrites a file whose
hash still matches its header (unmodified since the last install) but refuses a
file that was hand-edited or was never generated by `agentsview`, printing which
paths it refused and exiting non-zero; pass `--force` to overwrite anyway.
Re-run `agentsview skills install` after upgrading `agentsview` to pick up skill
content changes — the header records the CLI version for humans, but the content
hash, not the version, decides whether a reinstall is a no-op.

`agentsview skills list [--project] [--format json]` reports each harness's
install state — `missing`, `current`, `stale` (unmodified but older than the
current render), `modified`, or `foreign` (no header) — without writing
anything.
