# Artifact Export Reliability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make artifact publication origin-safe, memory-bounded before complete
session materialization, and able to durably reject one deterministic failure
without starving later queue claims.

**Architecture:** Keep the existing publication ledger and immutable artifact
formats. Add a database-owned bounded snapshot loader, validate origin inside
the locked publication transaction, and finalize success/rejection outcomes
atomically with checkpoint authority. Deterministic validation failures delete
stale publication authority and become durable generation-scoped diagnostics;
transient failures remain fail-fast.

**Tech Stack:** Go, `database/sql`, SQLite/FTS5, `testify`, the existing
filesystem-backed artifact store, and Go's `errors.Is` typed-error contract.

## Global Constraints

- Follow
  `docs/superpowers/specs/2026-07-27-artifact-export-reliability-design.md`.
- Preserve the SQLite archive through non-destructive column migrations; never
  drop, truncate, or recreate it.
- Keep preflight limits equal to existing artifact policy: 32,768 messages,
  32,768 usage events, 256 tool calls per message, 65,536 tool calls per
  session, 1,024 result events per call, 262,144 result events per session,
  256 MiB raw message/nested bytes, and 16 MiB raw usage bytes.
- `rejected_generation` is diagnostic only. Correctness always compares the
  claim against the queue row's current `generation`.
- A deterministic rejection removes prior publication authority; it never
  retains a stale manifest as current.
- Store, context, SQLite, origin-mismatch, and stale-claim failures remain
  transient/fatal and must not consume claims.
- Every new test uses real SQLite state or the real artifact boundary, uses
  `testify`, and must be observed failing before production code is added.
- After Go changes run `go fmt ./...` and `go vet ./...` before committing.
- If a future change tightens an export limit, that change must include a
  migration that requeues existing publications.

______________________________________________________________________

## File Map

- `internal/db/artifact_publication.go`: origin validation, claim outcomes,
  rejection diagnostics, and atomic finalization.
- `internal/db/artifact_export_load.go`: bounded snapshot loader and typed
  preflight limit errors.
- `internal/db/artifact_export_load_test.go`: cardinality, byte, snapshot, and
  scaling regressions for the loader.
- `internal/db/schema.sql`: rejection columns for newly created archives.
- `internal/db/db.go`: non-destructive rejection-column migrations plus queue
  trigger/requeue clearing.
- `internal/db/orphaned.go`: resync copy/requeue behavior for rejection state.
- `internal/db/artifact_publication_test.go`: origin, outcome, migration,
  mutation-retry, adoption, and stale-generation tests.
- `internal/artifact/export.go`: bounded loader use, typed deterministic
  classification, per-claim isolation, and rejected-result count.
- `internal/artifact/limits.go`: deterministic export-rejection sentinel and
  mapping from artifact policy to database load limits.
- `internal/artifact/export_limits_test.go`: limit classification and bounded
  loader integration.
- `internal/artifact/export_test.go`: mixed success/rejection checkpoint flow
  and transient-failure behavior.
- `internal/artifact/origin_test.go`: end-to-end rejection removal, mutation
  retry, and origin-adoption behavior.

______________________________________________________________________

### Task 1: Lock Publication Authority to the Persisted Origin

**Files:**

- Modify: `internal/db/artifact_publication.go:13-25,157-180`
- Test: `internal/db/artifact_publication_test.go`

**Interfaces:**

- Consumes: `lockArtifactPublicationTx(context.Context, *sql.Tx) error` and the
  `pg_sync_state` key `artifact_origin_id`.

- Produces: `db.ErrArtifactOriginMismatch`, returned by
  `ApplyArtifactPublicationChanges` before claim validation or mutation.

- [ ] **Step 1: Write the failing transaction-boundary test**

Add a test that seeds origin A, creates and claims one local session, then calls
`ApplyArtifactPublicationChanges` with origin B:

```go
func TestArtifactPublicationChangesRejectMismatchedPersistedOrigin(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	require.NoError(t, database.AdoptArtifactOrigin("origin-a1b2c3"))
	require.NoError(t, database.UpsertSession(Session{
		ID: "session", Project: "project", Machine: "local", Agent: "claude",
	}))
	claims, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	require.Len(t, claims, 1)

	revision, changed, err := database.ApplyArtifactPublicationChanges(
		ctx, "origin-d4e5f6", []ArtifactPublicationChange{{
			SessionID: "session", Generation: claims[0].Generation,
			ManifestHash: "manifest", SourceFingerprint: "fingerprint",
		}},
	)
	require.ErrorIs(t, err, ErrArtifactOriginMismatch)
	assert.Zero(t, revision)
	assert.False(t, changed)

	var publications []ArtifactPublication
	_, streamErr := database.StreamArtifactPublications(
		ctx, "origin-d4e5f6", func(row ArtifactPublication) error {
			publications = append(publications, row)
			return nil
		},
	)
	require.NoError(t, streamErr)
	assert.Empty(t, publications)
	pending, pendingErr := database.PendingArtifactExports(ctx, 10)
	require.NoError(t, pendingErr)
	require.Equal(t, claims, pending)
}
```

Add a missing-origin subtest by deleting the state key after the claim and
asserting the same typed error and unchanged claim.

- [ ] **Step 2: Run the test and observe the wrong-origin mutation**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db \
  -run '^TestArtifactPublicationChangesRejectMismatchedPersistedOrigin$' \
  -count=1 -v
```

Expected: FAIL because the wrong-origin publication is inserted and no
`ErrArtifactOriginMismatch` exists.

- [ ] **Step 3: Add the typed error and locked validation**

Add near `ErrArtifactExportClaimStale`:

```go
var ErrArtifactOriginMismatch = errors.New("artifact origin does not match persisted origin")
```

Add:

```go
func validateArtifactOriginTx(
	ctx context.Context, tx *sql.Tx, origin string,
) error {
	var persisted string
	err := tx.QueryRowContext(ctx, `
		SELECT value FROM pg_sync_state WHERE key = 'artifact_origin_id'`,
	).Scan(&persisted)
	if errors.Is(err, sql.ErrNoRows) || err == nil && persisted == "" {
		return fmt.Errorf("%w: no persisted artifact origin", ErrArtifactOriginMismatch)
	}
	if err != nil {
		return fmt.Errorf("reading persisted artifact origin: %w", err)
	}
	if persisted != origin {
		return fmt.Errorf(
			"%w: requested %s, persisted %s",
			ErrArtifactOriginMismatch, origin, persisted,
		)
	}
	return nil
}
```

Immediately after `lockArtifactPublicationTx` in
`ApplyArtifactPublicationChanges`, call:

```go
if err := validateArtifactOriginTx(ctx, tx, origin); err != nil {
	return 0, false, err
}
```

Do not rely on an unlocked preflight read for correctness.

- [ ] **Step 4: Verify matching and mismatching origins**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db \
  -run 'TestArtifactPublicationChangesRejectMismatchedPersistedOrigin|TestArtifactPublicationAtomicLifecycle' \
  -count=1 -v
```

Expected: PASS.

- [ ] **Step 5: Commit Task 1**

```bash
git add internal/db/artifact_publication.go internal/db/artifact_publication_test.go
git commit -m "fix(artifact): bind publication claims to active origin" \
  -m "The queue is global, so publication authority must verify the configured namespace under the same writer reservation as claim validation. Reject stale exporters before they can advance a dead origin."
```

______________________________________________________________________

### Task 2: Persist and Atomically Finalize Rejected Generations

**Files:**

- Modify: `internal/db/schema.sql:126-139`
- Modify: `internal/db/db.go:artifactSessionQueueTriggerCreatesSQL`
- Modify: `internal/db/db.go:schemaColumnMigrations`
- Modify: `internal/db/db.go:bootstrapArtifactExportQueueSQL`
- Modify: `internal/db/db.go:requeueArtifactExportsSQL`
- Modify: `internal/db/db.go:requeueArtifactOriginExportsSQL`
- Modify: `internal/db/artifact_publication.go:263-439,607-632`
- Modify: `internal/db/orphaned.go:380-445`
- Test: `internal/db/artifact_publication_test.go`

**Interfaces:**

- Produces:

```go
type ArtifactExportOutcome struct {
	Item      ArtifactExportQueueItem
	Rejection string
}

type ArtifactExportRejection struct {
	SessionID  string
	Generation int64
	Error      string
	RejectedAt string
}

func (db *DB) FinalizeArtifactExports(
	context.Context, []ArtifactExportOutcome,
) error

func (db *DB) RecordArtifactCheckpointHeadOutcomes(
	context.Context, ArtifactCheckpointHead, []ArtifactExportOutcome,
) error

func (db *DB) GetArtifactExportRejection(
	context.Context, string,
) (ArtifactExportRejection, bool, error)
```

- Preserves: `AcknowledgeArtifactExports` and `RecordArtifactCheckpointHead` as
  successful-outcome wrappers for existing callers.

- [ ] **Step 1: Add failing outcome lifecycle tests**

Add tests using a real database:

```go
func TestArtifactExportRejectionFinalizesExactGeneration(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	require.NoError(t, database.AdoptArtifactOrigin("origin-a1b2c3"))
	require.NoError(t, database.UpsertSession(Session{
		ID: "rejected", Project: "project", Machine: "local", Agent: "claude",
	}))
	claims, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	require.Len(t, claims, 1)

	require.NoError(t, database.FinalizeArtifactExports(ctx, []ArtifactExportOutcome{{
		Item: claims[0], Rejection: "session message limit exceeded",
	}}))
	pending, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
	rejection, ok, err := database.GetArtifactExportRejection(ctx, "rejected")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, claims[0].Generation, rejection.Generation)
	assert.Equal(t, "session message limit exceeded", rejection.Error)
	assert.NotEmpty(t, rejection.RejectedAt)

	require.NoError(t, database.ReplaceSessionMessages("rejected", []Message{{
		SessionID: "rejected", Ordinal: 0, Role: "user", Content: "changed",
	}}))
	pending, err = database.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Greater(t, pending[0].Generation, claims[0].Generation)
	_, ok, err = database.GetArtifactExportRejection(ctx, "rejected")
	require.NoError(t, err)
	assert.False(t, ok)
}
```

Add:

- `TestArtifactExportRejectionCannotConsumeNewerGeneration`: mutate after taking
  a claim, then assert `ErrArtifactExportClaimStale` and that the new
  generation remains pending.

- `TestArtifactCheckpointHeadAndRejectionsCommitAtomically`: record a head with
  one successful and one rejected outcome and assert head, clean rows, and
  diagnostics; inject a stale generation and assert none of them change.

- `TestArtifactRequeueLifecycleClearsRejection`: cover divergent origin adoption
  and resync, asserting new generations are pending and rejection fields are
  empty.

- Migration coverage opening a database whose queue lacks the three new columns,
  then asserting all columns exist without losing the queue row.

- [ ] **Step 2: Run the lifecycle tests and observe missing schema/API
  failures**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db \
  -run 'TestArtifactExportRejection|TestArtifactCheckpointHeadAndRejections|TestArtifactRequeueLifecycle' \
  -count=1 -v
```

Expected: build or test FAIL because rejection columns and outcome APIs do not
exist.

- [ ] **Step 3: Add queue columns and non-destructive migrations**

Change the queue schema to:

```sql
CREATE TABLE IF NOT EXISTS artifact_export_queue (
    session_id  TEXT PRIMARY KEY,
    enqueued_at TEXT NOT NULL
        DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    generation INTEGER NOT NULL DEFAULT 1,
    pending INTEGER NOT NULL DEFAULT 1 CHECK (pending IN (0, 1)),
    rejected_generation INTEGER,
    last_error TEXT NOT NULL DEFAULT '',
    rejected_at TEXT
);
```

Append these entries to `schemaColumnMigrations()`:

```go
{
	"artifact_export_queue", "rejected_generation",
	"ALTER TABLE artifact_export_queue ADD COLUMN rejected_generation INTEGER",
},
{
	"artifact_export_queue", "last_error",
	"ALTER TABLE artifact_export_queue ADD COLUMN last_error TEXT NOT NULL DEFAULT ''",
},
{
	"artifact_export_queue", "rejected_at",
	"ALTER TABLE artifact_export_queue ADD COLUMN rejected_at TEXT",
},
```

Every queue conflict that creates a new generation must add:

```sql
rejected_generation = NULL,
last_error = '',
rejected_at = NULL
```

Apply that reset to all three session triggers, `enqueueArtifactExportTx`,
`requeueArtifactExportsSQL`, and `requeueArtifactOriginExportsSQL`.
`bootstrapArtifactExportQueueSQL` remains `INSERT OR IGNORE`: it does not
advance an existing generation.

Update the resync queue copy to insert the new columns but deliberately clear
them because it advances every copied generation:

```sql
INSERT INTO main.artifact_export_queue(
    session_id, enqueued_at, generation, pending,
    rejected_generation, last_error, rejected_at
)
SELECT session_id, enqueued_at, generation + 1, 1, NULL, '', NULL
FROM old_db.artifact_export_queue WHERE true
ON CONFLICT(session_id) DO UPDATE SET
    enqueued_at = CASE
        WHEN artifact_export_queue.pending = 1 AND excluded.pending = 1
            THEN min(artifact_export_queue.enqueued_at, excluded.enqueued_at)
        WHEN artifact_export_queue.pending = 1
            THEN artifact_export_queue.enqueued_at
        ELSE excluded.enqueued_at
    END,
    generation = max(artifact_export_queue.generation, excluded.generation) + 1,
    pending = 1,
    rejected_generation = NULL,
    last_error = '',
    rejected_at = NULL
```

- [ ] **Step 4: Implement per-claim finalization**

Add the outcome and rejection structs exactly as declared in **Interfaces**.
Convert old item slices with:

```go
func successfulArtifactExportOutcomes(
	items []ArtifactExportQueueItem,
) []ArtifactExportOutcome {
	outcomes := make([]ArtifactExportOutcome, len(items))
	for i, item := range items {
		outcomes[i] = ArtifactExportOutcome{Item: item}
	}
	return outcomes
}
```

Replace the clean-row helper with outcome finalization:

```go
func finalizeArtifactExportOutcomesTx(
	ctx context.Context, tx *sql.Tx, outcomes []ArtifactExportOutcome,
) error {
	items := make([]ArtifactExportQueueItem, len(outcomes))
	for i, outcome := range outcomes {
		items[i] = outcome.Item
	}
	unique, err := validateArtifactExportClaimsTx(ctx, tx, items)
	if err != nil {
		return err
	}
	bySession := make(map[string]string, len(outcomes))
	for _, outcome := range outcomes {
		if previous, ok := bySession[outcome.Item.SessionID]; ok &&
			previous != outcome.Rejection {
			return fmt.Errorf(
				"conflicting artifact export outcomes for session %s",
				outcome.Item.SessionID,
			)
		}
		bySession[outcome.Item.SessionID] = outcome.Rejection
	}
	for _, item := range unique {
		rejection := bySession[item.SessionID]
		var rejectedGeneration any
		var rejectedAt any
		if rejection != "" {
			rejectedGeneration = item.Generation
			rejectedAt = time.Now().UTC().Format(time.RFC3339Nano)
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE artifact_export_queue SET
				pending = 0,
				rejected_generation = ?,
				last_error = ?,
				rejected_at = ?
			WHERE session_id = ? AND generation = ? AND pending = 1`,
			rejectedGeneration, rejection, rejectedAt,
			item.SessionID, item.Generation,
		)
		if err != nil {
			return fmt.Errorf("finalizing artifact export %s: %w", item.SessionID, err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("reading artifact export finalization %s: %w", item.SessionID, err)
		}
		if rows != 1 {
			return fmt.Errorf(
				"%w: session %s generation %d",
				ErrArtifactExportClaimStale, item.SessionID, item.Generation,
			)
		}
	}
	return nil
}
```

`rejected_generation` is written from the validated claim only and is never used
to decide staleness.

Implement `FinalizeArtifactExports` with the same lock/transaction structure as
`AcknowledgeArtifactExports`. Make `AcknowledgeArtifactExports` call it with
successful outcomes.

Move the current checkpoint body into `RecordArtifactCheckpointHeadOutcomes`,
call `finalizeArtifactExportOutcomesTx` after the head insert, and retain
`RecordArtifactCheckpointHead` as a successful-outcome wrapper.

Implement diagnostic lookup:

```go
func (db *DB) GetArtifactExportRejection(
	ctx context.Context, sessionID string,
) (ArtifactExportRejection, bool, error) {
	var rejection ArtifactExportRejection
	err := db.getReader().QueryRowContext(ctx, `
		SELECT session_id, rejected_generation, last_error, rejected_at
		FROM artifact_export_queue
		WHERE session_id = ?
		  AND rejected_generation IS NOT NULL
		  AND last_error <> ''
		  AND rejected_at IS NOT NULL`, sessionID,
	).Scan(
		&rejection.SessionID, &rejection.Generation,
		&rejection.Error, &rejection.RejectedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ArtifactExportRejection{}, false, nil
	}
	if err != nil {
		return ArtifactExportRejection{}, false,
			fmt.Errorf("reading artifact export rejection: %w", err)
	}
	return rejection, true, nil
}
```

- [ ] **Step 5: Verify schema, lifecycle, and existing acknowledgement
  behavior**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db \
  -run 'ArtifactExport|ArtifactPublication|ArtifactCheckpoint|ArtifactOrigin' \
  -count=1 -v
```

Expected: PASS.

- [ ] **Step 6: Commit Task 2**

```bash
git add internal/db/schema.sql internal/db/db.go \
  internal/db/orphaned.go internal/db/artifact_publication.go \
  internal/db/artifact_publication_test.go
git commit -m "feat(artifact): persist rejected export generations" \
  -m "Deterministic failures need a durable terminal outcome that remains generation-guarded and checkpoint-atomic. Record diagnostics on clean queue rows while making every later enqueue a fresh retry."
```

______________________________________________________________________

### Task 3: Add a Transactional Bounded Export Loader

**Files:**

- Create: `internal/db/artifact_export_load.go`
- Create: `internal/db/artifact_export_load_test.go`
- Modify: `internal/db/usage_events.go:285-335`

**Interfaces:**

- Produces:

```go
var ErrArtifactExportLimit = errors.New("artifact export load limit exceeded")

type ArtifactExportLoadLimits struct {
	Messages            int
	UsageEvents         int
	MessageToolCalls    int
	ToolResultEvents    int
	SessionToolCalls    int
	SessionResultEvents int
	MessageBytes        int64
	UsageBytes          int64
}

type ArtifactExportData struct {
	Messages    []Message
	UsageEvents []UsageEvent
}

func (db *DB) LoadArtifactExportData(
	context.Context, string, ArtifactExportLoadLimits,
) (ArtifactExportData, error)
```

- Consumes: `selectMessageCols`, `scanMessages`, `attachToolCallsWithQuerier`,
  and a factored `scanUsageEvents`.

- [ ] **Step 1: Write table-driven failing boundary tests**

Create `internal/db/artifact_export_load_test.go` with:

```go
func smallArtifactExportLoadLimits() ArtifactExportLoadLimits {
	return ArtifactExportLoadLimits{
		Messages: 2, UsageEvents: 2,
		MessageToolCalls: 2, ToolResultEvents: 2,
		SessionToolCalls: 3, SessionResultEvents: 3,
		MessageBytes: 1 << 20, UsageBytes: 1 << 20,
	}
}
```

Add exact-boundary and `limit + 1` subtests for:

- three messages with `Messages: 2`;
- three usage events with `UsageEvents: 2`;
- three calls on one message with `MessageToolCalls: 2`;
- four calls across messages with `SessionToolCalls: 3`;
- three result events on one call with `ToolResultEvents: 2`;
- four result events across calls with `SessionResultEvents: 3`.

Every failure must use:

```go
_, err := database.LoadArtifactExportData(ctx, "session", limits)
require.ErrorIs(t, err, ErrArtifactExportLimit)
```

For every collection, remove the last row and assert the exact boundary loads
successfully with literal expected lengths.

Add raw-byte tests with one message whose only non-empty exported strings are
role `"user"` and content `"abcd"`:

```go
limits := smallArtifactExportLoadLimits()
limits.MessageBytes = 8
data, err := database.LoadArtifactExportData(ctx, "session", limits)
require.NoError(t, err)
require.Len(t, data.Messages, 1)

limits.MessageBytes = 7
_, err = database.LoadArtifactExportData(ctx, "session", limits)
require.ErrorIs(t, err, ErrArtifactExportLimit)
```

Add an analogous usage event with source `"src"` and model `"model"`:
`UsageBytes: 8` succeeds and `UsageBytes: 7` fails.

- [ ] **Step 2: Add the cardinality-scaling regression**

Use two separate databases containing 3 and 10,000 messages for the claimed
session, with `Messages: 2`. Measure allocations around only
`LoadArtifactExportData` after setup:

```go
smallAllocs := testing.AllocsPerRun(5, func() {
	_, err := small.LoadArtifactExportData(ctx, "session", limits)
	require.ErrorIs(t, err, ErrArtifactExportLimit)
})
largeAllocs := testing.AllocsPerRun(5, func() {
	_, err := large.LoadArtifactExportData(ctx, "session", limits)
	require.ErrorIs(t, err, ErrArtifactExportLimit)
})
assert.LessOrEqual(t, largeAllocs, smallAllocs+50,
	"preflight allocation must remain bounded by limit, not session cardinality")
```

This catches removal of the SQL `LIMIT limits.Messages + 1` guard without
inspecting source text.

- [ ] **Step 3: Run tests and observe the missing bounded-loader API**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db \
  -run '^TestLoadArtifactExportData' -count=1 -v
```

Expected: build FAIL because the API does not exist.

- [ ] **Step 4: Add the limits, data type, and validation**

Create `internal/db/artifact_export_load.go` with the declarations from
**Interfaces** and:

```go
func validateArtifactExportLoadLimits(limits ArtifactExportLoadLimits) error {
	if limits.Messages < 1 || limits.UsageEvents < 1 ||
		limits.MessageToolCalls < 1 || limits.ToolResultEvents < 1 ||
		limits.SessionToolCalls < 1 || limits.SessionResultEvents < 1 ||
		limits.MessageBytes < 1 || limits.UsageBytes < 1 {
		return errors.New("artifact export load limits must all be positive")
	}
	if limits.Messages == math.MaxInt || limits.UsageEvents == math.MaxInt ||
		limits.SessionToolCalls == math.MaxInt ||
		limits.SessionResultEvents == math.MaxInt {
		return errors.New("artifact export load limits must permit a limit-plus-one probe")
	}
	return nil
}

func artifactExportLimitf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrArtifactExportLimit, fmt.Sprintf(format, args...))
}
```

- [ ] **Step 5: Implement bounded preflight queries**

Define exact raw-byte expressions:

```go
const artifactMessageRawBytesSQL = `
	length(CAST(role AS BLOB)) +
	length(CAST(content AS BLOB)) +
	length(CAST(thinking_text AS BLOB)) +
	length(CAST(COALESCE(timestamp, '') AS BLOB)) +
	length(CAST(model AS BLOB)) +
	length(CAST(token_usage AS BLOB)) +
	length(CAST(claude_message_id AS BLOB)) +
	length(CAST(claude_request_id AS BLOB)) +
	length(CAST(source_type AS BLOB)) +
	length(CAST(source_subtype AS BLOB)) +
	length(CAST(source_uuid AS BLOB)) +
	length(CAST(source_parent_uuid AS BLOB))`

const artifactToolCallRawBytesSQL = `
	length(CAST(tool_name AS BLOB)) +
	length(CAST(category AS BLOB)) +
	length(CAST(COALESCE(tool_use_id, '') AS BLOB)) +
	length(CAST(COALESCE(input_json, '') AS BLOB)) +
	length(CAST(COALESCE(skill_name, '') AS BLOB)) +
	length(CAST(COALESCE(result_content, '') AS BLOB)) +
	length(CAST(COALESCE(subagent_session_id, '') AS BLOB)) +
	length(CAST(COALESCE(file_path, '') AS BLOB))`

const artifactResultEventRawBytesSQL = `
	length(CAST(COALESCE(tool_use_id, '') AS BLOB)) +
	length(CAST(COALESCE(agent_id, '') AS BLOB)) +
	length(CAST(COALESCE(subagent_session_id, '') AS BLOB)) +
	length(CAST(source AS BLOB)) +
	length(CAST(status AS BLOB)) +
	length(CAST(content AS BLOB)) +
	length(CAST(COALESCE(timestamp, '') AS BLOB))`

const artifactUsageRawBytesSQL = `
	length(CAST(source AS BLOB)) +
	length(CAST(model AS BLOB)) +
	length(CAST(cost_status AS BLOB)) +
	length(CAST(cost_source AS BLOB)) +
	length(CAST(COALESCE(occurred_at, '') AS BLOB)) +
	length(CAST(dedup_key AS BLOB))`
```

Implement `preflightArtifactExportTx` as four streaming queries, each with
`LIMIT cap + 1` and no `ORDER BY`, so SQLite can stop from the session index
without sorting an oversized session:

```go
func preflightArtifactExportTx(
	ctx context.Context, tx *sql.Tx, sessionID string, limits ArtifactExportLoadLimits,
) error {
	messageBytes, err := preflightArtifactMessagesTx(ctx, tx, sessionID, limits)
	if err != nil {
		return err
	}
	nestedBytes, err := preflightArtifactToolCallsTx(ctx, tx, sessionID, limits)
	if err != nil {
		return err
	}
	resultBytes, err := preflightArtifactResultEventsTx(ctx, tx, sessionID, limits)
	if err != nil {
		return err
	}
	if messageBytes+nestedBytes+resultBytes > limits.MessageBytes {
		return artifactExportLimitf(
			"session %s raw message bytes exceed %d", sessionID, limits.MessageBytes,
		)
	}
	return preflightArtifactUsageTx(ctx, tx, sessionID, limits)
}
```

The four helpers must use these query shapes:

```sql
SELECT id, ordinal, <artifactMessageRawBytesSQL>
FROM messages WHERE session_id = ? LIMIT ?
```

```sql
SELECT message_id, <artifactToolCallRawBytesSQL>
FROM tool_calls WHERE session_id = ? LIMIT ?
```

```sql
SELECT tool_call_message_ordinal, call_index,
       <artifactResultEventRawBytesSQL>
FROM tool_result_events WHERE session_id = ? LIMIT ?
```

```sql
SELECT <artifactUsageRawBytesSQL>
FROM usage_events WHERE session_id = ? LIMIT ?
```

For each helper:

- iterate rows without appending objects;

- reject when the row count reaches the applicable global `limit + 1`;

- maintain bounded `map[int64]int` or `map[[2]int]int` parent counts for the
  per-message/per-call limits;

- add raw bytes using overflow-safe comparison
  `current > limit || additional > limit-current`;

- return errors wrapping `ErrArtifactExportLimit`;

- propagate query, scan, close, and context errors without wrapping the limit
  sentinel.

- [ ] **Step 6: Materialize from the same read snapshot**

Factor the body of `GetUsageEvents` into:

```go
func usageEventsWithQuerier(
	ctx context.Context, q messageRowsQuerier, sessionID string, limit int,
) ([]UsageEvent, error)
```

When `limit > 0`, append `LIMIT ?` to the existing stable-order query and bind
that value. When `limit == 0`, run the current query without a limit.
`GetUsageEvents` passes `0`, preserving its public behavior, while the artifact
loader passes `limits.UsageEvents + 1`. The helper only scans; the caller that
requested a cap compares the returned length with its policy limit.

Implement the loader:

```go
func (db *DB) LoadArtifactExportData(
	ctx context.Context, sessionID string, limits ArtifactExportLoadLimits,
) (_ ArtifactExportData, retErr error) {
	if sessionID == "" {
		return ArtifactExportData{}, errors.New("artifact export session id is required")
	}
	if err := validateArtifactExportLoadLimits(limits); err != nil {
		return ArtifactExportData{}, err
	}
	db.connMu.RLock()
	reader := db.reader.Load()
	if reader == nil {
		db.connMu.RUnlock()
		return ArtifactExportData{}, errors.New("database is closed")
	}
	tx, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	db.connMu.RUnlock()
	if err != nil {
		return ArtifactExportData{}, fmt.Errorf("beginning artifact export snapshot: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			retErr = errors.Join(retErr, tx.Rollback())
		}
	}()

	if err := preflightArtifactExportTx(ctx, tx, sessionID, limits); err != nil {
		return ArtifactExportData{}, err
	}
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s FROM messages
		WHERE session_id = ?
		ORDER BY ordinal ASC
		LIMIT ?`, selectMessageCols), sessionID, limits.Messages+1)
	if err != nil {
		return ArtifactExportData{}, fmt.Errorf("loading artifact export messages: %w", err)
	}
	messages, scanErr := scanMessages(rows)
	closeErr := rows.Close()
	if scanErr != nil || closeErr != nil {
		return ArtifactExportData{}, errors.Join(scanErr, closeErr)
	}
	if len(messages) > limits.Messages {
		return ArtifactExportData{}, artifactExportLimitf(
			"session %s message count exceeds %d", sessionID, limits.Messages,
		)
	}
	if err := attachToolCallsWithQuerier(ctx, tx, messages); err != nil {
		return ArtifactExportData{}, err
	}
	usage, err := usageEventsWithQuerier(
		ctx, tx, sessionID, limits.UsageEvents+1,
	)
	if err != nil {
		return ArtifactExportData{}, err
	}
	if len(usage) > limits.UsageEvents {
		return ArtifactExportData{}, artifactExportLimitf(
			"session %s usage count exceeds %d", sessionID, limits.UsageEvents,
		)
	}
	if err := tx.Commit(); err != nil {
		return ArtifactExportData{}, fmt.Errorf("committing artifact export snapshot: %w", err)
	}
	committed = true
	return ArtifactExportData{Messages: messages, UsageEvents: usage}, nil
}
```

- [ ] **Step 7: Verify boundaries and existing readers**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db \
  -run 'TestLoadArtifactExportData|TestGetUsageEvents|TestGetAllMessages' \
  -count=1 -v
```

Expected: PASS.

- [ ] **Step 8: Commit Task 3**

```bash
git add internal/db/artifact_export_load.go \
  internal/db/artifact_export_load_test.go internal/db/usage_events.go
git commit -m "feat(artifact): bound export snapshot loading" \
  -m "Wire limits must also cap construction memory. Preflight each claimed session through bounded SQLite queries and only materialize data proven to fit within the existing artifact policy."
```

______________________________________________________________________

### Task 4: Route Export Through the Bounded Loader and Type Deterministic Limits

**Files:**

- Modify: `internal/artifact/limits.go`
- Modify: `internal/artifact/export.go:14-32,104-148,333-370,458-635`
- Modify: `internal/artifact/export_limits_test.go`

**Interfaces:**

- Consumes: `db.LoadArtifactExportData` and `db.ErrArtifactExportLimit`.
- Produces:

```go
var ErrArtifactExportRejected = errors.New("artifact export rejected")

func artifactExportLoadLimits(artifactLimits) db.ArtifactExportLoadLimits

func isDeterministicArtifactExportError(error) bool

func exportToStoreWithLimits(
	context.Context, artifactExportStore, ArtifactStore, ExportOptions,
	artifactLimits,
) (ExportResult, error)

func exportClaimedSessionToStore(
	context.Context, artifactExportStore, ArtifactStore, string, *db.Session,
	artifactLimits,
) (string, bool, error)
```

- [ ] **Step 1: Write failing bounded-loader integration tests**

Add a wrapper that counts legacy reads while embedding the real DB:

```go
type boundedLoadExportStore struct {
	*db.DB
	allMessageLoads int
	usageLoads      int
}

func (s *boundedLoadExportStore) GetAllMessages(
	ctx context.Context, sessionID string,
) ([]db.Message, error) {
	s.allMessageLoads++
	return s.DB.GetAllMessages(ctx, sessionID)
}

func (s *boundedLoadExportStore) GetUsageEvents(
	ctx context.Context, sessionID string,
) ([]db.UsageEvent, error) {
	s.usageLoads++
	return s.DB.GetUsageEvents(ctx, sessionID)
}
```

Export a normal session and assert:

```go
assert.Zero(t, wrapped.allMessageLoads)
assert.Zero(t, wrapped.usageLoads)
```

Add a small-limit direct test through `exportClaimedSessionToStore` using the
signature in **Interfaces**. Seed `limits.sessionMessages + 1` messages and
assert the error satisfies both `ErrArtifactExportRejected` and
`db.ErrArtifactExportLimit`, with no manifest or segment created.

- [ ] **Step 2: Run tests and observe legacy loading and untyped errors**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/artifact \
  -run 'TestExportUsesBoundedSnapshotLoader|TestExportClassifiesBoundedLoadLimit' \
  -count=1 -v
```

Expected: FAIL because `ExportToStore` calls `GetAllMessages` and
`GetUsageEvents`, and limit errors are not typed as deterministic rejection.

- [ ] **Step 3: Map artifact policy to database limits**

Add:

```go
var ErrArtifactExportRejected = errors.New("artifact export rejected")

func artifactExportLoadLimits(limits artifactLimits) db.ArtifactExportLoadLimits {
	return db.ArtifactExportLoadLimits{
		Messages: limits.sessionMessages,
		UsageEvents: limits.manifestUsageEvents,
		MessageToolCalls: limits.messageToolCalls,
		ToolResultEvents: limits.toolResultEvents,
		SessionToolCalls: limits.sessionToolCalls,
		SessionResultEvents: limits.sessionResultEvents,
		MessageBytes: limits.sessionDecodedBytes,
		UsageBytes: manifestDecodedLimit,
	}
}

func rejectArtifactExportf(format string, args ...any) error {
	return fmt.Errorf(
		"%w: %s", ErrArtifactExportRejected, fmt.Sprintf(format, args...),
	)
}

func classifyArtifactExportLoadError(err error) error {
	if errors.Is(err, db.ErrArtifactExportLimit) {
		return fmt.Errorf("%w: %w", ErrArtifactExportRejected, err)
	}
	return err
}

func isDeterministicArtifactExportError(err error) bool {
	return errors.Is(err, ErrArtifactExportRejected) ||
		errors.Is(err, db.ErrArtifactExportLimit)
}
```

Wrap existing deterministic validation failures in `exportLoadedSessionToStore`,
`validateExportNestedCollections`, `exportMessageSegmentsToStore`, and generated
manifest/session/segment size checks with `ErrArtifactExportRejected`. Do not
wrap `store.Create`, spool I/O, context, database, or corruption errors.

- [ ] **Step 4: Replace unbounded reads in claimed and full-recovery paths**

Change `artifactExportStore` to consume:

```go
LoadArtifactExportData(
	context.Context, string, db.ArtifactExportLoadLimits,
) (db.ArtifactExportData, error)
```

Remove `GetAllMessages` and `GetUsageEvents` from the interface after all call
sites switch.

For a live local session:

```go
data, err := database.LoadArtifactExportData(
	ctx, sessionID, artifactExportLoadLimits(limits),
)
if err != nil {
	return "", false, fmt.Errorf(
		"loading bounded artifact export data %s: %w",
		sessionID, classifyArtifactExportLoadError(err),
	)
}
return exportLoadedSessionToStore(
	ctx, store, origin, sess, data.Messages, data.UsageEvents, limits,
)
```

Make `ExportToStore` a production-policy wrapper:

```go
return exportToStoreWithLimits(
	ctx, database, store, opts, productionArtifactLimits(),
)
```

Move its current body to `exportToStoreWithLimits`. Add
`exportFullToStoreWithDrainRoundsAndLimits(ctx, database, store, origin, drainRounds, limits)`
and have both existing full-export wrappers pass production limits. Every
recursive queue page inside full export must call `exportToStoreWithLimits` with
the same `limits`, so small-limit tests exercise the complete path.

Use `exportClaimedSessionToStore` in the normal claimed loop and in full
dependency recovery. Keep full recovery fail-fast because it has no claim to
finalize.

- [ ] **Step 5: Verify bounded loading and deterministic classification**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/artifact ./internal/db \
  -run 'ArtifactExport|ExportRejects|ExportUsesBounded|LoadArtifactExport' \
  -count=1 -v
```

Expected: PASS.

- [ ] **Step 6: Commit Task 4**

```bash
git add internal/artifact/limits.go internal/artifact/export.go \
  internal/artifact/export_limits_test.go
git commit -m "fix(artifact): enforce limits before export materialization" \
  -m "Artifact wire guards were too late to protect construction memory. Route every session export through the bounded SQLite snapshot and classify only deterministic policy failures as rejectable."
```

______________________________________________________________________

### Task 5: Isolate Deterministic Failures Within a Publication Batch

**Files:**

- Modify: `internal/artifact/export.go:33-44,95-280,405-425`
- Modify: `internal/artifact/export_test.go`
- Modify: `internal/artifact/export_limits_test.go`
- Modify: test wrappers overriding `AcknowledgeArtifactExports` or
  `RecordArtifactCheckpointHead`

**Interfaces:**

- Extends:

```go
type ExportResult struct {
	ExportedSessions   int
	RejectedSessions   int
	CheckpointCreated  bool
	CheckpointSequence int
}
```

- `artifactExportStore` consumes:

```go
FinalizeArtifactExports(context.Context, []db.ArtifactExportOutcome) error
RecordArtifactCheckpointHeadOutcomes(
	context.Context, db.ArtifactCheckpointHead, []db.ArtifactExportOutcome,
) error
```

- [ ] **Step 1: Write the mixed FIFO regression**

Seed an already-published session `rejected` and a later valid session `valid`.
Mutate `rejected` to exceed a small test limit, ensure its `enqueued_at` sorts
first, then export both through `exportToStoreWithLimits`.

Assert observable authority:

```go
require.NoError(t, err)
assert.Equal(t, 1, result.ExportedSessions)
assert.Equal(t, 1, result.RejectedSessions)
checkpoint := latestStoreCheckpointForTest(t, store, origin)
assert.NotContains(t, checkpoint.Sessions, origin+"~rejected")
assert.Contains(t, checkpoint.Sessions, origin+"~valid")
pending, err := database.PendingArtifactExports(t.Context(), 10)
require.NoError(t, err)
assert.Empty(t, pending)
rejection, ok, err := database.GetArtifactExportRejection(
	t.Context(), "rejected",
)
require.NoError(t, err)
require.True(t, ok)
assert.Contains(t, rejection.Error, "message limit")
```

This test must fail against the current early-return loop with the valid session
absent and both claims pending.

- [ ] **Step 2: Add transient and crash-boundary regressions**

Add separate tests:

- a store that fails `Create` for `rejected` returns the injected error, records
  no rejection, and leaves both claims pending;
- a store that fails checkpoint creation after one deterministic rejection
  leaves both claims pending and retains prior checkpoint authority;
- a mutation between collection and checkpoint finalization returns
  `db.ErrArtifactExportClaimStale`, leaves the newer generation pending, and
  records no stale rejection.

Use different store doubles for each branch and assert exact call arguments and
counts.

- [ ] **Step 3: Run the new tests and observe FIFO starvation**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/artifact \
  -run 'TestExportContinuesPastDeterministicRejection|TestExportTransientFailureDoesNotReject|TestExportRejectionWaitsForCheckpoint|TestExportStaleRejectionCannotConsumeMutation' \
  -count=1 -v
```

Expected: FAIL because the first deterministic error aborts the batch.

- [ ] **Step 4: Collect per-claim outcomes**

Refactor the session loop to use:

```go
changes := make([]db.ArtifactPublicationChange, 0, len(claims))
outcomes := make([]db.ArtifactExportOutcome, 0, len(claims))
```

For missing/deleted/foreign claimed sessions, append a delete change and a
successful outcome.

For live sessions:

```go
manifestHash, created, err := exportClaimedSessionToStore(
	ctx, database, store, opts.Origin, sess, limits,
)
if err != nil {
	if !claimed || !isDeterministicArtifactExportError(err) {
		return result, err
	}
	changes = append(changes, db.ArtifactPublicationChange{
		SessionID: sessionID, Generation: claim.Generation, Delete: true,
	})
	outcomes = append(outcomes, db.ArtifactExportOutcome{
		Item: claim, Rejection: err.Error(),
	})
	result.RejectedSessions++
	continue
}
if created && claimed {
	result.ExportedSessions++
}
if claimed {
	changes = append(changes, db.ArtifactPublicationChange{
		SessionID: sessionID, Generation: claim.Generation,
		ManifestHash: manifestHash, SourceFingerprint: manifestHash,
	})
	outcomes = append(outcomes, db.ArtifactExportOutcome{Item: claim})
}
```

Do not classify a deterministic failure from the unclaimed full-recovery pass as
a rejection.

- [ ] **Step 5: Finalize outcomes only behind checkpoint authority**

In the verified-unchanged-head fast path, replace acknowledgement with:

```go
if err := database.FinalizeArtifactExports(ctx, outcomes); err != nil {
	return result, err
}
```

In both checkpoint record paths, call:

```go
database.RecordArtifactCheckpointHeadOutcomes(ctx, head, outcomes)
```

Update every test wrapper that requeues after finalization to override the new
methods and delegate to the embedded `*db.DB`, preserving the existing
concurrent-write tests.

Update `mergeArtifactExportResult`:

```go
total.ExportedSessions += page.ExportedSessions
total.RejectedSessions += page.RejectedSessions
```

- [ ] **Step 6: Verify mixed success, transient failure, and full drains**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/artifact \
  -run 'ExportContinuesPast|ExportTransient|ExportRejection|ExportStale|ExportFullDrain' \
  -count=1 -v
```

Expected: PASS.

- [ ] **Step 7: Commit Task 5**

```bash
git add internal/artifact/export.go internal/artifact/export_test.go \
  internal/artifact/export_limits_test.go
git commit -m "fix(artifact): isolate deterministic claim failures" \
  -m "One permanently invalid FIFO entry must not block unrelated publication work. Delete stale authority, checkpoint successful claims, and finalize generation-scoped rejection diagnostics together."
```

______________________________________________________________________

### Task 6: Complete Lifecycle Coverage and Repository Verification

**Files:**

- Modify: `internal/db/artifact_publication_test.go`
- Modify: `internal/artifact/origin_test.go`

**Interfaces:**

- Consumes all interfaces from Tasks 1–5.

- Produces no new production API.

- [ ] **Step 1: Add the end-to-end lifecycle regression**

Add one real-store test in `internal/artifact/origin_test.go` that covers:

1. publish a session under origin A;
1. force a deterministic rejection and verify the next A checkpoint removes it;
1. mutate the session and verify rejection state clears and a later A checkpoint
   republishes it;
1. adopt origin B and verify all requeued rows have no stale rejection fields.

Expected checkpoint maps must use literal session GIDs and explicit
`Contains`/`NotContains` assertions.

- [ ] **Step 2: Run the lifecycle regression**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/artifact ./internal/db \
  -run 'Artifact.*Rejection|Origin.*Rejection|LoadArtifactExport|ExportContinuesPast' \
  -count=1 -v
```

Expected: PASS.

- [ ] **Step 3: Format and run complete affected-package tests**

```bash
go fmt ./...
git diff --check
CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/artifact -count=1
```

Expected: PASS with no formatting errors.

- [ ] **Step 4: Run repository verification**

Run:

```bash
make test-short
go vet ./...
```

Expected: both commands exit 0.

- [ ] **Step 5: Inspect the final diff and private-data hygiene**

```bash
git status --short
git diff --stat
git diff HEAD
git diff --check
git log --format='%s%n%b%n---' -10
```

Confirm the diff contains no private hostnames, identities, absolute paths,
credentials, generated attribution blocks, or unrelated changes.

- [ ] **Step 6: Commit lifecycle tests**

```bash
git add internal/db/artifact_publication_test.go \
  internal/artifact/origin_test.go
git commit -m "test(artifact): cover rejected claim lifecycle" \
  -m "Rejection correctness spans checkpoint removal, generation-driven retry, resync, and origin adoption. Protect the complete lifecycle with real database and artifact-store authority checks."
```

- [ ] **Step 7: Request focused code review**

Dispatch a read-only reviewer against the implementation range. Require review
of origin locking, snapshot lifetime, bounded query shapes, error
classification, checkpoint ordering, resync migration, and tests. Resolve every
Critical or Important issue before publishing.

- [ ] **Step 8: Push the completed branch**

After confirming a clean working tree and matching local tests:

```bash
git push origin artifact/2-export-ledger
```

Do not poll CI unless explicitly requested.
