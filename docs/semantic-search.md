---
title: Semantic Search
description: Vector (semantic) search over session messages, plus hybrid search and cursor-based context retrieval
---

AgentsView can index your user and assistant messages in a local vector store so
you can search by meaning instead of exact terms, alongside the existing
substring, regex, and FTS5 content search. The feature is opt-in and works with
Ollama, Kit's shared local server, or any OpenAI-compatible API you configure.
For uploaded sessions, see [hosted raw sync setup](/docs/hosted-raw-sync/).

For the architecture behind this page, including storage layout, generations,
concurrency, and the search path, see
[Semantic Search Internals](/docs/semantic-search-internals/).

!!! note "Backends"

    Semantic and hybrid search run on the local SQLite archive, on
    [PostgreSQL](#postgresql) with pgvector, and on [ClickHouse](#clickhouse).
    When search by meaning is unavailable, CLI and MCP hybrid searches return
    keyword matches with a note.

## Enabling `[vector]`

Semantic search is off by default. To turn it on in the UI, open
**Settings → Embeddings** and select **Turn on**, or select it from the search
palette. AgentsView asks for the required download through that button, shows
download and indexing progress, and starts semantic search without a restart.
Full-text search stays available while indexing runs.

To set it up from the CLI, run `agentsview index embeddings build` while a
writable daemon is running. AgentsView uses a running Ollama server with Nomic
or Kit's shared local server. If the local server needs files, the CLI asks
before downloading them. Searches and automatic builds reuse installed files,
and they point you to the CLI when a download needs approval. If you decline,
the download and build stop.

### Turned on in another app

If you turn on built-in search by meaning in any app that uses Kit, it turns on
for all of them. Once the model files are installed, AgentsView starts indexing
on its next launch without editing `config.toml`. To keep it off, set
`[vector] enabled = false`. Configuring a custom model or server, or a
PostgreSQL or ClickHouse target that receives vectors, also keeps it off.

The default recipe uses `nomic-embed-text` with 768 dimensions, 4,000 input
characters, 2,048 context tokens, `search_query: ` for queries, and
`search_document: ` for documents. Explicit model recipes keep their own
settings, including empty prefixes. Only untagged `nomic-embed-text` uses
automatic discovery without a server table. Tagged recipes such as
`nomic-embed-text:latest` and `nomic-embed-text:v1.5` need a named server with
`endpoint` or `local = true`.

To use an explicit model or endpoint, configure the recipe and named servers in
`config.toml`:

```toml
[vector]
enabled = true                    # default false; everything below is opt-in
# db_path defaults to <data_dir>/vectors.db
include_automated = false         # default; automated sessions (e.g. roborev) are not embedded -- set true to include
agents = []                       # default; empty embeds every agent -- e.g. ["claude"] embeds only Claude Code sessions
recall_max_revision_lag = 256     # default; Recall corpus revisions the Recall index may trail and still serve (0 = exact)

[vector.embeddings]
model = "nomic-embed-text"
dimension = 768                   # every returned vector must have this length
max_input_chars = 4000            # per-chunk rune cap (default 8192)
model_context_tokens = 2048       # optional model context used to cap build request size
query_prefix = "search_query: "    # prepended only to search queries
document_prefix = "search_document: " # prepended only to indexed document chunks
# request_dimensions = true      # ask for Matryoshka-reduced vectors of exactly `dimension` (see below)
# input_suffix = "<|endoftext|>"  # appended to every embedded text; default empty (see below)
default_server = "local"          # server used for query encoding and unnamed builds

[vector.embeddings.servers.local]
endpoint = "http://localhost:11434/v1"  # OpenAI-compatible base URL; "/embeddings" is appended
api_key_env = "OPENAI_API_KEY"    # name of an env var holding the key; omit for anonymous access
# api_key_file = "~/.config/agentsview/embeddings-key" # or a private file holding it
batch_size = 32                   # inputs per HTTP call (default 32; 8 for a built-in server)
max_batch_tokens = 120000         # optional provider cap across all inputs in one call
concurrency = 4                   # documents embedded in parallel during a build (default 4; 1 for a built-in server)
timeout = "30s"                   # per-HTTP-call timeout (default "30s"; "5m" for a built-in server)
max_retries = 3                   # attempts on 408/5xx/network errors; other 4xx fail fast (default 3)
                                   # document builds retry a 429 until it clears (or the daemon
                                   # shuts down) instead of spending this budget on it; query
                                   # encoders still count it here
# ollama_cpu_fallback = true      # Ollama only: retry invalid Metal vectors once on CPU

[vector.embed]
run_after_sync = true             # daemon embeds deltas after each sync, debounced ~30s (default true)
recall = false                    # default; set true to permit automatic Recall embedding
backstop_interval = "24h"         # periodic full reconciliation scan; negative disables (default "24h")
```

For English and code sessions, the 4,000-character cap usually fits inside
`nomic-embed-text`'s 2,048-token window in Ollama. If your sessions contain
token-dense text such as Chinese, Japanese, or Korean, lower `max_input_chars`,
because Ollama silently truncates input past its window. Changing this cap on an
existing index [creates a new generation and re-embeds the
archive](#building-the-index) on the next build.

When you configure explicit servers, `model` and `dimension` are required, and
each server must set either `endpoint` or `local = true`. Invalid settings fail
when AgentsView loads the config. After you edit `config.toml`, restart the
daemon or run a CLI command.

Automatic Recall embedding has its own consent gate, which is off by default.
Setting `[vector.embed] recall = true` lets daemon startup, corpus mutations,
and periodic backstops send accepted Recall entry titles, bodies, and triggers
to your configured embeddings endpoint. `run_after_sync` controls only session
archive sync notifications and does not suppress Recall startup or Recall
corpus-mutation refreshes once Recall embedding is enabled. You can still run
`agentsview index embeddings build --store recall` manually without this setting
because that command is an explicit one-time request. Recall vector and hybrid
queries fail closed as unavailable once the served Recall corpus is more than
`recall_max_revision_lag` revisions newer than the last completed build, while
lexical queries stay available as an automatic or manual refresh catches up. See
[Recall](/docs/recall/#vector-and-hybrid-retrieval).

### Named embeddings servers

To use Kit's local server while Ollama stays running, keep your explicit Nomic
recipe and add a named server:

```toml
[vector.embeddings.servers.shared]
local = true
```

If you started with only `[vector] enabled = true`, write out its default recipe
when adding the server table:

```toml
[vector]
enabled = true

[vector.embeddings]
model = "nomic-embed-text"
dimension = 768
max_input_chars = 4000
model_context_tokens = 2048
query_prefix = "search_query: "
document_prefix = "search_document: "
default_server = "shared"

[vector.embeddings.servers.shared]
local = true
```

Set `default_server = "shared"` under `[vector.embeddings]` for searches and
scheduled builds, or run `agentsview index embeddings build --using shared` for
a single build. To reuse the active generation so only new or changed content
needs embedding, keep your existing model, dimensions, prefixes, suffix, and
input cap. `local = true` supports `nomic-embed-text`,
`nomic-embed-text:latest`, and `nomic-embed-text:v1.5`, and it cannot be
combined with `endpoint`. Keep the same model spelling when you switch servers,
because changing model identity requires rebuilding the index.

Model identity is global: `model`, `dimension`, `request_dimensions`,
`max_input_chars`, `query_prefix`, `document_prefix`, and `input_suffix` apply
across every entry in `servers`. Every server must serve that same model and
input recipe so the vectors they produce are interchangeable and land in the
same generation. Transport and capacity settings vary per server: `endpoint`,
`local`, `api_key_env` or `api_key_file`, `timeout`, `max_retries`,
`batch_size`, `max_batch_tokens`, `concurrency`, and `ollama_cpu_fallback`.

When both `model_context_tokens` and a server's `max_batch_tokens` are set,
builds cap that server's effective batch size at
`floor(max_batch_tokens / model_context_tokens)`. AgentsView conservatively
charges every input the full model context because providers may truncate each
oversized input to that exact length before enforcing their aggregate request
cap. For example, a 32,000-token context with a 120,000-token request cap
reduces `batch_size = 4` to three inputs per call (96,000 worst-case tokens),
preventing the 128,000-token request that four truncated inputs could produce.

`max_batch_tokens` must be at least `model_context_tokens`, and setting it
requires `model_context_tokens`. `max_batch_tokens` defaults to zero (disabled).
`model_context_tokens` also defaults to zero in an explicit model recipe, while
the default recipe (only `[vector] enabled = true`) sets it to 2048. Because
`max_batch_tokens` is zero by default, servers keep document-count batching
until you set it. These settings only shape build and repair requests; they do
not alter input text or the vector-generation fingerprint.

Any `agentsview` command that loads the config, including `sync` and
`embeddings build`, fails if `[vector]` or its subtables contain an unknown key.
This check runs even when vector search is disabled, so a misspelled `enabled`
key cannot silently leave it off. The error names the unknown key, and if you
put a per-server key in the parent `[vector.embeddings]` table, the error also
points to `[vector.embeddings.servers.<name>]` as the table that owns it. Fix or
remove the key before running the command again.

Splitting model identity from server transport lets you encode search queries on
a fast local server while running bulk index builds on a larger remote machine:

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
any build that doesn't pick a server. When you define only one server, it is
implicit; with more than one, `default_server` is required. To run a single
build on a different server without changing the default, pass `--using`, as in
`agentsview index embeddings build --using build-box`. Because model identity is
global, server choice is not part of the generation fingerprint, so you can
start a build on one server and top it up incrementally from another.
`ollama_cpu_fallback` is also transport-only, so toggling it does not change the
generation fingerprint or rebuild the index.

One caveat: serving the same model at different quantizations, such as F16 on
one box and Q8 on another, produces slightly different vectors for the same
text. They still live in the same embedding space and search keeps working, but
if you want bit-identical vectors, serve the same weights everywhere.

`concurrency` limits how many documents a build embeds in parallel. Builds are
usually round-trip-bound rather than compute-bound, especially against a remote
endpoint, so keeping a few requests in flight multiplies throughput. Servers
that handle one request at a time queue the extras. Raise `concurrency` if your
endpoint has spare parallel capacity, or set it to 1 to send one request at a
time.

### Transport trade-offs

`batch_size`, `concurrency`, and `timeout` control how a build spreads its work
across HTTP requests. They do not change the vectors or the generation
fingerprint, so you can adjust them at any time. They trade throughput against
how long each request takes, and the best values depend on how your server runs
the model.

| Setting       | Raise it when                                                                                    | Cost of raising it                                                                                                                                      |
| ------------- | ------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `batch_size`  | Each request has a high fixed cost, such as a remote API, or the server batches inputs on a GPU. | Each request takes longer and must still finish within `timeout`. A failed request wastes more work. Provider token caps apply; see `max_batch_tokens`. |
| `concurrency` | The server runs several requests in parallel, such as a hosted API or a multi-GPU machine.       | A server that runs one input at a time only queues the extra requests, and each request also waits for the inputs of the others.                        |
| `timeout`     | The hardware is slow or batches are large.                                                       | A server that hangs takes longer to detect. Search queries use the same timeout, so a hung server holds a search for that long.                         |

Every request must finish within `timeout`. On a server that embeds one input at
a time, inputs from all requests in flight share one queue, so a single request
can take roughly `batch_size × concurrency ×` the time for one input.

#### What the built-in server does

A built-in server has no `endpoint`; it uses either automatic discovery or a
server with `local = true`. It runs one of two programs:

- **Kit's shared local server.** Runs `nomic-embed-text` with ONNX Runtime on
  the CPU and has no GPU support yet. Because it embeds one input at a time
  across all requests, `batch_size` and `concurrency` do not change its
  throughput; they only change how long each request waits. A full 2,048-token
  chunk can take a few seconds on a laptop CPU.
- **Ollama.** When automatic discovery finds Ollama running, it uses a GPU when
  one is available, such as Metal on Apple Silicon. It can process several
  inputs in one request together, so larger batches help it.

#### Built-in defaults

A built-in server defaults to `batch_size = 8`, `concurrency = 1`, and
`timeout = "5m"`. A server with an `endpoint` defaults to 32, 4, and `"30s"`.

The built-in defaults are sized for Kit's server on a CPU. A request of 8 full
chunks takes about 20 seconds on a recent laptop, so a 5-minute timeout leaves
headroom for slower machines. Sending one request at a time with 8 inputs per
request does not cost Kit's server any throughput because it runs one input at a
time anyway.

Those defaults come with a few trade-offs:

- **Ollama runs slower than it could.** Automatic discovery may pick up Ollama
  instead of Kit's server, and the built-in defaults leave its GPU underused. To
  get the endpoint defaults, configure Ollama as a named server with
  `endpoint = "http://localhost:11434/v1"`.
- **A hung server takes a long time to fail.** The 5-minute timeout applies to
  each attempt, and retries and batch splits add more attempts. A search makes
  up to `max_retries` attempts (3 by default), so it can wait about 15 minutes.
  A build also halves a batch of 8 three times before reaching a single input,
  so it can wait about half an hour before reporting the failure.
- **CPU builds are slow.** The first build of a large archive on Kit's server
  can take hours, though later builds embed only new or changed content. You can
  run the first build faster on a [remote server](#named-embeddings-servers)
  with a GPU by passing `--using`.

To tune Kit's server, name it with `local = true` and set the transport fields,
keeping the default recipe shown in
[Named embeddings servers](#named-embeddings-servers):

```toml
[vector.embeddings.servers.shared]
local = true
batch_size = 4
timeout = "10m"
```

#### Timeouts during a build

When a multi-input request times out, Kit's client splits it in half and retries
as two smaller requests, continuing to halve while requests time out. Each split
waits through one full timeout first, and Kit's server stops working on an
abandoned request after it finishes its current input. If a single input times
out, the build fails with `embed request timed out` and the next build retries
it. If you keep hitting this error, raise `timeout` or lower `batch_size`.

### Role-aware task prefixes

Some embedding models expect different instruction prefixes for search queries
and indexed documents. AgentsView prepends `query_prefix` only when embedding a
search query, and prepends `document_prefix` to every document chunk during
manual, scheduled, and repair builds. Both prefixes default to empty in an
explicit model recipe, which preserves the input bytes and generation
fingerprints of older configurations. The default recipe (only
`[vector] enabled = true`) uses `search_query: ` and `search_document: `.

Here is EmbeddingGemma's standard retrieval recipe without document titles:

```toml
[vector.embeddings]
model = "embeddinggemma"
dimension = 768
query_prefix = "task: search result | query: "
document_prefix = "title: none | text: "
```

Nomic uses `search_query: ` and `search_document: `, as shown in the initial
configuration example. E5 models commonly use `query: ` and `passage: `. For
models such as Qwen3-Embedding that recommend an instruction only for queries,
you can leave `document_prefix` unset.

AgentsView applies prefixes after chunking document content, so
`max_input_chars` limits the original chunk rather than the final affixed text.
If your embedding endpoint has a strict context limit, leave enough headroom for
the configured prefix and suffix. When an endpoint explicitly rejects an input
for exceeding its token or context limit, AgentsView logs the rejection and
skip-stamps that document for the generation so one oversized input does not
block the rest of the archive. To include that document, lower `max_input_chars`
and rebuild.

Both prefixes are part of the generation fingerprint. Adding, removing, or
changing either one creates a new generation and re-embeds the archive on the
next build. AgentsView treats the query and document recipe as one reproducible
embedding configuration, even though changing only `query_prefix` does not alter
stored document vectors.

`input_suffix` is appended verbatim to every text sent to the endpoint, for both
documents at build time and queries at search time. Use it for models that
expect a terminator the serving layer does not add, such as Qwen3-Embedding on
llama.cpp, which is benchmarked with `<|endoftext|>` appended to each input.
Because the suffix is part of the generation fingerprint, changing it, including
setting it for the first time, re-embeds the whole archive on the next build.

#### EmbeddingGemma 2 text endpoint

You can use EmbeddingGemma 2 for text search through any OpenAI-compatible
endpoint that serves it. This recipe targets `google/embeddinggemma-2` at
checkpoint revision `914f7f89142e33e77833254d9c9b90c3cef7303b`, with its native
768-dimensional output. AgentsView sends only text and does not embed images,
audio, or video.

```toml
[vector]
enabled = true

[vector.embeddings]
model = "embeddinggemma-2-914f7f89-text-768" # your serving alias
dimension = 768
query_prefix = "task: search result | query: "
document_prefix = "title: none | text: "

[vector.embeddings.servers.local]
endpoint = "http://127.0.0.1:8000/v1"       # your endpoint
batch_size = 4
concurrency = 1
timeout = "120s"
```

The prompts match the [EmbeddingGemma v1 example](#role-aware-task-prefixes) and
the checkpoint's `config_sentence_transformers.json`. Keep the trailing spaces,
leave `input_suffix` empty, and do not configure the endpoint to add the prompts
a second time. The batch, concurrency, and timeout values above are a cautious
starting point for CPU serving, not measured settings.

AgentsView cannot inspect how the endpoint runs the model. You need to serve the
alias with this checkpoint and its tokenizer, mean pooling that includes the
prompt tokens, and L2 normalization, using `bfloat16` or `float32`. The model
card warns that `float16` produces NaN or degraded embeddings. AgentsView
rejects vectors that are non-finite, all zero, or not exactly 768 wide, and stores the
rest unchanged. Search uses cosine similarity, so vector length does not affect
ranking. If you change any part of the serving recipe, give `model` a new alias
so AgentsView builds a new generation.

The model accepts 8192 tokens per input, counting the prompt and special tokens.
Because `max_input_chars` counts runes rather than tokens, the endpoint must
enforce that token limit. Leave headroom as described in
[Role-aware task prefixes](#role-aware-task-prefixes).

For 512, 256, or 128 dimensions, set `dimension` and `request_dimensions = true`
only if your endpoint truncates and re-normalizes server-side. See
[Reduced output dimensions](#reduced-output-dimensions-matryoshka).

Sources: the
[EmbeddingGemma 2 model card](https://ai.google.dev/gemma/docs/embeddinggemma/model_card_2)
and the
[pinned checkpoint files](https://huggingface.co/google/embeddinggemma-2/tree/914f7f89142e33e77833254d9c9b90c3cef7303b)
(`config_sentence_transformers.json`, `1_Pooling/config.json`, and
`modules.json`).

### Reduced output dimensions (Matryoshka)

Matryoshka-trained embedding models, such as Qwen3-Embedding and OpenAI's
`text-embedding-3-*`, can return shorter vectors than their native output by
truncating and renormalizing server-side. You trade a little recall for a
smaller, faster index. By default, AgentsView never requests reduced dimensions:
`dimension` only checks that responses have the expected length, and nothing
extra goes on the wire. Setting `request_dimensions = true` sends `dimension` as
the OpenAI-compatible `dimensions` request field on every embeddings call, for
both document builds and search queries, so both always use the same requested
length:

```toml
[vector]
enabled = true

[vector.embeddings]
model = "qwen3-embedding:0.6b"
dimension = 256                 # reduced from the model's native 1024
request_dimensions = true

[vector.embeddings.servers.local]
endpoint = "http://localhost:11434/v1"
```

```bash
# Qwen3-Embedding supports Matryoshka reduction; Ollama's OpenAI-compatible
# /v1/embeddings route passes the dimensions field through to it.
ollama pull qwen3-embedding:0.6b
```

Both the endpoint and the model must support dimension selection. If an endpoint
rejects the `dimensions` field, the build or query fails with the endpoint's
error, for example `embed endpoint returned 400 (invalid request)`. If an
endpoint silently ignores the field and returns native-length vectors, the build
or query fails the dimension check with
`embed vector <n>: has <x> dimensions, expected <y>`. AgentsView never truncates
vectors itself. Endpoints that do not support the field keep working as long as
`request_dimensions` stays unset.

`request_dimensions` is part of the generation fingerprint when enabled, like
`input_suffix`. Because reduced vectors are renormalized prefixes rather than
byte-identical copies of the native output, turning `request_dimensions` on or
changing `dimension` makes the existing index stale and re-embeds the archive on
the next build.

Scheduled builds pause on battery power and resume once AC power returns, or at
the next daemon start if the daemon exited while waiting. Manual builds and
`pg push --watch --embed` still run on battery.

The first scheduled build that `run_after_sync` triggers after you turn on
`[vector]` embeds the entire existing archive, not only deltas, because the
mirror starts empty and every document is pending. With a hosted embeddings API,
that first pass is a real cost event. To control when that cost lands instead of
letting the debounced after-sync scheduler trigger it, run
`agentsview index embeddings build` yourself. The same cost event can happen
again on upgrade: when a new AgentsView version changes the index's internal
mirror schema or document-identity scheme, the next writable open resets the
mirror, and with `run_after_sync = true` the next sync automatically re-embeds
the archive against your configured endpoint.

By default, `include_automated = false` keeps automated sessions, such as
roborev, out of the embedding index entirely, matching session search's default exclusion
of those sessions. This matters most on large archives full of automated
sessions: embedding content that search already hides by default adds API cost
and dilutes semantic ranking with results you are not looking for. Because an
unembedded session has no vector to match,
`session search --semantic --include-automated` still returns no semantic hits
for automated sessions unless you built the index with
`include_automated = true` or ran a one-off
`embeddings build --include-automated` (see below). Changing `include_automated`
between builds, in config or with the flag, triggers a full mirror
reconciliation on the next build. AgentsView removes now-out-of-scope rows and
their vectors or picks up newly-in-scope sessions without re-embedding documents
that stayed in scope and unchanged.

`agents` limits the session index to the listed agents, so a build skips the
embedding work for every other agent. For example, `agents = ["claude"]` embeds
only Claude Code sessions. An empty or omitted list embeds every agent. Names
are the agent IDs AgentsView stores on each session, such as `claude` or
`codex`. An unknown name fails config load with
`[vector] agents: unknown agent "<name>"`, so a typo never empties the index.

`agents` combines with `include_automated`: a session is embedded only when its
agent is in the list (or the list is empty) and it is a human session or
`include_automated = true`. The after-sync build, the periodic backstop, and
`embeddings build` all use the configured list; there is no command-line
override. Changing the list triggers the same full mirror reconciliation on the
next build: vectors for agents you removed are deleted, so semantic search stops
returning them. Hybrid search can still return them through keyword matches.
Sessions from agents you added are embedded, including ones older than the last
build. A running daemon picks up the new list after it restarts. Keyword search
is unaffected. The list does not apply to the Recall index or hosted PostgreSQL
embeddings.

### Ollama quickstart

```bash
# Pull an embeddings model once.
ollama pull nomic-embed-text

# Ollama serves an OpenAI-compatible endpoint at /v1; no API key needed.
```

For `max_input_chars` guidance, see [Enabling `[vector]`](#enabling-vector).

```toml
[vector]
enabled = true

[vector.embeddings]
model = "nomic-embed-text"
dimension = 768
max_input_chars = 4000

[vector.embeddings.servers.local]
endpoint = "http://localhost:11434/v1"
ollama_cpu_fallback = true
```

The encoder POSTs `{"model": ..., "input": [...]}` to `<endpoint>/embeddings`
and expects `{"data": [{"index": 0, "embedding": [...]}]}` in response. That
matches Ollama's `/v1/embeddings` route as well as OpenAI and most self-hosted
OpenAI-compatible servers. AgentsView rejects any response whose embedding
length does not match `dimension`, along with non-finite components (`NaN` or
infinity), JSON `null` components, and zero-norm vectors before writing them to
the index. Any of those fails the request, stops the build, and leaves the
document pending for the next build.

Every endpoint must be an absolute HTTP(S) URL without credentials, a query
string, or a fragment. Plain `http://` works for loopback addresses,
private-network addresses, and host names, while an endpoint on a public IP
address requires `https://`.

For Ollama on Apple Metal, `ollama_cpu_fallback = true` recovers a response that
contains invalid vectors instead of failing the request. AgentsView keeps the
valid vectors and sends only the invalid inputs to Ollama's native `/api/embed`
route with `truncate = false`:

1. AgentsView sends the first request with `keep_alive = "0s"`, so Ollama
   unloads the runner after answering. AgentsView then waits up to 10 seconds
   for `/api/ps` to stop listing the model.
1. If the runner unloaded, AgentsView sends one more request on a fresh Metal
   runner.
1. If that still fails, AgentsView sends one request on the CPU with
   `options.num_gpu = 0`.

Recovery requests use a 30-minute timeout instead of the server's `timeout`,
because a model reload or CPU pass can take minutes. The endpoint must end in
`/v1`; AgentsView derives the native routes from it while keeping any proxy path
prefix.

Within the AgentsView process, recoveries against the same Ollama instance run
one at a time. Ordinary embedding requests are not paused during a recovery and
can reload the Metal runner while it runs. Each CPU recovery can add model-load
and CPU-inference latency, followed by another model load for the next Metal
request. Canceled requests leave the recovery queue promptly.

With Ollama 0.32.7 during diagnosis, the CPU request replaced the Metal runner,
unloaded after its response, and caused the next request to load a fresh Metal
runner. That sequence is observed behavior rather than an Ollama scheduler
guarantee, and AgentsView does not verify Ollama's internal runner lifecycle.
Use `ollama_cpu_fallback` as automatic recovery for rare invalid output, not as
a permanent CPU serving mode.

### Direct `llama-server` for high-throughput Ollama models

Ollama's normal `/v1/embeddings` route above is the supported, simplest setup.
If you run the `llama-server` binary bundled with Ollama directly for explicit
slot and batch controls, disable its prompt-state cache for a dedicated
embedding service:

```bash
llama-server \
  --model /path/to/embedding-model.gguf \
  --alias my-embedding-model \
  --host 127.0.0.1 --port 11435 \
  --embedding --parallel 4 \
  --batch-size 8192 --ubatch-size 8192 \
  --cache-ram 0 --no-cache-prompt --no-cache-idle-slots \
  --slot-prompt-similarity 0
```

These are `llama-server` command-line flags, not `ollama serve` environment
variables. Because its prompt cache stores reusable inference state rather than
a final embedding API result, disabling it can still help a dedicated embedding
service that does not benefit from conversational prefix reuse. It does not,
however, fix the Metal corruption diagnosed here: repeated Metal embedding
requests were observed to become non-finite even with prompt caching, context
checkpoints, and slot-similarity routing disabled. Cache and slot settings do
not replace output validation or recovery through an Ollama-managed CPU runner.

Run `llama-server --help` on your installed binary before adopting this direct
setup. If any of these flags are unavailable, upgrade the bundled llama.cpp
binary or use Ollama's normal `/v1/embeddings` route instead. Either way,
AgentsView's validation stays the final safety check and never writes invalid
endpoint output. Explicit `ollama_cpu_fallback` recovery works only through
Ollama's `/v1` endpoint because it depends on Ollama's native `/api/embed`
runner controls, so it does not apply to a standalone `llama-server`.

## What gets embedded: units, not messages

The index embeds **unit documents**, not individual messages:

- Every embeddable user message (non-system and not system-prefixed) becomes its
  own document.
- Assistant messages between user messages are joined in order, separated by
  blank lines, into **run** documents. A run closes after the message that
  brings its joined text to `4 × max_input_chars` runes or more. The next
  assistant message starts a new piece, so appending to a long session
  re-embeds only its trailing piece.

Grouping messages this way helps both search quality and cost. Most assistant
messages are short, procedural narration that makes little sense on its own;
grouped into runs, a hit includes the surrounding narration, and the size cap
keeps each piece to about five chunks. AgentsView chunks long documents at
`max_input_chars` runes (default 8192) with a 15% overlap between consecutive
chunks (1228 runes at the default, or 375 at a 2500 cap), so an initial build
sends several times fewer encode requests than a per-message scheme would.

Content from sidechains and delegated (subagent or fork) sessions is also
embedded, classified as **subordinate**. It stays searchable, is annotated in
results, and ranks below top-level human-driven work. The
[`--scope` flag](#scoping-results-scope) controls whether you see it.

## Building the index

```bash
agentsview index embeddings build            # incremental: refresh + fill whatever's missing
agentsview index embeddings build --yes      # skip the full-rebuild prompt; setup and downloads still ask
agentsview index embeddings build --full-rebuild --yes  # re-embeds every document
agentsview index embeddings build --backstop # force a full mirror reconciliation scan
agentsview index embeddings build --repair-invalid # regenerate only documents with invalid stored vectors
agentsview index embeddings build --include-automated  # embed automated sessions for this build only
agentsview index embeddings build --using build-box    # encode against a named server instead of the default
```

`--using <name>` picks which `[vector.embeddings.servers.<name>]` entry the
build encodes against; if you omit it, the build uses `default_server`. A
mistyped name fails right away, before any work starts.

`--include-automated` overrides `[vector].include_automated` for a single build
without changing `config.toml`. Passing `--include-automated` embeds automated
sessions, while `--include-automated=false` excludes them even if the config
default is `true`. Use this flag for one-off builds, not scheduled ones: the
after-sync scheduler and periodic backstop always follow the config value, so
mixing the flag with a different config default flips the index's scope back and
forth on every other build and forces a full mirror reconciliation each time. If
you want automated sessions embedded on every build, set
`include_automated = true` in `config.toml` instead.

`--repair-invalid` targets the existing generation whose fingerprint matches
your current embedding configuration, and it fails without writing if that
generation does not exist. It does not accept a generation ID. The command scans
stored documents with a revision-current generation stamp in bounded batches and
checks for malformed byte length, a length that differs from the generation's
declared dimension, missing referenced vector rows, duplicate, missing, or
unexpected chunk indices, non-finite components, and zero norm. Documents that
are already pending because their content changed are left for an ordinary
build. If any chunk is invalid, AgentsView invalidates that document's
target-generation chunks and stamp, then runs a repair-only fill restricted to
the affected document keys. Each bounded invalidation transaction queues its
targets before removing old data, so an interrupted scan can resume without
holding every affected document in memory.

Repair never refreshes or expands the document mirror, creates or resets a
generation, or changes generation state. It preserves valid documents, other
generations, and unrelated work that was already pending. A later ordinary
`embeddings build` handles that unrelated pending work. Queued targets clear
only after non-empty replacement vectors are saved,
except when the current queued revision splits into zero chunks and finishes
validly with an empty-document stamp. Once queued, a document stays a repair
target across content revisions: a stale save writes nothing, and the next
repair reads the latest mirrored content. That ownership applies only to
documents already identified as corrupt, not to unrelated pending changes. An
ordinary build that saves non-empty validated vectors for the queued generation
also clears the target. An ordinary permanent rejection may write an empty skip
stamp while leaving the repair target queued, whereas a rejection inside repair
writes no stamp. Timeouts, cancellations, and retryable service errors abort
repair immediately. Full rebuilds preserve queued targets before refilling, so
an empty permanent-skip stamp during that rebuild cannot erase repair ownership.
A permanent rejection inside repair stays queued and unstamped, though repair
still attempts later targets before returning a nonzero aggregate error. If a
failure happens after a valid save and before queue cleanup, a valid stamp may
remain and the queued target still retries safely. Repair never uses the
ordinary build's empty-vector skip stamp. Mirror deletion removes queued targets
for documents that no longer exist, and the queue has a document-key index so
that cleanup does not scan all interrupted repairs.

A nonempty mapping is valid only when its sorted chunk indices match the
configured splitter's output and every mapping references a valid vector. A
stamped document with no chunk mappings is not treated as corruption, because
ordinary builds intentionally use that representation for documents the endpoint
permanently rejects. Raw vector rows with no chunk mapping are also outside
repair scope because they cannot appear as search hits. Because the scan's work
scales with the stored generation, you run it explicitly; scheduled delta and
backstop builds do not run it automatically. CLI repair statistics report the
queued target count and the chunk mappings invalidated by the current scan, and
the ordinary embedded count reports successful replacements. Repair succeeds
only after the integrity scan completes and the durable target queue is empty.
On a failed repair, the CLI prints the partial result, including whether the
scan completed along with failed and remaining target counts, before returning a
nonzero status. A later scan-batch failure keeps counts from earlier committed
batches and best-effort reports the durable queue size. The committed count
stays as the fallback, and cancellation does not cancel the short queue recount.
If that recount also fails, the result marks the count unknown and the CLI
prints `remaining unknown` instead of presenting the fallback as exact. Failed
target counts cover non-stale encode or save failures that determine or
accompany the invocation failure. Stale saves stay in the ordinary stale count,
sibling work canceled after another failure is not counted, and scan failures
are reported through the incomplete scan state.

The CLI requires a local daemon whose API version exactly matches its own. After
upgrading AgentsView, restart any explicitly managed daemon; the CLI rejects a
daemon with a different API version instead of treating absent completion or
certainty fields as false.

`--repair-invalid` is mutually exclusive with both `--full-rebuild` and
`--backstop`, because repair preserves the existing mirror and valid work while
those modes deliberately rebuild or reconcile broader index state.

`embeddings build` mirrors the embeddable universe (user documents and assistant
runs, see [What gets embedded](#what-gets-embedded-units-not-messages)) into
`vectors.db`, then fills whatever the active generation is missing.
`--full-rebuild` re-embeds every document. If the target fingerprint (derived
from `model`, `dimension`, `max_input_chars`, configured input affixes, the
document-unit scheme, and the derived chunk overlap) differs from the active
generation, it cuts a new **generation**. If the fingerprint is unchanged, it
resets and refills the active generation in place instead. Unless you pass
`--yes`, it prompts for confirmation with a live count of embeddable unit
documents. Progress prints about every 2 seconds during a build, and a summary
line reports embedded documents, chunks, skipped counts, and stale counts when
it finishes.

The web Settings page shows the same daemon-owned build while it runs, including
the scan or embedding phase, model and dimension, chunk progress, throughput,
elapsed time, estimated completion, and the generations already stored in
`vectors.db`.

![Embedding build progress](/docs/assets/generated/screenshots/settings-embeddings.png)

When a writable local daemon is running, `build`, `activate`, and `retire` proxy
to it over HTTP so the daemon stays the sole writer of `vectors.db`. Without a
daemon, the CLI takes a dedicated `vectors.write.lock` in the data directory and
runs an enabled build in-process. Initial setup still requires the daemon. If
[`run_after_sync`](#enabling-vector) is enabled, the daemon also embeds sync
deltas automatically on a debounce, so you mainly run a manual `build` for the
initial index or a `--full-rebuild`.

```bash
agentsview index embeddings list
```

```text
STORE     ID  STATE   MODEL             DIM  EMBEDDED  MISSING  FINGERPRINT
messages  1   active  nomic-embed-text  768  482       0        3f2a9c1e0b7d
```

Generations move from **building** to **active** to **retired**. A first build
activates automatically once it reaches full coverage.

```bash
agentsview index embeddings activate <id> [--force]
agentsview index embeddings retire <id> [--force]
```

AgentsView refuses `activate` on a generation with incomplete coverage and
refuses `retire` on the active generation unless you pass `--force`.

## Searching: `session search --semantic` / `--hybrid`

### Command Palette

The web command palette offers **Full text**, **Semantic**, and **Hybrid**
modes. **Semantic** ranks message content by meaning, and **Hybrid** combines
semantic and FTS5 rankings. Both modes show the highest-ranked match from each
session, with at most one result per session, and remember your selected mode
across palette openings and browser sessions.

After you enter a query, the project selector shows the current project scope.
Choose **All Projects** to search across projects without changing the sidebar.

Use the date-range picker beside the search modes to limit results to sessions
active during a relative, calendar, or custom range. The default is **All
time**. Changing the range reruns the query, and your chosen range stays
selected when you switch search modes. When you open the palette after closing
it on a picked result, it reopens that search with the same query, project,
selected result, and date range. Any other close, such as pressing Escape or
clicking outside, clears the search and resets the range to **All time**, and
the next open starts with the sidebar project filter. Semantic and Hybrid
interpret dates in your browser's timezone, whereas Full text uses UTC. Dates
filter session activity rather than individual message timestamps, so a match
from a session that overlaps the range can contain an earlier or later message.

Semantic and Hybrid depend on the enabled `[vector]` configuration and active
embeddings index described above. If `[vector]` is not configured, the palette
shows **Turn on** along with download and indexing progress, and Full-text
results offer the same action while semantic search is off. If vector search is
configured without an active index, **Build embeddings** starts the build
through the local daemon and shows scanning, progress, throughput, and
completion in place. The palette follows an already running build and retries
the query after the build succeeds.

![Guided semantic-search setup in the command palette](/docs/assets/generated/screenshots/semantic-search-setup.png)

Setup and rebuild errors stay visible in the selected mode. To switch back to
**Full text** after an error, select it explicitly, because the UI never falls
back automatically. The in-session find bar is unchanged and does not support
semantic search.

Palette searches run after a 300ms typing pause. Because each Semantic or Hybrid
query has to be encoded, a remote embeddings server can add latency and
per-request cost compared with a local encoder.

### Command-line search

`--semantic` and `--hybrid` add two content-search modes alongside `--regex` and
`--fts`. They are mutually exclusive with each other and with the substring
default:

```bash
agentsview session search "database connection pooling" --semantic --limit 10
agentsview session search "flaky test" --hybrid --project myapp
```

- `--semantic` ranks results by cosine similarity against the query's embedding.
- `--hybrid` combines the semantic ranking with an FTS5 ranking of the same
  corpus using reciprocal rank fusion, so both exact-term matches and
  meaning-based matches appear. When search by meaning is unavailable, CLI and
  MCP return keyword matches with `effective_mode: hybrid_keyword` and a note
  beside `coverage`. That note states the readiness reason (`not set up` or
  indexing progress) and explains that only keyword matches were returned. On
  CockroachDB, every keyword term must appear; if you get few results, try
  short substring queries with key terms or synonyms while keeping your filters.
- Both modes search only the `messages` source, the same restriction `--fts`
  has, because AgentsView embeds only user and assistant message content (never
  raw `tool_input` or `tool_result` rows or system messages). Passing `--in`
  with any other source returns an error.
- All the usual filters apply, including `--project`, `--agent`, `--machine`,
  and `--date*`. AgentsView applies metadata filters *after* the vector leg
  over-fetches candidates (4x the requested limit, with a minimum of 200); see
  [Limitations](#limitations). In these two modes,
  [`--scope`](#scoping-results-scope) replaces `--include-children` to control
  whether delegated-session content appears.
- Results come back as a single ranked page. `--cursor` is rejected for
  `--semantic` and `--hybrid` with a clear error because RRF and cosine ranking
  do not have a stable offset to page from. Every match includes a `score`
  field (cosine similarity for semantic, or the RRF score for hybrid), while
  substring, regex, and FTS matches leave `score` unset.
- An empty query pattern (`""`) returns no matches rather than an error in
  every mode.

Without `--context`, human output prints an aligned table with `ID`, `MATCH`,
`AGE`, `SCORE` (included only when matches have scores), `PROJECT`, `LOCATION`,
and `SNIPPET` columns:

```text
ID      MATCH  AGE  SCORE  PROJECT  LOCATION  SNIPPET
abc123  #42    2h   0.87   myapp    message   ...ideas for pooling database connections across worker threads...
```

### Hit shape: ranges and anchors

In every mode, each content-search match cites a *conversation unit*, which is
either a user message or a run of assistant messages between user turns,
anchored to one specific message inside it:

- `ordinal` is the **anchor**, naming the exact matched message as in every
  other release. For user messages and single-message units, it is the message's
  own ordinal.
- `ordinal_range` is `[start, end]`, giving the span of the conversation unit
  that contains the anchor. It is always present and never omitted: a
  single-message unit serializes `[ordinal, ordinal]`, and a unit starting at
  ordinal 0 still serializes its start.
- `subordinate`, `relationship`, `parent_session_id`, and `is_sidechain` carry
  the hit's lineage in every mode, showing whether it came from a sidechain or
  a delegated (subagent or fork) session and which parent session to check
  against. These fields stay `omitempty`, so a missing key unambiguously means
  top-level with no lineage.

What the range *means* depends on the search mode:

- **Semantic hits and hybrid unit hits.** Carry the embedded unit's span from
  the vector index, which is the identity of the document that matched.
- **Substring, regex, and FTS matches.** Along with hybrid hits whose message
  has no embedded unit, these carry a **structurally derived** unit computed
  from the archive's messages alone. It follows the index's user-message and
  assistant-run boundaries without applying the run size cap, so a derived run
  spans the whole stretch of assistant messages. Lexical citations therefore
  need no vector index and never change with index state. Derived and embedded
  spans differ for assistant runs the index split into capped pieces, sessions
  excluded from the build (`include_automated = false` or an `agents` list
  that omits their agent), and messages newer than the last index refresh.

Lexical row cardinality is unchanged: substring, regex, and FTS still return one
row per matching source row with the same snippets, and the range and lineage
fields are additive metadata on each row.

In the `MATCH` column, human output renders a multi-message unit as
`#<start>-<end> @<anchor>` and marks subordinate hits with `sub`; both can
appear in any mode:

```text
ID      MATCH           AGE  SCORE  PROJECT  LOCATION  SNIPPET
def456  #12-40 @19 sub  3d   0.71   myapp    message   ...decided to key runs on the first member so tail growth is cheap...
```

### Scoping results: `--scope`

`--scope top|all|subordinate` (or `scope` in HTTP and MCP) controls whether
subordinate content, meaning sidechain runs and subagent or fork sessions,
appears in semantic and hybrid results:

- `all` (default): Everything is searchable. Subordinate hits rank below
  top-level hits of similar relevance and are annotated rather than hidden.
- `top`: Only top-level, human-driven conversation. Use this when you are
  reconstructing decisions, because delegated sessions repeat their parent's
  instructions and can drown out the conversation where the decision was made.
- `subordinate`: Only sidechain and delegated content, for example to find what
  a subagent did.

`--scope` is valid only with `--semantic` or `--hybrid` (other CLI modes reject
it) and replaces `--include-children` there: child sessions are always visible
to these modes so `scope` alone governs what you see. Subagent/fork-typed and
parent-linked sessions are also exempt from the default one-shot exclusion in
these modes, because a subagent session structurally has exactly one "user" message (its
task prompt) and the one-shot gate would otherwise hide nearly all of them. CLI
substring, regex, and FTS searches keep their existing `--include-children` and
one-shot behavior. HTTP and MCP substring and regex searches also accept `scope`
and exempt child sessions from the one-shot exclusion when `scope` is explicit.
MCP defaults `scope` to `all`; HTTP leaves `scope` blank unless you request it.

### MCP all-terms exchange search

The MCP `search_content` tool also supports a `terms` mode for literal
multi-term recall without an embedding query. It splits `pattern` on whitespace
and requires every term to appear within one exchange, meaning a user message
plus the assistant run that follows it on the same main or sidechain branch.
Terms can appear in different messages. Tool and system content does not
participate, and `%`, `_`, and backslashes are treated as ordinary characters
rather than wildcards.

Terms results appear with top-level exchanges first, then newest session
activity, with stable session and ordinal tie breaks. `scope=top`, `all`, or
`subordinate` applies before the final limit. Exact `session_id`, raw
`git_branch`, project, agent, and UTC date filters use the same SQLite and
PostgreSQL session scope as semantic and hybrid retrieval.

When you run a recall request from a known running conversation, pass its full
ID as `current_session_id`. That excludes the current session before the limit
and disables the broader ten-minute activity guard, so an unrelated recent
session stays searchable. Limits default to 10 and go up to 50; any value
outside that range falls back to the default. Responses report the effective
mode, the effective scope, and which default exclusions applied, and include
`next_cursor` when another page exists.

### MCP multi-concept semantic search

When you need to find a single session that covers several distinct ideas, pass
`concepts` with two to five natural-language strings and omit `pattern`.
A `concepts` search always runs in `semantic` mode, no matter what you set
`mode` to. The HTTP API accepts the same list as repeated `concepts` query
parameters and requires the `X-AgentsView-Search-Intent: semantic` header.

AgentsView runs one semantic search per concept and keeps only the sessions
found by every search, even if the concepts match different exchanges. A
session's score is the mean of its best score for each concept, and equal means
keep the order of the per-concept rankings. Every result includes
`concept_evidence` with the supporting snippet, score, and conversation-unit
range for each concept.

Project, branch, agent, session, date, scope, one-shot, automated-session, and
current-session filters apply to every concept search before the intersection.
Each concept search considers five times the limit, up to 250 candidates. When
filters leave fewer candidates than that, the concept search fetches again with
twice as many vector candidates, up to the same 4096-chunk cap described under
[Limitations](#limitations). A session that one concept ranks outside its window
is dropped from the intersection. The response sets `candidate_truncated`
whenever a window fills, the cap stops a concept search before the vector index
runs out, or more sessions match than the limit.

### Inline context: `--context N`

```bash
agentsview session search "database connection pooling" --semantic --context 2
```

Every match includes `N` messages of context before and after it in the same
response: `context_before` and `context_after` arrays in JSON, or a match line
with `score=` inline and indented `role: content` lines around it in human
output. This works in every search mode and costs one extra windowed query per
hit. Values above 10 are rejected with `context: maximum is 10` rather than
silently clamped. Context messages have secrets redacted by default, governed
by `--reveal` like the match snippet itself.

## Cursor-follow: from a hit to its surrounding conversation

Every content-search match, in any mode, returns a `(session_id, ordinal)`
cursor. Use `session messages --around` to fetch a window of messages around
that ordinal without re-running the search:

```bash
agentsview session messages <session-id> --around 42 --before 5 --after 5
agentsview session messages <session-id> --around 42 --role user,assistant
```

- `--around <ordinal>` centers a window on that message. `--before` and
  `--after` default to 5 and require `--around`. `--around` cannot be combined
  with `--from` or `--direction`.
- `--role` filters to a comma-separated list of roles, for example
  `user,assistant`. With a role filter, `--before` and `--after` count
  *filtered* messages rather than raw ordinals, and the anchor message is always
  included regardless of its role.
- The response reports the window's first and last ordinals so you can keep
  paging forward with
  `agentsview session messages <id> --from <last+1> --role user,assistant` to
  walk the rest of the session's user and assistant history. There is no
  unpaginated "give me everything" mode.
- `--before` and `--after` are clamped so the total window never exceeds the
  server's message-page limit (1000 messages); oversized requests are silently
  capped rather than rejected.

A typical workflow is to run `session search --semantic "<query>"`, take the
`session_id` and `ordinal` from a hit, and run
`session messages <session-id> --around <ordinal>` to read what led up to it and
what followed. For a hit whose unit spans a multi-message run, `ordinal` is the
anchor, meaning the member the matched text belongs to, so centering `--around`
on it lands in the right part of the run. Widen `--before` and `--after` toward
the ends of `ordinal_range` to read the whole stretch.

## Browser setup

On a local SQLite daemon, selecting **Turn on** in **Settings → Embeddings** or
the search palette enables semantic search, installs missing shared server
files, and starts indexing. Replica servers have no local setup control.

`GET /api/v1/embeddings/status` includes `setup` when browser setup is
available. `POST /api/v1/embeddings/setup` accepts `approved_bytes`, the
download size disclosed to the user. It returns `202` with `{"started":true}`
when setup is accepted, or `409` if another setup is already running.

Approval covers at most `approved_bytes`. If installation verifies that a larger
download is needed, it declines the download and reports the verified size in
`setup.missing_bytes`. A retry must disclose and approve that amount.

## Error taxonomy

CLI and MCP hybrid searches turn unavailable-index errors into keyword matches
with a note. Semantic searches and ordinary HTTP or UI hybrid requests return
the errors below.

| Situation                                                                                                       | Message                                                                                                                                                               |
| --------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `[vector]` not enabled                                                                                          | `vector search is not enabled: run 'agentsview index embeddings build' or turn it on in Settings → Embeddings → Turn on` (from `agentsview index embeddings ...`)     |
| No `VectorSearcher` wired (index never built, or a PG or ClickHouse replica with no matching pushed generation) | `semantic search not available: run 'agentsview index embeddings build' or turn it on in Settings → Embeddings → Turn on`                                             |
| Only a building generation exists                                                                               | same message, plus `: index is building: N% complete`                                                                                                                 |
| Active generation's fingerprint no longer matches config (model, dimension, or chunking changed)                | same message, plus `: index is stale (embedding config changed): run 'agentsview index embeddings build --full-rebuild'`                                              |
| Index was built by an incompatible agentsview version (mirror schema mismatch)                                  | same message, plus `` : vector index was built by an incompatible version: run `agentsview index embeddings build` ``                                                 |
| `--scope` with a lexical mode (or without `--semantic`/`--hybrid`)                                              | CLI: `--scope requires --semantic or --hybrid`; HTTP and MCP accept scope with substring, regex, terms, semantic, and hybrid                                          |
| An unattended search or build needs a first local download                                                      | `local embeddings: run agentsview index embeddings build to approve the first download`; run `agentsview index embeddings build` in the CLI and approve the download. |
| Embeddings endpoint unreachable                                                                                 | `embed request failed`                                                                                                                                                |
| Embeddings request timed out                                                                                    | `embed request timed out`; see [Transport trade-offs](#transport-trade-offs)                                           |
| Embeddings endpoint returned non-200                                                                            | `embed endpoint returned <code> (<reason>)`                                                                                                                           |
| Embeddings endpoint returned a non-finite or zero-norm vector                                                   | `embed vector <n>: component <i> is not finite` or `embed vector <n>: has zero norm`; correct the endpoint/cache configuration, then run `agentsview index embeddings build --repair-invalid`    |
| `pg push` reports a stored vector has zero norm                                                                | Run `agentsview index embeddings build --repair-invalid` on the source, then retry the push.                                                                       |
| Embeddings endpoint returned a JSON `null` component                                                            | `embed vector <n>: component <i> is null`; correct the endpoint/cache configuration, then run the targeted repair                        |
| `--in` names a source other than `messages` with `--semantic`/`--hybrid`                                        | CLI: `--semantic searches messages only; drop --in` (or `--hybrid ...`); HTTP/MCP: `search: semantic search only supports the messages source (got "...")`            |
| `--cursor` with `--semantic`/`--hybrid`                                                                         | `semantic search returns a single ranked page; cursor pagination is not supported`                                                                                    |

MCP `search_content` in hybrid mode and CLI `session search --hybrid` return
keyword matches when semantic search is unavailable. The response includes
`effective_mode: hybrid_keyword` and a `note` explaining why. Semantic-only
requests and HTTP requests without `allow_keyword_fallback=true` still return
HTTP `501 Not Implemented` or the corresponding MCP tool error with remediation
text.

## PostgreSQL

The `--pg` read path and `agentsview replica pg serve` support semantic and
hybrid search backed by [pgvector](https://github.com/pgvector/pgvector), so a
shared PostgreSQL deployment answers `--semantic` and `--hybrid` queries the
same way a local SQLite index does. [ClickHouse](#clickhouse) provides the same
behavior with its own tables.

### Pushing embeddings

`agentsview replica pg push` runs a vector phase after the session and message
phases, copying the machine's active generation from the local `vectors.db`
mirror into PostgreSQL as per-generation `halfvec` chunk tables. It pushes only
the active generation and re-sends only sessions whose document set changed
since the last push, matching incremental session push. Running
`pg push --full` bypasses change detection and re-sends every session's vectors.
The `pg push` summary reports the phase as
`Vectors: N session(s) pushed, N unchanged, ... docs, ... chunks`. When the
phase skips for a reason, such as pgvector being unavailable or too old, or no
active local generation, the summary prints `Vectors: skipped (<reason>)`. When
`--no-vectors`, `push_vectors = false`, or a disabled `[vector]` turns the phase
off, the summary prints no `Vectors` line.

To skip the vector phase for one run, pass `--no-vectors`. To turn it off
persistently, configure `[pg]` in `config.toml`:

```toml
[pg]
push_vectors = false
```

`push_vectors` defaults to `true`, so a machine with `[vector]` enabled pushes
its embeddings automatically. A machine without `[vector]` enabled has no
generation to push and skips the phase regardless.

### Serving semantic search

`pg serve` and every `--pg` direct-read command wire up PostgreSQL-backed
semantic search at startup when three conditions hold on the serving host:

1. `[vector]` is enabled in that host's `config.toml`.
1. The host's embedding config fingerprint (model, dimension, chunking, and
   prompt affixes) matches a generation already pushed to PostgreSQL. Because
   the fingerprint is immutable, a startup match cannot go stale while the
   process runs; changing the local embeddings config changes the fingerprint
   and requires a restart to take effect.
1. The configured embeddings server is reachable from the serving host, because
   query text is embedded at search time with the same encoder that built the
   index.

Only a machine with `[vector]` configured publishes the recipe (model,
dimension, chunking, affixes, and `request_dimensions`) alongside its
generation, and only when the configured recipe reproduces that generation's
fingerprint. Published recipes stay unchanged. An adopting host pushes vectors
without publishing the adopted recipe.

A serving host without `[vector]` can adopt a published recipe instead. Under
[`AGENTSVIEW_MODE`](/docs/configuration/#container-deployment), set
`AGENTSVIEW_EMBEDDINGS_ENDPOINT` to an embeddings server that serves the
published model. If the endpoint needs a key, also set either
`AGENTSVIEW_EMBEDDINGS_API_KEY` or `AGENTSVIEW_EMBEDDINGS_API_KEY_FILE`, never
both. `pg serve` and `--pg` reads then use the single published recipe whose
parameters rebuild its fingerprint. You cannot combine a `[vector]` section in
`config.toml` with the `AGENTSVIEW_EMBEDDINGS_*` variables; startup stops if
both are present. If PostgreSQL holds several published recipes, adoption
refuses to guess and semantic search reports the count. To resolve that, stop
the hub's `pg push --watch --embed`, remove obsolete generations with
`agentsview replica pg vectors drop <id>`, then start the watcher and restart
`pg serve`. Generations pushed before recipe publication have no recipe until a
configured machine pushes them again. If `pg serve` starts before a workstation
pushes its first generation, restart `pg serve` after that push to enable
semantic search.

If no generation matches, `pg serve` still starts normally. Semantic search
returns the 501 "not available" error, and CLI and MCP hybrid searches return
keyword matches with the mismatch reason in a note. That reason lists the
fingerprints PostgreSQL does have so you can tell a "wrong config" miss from a
"never pushed" one. A missing pgvector extension or missing vector tables
degrade the same way; see
[Backends without pgvector](#backends-without-pgvector).

### Shared generations across machines

Because a generation is keyed by its config fingerprint, every machine with an
identical `[vector.embeddings]` config pushes into the *same* PostgreSQL
generation. Their documents and chunks accumulate side by side, and any serving
host with the matching fingerprint searches the union. Coverage is partial by
construction: a session becomes semantically searchable only after the machine
that owns it pushes its embeddings, so a freshly pushed session's text can match
lexically through the hybrid keyword leg before its vectors arrive.

In memory status and search responses, PostgreSQL semantic coverage reports
`ready` with `counts_unavailable`, omits `embedded` and `missing`, and cannot
report `partial` because it cannot measure source sessions that have not been
pushed.

### Storage and indexing

PostgreSQL stores embeddings in pgvector `halfvec` (16-bit) columns, with one
chunk table per generation and an HNSW cosine index on each table. `halfvec`
halves storage compared with 32-bit `vector` and stays under HNSW's dimension
ceiling, so models up to 2560 dimensions index cleanly where plain `vector`
cannot. Per-generation tables are required because a pgvector column has a fixed
typed dimension.

### Backends without pgvector

Semantic search degrades cleanly when the extension is absent or too old.
`pg push` best-effort runs `CREATE EXTENSION IF NOT EXISTS vector`. If that
fails, for example on CockroachDB (which has no pgvector), on a database where
the extension package is not installed, or with a role lacking `CREATE`
privilege, schema setup logs a one-line notice, continues, and skips the vector
phase. Semantic search needs pgvector 0.8.0 or newer. Because
`CREATE EXTENSION IF NOT EXISTS` never upgrades an existing extension object,
when an older version is installed `pg push` attempts
`ALTER EXTENSION vector UPDATE` (healing servers whose pgvector package was
upgraded in place) and otherwise skips the vector phase while reporting the
installed version. Existing indexes on pgvector 0.7.x resume semantic search
after you upgrade pgvector, and you do not need to rebuild them. Session and
message sync are unaffected in every case. On the read side, if the extension
is unsupported or the vector table is missing, semantic search is unavailable.
`pg serve` still starts, semantic search returns 501, CLI and MCP hybrid
searches return keyword matches with a note, and lexical search keeps working.

### Maintenance

`agentsview replica pg vectors list` prints every generation with its model,
dimension, document and chunk counts, contributing machines, and creation time.
To reclaim space after retiring an embedding model, run
`agentsview replica pg vectors drop <id>` to remove a generation and all of its
embeddings (it prompts unless you pass `--yes`). Both commands accept `--target`
to pick a non-default PostgreSQL target.

### Hybrid keyword leg

Both backends fuse the same two legs with the same reciprocal-rank merge, but
their keyword legs rank differently. For natural-language queries, both backends
rank keyword matches by how many query words a message matches first. SQLite
then orders by BM25 through FTS5, while PostgreSQL orders by recency (newest
first). Explicit FTS expressions on PostgreSQL, and all queries on CockroachDB,
use an `ILIKE` scan ordered by recency. The vector leg, RRF fusion,
subordinate-unit penalty, scope filtering, and hit anchoring are identical, so
top results usually agree, though the keyword-leg input order and fusion ties
broken by keyword rank can differ between the backends.

## ClickHouse

`agentsview replica clickhouse serve` supports semantic and hybrid search the
same way `pg serve` does. For step-by-step setup with the output that confirms
each step, see
[ClickHouse sync: Enabling Semantic Search](/docs/clickhouse-sync/#enabling-semantic-search).
`clickhouse push` runs a vector phase after the session phase when `[vector]` is
enabled locally and the target has not set `push_vectors = false`; pass
`--no-vectors` to skip it for one run. The phase copies the machine's active
embedding generation into four ClickHouse tables (`vector_generations`,
`vector_documents`, `vector_chunks`, and `vector_push_state`), keyed by the
generation's config fingerprint and by the pushing archive, so several machines
can share one generation while each keeps its own delta state. Only changed
sessions are re-sent, and a session's vectors are evicted when its session row
leaves the mirror and no other archive still records them. A change-scoped watch
push widens to the whole generation until the pushing archive has recorded one
clean generation-wide pass.

On startup, `clickhouse serve` looks for a pushed generation whose fingerprint
matches the local `[vector.embeddings]` config. A match installs the searcher,
while a miss starts the server normally. Semantic search returns the 501 "not
available" error, and CLI and MCP hybrid searches return keyword matches with a
note listing the fingerprints the mirror does have. Ranking uses an exact cosine
scan over the generation's chunks (`cosineDistance`), so results match the local
SQLite index rather than an approximate index. As on PostgreSQL,
natural-language queries in the hybrid keyword leg rank by matched-word count
and then recency; other queries use the mirror's recency-ordered term match.
`pg vectors list` and `drop` have no ClickHouse counterpart yet.

## Limitations

- **Metadata filters post-filter the vector leg.** `--semantic` and `--hybrid`
  over-fetch candidates from the vector index (4x the requested limit, or a
  fixed minimum if that is larger), drop hits whose session fails filters such
  as `--project`, `--agent`, or `--date*`, and then truncate to the requested
  limit. When the recent-activity cutoff rejects otherwise eligible hits,
  semantic and hybrid searches fetch again with twice as many candidates. They
  start at `max(4 * limit, 200)` and stop at 4096 chunks on every store, which
  allows at most six fetches. Multi-concept searches expand the same way
  whenever filters leave a concept short. Other queries use a single candidate
  batch. Narrow filters or scope can still return fewer than `--limit` results
  when eligible matches lie beyond that budget. The hybrid keyword leg keeps
  its own bounded search budget.
- **Legacy no-`source_uuid` rows re-embed on ordinal shifts.** Each embedded
  document is keyed by its first message's stable per-message UUID when the
  parser recorded one, or by `(session_id, ordinal)` when it did not.
  UUID-keyed documents survive ordinal renumbering, for example from a resync,
  as a cheap metadata update with no re-embed. Ordinal-keyed documents are
  treated as new and re-embedded when their ordinal shifts. This affects only
  older parsed data that predates per-message UUIDs and is an accepted cost
  rather than a bug.
- **Only the last run piece re-embeds as it grows.** Assistant runs close after
  the message that reaches `4 × max_input_chars` runes. An append re-embeds
  only the trailing piece, bounded by that cap plus one message, and earlier
  pieces keep their vectors.
- **The index embeds message `content` verbatim.** Like `--fts`, it reads only
  from the `messages` source, so raw `tool_input` and `tool_result` rows are
  never candidates. System messages are handled more strictly: `--fts` includes
  them unless you pass `--exclude-system`, whereas `--semantic` and `--hybrid`
  always exclude system messages from the index with no flag to opt back in.
  Anything a parser rendered *into* a user or assistant message's content is
  embedded with it, including thinking text flattened inline as
  `[Thinking]...[/Thinking]` markers and tool-call summaries that some parsers
  render into assistant content. Run documents concatenate that per-message
  text unchanged, with no role labels or markers injected between members.

## Skills for coding agents

`agentsview skills install` writes a proactive conversation-recall skill. Agents
consult history when prior decisions, rationale, solutions, pitfalls, or project
context may help, when stuck, and before guessing about something learned
previously. The skill starts with the registered `search_content` MCP tool,
reads the top sources with `get_messages`, follows continuation, and
distinguishes snippets and summaries from messages read in detail. The runtime
supplies the client-specific MCP tool prefix. To also register the MCP server,
see [Choose an installation route](/docs/mcp/#choose-an-installation-route).

Claude Code also receives an `agentsview-search-conversations` agent that runs
the bounded search and returns Summary, Sources, and For Follow-Up. Its `tools`
frontmatter allowlists only `mcp__agentsview__search_content` and
`mcp__agentsview__get_messages`. Because Claude Code treats that list as the
agent's complete tool set, built-in tools and tools from every other MCP server
are unavailable to it. Register the AgentsView server as `agentsview`, the name
used in the [MCP quick start](/docs/mcp/#quick-start). If you register the
server under another name, it does not match the allowlist, the agent does not
start, and the skill runs the same MCP workflow directly. Codex and other
`.agents/skills` readers run that workflow directly when they do not have a
permitted search agent. The generated skill keeps CLI examples as a secondary
fallback and preserves baked remote-server targeting across reinstalls.

```bash
agentsview skills install                    # both harnesses, user level
agentsview skills install --harness claude   # one harness only
agentsview skills install --project          # install under the current git root
agentsview skills install --server URL       # bake remote-daemon flags into examples
agentsview skills list                       # show install state per artifact
```

| `--harness` | Artifact     | Target                                                                                                                     |
| ----------- | ------------ | -------------------------------------------------------------------------------------------------------------------------- |
| `claude`    | skill        | `~/.claude/skills/agentsview-finding-history/SKILL.md`                                                                     |
| `claude`    | license      | `~/.claude/skills/agentsview-finding-history/LICENSE`                                                                      |
| `claude`    | search-agent | `~/.claude/agents/agentsview-search-conversations.md`                                                                      |
| `agents`    | skill        | `$HOME/.agents/skills/agentsview-finding-history/SKILL.md` — the open convention Codex reads (per Codex's own skills docs) |
| `agents`    | license      | `$HOME/.agents/skills/agentsview-finding-history/LICENSE`                                                                  |

`--project` changes the base from your home directory to the current git root
(or the working directory outside a repo), writing to `.claude/skills/...` and
`.agents/skills/...` instead.

Every rendered artifact includes a `generated-by` header with a content hash.
Skill and agent files place that comment right inside the frontmatter fence so
the file still starts with `---` and harnesses keep discovering it, and the MIT
sidecar puts the same header on line one. `install` classifies and protects each
file independently, so one refused local edit does not block safe package files
from installing. Pass `--force` to overwrite modified or foreign files. After
upgrading `agentsview`, re-run `agentsview skills install`; the content hash,
rather than the displayed version, decides whether each artifact is current.

`agentsview skills list [--project] [--format json]` reports each artifact's
harness, kind, path, and install state (`missing`, `current`, `stale` for
unmodified files older than the current render, `modified`, or `foreign` for
files with no header) without writing anything.

The recall text is adapted from `obra/episodic-memory` at pinned commit
`7e06519357777badd7a115d2014a7ef845904310` under the MIT License; see the
[adaptation record](https://github.com/kenn-io/agentsview/blob/main/docs/internal/episodic-memory-adaptation.md).
The native AgentsView Memory package bundles these recall artifacts with the
[focused MCP profile](/docs/mcp/#focused-memory-profile), a SessionStart
lifecycle hook, and readiness diagnostics through `agentsview doctor memory`.
Use one installation route per client to avoid duplicate skills.
