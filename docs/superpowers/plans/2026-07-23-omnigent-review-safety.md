# Omnigent Review Safety Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Preserve missing Omnigent conversations as revivable archive rows and
make watcher retries plus split-schema metadata updates converge without
archive-sized warm-event work.

**Architecture:** Keep explicit provider exclusions reserved for identity
migrations and route inferred missing container ownership through the existing
`source_missing` tombstone seam. Thread the engine's generation-safe full-retry
state into changed-path provider construction. Extend split-schema tracking with
an index-seeking `updated_at` cursor for the source's single-workspace layout
and a capped, expiring replay set for metadata commits that Omnigent writes
separately from the conversation row.

**Tech Stack:** Go 1.26, SQLite, testify, the provider/sync integration harness.

## Global Constraints

- Persistent archive rows and messages must never be hard-deleted because a
  source conversation disappeared.
- Warm watcher work must stay bounded by the changed/recent batch, not total
  conversation count.
- All new Go assertions use `require` for prerequisites and `assert` for
  independent outcomes.
- Implement all three review fixes in one commit after every regression test has
  completed its red-green cycle.

______________________________________________________________________

### Task 1: Preserve Missing Container Members

**Files:**

- Modify: `internal/sync/engine.go`
- Test: `internal/sync/omnigent_integration_test.go`

**Interfaces:**

- Consumes: `providerSourceSessionOwnershipsForForceReplace`,
  `tombstoneSessionSourceOwnership`

- Produces:
  `providerSourceMissingSessionOwnershipsForCompleteResult(parser.Provider, parser.SourceRef, []parser.ParseResult) []sourceMissingMember`

- [x] **Step 1: Strengthen the reconciliation regression**

After deleting `conv_0064` and reconciling, assert that `GetSession` hides the
session while `GetSessionFull` still returns its messages and reports
`DeletionCause == "source_missing"`.

- [x] **Step 2: Run the focused test and verify the hard-delete failure**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/sync \
  -run TestReconcileOmnigentRetiresDeletedConversationAndPreservesSurvivors \
  -count=1
```

Expected: FAIL because `GetSessionFull("omnigent:conv_0064")` returns `nil`.

- [x] **Step 3: Route inferred ownership to the tombstone seam**

Replace the complete-result ID helper with an ownership helper:

```go
func (e *Engine) providerSourceMissingSessionOwnershipsForCompleteResult(
	provider parser.Provider,
	source parser.SourceRef,
	results []parser.ParseResult,
) []sourceMissingMember
```

Filter stored ownerships against emitted IDs, assign the result to
`processResult.sourceMissingMembers`, and leave `outcome.ExcludedSessionIDs` as
the only hard-exclusion source.

- [x] **Step 4: Run the focused test and verify preservation**

Run the command from Step 2. Expected: PASS with the deleted member hidden,
archived, and marked `source_missing`.

### Task 2: Retry Failed Watcher Updates Authoritatively

**Files:**

- Modify: `internal/sync/engine.go`
- Modify: `internal/parser/omnigent_provider.go`
- Test: `internal/sync/omnigent_integration_test.go`

**Interfaces:**

- Consumes: `Engine.omnigentFullRetry`, `Engine.completeOmnigentFullRetry`,
  `omnigentSourceSet.forceFullDiscovery`

- Produces: an explicit
  `omnigentSourceSet.SourcesForChangedPath(context.Context, ChangedPathRequest) ([]SourceRef, error)`
  override

- [x] **Step 1: Add a watcher failure-and-retry regression**

Seed one conversation, append a second message, inject one parse failure for the
virtual member, and call `SyncPathsContext` twice with the same `chat.db` path.
Assert the first pass fails and the second pass stores both messages.

- [x] **Step 2: Run the focused test and verify the stale retry**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/sync \
  -run TestSyncPathsOmnigentFailedMemberRetryReplaysContainer -count=1
```

Expected: FAIL because the second classification emits no source and the archive
remains at one message.

- [x] **Step 3: Thread and consume the retry generation**

Construct the Omnigent changed-path provider with `ForceFullDiscovery: pending`.
When set, the source set must classify the event to the physical container
instead of consulting advanced member cursors. Capture the pending generation
only when an Omnigent source was classified, and call
`completeOmnigentFullRetry(generation)` only after a complete successful
changed-path pass. The existing generation comparison must continue to protect
retries requested during that pass.

- [x] **Step 4: Run the focused test and verify convergence**

Run the command from Step 2. Expected: PASS with two persisted messages and zero
failures on the retry pass.

### Task 3: Discover Split-Schema Metadata Updates

**Files:**

- Modify: `internal/parser/omnigent.go`
- Modify: `internal/parser/omnigent_provider.go`
- Test: `internal/parser/omnigent_test.go`

**Interfaces:**

- Consumes: `omnigentSchema.changeIndexName`,
  `omnigentTrackedContainer.checkedAt`, `queryOmnigentConversationMetas`

- Produces:
  `listOmnigentSplitConversationMetasSince(context.Context, *sql.DB, omnigentSchema, int64, int64, int64) ([]omnigentMeta, error)`
  and a capped recent-member replay list

- [x] **Step 1: Add metadata-only and cardinality regressions**

Initialize a split-schema provider, update an existing workspace-0
conversation's title/`updated_at` plus workspace, branch, model configuration,
and usage fields without inserting a row, then assert the filesystem event
includes that virtual member within the fixed replay cap and parsing observes
the literal updated values. Repeat a cursor-only update against small and large
archives and assert one emitted member. Add an `EXPLAIN QUERY PLAN` case
requiring the configured split change index and rejecting `SCAN CONVERSATIONS`.

- [x] **Step 2: Run the focused parser tests and verify the missing member**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/parser \
  -run 'TestOmnigentSplitMetadataOnlyChange|TestOmnigentIncrementalQueriesUseSeekableIndexes' \
  -count=1
```

Expected: FAIL because split discovery currently consults only inserted rowids.

- [x] **Step 3: Add the bounded split cursor and replay**

Detect either `(workspace_id, updated_at, ...)` or
`(workspace_id, archived, updated_at, ...)` as a split change index. For a
single-workspace container, seek it with the complete leading prefix and the
same capture-before-query time window used by the legacy schema. Merge those
metas with new conversation/item rows. Retain at most `omnigentChangePageSize`
recently changed member keys with their original observation timestamps and
replay only unexpired entries so a subsequent standalone usage commit re-parses
the active member without scanning stored members.

- [x] **Step 4: Run the focused parser tests and verify bounded discovery**

Run the command from Step 2. Expected: PASS with one changed member at each
archive size and an index-seeking query plan.

### Task 4: Quality Gate and Single Commit

**Files:**

- Modify: only the production/tests/plan files listed above

**Interfaces:**

- Consumes: all three completed tasks

- Produces: one reviewed commit containing every valid finding fix

- [x] **Step 1: Format and inspect**

```bash
go fmt ./...
git diff --check
git diff --stat
```

- [x] **Step 2: Run repository verification**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/parser ./internal/sync
go vet ./...
make lint-ci
```

Expected: every command exits zero and every linter reports zero issues.

Observed: parser tests, all Omnigent sync tests, vet, golangci-lint, and NilAway
passed. The package-wide sync run reproduced unrelated macOS filesystem-watcher
timeouts; the same watcher test passed alone.

- [x] **Step 3: Scrub public surfaces**

Scan the full pending diff and commit message using the repository private-data
denylist plus absolute-path, hostname, and non-public-email heuristics.
Expected: zero matches.

- [x] **Step 4: Commit**

Create one conventional commit whose body records:

```text
VALID (fixed): #1, #2, #3
INVALID (dismissed): none
PEDANTIC (skipped): none
```
