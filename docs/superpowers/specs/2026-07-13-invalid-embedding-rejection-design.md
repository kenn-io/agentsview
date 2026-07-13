# Invalid embedding rejection and recovery

## Problem

An OpenAI-compatible embeddings server can return a vector with the configured
dimension while its components are non-finite or its norm is zero. This was
observed when a long-running `llama-server` reused saturated prompt-cache state:
the JSON float form contained `null` values corresponding to non-finite floats,
while the base64 form carried the raw invalid float values. AgentsView currently
checks only response count, indexes, and dimensions, so the invalid vector can
reach sqlite-vec. Cosine distance against the resulting unusable vector is
undefined, and semantic search can fail while scanning a `NULL` distance.

Faulty embedding data must never be written or stamped as complete. A build may
stop and leave work pending, but it must not repair, substitute, skip-stamp, or
otherwise hide an invalid vector.

## Goals

- Reject embeddings containing `NaN`, positive infinity, or negative infinity.
- Reject zero-norm embeddings because cosine distance is undefined for them.
- Apply validation before any build path can persist a vector.
- Retry malformed endpoint responses according to the existing server retry
  configuration, without persisting any part of the failed response batch.
- If retries are exhausted, abort safely with the affected document pending and
  without a vector or completion stamp.
- Document cache-safe Ollama and direct `llama-server` operation for embeddings.
- Recover a corrupted local generation through the supported transactional full
  rebuild path and verify semantic search afterward.

## Non-goals

- Do not normalize, truncate, clamp, or otherwise repair endpoint output.
- Do not alter input text with a nonce, newline, suffix, or other cache buster.
- Do not add a second embedding-result cache to AgentsView.
- Do not change the generic vector library's encode-error callback in this
  change. Its continue behavior intentionally skip-stamps a permanently
  rejected document, which is not valid for malformed embedding output.
- Do not modify or recreate the persistent session archive.

## Design

### Shared validation

The vector package will define one shared validator for a batch of embeddings.
For every vector it will:

1. identify the response index so errors point to the affected item;
1. reject any component for which `math.IsNaN` or `math.IsInf` is true;
1. accumulate the squared norm in `float64`; and
1. reject a norm of zero.

The validator will not require unit normalization. Different embedding services
may return valid vectors with different magnitudes, and sqlite-vec's cosine
metric handles any finite, non-zero vector.

Validation will run at two owned boundaries:

- `reorderAndValidate` will validate decoded OpenAI-compatible responses after
  count, index, and dimension checks. This catches malformed float and base64
  responses early enough for the HTTP encoder to classify them for retry.
- `Index.Build` will wrap the supplied encoder with the same validator. This is
  the persistence safety boundary and prevents a future or alternate encoder
  from bypassing the HTTP client's checks.

Query embeddings use the same OpenAI-compatible encoder, so malformed query
vectors will fail before sqlite-vec is queried.

### Error and retry behavior

Invalid vector output will use a typed error that reports the response index and
whether the failure was a non-finite component or a zero norm. The HTTP encoder
will treat this response-shape failure as retryable. Each failed attempt returns
no vectors, so a partly valid batch is never exposed to the caller.

After the configured attempts are exhausted, the encoder returns the typed
error. The build path will not classify it as a permanent, document-specific
rejection. Therefore the generic fill operation aborts without calling
`SaveVectors` for that document, leaving it pending with no stamp. This is
deliberately safer than the vector library's continue action, which stamps a
permanently rejected document without vectors.

### Ollama documentation

The semantic-search guide will retain the ordinary Ollama `/v1/embeddings`
quickstart and add an explicit section for high-throughput deployments that run
Ollama's bundled `llama-server` directly.

The direct-server example will disable prompt caching with:

```text
--cache-ram 0 --no-cache-prompt --no-cache-idle-slots
```

The guide will explain that the prompt cache stores model inference state, not a
reusable final embedding API response. Reusing bad or saturated cache state can
produce non-finite output for an otherwise valid input. Embedding-only servers
should disable that cache, since independent document embeddings do not benefit
from conversational prefix reuse.

The error taxonomy will include non-finite and zero-norm response failures and
will direct operators to correct the endpoint configuration before running a
full rebuild.

### Local recovery

The existing embedding LaunchAgent will be updated to pass the cache-disabling
flags, then only that embedding service will be restarted. A document known to
have produced an invalid vector will be sent to the restarted endpoint and its
returned vector will be checked for the configured dimension, finite components,
and non-zero norm.

Recovery will use:

```text
agentsview embeddings build --full-rebuild --yes
```

For an unchanged active fingerprint, the existing full-rebuild implementation
transactionally clears that generation's vec0 rows, chunk map, and stamps, then
refills it. This removes both known and undiscovered invalid vectors without
ad-hoc SQL against a database owned by the daemon. It does not touch the session
archive.

After completion, verification will require:

- the active generation reports no missing documents;
- a known formerly invalid document has a finite, non-zero vector;
- a nearest-neighbor query produces no `NULL` distances; and
- semantic search returns successfully.

## Tests

Focused Go tests will cover behavior owned by AgentsView:

- table-driven base64 responses containing `NaN`, positive infinity, negative
  infinity, and an all-zero vector are rejected with no returned vectors;
- an invalid response followed by a valid response succeeds through the existing
  retry mechanism;
- a custom build encoder returning an invalid vector cannot create either a
  chunk vector or a generation stamp; and
- valid finite, non-zero vectors continue through the existing response and
  build paths.

The infrastructure configuration itself will be verified by running the local
service and probing its endpoint, not by a test that reads the LaunchAgent file.

## Operational trade-offs

An exhausted invalid-vector retry aborts the current fill, so later documents
wait for the next build. Continuing while leaving only the failed document
pending would require a new tri-state error action in the generic vector
library; its current boolean callback can only abort or stamp the document as
skipped. Safe abort preserves the stronger data-integrity invariant without
expanding this repair across another repository and release.
