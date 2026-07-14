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
- Repair a corrupted local generation by invalidating and regenerating only
  documents that contain an invalid stored chunk or a partial chunk mapping.

## Non-goals

- Do not normalize, truncate, clamp, or otherwise repair endpoint output.
- Do not alter input text with a nonce, newline, suffix, or other cache buster.
- Do not add a second embedding-result cache to AgentsView.
- Do not change the generic vector library's encode-error callback in this
  change. Its continue behavior intentionally skip-stamps a permanently
  rejected document, which is not valid for malformed embedding output.
- Do not classify a stamped document with no chunk mappings as corrupt. Ordinary
  builds intentionally use that representation when an endpoint permanently
  rejects a document, so absence alone is ambiguous.
- Do not clean raw vector rows that have no chunk mapping. They cannot surface
  as search hits and are not attributable to a document through the repair
  scan.
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

JSON float arrays need one additional decode-time guard. Go's JSON decoder maps
a `null` numeric-array element to the float type's zero value without returning
an error, which would hide a mixed response such as `[0.5, null]` from the
post-decode finite check. `embeddingVector.UnmarshalJSON` will inspect
individual array elements and reject `null` before converting them to `float32`.
Base64 responses retain their raw non-finite bit patterns and are handled by the
shared validator.

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
will direct operators to correct the endpoint configuration before repairing the
affected stored vectors.

### Targeted stored-vector repair

`agentsview embeddings build` will gain an explicit `--repair-invalid` option.
It targets only the existing generation whose fingerprint matches the current
embedding configuration and fails without writing if that generation does not
exist. It does not refresh the document mirror, resolve or create a broader
build target, or change generation state. The scan applies the same finite and
non-zero validation used for new endpoint responses to each mapped sqlite-vec
float32 blob. Malformed byte length, wrong dimension, a missing referenced
vector row, or duplicate, missing, or unexpected chunk indices are also invalid
stored data. Only documents whose generation stamp matches the mirror's current
content revision participate in integrity detection; stale stamps identify
ordinary pending content changes and repair must not broaden into that work.
Durable queue entries remain independent of this scan gate so interrupted
repairs still resume. Once a document is queued, the queue owns it across
revision changes: a stale save writes nothing and leaves the target queued, and
the next repair attempt reads the latest mirrored content. This does not claim
unrelated pending documents because only keys already identified as corrupt
receive that durable ownership.

The structural invariant is exact: when at least one mapping exists, its sorted
chunk indices must equal `kitvec.Split(content, options)` and every mapping must
reference a valid vector. The zero-mapping exception in the non-goals remains
intentional because it represents ordinary permanent-error skip stamps.

If any chunk is invalid, the whole document must be regenerated because the
generation stamp covers the document and `SaveVectors` replaces all of its
chunks together. The scan keyset-pages through bounded document groups. After
each group's read cursor closes, one bounded transaction durably queues its
affected keys before removing only those documents' rows from the target
generation's vec0 table, their target-generation chunk-map rows, and their
target-generation stamps. Other documents and other generations remain
unchanged. Committing per group keeps both memory and transaction size bounded;
the durable queue preserves already-invalidated work if a later group or refill
fails.

The refill store exposes only queued keys. It removes a key only after a
non-empty replacement save succeeds, except when the current queued revision
deterministically splits into zero chunks; that case completes with the valid
empty-document stamp. It treats the queue—not the generation stamp—as the
authority so a failure between saving vectors and deleting the key is retried
safely. Repair does not use the ordinary fill's permanent-error skip callback,
and the store rejects every other empty save as defense in depth. A permanent
rejection inside the repair fill remains queued and unstamped, while a keyset
cursor lets the invocation attempt every later target once before returning an
aggregate error. Retryable endpoint or system errors, timeouts, and cancellation
abort the current invocation instead of continuing against a failing service. An
ordinary build that saves non-empty validated vectors for the queued generation
clears the same target. Its ordinary permanent rejection may leave a stamp-only
record, but the target remains queued; a repair-fill permanent rejection writes
no stamp. A full generation rebuild also preserves queued targets before refill,
so its own empty skip stamp cannot erase repair ownership. Encode or save
failures leave a target unstamped, while a failure after a valid save but before
queue cleanup may leave a valid stamp and is retried because the queue remains
authoritative. Success means the integrity scan finished and the durable queue
is empty. Mirror deletion removes queued keys for documents that no longer
exist, using a document-key-first index so cleanup cost does not scale with the
total queue.

Every non-empty ordinary save performs one repair-queue completion delete for
its generation and document. That lookup uses the queue's `(ordinal, doc_key)`
primary key even when no target exists; a query-plan regression pins the bounded
cost.

The integrity scan is opt-in rather than part of every scheduled incremental
build. It necessarily scales with the stored generation, while background sync
work must remain bounded by changed input. Build results and CLI output report
the target count (newly detected plus resumed queue entries) and mappings
invalidated by this scan separately from the fill's successful replacement
count. `Scanned` means the integrity pass began after resolving its generation;
`ScanComplete` means it traversed every bounded batch. A later-batch failure
returns the accumulated committed target counts and a best-effort durable queue
count. The committed count is the fallback, and the recount uses a short timeout
detached from build cancellation so cancellation cannot turn known work into a
misleading zero. `RemainingKnown` distinguishes an exact recount from that
fallback; when false, CLI output says the remaining count is unknown rather than
presenting the fallback as exact. `Failed` counts non-stale target encode/save
failures that determine or accompany the invocation failure. Stale saves remain
in `Fill.Stale`, sibling work canceled after another failure is not counted, and
scan failures are represented by `ScanComplete == false`. Failed builds retain
and expose this partial result before returning a nonzero status.

These repair status semantics bump the local daemon API from version 2 to 3. The
CLI rejects an older daemon instead of interpreting absent fields as false; an
explicitly managed daemon must be restarted after upgrading AgentsView.

### Local recovery

The existing embedding LaunchAgent will be updated to pass the cache-disabling
flags, then only that embedding service will be restarted. A document known to
have produced an invalid vector will be sent to the restarted endpoint and its
returned vector will be checked for the configured dimension, finite components,
and non-zero norm.

Recovery will use:

```text
agentsview embeddings build --repair-invalid
```

The command will scan the configured existing generation through the daemon,
remove only revision-current documents containing an invalid mapped vector or a
partial nonempty mapping, and run the queue-restricted repair fill. This avoids
ad-hoc SQL against a database owned by the daemon, does not recompute valid
embeddings, and does not touch the session archive.

After completion, verification will require:

- the active generation reports no missing documents;
- a known formerly invalid document has a finite, non-zero vector;
- a nearest-neighbor query produces no `NULL` distances; and
- semantic search returns successfully.

## Tests

Focused Go tests will cover behavior owned by AgentsView:

- table-driven base64 responses containing `NaN`, positive infinity, negative
  infinity, and an all-zero vector are rejected with no returned vectors;
- JSON float responses containing either a mixed `null` element or only `null`
  elements fail during embedding-vector decoding rather than being converted
  to zeros;
- an invalid response followed by a valid response succeeds through the existing
  retry mechanism;
- a custom build encoder returning an invalid vector cannot create either a
  chunk vector or a generation stamp;
- targeted repair invalidates every chunk and the stamp for a document with one
  bad chunk, while preserving valid documents and every other generation;
- repair detects a missing vector row and a partial chunk mapping;
- queued work remains visible if completion cleanup fails after a valid save,
  then a retry replaces it and drains the queue;
- a content change that alters chunk layout remains ordinary pending work and is
  not claimed by repair;
- a revision change after a target is queued causes the stale save to write
  nothing, retains queue ownership, and repairs the latest mirrored revision
  on retry;
- a queued revision that becomes zero-chunk content completes with a valid empty
  stamp without invoking the endpoint;
- an ordinary build that saves non-empty vectors for a queued generation clears
  that target, while a stamp-only skip does not;
- a full rebuild preserves queued targets before refill, including when its
  encoder permanently skips one of them;
- failure in a later scan batch leaves the earlier batch durably queued, exposes
  scan-incomplete and remaining counts, and a retry converges;
- a canceled scan retains its committed-count fallback and can recount the queue
  without inheriting the canceled build context;
- a failed detached recount marks the remaining count unknown instead of
  presenting the committed fallback as exact;
- permanent rejection of one target does not prevent later targets from being
  repaired, and the command returns a nonzero aggregate failure with the
  rejected target still queued;
- failed manager, direct CLI, and daemon CLI paths expose the partial result and
  print failed and remaining counts before returning the error;
- mirror deletion clears orphaned queue targets through a document-key index;
- ordinary save completion uses the repair queue's composite primary key even
  when no matching target exists; and
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
