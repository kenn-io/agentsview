# Artifact Normalized Import Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Import foreign normalized artifact checkpoints from an existing
`ArtifactStore` into SQLite through a durable, bounded, idempotent pipeline.

**Architecture:** Add SQLite authority for exact pending checkpoint identities,
highest observed peer heads, per-session import provenance, and fully landed
checkpoint maps. A store coordinator verifies and decodes one bounded claim at a
time, imports complete manifest/segment closures, and acknowledges a checkpoint
only after its landing state is durable. Folder transport, CLI, metadata replay,
raw-source stewardship, and provider-file eviction remain separate changes.

**Tech Stack:** Go, SQLite/FTS5, Docbank v0.11, canonical artifact JSON/NDJSON,
`github.com/stretchr/testify`.

## Global Constraints

- Base implementation work on current `origin/main`; do not extract the
  reference branch wholesale.
- Use a fresh worktree and branch named `artifact/3-normalized-import`.
- Preserve the frozen artifact wire. Checkpoints are v1, manifests are v2, and
  message segments are v1.
- `raw_source` remains optional, is validated when present, and is not fetched
  or populated by normalized import.
- Do not add folder transport, CLI commands, metadata events, watch mode, source
  stewardship, or provider-file deletion.
- Keep import work bounded to 128 checkpoint claims per drain and one session
  closure at a time.
- Enforce the existing decoded limits exactly: checkpoints 64 MiB, manifests 16
  MiB, segments 64 MiB, sessions 256 MiB, 32,768 messages, 32,768 usage
  events, 65,536 tool calls, and 262,144 result events.
- Artifact kinds have independent schema versions. Durable deferral must not
  compare a future manifest version to the checkpoint version.
- Accept semantically valid noncanonical peer JSON/NDJSON under its verified
  stored-byte identity. Reject trailing JSON; do not reject content solely for
  whitespace, object-key order, or equivalent escaping.
- A future checkpoint is authoritative even if its `sessions` representation
  differs from v1. Retain it without interpreting current-version fields.
- Missing dependencies defer the claim. Complete invalid content is quarantined.
  Operational store or database errors remain retryable.
- Checkpoint absence never deletes a normalized session.
- Ignore artifacts belonging to the configured local origin.
- Permanently excluded or locally trashed sessions count as intentionally
  satisfied; import must not resurrect them or prevent the checkpoint landing.
- Imported secret-derived state is cleared so the session is locally unscanned.
  Do not trust a count without local findings.
- Use non-destructive `CREATE TABLE IF NOT EXISTS` schema additions.
- Use `testify`; use `require` for setup and index-safety checks and `assert`
  for independent observations.
- After Go changes run `go fmt ./...`, `go vet ./...`, and the relevant tests.
- Follow the private-data scrub and current repository commit rules. Do not add
  attribution blocks.

______________________________________________________________________

## File Map

### Create

- `internal/db/artifact_import.go` — durable import queue, peer-head, landing,
  and per-session provenance APIs.
- `internal/db/artifact_import_test.go` — real-SQLite contracts for those APIs.
- `internal/artifact/import.go` — public coordinator, bounded queue drain, and
  result accounting.
- `internal/artifact/import_checkpoint.go` — verified checkpoint reads,
  current/future decoding, and checkpoint validation.
- `internal/artifact/import_session.go` — manifest/segment closure loading,
  validation, and normalized session rewrite.
- `internal/artifact/import_test.go` — real Docbank end-to-end import and crash
  recovery tests.
- `internal/artifact/import_checkpoint_test.go` — version, canonicality,
  corruption, and decoded-limit tests.
- `internal/artifact/import_session_test.go` — rewrite, limits, missing
  dependency, exclusion, and secret-state tests.

### Modify

- `internal/db/schema.sql` — add import-ledger tables and indexes.
- `internal/db/db.go` — require the new tables for read-only schema
  compatibility.
- `internal/db/orphaned.go` — preserve import authority across full-resync
  database swaps.
- `internal/db/orphaned_test.go` — verify resync-copy preservation and old-DB
  compatibility.
- `internal/db/legacy_schema_test.go` — verify non-destructive creation on a
  pre-import schema.
- `internal/artifact/limits.go` — add a typed future-version error that still
  unwraps to `errFutureArtifactVersion`.
- `internal/artifact/wire_decode.go` — return typed manifest/segment future
  versions and retain existing bounded preflight.

## Interfaces Locked by This Plan

The following names and signatures are shared across tasks:

```go
// internal/db/artifact_import.go
var ErrArtifactImportConflict = errors.New("artifact import conflict")

type ArtifactImportVersions struct {
	Checkpoint int
	Manifest   int
	Segment    int
}

type ArtifactImportWork struct {
	Origin                    string
	Kind                      string
	Name                      string
	SHA256                    string
	Size                      int64
	RequiredCheckpointVersion int
	RequiredManifestVersion   int
	RequiredSegmentVersion    int
	AttemptGeneration         int64
	EnqueuedAt                string
}

type ArtifactPeerCheckpointHead struct {
	Origin           string
	Sequence         int
	CheckpointSHA256 string
	CheckpointSize   int64
}

type ArtifactCheckpointLanding struct {
	Origin           string
	Sequence         int
	CheckpointSHA256 string
	CheckpointSize   int64
}

type ArtifactImportedSession struct {
	Origin            string
	GID               string
	ManifestHash      string
	ImportedSessionID string
}

func (db *DB) EnqueueArtifactImport(context.Context, ArtifactImportWork) error
func (db *DB) PendingArtifactImports(
	context.Context, ArtifactImportVersions, int64, int,
) ([]ArtifactImportWork, error)
func (db *DB) ReserveArtifactImportAttemptGeneration(
	context.Context,
) (int64, error)
func (db *DB) MarkArtifactImportAttempted(
	context.Context, ArtifactImportWork, int64,
) (bool, error)
func (db *DB) AcknowledgeArtifactImport(
	context.Context, ArtifactImportWork,
) (bool, error)
func (db *DB) ArtifactImportQueueStats(context.Context) (int, string, error)
func (db *DB) RecordArtifactPeerCheckpointHead(
	context.Context, ArtifactPeerCheckpointHead,
) (bool, error)
func (db *DB) GetArtifactPeerCheckpointHead(
	context.Context, string,
) (ArtifactPeerCheckpointHead, bool, error)
func (db *DB) RecordArtifactCheckpointLanding(
	context.Context, ArtifactCheckpointLanding, map[string]string,
) error
func (db *DB) GetArtifactCheckpointLanding(
	context.Context, string,
) (ArtifactCheckpointLanding, map[string]string, bool, error)
func (db *DB) ArtifactImportedManifestHashes(
	context.Context, string, []string,
) (map[string]string, error)
func (db *DB) RecordArtifactImportedSession(
	context.Context, ArtifactImportedSession,
) error
```

```go
// internal/artifact/import.go
type ImportResult struct {
	Sessions    int
	Messages    int
	Deferred    int
	Quarantined int
	More        bool
}

type StoreImportCoordinator struct {
	database    *db.DB
	store       ArtifactStore
	localOrigin string
	runMu       sync.Mutex
	signalMu    sync.Mutex
	generation  uint64
	completed   uint64
	activeAttemptGeneration int64
}

func NewStoreImportCoordinator(
	database *db.DB, store ArtifactStore, localOrigin string,
) *StoreImportCoordinator
func (c *StoreImportCoordinator) RecordChanged(
	context.Context, Entry,
) error
func (c *StoreImportCoordinator) Finalize(
	context.Context,
) (ImportResult, error)
```

```go
// internal/artifact/limits.go
type futureArtifactVersionError struct {
	Kind    Kind
	Version int
}

func (e *futureArtifactVersionError) Error() string
func (e *futureArtifactVersionError) Unwrap() error
```

______________________________________________________________________

### Task 1: Add Durable Import Queue and Peer-Head Authority

**Files:**

- Create: `internal/db/artifact_import.go`
- Create: `internal/db/artifact_import_test.go`
- Modify: `internal/db/schema.sql`
- Modify: `internal/db/db.go`
- Modify: `internal/db/legacy_schema_test.go`

**Interfaces:**

- Consumes: existing `DB` reader/writer handles and `db.mu`.

- Produces: `ArtifactImportWork`, `ArtifactImportVersions`,
  `ArtifactPeerCheckpointHead`, queue APIs, and peer-head APIs from the locked
  interface above.

- [ ] **Step 1: Write failing schema and queue tests**

Add table-driven tests using `testDB(t)`:

```go
func TestArtifactImportQueueExactClaimsAndVersionGates(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	work := ArtifactImportWork{
		Origin: "peer-a1b2c3", Kind: "checkpoints",
		Name: "cp-0000000002.json", SHA256: strings.Repeat("a", 64),
		Size: 42, RequiredCheckpointVersion: 1,
		RequiredManifestVersion: 2, RequiredSegmentVersion: 1,
	}
	require.NoError(t, database.EnqueueArtifactImport(ctx, work))
	attempt, err := database.ReserveArtifactImportAttemptGeneration(ctx)
	require.NoError(t, err)

	pending, err := database.PendingArtifactImports(
		ctx, ArtifactImportVersions{Checkpoint: 1, Manifest: 2, Segment: 1},
		attempt, 10,
	)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, work.Name, pending[0].Name)

	future := pending[0]
	future.RequiredManifestVersion = 3
	require.NoError(t, database.EnqueueArtifactImport(ctx, future))
	pending, err = database.PendingArtifactImports(
		ctx, ArtifactImportVersions{Checkpoint: 1, Manifest: 2, Segment: 1},
		attempt, 10,
	)
	require.NoError(t, err)
	assert.Empty(t, pending)

	pending, err = database.PendingArtifactImports(
		ctx, ArtifactImportVersions{Checkpoint: 1, Manifest: 3, Segment: 1},
		attempt, 10,
	)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	acknowledged, err := database.AcknowledgeArtifactImport(ctx, pending[0])
	require.NoError(t, err)
	assert.True(t, acknowledged)
}
```

Also test:

- enqueueing the same reference and identity is idempotent;

- the same reference with another SHA or size returns
  `ErrArtifactImportConflict`;

- checkpoint sequence 3 deletes queued sequences 1 and 2 for that origin;

- an older checkpoint observed after sequence 3 is ignored;

- an acknowledgement with a changed timestamp or identity deletes nothing;

- a claim marked attempted disappears from the same attempt generation but
  reappears under the next reserved generation;

- 129 deferred claims paginate as 128 then 1 without retrying the first page;

- limits 0 and 1,025 are rejected;

- `ArtifactImportQueueStats` counts future-version rows; and

- a legacy database opened writable gains every new table without losing
  sessions.

- [ ] **Step 2: Run the queue tests and observe the expected failure**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db \
  -run 'TestArtifactImportQueue|TestLegacySchema.*ArtifactImport' -count=1
```

Expected: compilation fails because `ArtifactImportWork` and its tables do not
exist.

- [ ] **Step 3: Add the queue and peer-head schema**

Add this schema, preserving the independent version columns:

```sql
CREATE TABLE IF NOT EXISTS artifact_import_queue (
    origin                      TEXT NOT NULL,
    kind                        TEXT NOT NULL,
    name                        TEXT NOT NULL,
    sha256                      TEXT NOT NULL,
    size                        INTEGER NOT NULL CHECK (size >= 0),
    required_checkpoint_version INTEGER NOT NULL CHECK (
        required_checkpoint_version >= 1
    ),
    required_manifest_version   INTEGER NOT NULL CHECK (
        required_manifest_version >= 1
    ),
    required_segment_version    INTEGER NOT NULL CHECK (
        required_segment_version >= 1
    ),
    attempt_generation          INTEGER NOT NULL DEFAULT 0 CHECK (
        attempt_generation >= 0
    ),
    enqueued_at                 TEXT NOT NULL DEFAULT (
        strftime('%Y-%m-%dT%H:%M:%fZ','now')
    ),
    PRIMARY KEY (origin, kind, name)
);

CREATE INDEX IF NOT EXISTS idx_artifact_import_queue_pending
ON artifact_import_queue (
    required_checkpoint_version,
    required_manifest_version,
    required_segment_version,
    attempt_generation,
    enqueued_at,
    origin,
    kind,
    name
);

CREATE TABLE IF NOT EXISTS artifact_import_attempt_generations (
    singleton  INTEGER PRIMARY KEY CHECK (singleton = 1),
    generation INTEGER NOT NULL CHECK (generation >= 0)
);

CREATE TABLE IF NOT EXISTS artifact_peer_checkpoint_heads (
    origin            TEXT PRIMARY KEY,
    sequence          INTEGER NOT NULL CHECK (sequence >= 1),
    checkpoint_sha256 TEXT NOT NULL,
    checkpoint_size   INTEGER NOT NULL CHECK (checkpoint_size >= 0)
);
```

Append all three tables to `readOnlyRequiredTables`. Update the legacy-schema
fixture to assert they are created non-destructively.

- [ ] **Step 4: Implement exact queue validation and comparison**

Implement `validateArtifactImportWork` with these rules:

```go
func validateArtifactImportWork(work ArtifactImportWork, requireClaim bool) error {
	if strings.TrimSpace(work.Origin) == "" ||
		work.Origin != strings.TrimSpace(work.Origin) {
		return errors.New("artifact import origin is required")
	}
	if work.Kind != "checkpoints" {
		return errors.New("artifact import kind must be checkpoints")
	}
	if _, err := artifactImportCheckpointSequence(work.Name); err != nil {
		return err
	}
	if len(work.SHA256) != 64 || work.Size < 0 {
		return errors.New("complete artifact import identity is required")
	}
	for _, version := range []int{
		work.RequiredCheckpointVersion,
		work.RequiredManifestVersion,
		work.RequiredSegmentVersion,
	} {
		if version < 1 {
			return errors.New("artifact import versions must be positive")
		}
	}
	if requireClaim && strings.TrimSpace(work.EnqueuedAt) == "" {
		return errors.New("artifact import enqueue time is required")
	}
	return validateLowerHex(work.SHA256)
}
```

Keep `validateLowerHex` and `artifactImportCheckpointSequence` private to
`internal/db/artifact_import.go`. The sequence parser must require exactly
`cp-%010d.json` and a positive value.

`EnqueueArtifactImport` must:

1. begin a writer transaction under `db.mu`;
1. reject a conflicting identity for the same primary key;
1. refresh each required version with `max(existing, incoming)`;
1. ignore an older checkpoint when a higher sequence is already queued;
1. delete lower queued checkpoint sequences for the same origin; and
1. commit without changing the original FIFO timestamp on idempotent refresh.

`PendingArtifactImports` selects only rows for which all three required versions
are understood and `attempt_generation` is below the active generation.
`ReserveArtifactImportAttemptGeneration` atomically inserts generation 1 or
increments the singleton and returns it. `MarkArtifactImportAttempted`
compare-updates the exact identity, required versions, prior attempt generation,
and `enqueued_at`; stale work returns `false` without changing the newer row.
`AcknowledgeArtifactImport` compare-deletes every field, including
`attempt_generation` and `enqueued_at`.

- [ ] **Step 5: Implement monotonic peer-head recording**

Use one writer transaction. Return values mean:

- `(true, nil)` for a newly inserted or higher sequence;
- `(false, nil)` for an older sequence or the exact same sequence and identity;
- `(false, ErrArtifactImportConflict)` for the same sequence with another
  identity.

Do not let an older observation regress the row.

- [ ] **Step 6: Run focused DB tests**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db \
  -run 'TestArtifactImportQueue|TestArtifactPeerCheckpoint|TestLegacySchema' \
  -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit the queue authority**

```bash
git add internal/db/schema.sql internal/db/db.go \
  internal/db/artifact_import.go internal/db/artifact_import_test.go \
  internal/db/legacy_schema_test.go
git commit -m "feat(artifact): add durable import claims" \
  -m "Foreign checkpoint work must survive restarts without conflating independently versioned artifact schemas. Persist exact identities, version gates, and monotonic peer heads before any content is consumed."
```

______________________________________________________________________

### Task 2: Add Landing, Provenance, and Resync Preservation

**Files:**

- Modify: `internal/db/schema.sql`
- Modify: `internal/db/artifact_import.go`
- Modify: `internal/db/artifact_import_test.go`
- Modify: `internal/db/db.go`
- Modify: `internal/db/orphaned.go`
- Modify: `internal/db/orphaned_test.go`

**Interfaces:**

- Consumes: Task 1 peer-head authority.

- Produces: `ArtifactCheckpointLanding`, `ArtifactImportedSession`, landing
  APIs, and provenance APIs from the locked interface.

- [ ] **Step 1: Write failing landing and resync-copy tests**

Cover exact landing replacement and provenance:

```go
func TestArtifactCheckpointLandingBindsPeerIdentity(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	head := ArtifactPeerCheckpointHead{
		Origin: "peer-a1b2c3", Sequence: 2,
		CheckpointSHA256: strings.Repeat("a", 64), CheckpointSize: 99,
	}
	_, err := database.RecordArtifactPeerCheckpointHead(ctx, head)
	require.NoError(t, err)

	landing := ArtifactCheckpointLanding{
		Origin: head.Origin, Sequence: head.Sequence,
		CheckpointSHA256: head.CheckpointSHA256,
		CheckpointSize: head.CheckpointSize,
	}
	want := map[string]string{
		head.Origin + "~one": strings.Repeat("b", 64),
		head.Origin + "~two": strings.Repeat("c", 64),
	}
	require.NoError(t,
		database.RecordArtifactCheckpointLanding(ctx, landing, want))

	gotLanding, got, found, err :=
		database.GetArtifactCheckpointLanding(ctx, head.Origin)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, landing, gotLanding)
	assert.Equal(t, want, got)
}
```

Also test:

- landing an identity other than the recorded peer head conflicts;

- an older landing cannot replace a newer landing;

- same landing and map are idempotent;

- a higher landing atomically replaces the old map;

- provenance queries return only requested GIDs;

- recording the same provenance is idempotent and a new manifest advances it;

- `CopySyncStateFrom` preserves queue rows, peer heads, landings, landing maps,
  and provenance; and

- copying from an older database without these tables succeeds.

- [ ] **Step 2: Run tests and observe missing APIs**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db \
  -run 'TestArtifactCheckpointLanding|TestArtifactImported|TestCopySyncState.*ArtifactImport' \
  -count=1
```

Expected: compilation fails for the landing and provenance types.

- [ ] **Step 3: Add landing and provenance tables**

```sql
CREATE TABLE IF NOT EXISTS artifact_checkpoint_landings (
    origin            TEXT PRIMARY KEY,
    sequence          INTEGER NOT NULL CHECK (sequence >= 1),
    checkpoint_sha256 TEXT NOT NULL,
    checkpoint_size   INTEGER NOT NULL CHECK (checkpoint_size >= 0)
);

CREATE TABLE IF NOT EXISTS artifact_checkpoint_landing_sessions (
    origin        TEXT NOT NULL,
    gid           TEXT NOT NULL,
    manifest_hash TEXT NOT NULL,
    PRIMARY KEY (origin, gid),
    FOREIGN KEY (origin) REFERENCES artifact_checkpoint_landings(origin)
        ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS artifact_imported_sessions (
    origin              TEXT NOT NULL,
    gid                 TEXT NOT NULL,
    manifest_hash       TEXT NOT NULL,
    imported_session_id TEXT NOT NULL,
    imported_at         TEXT NOT NULL DEFAULT (
        strftime('%Y-%m-%dT%H:%M:%fZ','now')
    ),
    PRIMARY KEY (origin, gid)
);
```

Add the tables to `readOnlyRequiredTables`.

- [ ] **Step 4: Implement identity-bound landing replacement**

Within one writer transaction:

1. read the peer head for `landing.Origin`;
1. require sequence, SHA, and size to equal the head;
1. reject regression below the existing landing;
1. upsert the landing;
1. delete the old landing map;
1. insert the new map in sorted GID order; and
1. commit.

Validate every GID has the `origin + "~"` prefix and every manifest hash is
lowercase SHA-256.

- [ ] **Step 5: Implement bounded provenance reads**

`ArtifactImportedManifestHashes` must deduplicate requested GIDs, reject more
than 1,024 IDs, and issue one bounded `IN` query. Return a map keyed by GID.

`RecordArtifactImportedSession` upserts only when the manifest hash or imported
session ID changes. It must not delete historical normalized sessions when a
later checkpoint omits them.

- [ ] **Step 6: Preserve every import table during resync**

Extend `CopySyncStateFrom` table probing and copying. Copy parent tables before
child landing rows:

```text
artifact_import_queue
artifact_import_attempt_generations
artifact_peer_checkpoint_heads
artifact_checkpoint_landings
artifact_checkpoint_landing_sessions
artifact_imported_sessions
```

For queue conflicts, retain the newer sequence, maximum required versions, and
maximum attempt generation. Preserve the maximum reserved attempt generation.
For peer heads and landings, retain the higher sequence; equal-sequence
different identities must abort the resync copy rather than pick one. Preserve
the exact landing map only for the retained landing identity.

- [ ] **Step 7: Run DB and resync tests**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db \
  -run 'TestArtifactCheckpointLanding|TestArtifactImported|TestCopySyncState' \
  -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit landing and resync authority**

```bash
git add internal/db/schema.sql internal/db/db.go \
  internal/db/artifact_import.go internal/db/artifact_import_test.go \
  internal/db/orphaned.go internal/db/orphaned_test.go
git commit -m "feat(artifact): preserve import landing authority" \
  -m "A rebuilt normalized database must retain which peer checkpoint and manifests were durably consumed. Bind landing maps to exact peer identities and copy that authority across full-resync swaps."
```

______________________________________________________________________

### Task 3: Add Bounded Verified Decoding

**Files:**

- Create: `internal/artifact/import_checkpoint.go`
- Create: `internal/artifact/import_checkpoint_test.go`
- Modify: `internal/artifact/limits.go`
- Modify: `internal/artifact/wire_decode.go`

**Interfaces:**

- Consumes: `ArtifactStore`, `Entry`, existing decoded limits, current wire
  structs, and `errFutureArtifactVersion`.

- Produces: typed future-version errors, `importCheckpoint`, and bounded
  verified-read helpers used by Tasks 4 and 5.

- [ ] **Step 1: Write failing typed-version behavior tests**

Feed literal future-version manifest and segment bytes through
`decodeManifestWithLimits` and `decodeSegmentWithLimits`. Assert the observable
decoder error still satisfies `errors.Is(err, errFutureArtifactVersion)` and
that `errors.As` reports the exact kind and input version. These tests fail if a
decoder loses the dependency kind, reports the wrong version, or stops
preserving the existing sentinel contract. Include a complete future segment
whose record count exceeds the current v1 per-segment limit: authentication and
version detection must precede all current-schema cardinality and nested
collection checks. Do not test the error struct's constructor or fields in
isolation.

- [ ] **Step 2: Implement the typed error**

```go
func (e *futureArtifactVersionError) Error() string {
	return fmt.Sprintf("%s has future artifact version %d", e.Kind, e.Version)
}

func (e *futureArtifactVersionError) Unwrap() error {
	return errFutureArtifactVersion
}
```

Return it from future manifest and message-segment decoding without changing
existing `errors.Is` behavior. Segment preflight first scans the complete
byte-bounded NDJSON body for one consistent record version. A future version
returns the typed error only after that complete structural scan; current v1
record, tool-call, and result-event limits are applied only after the version is
known to be current.

- [ ] **Step 3: Write failing checkpoint-decoder tests**

Use table cases for:

- canonical current checkpoint;
- current checkpoint with whitespace and reordered keys, which succeeds;
- escaped current field names, which succeeds semantically;
- trailing JSON, unknown current fields, wrong origin, wrong sequence name,
  malformed GID, invalid manifest hash, and duplicate JSON keys, which fail;
- future checkpoint with a scalar or array `sessions` field, which returns a
  typed future checkpoint error rather than current-schema corruption;
- future checkpoint with additional fields, which defers;
- old checkpoint version, which fails; and
- `checkpointDecodedLimit + 1`, which fails before allocation.

The current-version success assertion is:

```go
assert.Equal(t, importCheckpoint{
	Version: 1, Origin: origin, Sequence: 7,
	Sessions: map[string]string{
		origin + "~session": strings.Repeat("a", 64),
	},
}, got)
```

- [ ] **Step 4: Run decoder tests and observe missing symbols**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/artifact \
  -run 'TestFutureArtifactVersion|TestDecodeImportCheckpoint|TestReadVerifiedImport' \
  -count=1
```

Expected: compilation fails for `importCheckpoint` and its decoder.

- [ ] **Step 5: Implement bounded verified reads**

Define:

```go
func readVerifiedImportArtifact(
	ctx context.Context,
	store ArtifactStore,
	entry Entry,
	decodedLimit int64,
) ([]byte, error)
```

The function must:

1. validate the reference and identity;
1. reject `entry.Identity.Size > decodedLimit` as `ErrArtifactInvalid`;
1. open the exact reference;
1. require the returned entry identity to equal the claim;
1. read through `io.LimitReader(reader, decodedLimit+1)`;
1. reject a result above the limit;
1. call `Verify` and `Close`;
1. preserve operational errors rather than wrapping them as corrupt; and
1. require the decoded byte count to equal `entry.Identity.Size`.

Use a pooled 32 KiB buffer or `bytes.Buffer.Grow` bounded by the claimed size;
do not allocate from an unchecked size.

- [ ] **Step 6: Implement semantic checkpoint decoding**

Define:

```go
type importCheckpoint struct {
	Version  int
	Origin   string
	Sequence int
	Sessions map[string]string
}
```

First scan the JSON object into `map[string]json.RawMessage` with a token-level
duplicate-key check and an EOF check. Decode `v` before applying the current
schema:

- if `v > checkpointFormatVersion`, return
  `futureArtifactVersionError{Kind: KindCheckpoints, Version: v}` after
  confirming only that the complete value is authenticated JSON;
- if `v < checkpointFormatVersion`, reject it;
- for current v1 require exactly `origin`, `seq`, `sessions`, and `v`;
- validate origin and filename sequence; and
- decode the session map, enforcing origin-prefixed GIDs and lowercase manifest
  hashes.

Do not compare re-encoded bytes to the stored identity. The verified stored
bytes are authoritative even when whitespace, key order, or escaping differs.

- [ ] **Step 7: Run decoder and existing export tests**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/artifact \
  -run 'TestFutureArtifactVersion|TestDecodeImportCheckpoint|TestReadVerifiedImport|TestDecodeCanonicalCheckpoint' \
  -count=1
```

Expected: PASS. Existing strict export checkpoint authority remains strict; only
the import decoder accepts noncanonical syntax.

- [ ] **Step 8: Commit bounded import decoding**

```bash
git add internal/artifact/limits.go internal/artifact/wire_decode.go \
  internal/artifact/import_checkpoint.go \
  internal/artifact/import_checkpoint_test.go
git commit -m "feat(artifact): decode verified import checkpoints" \
  -m "Peer bytes need bounded authentication without rejecting harmless JSON representation differences. Separate strict local checkpoint recovery from semantic import decoding and retain exact future-version requirements."
```

______________________________________________________________________

### Task 4: Load and Rewrite One Session Closure

**Files:**

- Create: `internal/artifact/import_session.go`
- Create: `internal/artifact/import_session_test.go`
- Modify: `internal/artifact/wire_decode.go`

**Interfaces:**

- Consumes: Task 3 verified reads, `manifest`, `decodeManifestWithLimits`,
  `decodeSegmentWithLimits`, `db.SessionBatchWrite`, and Task 2 provenance.
- Produces:

```go
type importClosureOutcome uint8

const (
	importClosureComplete importClosureOutcome = iota
	importClosureDeferred
	importClosureInvalid
)

func loadImportedSession(
	context.Context,
	*db.DB,
	ArtifactStore,
	string,
	string,
	string,
	artifactLimits,
) (db.SessionBatchWrite, importClosureOutcome, error)

func rewriteManifestForImport(
	manifest, []db.Message,
) db.SessionBatchWrite
```

- [ ] **Step 1: Write failing rewrite tests**

Build a manifest containing source file metadata, relationships, usage money,
quality signals, and a nonzero secret count. Assert:

```go
write := rewriteManifestForImport(m, messages)
assert.Equal(t, m.Origin+"~"+m.NativeSessionID, write.Session.ID)
assert.Equal(t, m.Origin, write.Session.Machine)
assert.Nil(t, write.Session.FilePath)
assert.Nil(t, write.Session.FileSize)
assert.Nil(t, write.Session.FileMtime)
assert.Nil(t, write.Session.FileInode)
assert.Nil(t, write.Session.FileDevice)
assert.Nil(t, write.Session.FileHash)
assert.Zero(t, write.Session.SecretLeakCount)
assert.Empty(t, write.Signals.SecretsRulesVersion)
assert.Equal(t, m.SessionQualitySignals.dbQualitySignals(),
	write.Session.StoredQualitySignals())
require.Len(t, write.UsageEvents, 1)
assert.Equal(t, m.UsageEvents[0].Cost, write.UsageEvents[0].Cost)
```

Also assert origin-prefixing of `SourceSessionID`, `ParentSessionID`, tool-call
subagent IDs, and result-event subagent IDs. Already-prefixed IDs remain
unchanged.

- [ ] **Step 2: Write failing closure tests**

Create manifest and segment artifacts directly in a real test store. Cover:

- complete closure;
- missing manifest;
- missing segment;
- future manifest;
- future segment;
- old or invalid manifest;
- wrong origin/session/native ID;
- native ID containing `~`;
- duplicate segment references;
- aggregate message, decoded-byte, tool-call, and result-event limits; and
- duplicate message ordinals, manifest/message count disagreement, and duplicate
  nonempty usage-event persistence keys;
- invalid or corrupt manifest and segment identities returned by either `Stat`
  or `Open`; and
- semantically valid noncanonical manifest and segment bytes.

Missing and future dependencies return `importClosureDeferred`. Complete invalid
dependencies are quarantined and return `importClosureInvalid`. Operational
reader failures return a non-nil error.

- [ ] **Step 3: Run closure tests and observe missing implementation**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/artifact \
  -run 'TestRewriteManifestForImport|TestLoadImportedSession' -count=1
```

Expected: compilation fails for the new functions.

- [ ] **Step 4: Implement manifest validation**

For current manifests require:

- `Version == manifestFormatVersion`;
- matching origin;
- nonempty native ID without `~`;
- checkpoint GID equal to `origin + "~" + native ID`;
- session wire ID equal to native ID;
- session machine equal to origin;
- at least one segment;
- unique lowercase segment hashes; and
- all existing manifest collection limits.

`raw_source`, when present, receives existing wire validation only. Normalized
import does not read `KindRaw`.

Before returning a complete closure, validate every invariant needed by the
normalized persistence schema: message ordinals are unique across all segments,
the manifest message count equals the decoded message count, the user-message
count is within the message-count range, and nonempty usage-event keys are
unique within their database uniqueness scope. Violations are deterministic
invalid closures and quarantine the manifest rather than reaching SQLite as
retryable write failures.

- [ ] **Step 5: Implement bounded closure loading**

Load the manifest first, then segments in manifest order. For each segment:

1. stat the hash-derived reference;
1. defer on not found;
1. read and verify with `segmentDecodedLimit`;
1. decode through existing nested-collection preflight;
1. reject typed future versions as deferred and propagate their kind/version;
1. accumulate decoded bytes and collection counts using overflow-safe
   `limit-current` checks; and
1. append messages only after the segment passes.

Do not perform a separate closure pre-inspection pass. One session closure is
the unit of work, and only that closure is retained in memory.

- [ ] **Step 6: Implement imported-session rewriting**

Use explicit conversion rather than JSON round trips:

```go
func rewriteManifestForImport(m manifest, messages []db.Message) db.SessionBatchWrite {
	importedID := m.Origin + "~" + m.NativeSessionID
	session := m.Session.dbSession()
	session.ID = importedID
	session.Machine = m.Origin
	session.SessionName = m.SessionName
	clearImportedSessionSourceState(&session)
	session.HasToolCalls = m.SessionHasToolCalls
	session.HasContextData = m.SessionHasContextData
	session.ApplyQualitySignals(m.SessionQualitySignals.dbQualitySignals())
	session.SecretLeakCount = 0
	session.SecretsRulesVersion = ""
	session.SourceSessionID = prefixImportedSessionID(
		m.Origin, session.SourceSessionID,
	)
	if session.ParentSessionID != nil {
		parent := prefixImportedSessionID(m.Origin, *session.ParentSessionID)
		session.ParentSessionID = &parent
	}
	for i := range messages {
		messages[i].ID = 0
		messages[i].SessionID = importedID
		for j := range messages[i].ToolCalls {
			call := &messages[i].ToolCalls[j]
			call.MessageID = 0
			call.SessionID = importedID
			call.SubagentSessionID = prefixImportedSessionID(
				m.Origin, call.SubagentSessionID,
			)
			for k := range call.ResultEvents {
				event := &call.ResultEvents[k]
				event.SubagentSessionID = prefixImportedSessionID(
					m.Origin, event.SubagentSessionID,
				)
			}
		}
	}
	return db.SessionBatchWrite{
		Session: session, Messages: messages,
		UsageEvents: importedUsageEvents(m.UsageEvents, importedID),
		Signals: signalsFromImportedSession(session),
		DataVersion: m.DataVersion, ReplaceMessages: true,
	}
}
```

Use these helpers:

```go
func clearImportedSessionSourceState(session *db.Session) {
	session.FilePath = nil
	session.FileSize = nil
	session.FileMtime = nil
	session.NextOrdinal = 0
	session.LastEntryUUID = nil
	session.FileInode = nil
	session.FileDevice = nil
	session.FileHash = nil
}

func prefixImportedSessionID(origin, id string) string {
	if id == "" || strings.Contains(id, "~") {
		return id
	}
	return origin + "~" + id
}

func importedUsageEvents(
	events []artifactUsageEvent, sessionID string,
) []db.UsageEvent {
	out := make([]db.UsageEvent, len(events))
	for i, event := range events {
		out[i] = db.UsageEvent{
			SessionID: sessionID, MessageOrdinal: event.MessageOrdinal,
			Source: event.Source, Model: event.Model,
			InputTokens: event.InputTokens, OutputTokens: event.OutputTokens,
			CacheCreationInputTokens: event.CacheCreationInputTokens,
			CacheReadInputTokens: event.CacheReadInputTokens,
			ReasoningTokens: event.ReasoningTokens, Cost: event.Cost,
			CostStatus: event.CostStatus, CostSource: event.CostSource,
			OccurredAt: event.OccurredAt, DedupKey: event.DedupKey,
		}
	}
	return out
}

func signalsFromImportedSession(session db.Session) db.SessionSignalUpdate {
	signals := db.SessionSignalUpdate{
		ToolFailureSignalCount: session.ToolFailureSignalCount,
		ToolRetryCount: session.ToolRetryCount,
		EditChurnCount: session.EditChurnCount,
		ConsecutiveFailureMax: session.ConsecutiveFailureMax,
		Outcome: session.Outcome,
		OutcomeConfidence: session.OutcomeConfidence,
		EndedWithRole: session.EndedWithRole,
		FinalFailureStreak: session.FinalFailureStreak,
		SignalsPendingSince: session.SignalsPendingSince,
		CompactionCount: session.CompactionCount,
		MidTaskCompactionCount: session.MidTaskCompactionCount,
		ContextPressureMax: session.ContextPressureMax,
		HealthScore: session.HealthScore,
		HealthGrade: session.HealthGrade,
		HasToolCalls: session.HasToolCalls,
		HasContextData: session.HasContextData,
	}
	if quality := session.StoredQualitySignals(); quality != nil {
		signals.QualitySignals = *quality
	}
	return signals
}
```

- [ ] **Step 7: Run rewrite, closure, and wire tests**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/artifact \
  -run 'TestRewriteManifestForImport|TestLoadImportedSession|TestWire|TestManifest' \
  -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit session closure import**

```bash
git add internal/artifact/import_session.go \
  internal/artifact/import_session_test.go internal/artifact/wire_decode.go
git commit -m "feat(artifact): rebuild imported session closures" \
  -m "Normalized import must reconstruct one complete bounded session while stripping source-machine state and unverified secret findings. Preserve relationships, usage money, and quality signals under origin-qualified identity."
```

______________________________________________________________________

### Task 5: Coordinate Durable Import and Landing

**Files:**

- Create: `internal/artifact/import.go`
- Create: `internal/artifact/import_test.go`
- Modify: `internal/db/artifact_import.go`
- Modify: `internal/db/artifact_import_test.go`

**Interfaces:**

- Consumes: every locked DB interface, Task 3 checkpoint decoder, and Task 4
  closure loader.

- Produces: `ImportResult`, `StoreImportCoordinator`, `RecordChanged`, and
  `Finalize`.

- [ ] **Step 1: Write failing end-to-end coordinator tests**

Use two real SQLite databases and one real Docbank store:

1. export two sessions from the source using `ExportToStore`;
1. locate the checkpoint entry;
1. construct a coordinator with a different local origin;
1. call `RecordChanged` for dependencies and checkpoint;
1. call `Finalize`; and
1. assert both imported sessions, messages, usage, signals, provenance, landing
   map, and empty eligible queue.

Add focused tests for:

- local-origin checkpoint ignored;

- replay returns zero changes;

- checkpoint 2 supersedes pending checkpoint 1;

- missing segment leaves the exact claim pending;

- adding the segment and recording it allows the next finalize to land;

- future manifest updates only `RequiredManifestVersion`;

- future segment updates only `RequiredSegmentVersion`;

- invalid complete checkpoint is quarantined and acknowledged;

- invalid dependency is quarantined while its checkpoint remains pending;

- a transient store error returns an error and retains the claim;

- one invalid origin does not prevent a valid later claim in the same 128-row
  page;

- excluded and trashed sessions are not resurrected but do not block landing;
  and

- checkpoint 2 omitting a previously imported GID leaves that normalized session
  intact.

- [ ] **Step 2: Run coordinator tests and observe missing implementation**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/artifact \
  -run 'TestStoreImportCoordinator|TestArtifactImportEndToEnd' -count=1
```

Expected: compilation fails for `NewStoreImportCoordinator`.

- [ ] **Step 3: Implement `RecordChanged`**

Validation flow:

```go
func NewStoreImportCoordinator(
	database *db.DB, store ArtifactStore, localOrigin string,
) *StoreImportCoordinator {
	return &StoreImportCoordinator{
		database: database, store: store, localOrigin: localOrigin,
		generation: 1,
	}
}

func (c *StoreImportCoordinator) requestDrain() {
	c.signalMu.Lock()
	c.generation++
	c.signalMu.Unlock()
}

func (c *StoreImportCoordinator) RecordChanged(
	ctx context.Context, entry Entry,
) error {
	if c == nil || c.database == nil || c.store == nil {
		return errors.New("artifact import coordinator is required")
	}
	if err := validateStoreRef(entry.Ref); err != nil {
		return err
	}
	if err := validateStoreIdentity(entry.Identity); err != nil {
		return err
	}
	if err := validateRefIdentity(entry.Ref, entry.Identity); err != nil {
		return err
	}
	if entry.Ref.Origin == c.localOrigin {
		return nil
	}
	if entry.Ref.Kind != KindCheckpoints {
		c.requestDrain()
		return nil
	}
	sequence, err := checkpointSequence(entry.Ref.Name)
	if err != nil {
		return err
	}
	advanced, err := c.database.RecordArtifactPeerCheckpointHead(
		ctx,
		db.ArtifactPeerCheckpointHead{
			Origin: entry.Ref.Origin, Sequence: sequence,
			CheckpointSHA256: entry.Identity.SHA256,
			CheckpointSize: entry.Identity.Size,
		},
	)
	if err != nil {
		return err
	}
	if !advanced {
		head, found, err := c.database.GetArtifactPeerCheckpointHead(
			ctx, entry.Ref.Origin,
		)
		if err != nil {
			return err
		}
		if found && head.Sequence > sequence {
			return nil
		}
	}
	err = c.database.EnqueueArtifactImport(ctx, db.ArtifactImportWork{
		Origin: entry.Ref.Origin, Kind: string(entry.Ref.Kind),
		Name: entry.Ref.Name, SHA256: entry.Identity.SHA256,
		Size: entry.Identity.Size,
		RequiredCheckpointVersion: checkpointFormatVersion,
		RequiredManifestVersion: manifestFormatVersion,
		RequiredSegmentVersion: messageSegmentFormatVersion,
	})
	if err != nil {
		return err
	}
	c.requestDrain()
	return nil
}
```

Always enqueue an exact replay of the current unlanded head; a quarantined
checkpoint may be received again with the same identity. Ignore an older head.

- [ ] **Step 4: Implement one bounded drain**

`Finalize` serializes drains with a mutex and consumes at most
`artifactImportDrainLimit = 128` eligible claims. It never loops until empty.
When no drain is active it reserves a durable attempt generation. Every deferred
or invalid-dependency claim is marked attempted for that generation, so a full
page cannot remain at the FIFO front and starve later origins.

For each claim:

1. skip and acknowledge if a newer peer head exists;
1. short-circuit and acknowledge an exact already-landed identity;
1. read and decode the checkpoint;
1. quarantine and acknowledge a complete invalid checkpoint;
1. update the exact claim's required checkpoint version on a future checkpoint;
1. query provenance for at most 1,024 GIDs at a time;
1. load and write each changed session closure;
1. treat `db.ErrSessionExcluded` and `db.ErrSessionTrashed` as satisfied and
   record suppression provenance without recreating the session;
1. record per-session provenance after its durable session write or confirmed
   suppression;
1. retain the claim if any dependency is missing, invalid, or future;
1. record the complete identity-bound landing map when every GID is satisfied;
   and
1. compare-delete the exact claim only after landing commits.

Continue after deterministic invalid content. Abort the drain on context,
database, or operational store errors, leaving the current and later claims
unchanged. If the page contains 128 claims, return `More: true` and retain the
active attempt generation; the next `Finalize` continues with claims not yet
attempted in that generation. When a shorter page completes, clear the active
attempt generation and set `More: false`. A later `RecordChanged` signal starts
a newly reserved generation in which all still-pending claims become eligible
again.

- [ ] **Step 5: Update independent future-version requirements**

When `errors.As` finds `futureArtifactVersionError`, re-enqueue the same exact
claim with only the corresponding maximum raised:

```go
switch future.Kind {
case KindCheckpoints:
	work.RequiredCheckpointVersion = future.Version
case KindManifests:
	work.RequiredManifestVersion = future.Version
case KindSegments:
	work.RequiredSegmentVersion = future.Version
}
```

Do not acknowledge the claim. `ArtifactImportQueueStats` must still report it
although `PendingArtifactImports` hides it from the current reader.

- [ ] **Step 6: Run coordinator and end-to-end tests**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/artifact \
  -run 'TestStoreImportCoordinator|TestArtifactImportEndToEnd' -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit the coordinator**

```bash
git add internal/artifact/import.go internal/artifact/import_test.go \
  internal/db/artifact_import.go internal/db/artifact_import_test.go
git commit -m "feat(artifact): land normalized peer checkpoints" \
  -m "A checkpoint becomes consumed only after every available session closure and the exact landing map are durable. Coordinate retries so missing or future dependencies remain pending while unrelated valid work continues."
```

______________________________________________________________________

### Task 6: Prove Bounds, Crash Recovery, and Existing Behavior

**Files:**

- Modify: `internal/artifact/import_test.go`
- Modify: `internal/artifact/import_checkpoint_test.go`
- Modify: `internal/artifact/import_session_test.go`
- Modify: `internal/db/orphaned_test.go`

**Interfaces:**

- Consumes: complete import pipeline.

- Produces: regression evidence for bounded work, crash convergence, and
  no-double-import behavior.

- [ ] **Step 1: Add exact boundary tests**

Use test-only reduced `artifactLimits` for collection boundaries and production
constants for byte limits. Every limit must have:

- exactly-at-limit succeeds; and
- limit-plus-one defers or quarantines according to whether the version is
  unsupported or the current artifact is invalid.

Cover checkpoint decoded bytes, manifest bytes and usage count, segment bytes
and messages, session aggregate bytes/messages, tool calls, and result events.

- [ ] **Step 2: Add crash-window tests**

Inject failures at these durable boundaries:

1. after session write but before provenance;
1. after provenance but before landing;
1. after landing but before queue acknowledgement; and
1. after a newer head is recorded but before its queue row is inserted.

Reopen the SQLite database and Docbank store for each case. The next
`RecordChanged`/`Finalize` must converge without duplicate messages, duplicate
usage events, regressed heads, or lost claims.

- [ ] **Step 3: Add cardinality-scaling regression**

Create a checkpoint with one changed session and compare it with a checkpoint
containing 10,000 already-provenance-matched sessions plus the same one changed
session. Add narrow test-only query observer hooks at the concrete `*db.DB`
provenance and queue page boundaries, and instrument the store:

- at most 128 queue claims are read;
- only the changed session's manifest and segments are opened;
- provenance lookups contain no more than 1,024 literal GIDs per query;
- no provider archive or unrelated origin is scanned.

The checkpoint map itself remains bounded by `checkpointDecodedLimit`. Do not
assert an environment-specific allocation count; the observable query and store
call counts protect the changed-session work bound without re-testing Go's map
allocator.

- [ ] **Step 4: Add no-double-import and absence tests**

Assert:

- observing manifest and segment arrivals requests retry but does not create
  another queue claim;

- the same checkpoint entry observed repeatedly produces one session revision;

- an unchanged imported manifest is skipped by provenance;

- a higher checkpoint with the same manifest does not rewrite the session; and

- a higher checkpoint omitting the session does not delete, trash, or exclude
  it.

- [ ] **Step 5: Run the complete affected packages**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/artifact ./internal/db -count=1
```

Expected: PASS.

- [ ] **Step 6: Run formatting, vet, and repository tests**

Run:

```bash
go fmt ./...
go vet ./...
make test
make lint
```

Expected: all commands pass.

- [ ] **Step 7: Run the private-data scrub**

Run:

```bash
git status --short
git diff --stat origin/main...HEAD
git diff origin/main...HEAD
git diff --check origin/main...HEAD
git log --format='%s%n%b%n---' origin/main..HEAD
```

Inspect the output and confirm it contains no private names, hostnames, absolute
user paths, credentials, infrastructure details, generated attribution blocks,
or unrelated changes.

- [ ] **Step 8: Commit final regressions**

```bash
git add internal/artifact/import_test.go \
  internal/artifact/import_checkpoint_test.go \
  internal/artifact/import_session_test.go \
  internal/db/orphaned_test.go
git commit -m "test(artifact): prove import recovery and bounds" \
  -m "Import correctness spans process crashes, database rebuilds, oversized peer content, and repeated observations. Protect those boundaries before transport and long-running synchronization begin calling the pipeline."
```

______________________________________________________________________

## Completion Criteria

- Foreign normalized checkpoints already present in an `ArtifactStore` can be
  imported without folder transport or CLI code.
- Queue claims, peer heads, landings, maps, and provenance survive process
  restart and full-resync database swaps.
- Current-version noncanonical syntax is accepted under verified byte identity.
- Future checkpoint, manifest, and segment versions defer independently.
- Missing and invalid dependencies cannot falsely acknowledge a checkpoint.
- Deferred FIFO claims cannot starve later origins in the same retry generation.
- Excluded or trashed sessions are not resurrected and do not wedge landing.
- Checkpoint absence never deletes normalized archive data.
- Repeated observations and crash retries do not duplicate normalized content.
- All decoded data and per-drain work obey explicit existing bounds.
- `raw_source`, metadata replay, transport, CLI, stewardship, and eviction
  remain outside the diff.
