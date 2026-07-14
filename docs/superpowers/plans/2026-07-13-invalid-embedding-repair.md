# Invalid Embedding Repair Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prevent malformed embeddings from being persisted and provide an
opt-in build mode that regenerates only documents whose stored vectors are
invalid.

**Architecture:** A shared validator rejects non-finite and zero-norm vectors at
both the HTTP-response and index-persistence boundaries. The target-generation
repair pass streams stored vectors, durably queues affected document keys,
invalidates only those documents, and refills through a queue-backed store that
cannot see unrelated pending work. Repair bypasses mirror refresh and generation
lifecycle operations entirely.

**Tech Stack:** Go, `database/sql`, sqlite-vec, Cobra, Huma, testify, Markdown.

## Global Constraints

- Never persist or stamp a document whose embedding is malformed, non-finite, or
  zero norm.
- Never normalize, truncate, clamp, substitute, or skip-stamp invalid endpoint
  output.
- Repair only the selected target generation and only revision-current documents
  containing an invalid mapped vector or a partial nonempty mapping.
- Repair must not refresh or expand the mirror, create or reset a generation, or
  change generation state.
- Do not scan for corruption during background builds; repair is explicitly
  opt-in.
- Do not modify or recreate the persistent session archive.
- Preserve existing generation selection, activation, and incremental-fill
  behavior.

______________________________________________________________________

### Task 1: Validate Endpoint and Build Encoder Output

**Files:**

- Modify: `internal/vector/encoder.go`
- Modify: `internal/vector/encoder_test.go`
- Modify: `internal/vector/build.go`
- Modify: `internal/vector/build_test.go`

**Interfaces:**

- Produces: `validateEmbedding([]float32, int) error` and
  `validateEmbeddings([][]float32) error`.

- Produces: `InvalidEmbeddingError`, returned for non-finite components and zero
  norms.

- Consumes: `kitvec.EncodeFunc`; `Index.Build` wraps it before passing it to
  `kitvec.Fill`.

- [x] **Step 1: Add failing endpoint-validation tests**

Add table-driven tests that return base64 vectors containing `NaN`, positive
infinity, negative infinity, and all zeros, and assert `NewEncoder` returns no
vectors plus an `InvalidEmbeddingError`. Add literal JSON response tests for
`[0.5,null]` and `[null,null]`, asserting decoding fails rather than converting
nulls to zero. Add a server that returns invalid output once and valid output
next, asserting two requests and a successful vector.

- [x] **Step 2: Run endpoint tests and confirm the intended failures**

Run:
`go test -tags fts5 ./internal/vector -run 'TestEncoder.*(Invalid|Null|Retries)' -count=1`

Expected: FAIL because invalid vectors are accepted and JSON null elements
decode as zeros.

- [x] **Step 3: Implement shared validation and retry classification**

In `embeddingVector.UnmarshalJSON`, decode JSON arrays as `[]json.RawMessage`,
reject a trimmed element equal to `null`, and unmarshal each remaining element
to `float32`. Define `InvalidEmbeddingError` with response/component context.
Implement validation using `math.IsNaN`, `math.IsInf`, and a `float64` squared
norm. Call it from `reorderAndValidate`, and make validation failures from
`attemptEncode` retryable.

- [x] **Step 4: Add a failing persistence-boundary test**

Build with a custom `kitvec.EncodeFunc` that returns a zero or non-finite
vector. Assert `Index.Build` returns `InvalidEmbeddingError`, and query the
target generation to prove it has no vector chunk and no completion stamp.

- [x] **Step 5: Run the persistence-boundary test and confirm failure**

Run:
`go test -tags fts5 ./internal/vector -run TestBuildRejectsInvalidEncoderOutput -count=1`

Expected: FAIL because the alternate encoder currently reaches `SaveVectors`.

- [x] **Step 6: Wrap every build encoder with validation**

Add a small `validatingEncoder` wrapper in `build.go` that returns no vectors
when `validateEmbeddings` fails, and pass it into the existing progress wrapper
and fill operation.

- [x] **Step 7: Run focused vector tests**

Run:
`go test -tags fts5 ./internal/vector -run 'TestEncoder|TestBuildRejectsInvalidEncoderOutput' -count=1`

Expected: PASS.

### Task 2: Durable Repair Queue Lifecycle

**Files:**

- Modify: `internal/vector/index.go`
- Modify: `internal/vector/mirror.go`
- Modify: `internal/vector/mirror_test.go`
- Create: `internal/vector/repair_test.go`

**Interfaces:**

- Produces: `message_vectors_repair_queue`, keyed by generation ordinal and
  document key, plus a document-key-first cleanup index.

- [x] **Step 1: Create the durable queue with the vector mirror**

Create the queue on writable index open and add the document-key index required
by mirror cleanup. A whole mirror-schema replacement recreates the queue with
the mirror, but a generation full rebuild must preserve queued targets: only a
later ordinary non-empty validated save or a repair-verified zero-chunk save may
complete them.

- [x] **Step 2: Keep queue lifecycle aligned with mirror deletion**

Delete a document's repair targets from both mirror deletion paths and cover the
indexed deletion query plan. Orphan pruning belongs to the detection stage that
owns repair startup; post-save cleanup belongs to the refill stage that owns
completion.

### Task 3: Bounded Integrity Detection and Invalidation

**Files:**

- Create: `internal/vector/repair.go`
- Modify: `internal/vector/repair_test.go`
- Modify: `internal/vector/build.go`

**Interfaces:**

- Produces:
  `(*Index).repairInvalidVectors(context.Context, string) (repairResult, error)`.

- Extends: `BuildOptions.RepairInvalid bool` and
  `BuildResult.Repair RepairStats`.

- Produces scan-owned
  `RepairStats{Scanned, ScanComplete, RemainingKnown bool; Documents, Chunks, Remaining int}`
  plus the bounded queue-count helper. Refill adds `Failed` in Task 4.

- [x] **Step 1: Add failing targeted-detection tests**

Create two documents in a target generation and a second generation, corrupt one
target-generation chunk through sqlite-vec, then invoke the detection and
invalidation pass directly. Assert exactly one document is durably queued, its
target-generation chunks and stamp are removed, the valid document and other
generation are byte-for-byte unaffected, and the result reports one repair
target plus the number of invalidated chunks. Also cover a missing vector row, a
partial nonempty mapping, and a stale-stamp content change that must remain
ordinary pending work. Re-encoding is reserved for Task 4.

- [x] **Step 2: Run the repair test and confirm failure**

Run:
`go test -tags fts5 ./internal/vector -run TestRepairInvalidVectorsQueuesOnlyAffectedDocument -count=1`

Expected: FAIL because `RepairInvalid`, `RepairStats`, and the repair pass do
not exist.

- [x] **Step 3: Implement bounded detection and invalidation**

In `repair.go`, resolve the generation ordinal and vector table, then
keyset-page through bounded groups of documents. Stream each group's chunk
indices and raw `embedding` values, decode little-endian float32 blobs, and
reuse the shared validator. Treat malformed blob lengths, wrong dimensions,
missing referenced vector rows, and duplicate, missing, or unexpected chunk
indices as invalid. Only scan documents whose generation stamp matches the
mirror's current content revision, leaving ordinary pending content changes
untouched. Commit one invalidation transaction per bounded group, queueing each
key before deleting its target-generation vectors, chunk-map rows, and stamp.
Keep queued keys until the canonical completion policy in Task 4 is satisfied,
clear queued keys when their mirror document is deleted, and prune orphans at
repair startup. Count every invalidated chunk without materializing the affected
corpus or creating one corpus-sized transaction.

- [x] **Step 4: Verify interruption between scan batches**

Inject a failure when the second batch queues its first key. Prove the first
batch remains committed and queued and the partial result reports scan-started,
scan-incomplete, and the durable remaining count. Use the committed count as a
fallback and run the best-effort recount under a short context detached from
build cancellation. Remove the failure, rerun the detection scan, and verify it
completes while preserving the durable queue. Queue drainage and repaired-vector
convergence belong to Task 4A.

### Task 4: Restricted Refill and Resumption

**Files:**

- Modify: `internal/vector/repair.go`
- Modify: `internal/vector/repair_test.go`
- Modify: `internal/vector/build.go`

**Interfaces:**

- Produces: queue-backed `repairStore` and `fillRepairQueue`, whose pending and
  progress queries read only durable repair targets.

- Extends the scan-owned statistics with `Failed`; final refill recounts update
  `Remaining` and `RemainingKnown`.

#### Task 4A: Restricted Refill and Failure Handling

- [x] **Step 1: Add failing refill and failure-policy tests**

Cover retryable errors, cancellation, permanent rejection followed by later
targets, configured document concurrency, and failure between a successful
vector save and queue deletion. Encode and save failures remain queued and
unstamped; a completion-cleanup failure may leave a valid stamp but must remain
queued and retryable. Resume a queue preserved by Task 3's interrupted scan and
prove the restricted refill drains it without broadening into unrelated pending
work.

- [x] **Step 2: Invoke the restricted repair fill**

When `BuildOptions.RepairInvalid` is true, bypass mirror refresh and generation
resolution entirely. Use the configured fingerprint only if that generation
already exists, store the scan stats in `BuildResult`, and run a fill through a
store restricted to the affected document keys. Do not create, reset, activate,
or retire any generation. Do not install the ordinary build's permanent-error
skip callback on the repair fill, and make the repair store reject empty saves
unless the current queued revision deterministically splits into zero chunks.
That exception is a valid empty-document completion, not an encoder-error skip.
Keyset-page the repair fill so a permanently rejected target remains queued
without starving later targets; continue to every later target after permanent
document-specific rejection, then return an aggregate error unless the queue is
empty. Retryable endpoint or system errors, timeouts, and cancellation abort the
current invocation. The restricted refill reads only queue membership and never
broadens into unrelated work.

#### Task 4B: Cross-Build Queue Ownership

- [x] **Step 3: Preserve ownership across revisions and ordinary builds**

The queue owns its document across revision changes and a later repair reads the
latest mirrored revision; a stale save writes nothing and leaves the key queued.
An ordinary build clears a target only after a non-empty validated save for that
generation. Its empty permanent-skip stamp may remain, but cannot clear the
target. Full generation reset also preserves the queue. Cover revision drift,
ordinary completion, and a full rebuild whose permanent skip writes only an
empty stamp.

#### Task 4C: Zero-Chunk Completion and Bounded Ordinary Cleanup

- [x] **Step 4: Converge empty revisions without weakening persistence**

Repair may clear a target after a revision-current save only when the current
content deterministically splits into zero chunks. Every other empty repair save
is rejected. Cover a queued nonempty revision becoming zero-chunk content
without calling the encoder. Pin ordinary completion's query plan to the queue's
composite primary key so the additional no-match lookup stays bounded per saved
document.

- [x] **Step 5: Run vector tests**

Run: `go test -tags fts5 ./internal/vector -count=1`

Expected: PASS, including preservation of valid documents and retired
generations.

### Task 5: Expose Repair Through Manager, API, CLI, and Documentation

**Files:**

- Modify: `internal/vector/manager.go`
- Modify: `internal/vector/manager_test.go`
- Modify: `internal/server/huma_routes_embeddings.go`
- Modify: `internal/server/huma_routes_embeddings_test.go`
- Modify: `internal/server/server.go`
- Modify: `internal/server/server_test.go`
- Modify: `cmd/agentsview/embeddings.go`
- Modify: `cmd/agentsview/embeddings_test.go`
- Modify: `cmd/agentsview/daemon_runtime.go`
- Modify: `cmd/agentsview/daemon_runtime_test.go`
- Modify: `docs/semantic-search.md`
- Modify: `frontend/src/lib/api/generated/models/VectorBuildRequest.ts`
- Modify: `frontend/src/lib/api/generated/models/VectorBuildResult.ts`
- Create: `frontend/src/lib/api/generated/models/VectorRepairStats.ts`

**Interfaces:**

- Extends:
  `BuildRequest.RepairInvalid bool \`json:"repair_invalid,omitempty"\`\`.

- Extends: `EmbeddingsBuildOptions.RepairInvalid bool`.

- Adds: `agentsview embeddings build --repair-invalid`.

- [x] **Step 1: Add failing request and CLI behavior tests**

Assert the Huma build endpoint accepts `{"repair_invalid":true}` and passes it
to the manager. Assert `runEmbeddingsBuild` sends `RepairInvalid: true` to the
daemon without triggering the full-rebuild confirmation. Assert
`printBuildSummary` reports repair targets and invalidated chunks separately
from successfully embedded documents. Use a real failed repair through the
manager, direct CLI, and daemon-status serialization; assert its scan state,
partial result, failed count, and remaining queue count are printed before the
command returns a nonzero error. Exercise a real context cancellation after one
scan batch commits, then prove `Index.Build`, manager status serialization, and
direct CLI output preserve the exact remaining count.

- [x] **Step 2: Run focused tests and confirm failure**

Run:
`go test -tags fts5 ./internal/server ./cmd/agentsview -run 'Test.*RepairInvalid' -count=1`

Expected: FAIL because the request field, flag, threading, and summary are
absent.

- [x] **Step 3: Thread the option through each owned boundary**

Add the JSON request field and copy it into `BuildOptions` in
`Manager.runBuild`. Register the Cobra boolean flag, copy it into
`BuildRequest`, and print a repair summary when the scan invalidates documents.
Always store the most recent `BuildResult`, including a failed attempt's partial
repair statistics, so a new error is never paired with a stale prior success.
Print that result before returning the direct or daemon error. Validate
repair/full-rebuild and repair/backstop conflicts synchronously in the manager
and map them to HTTP 400 before a build starts. Regenerate the tracked
TypeScript API client with `npm run generate:api` and run `npm run check`. Leave
scheduler-generated requests at the default `false` so background builds remain
bounded. Define one shared HTTP/daemon API version, bump it to version 3 for the
expanded repair status contract, and reject a still-running version-2 daemon
with restart guidance rather than decoding absent status fields as false.

- [x] **Step 4: Update semantic-search documentation**

Document `--repair-invalid` as the targeted recovery command. Retain the
standard Ollama `/v1/embeddings` setup, add a distinct direct-`llama-server`
high-throughput example with
`--cache-ram 0 --no-cache-prompt --no-cache-idle-slots`, explain why
prompt-state caching is unsafe for independent embeddings, and add
invalid-vector guidance to the error taxonomy.

- [x] **Step 5: Run focused CLI, server, manager, and vector tests**

Run:
`go test -tags fts5 ./internal/vector ./internal/server ./cmd/agentsview -count=1`

Expected: PASS.

### Task 6: Format, Verify, and Commit

**Files:**

- No additional files.

**Interfaces:**

- Produces: a verified, reviewable implementation commit.

- [x] **Step 1: Format and run repository validation**

Run: `gofmt -w` on changed Go files, `go fmt ./...`, `go vet ./...`,
`make test`, and `make build`.

Expected: all commands exit zero.

- [x] **Step 2: Commit the tracked implementation**

Stage every tracked implementation artifact from the preceding tasks, including
the generated TypeScript API models, along with the plan, Go source/tests, and
documentation. Commit with a focused conventional message and without generated
attribution.
