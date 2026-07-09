# Recall Population Foundation Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development
> (if subagents are available) or superpowers:executing-plans to implement this
> plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make recall population safe to measure by adding authoritative review
state, durable query/exposure events, and host-built evidence windows that
remain auditable when transcripts change.

**Architecture:** SQLite remains the sole recall authority. The database owns
review-state enforcement, evidence fingerprints, reconciliation, and the
append-only ledger. The service layer owns query outcome classification because
only it knows both ranked results and packed context. CLI and HTTP transports
carry the same query ID and miss reason. Model execution and automatic
population remain absent.

**Tech Stack:** Go, SQLite/FTS5, Cobra, `net/http`, testify

______________________________________________________________________

## Working constraints

- Work on the existing `memory-substrate` branch and worktree. Do not create or
  switch branches.
- Do not touch the live daemon or `~/.agentsview`. Set `AGENTSVIEW_DATA_DIR` to
  a fresh temporary directory and `AGENTSVIEW_NO_DAEMON=1` for every test
  command.
- Add tests before implementation and observe the focused test fail for the
  intended missing behavior.
- Use testify in all new Go tests. Assert stored behavior and response shape;
  never assert that a source file contains an implementation string.
- Preserve SQLite rows with additive migrations only. Do not bump `dataVersion`
  because no parser-derived data changes.
- PostgreSQL and DuckDB keep their current recall-unavailable `ErrReadOnly`
  behavior, but must implement any new `db.Store` method so backend contracts
  continue to compile.
- Before each commit, use `kenn:commit`. Do not push, pull, rebase, amend, or
  merge.

## Task 1: Make review state an enforced trust boundary

**Files:**

- Modify: `internal/recall/types.go`

- Modify: `internal/db/schema.sql`

- Modify: `internal/db/db.go`

- Modify: `internal/db/recall.go`

- Modify: `internal/db/recall_import.go`

- Modify: `internal/db/recall_eval_ingest.go`

- Test: `internal/db/recall_test.go`

- Test: `internal/db/recall_import_test.go`

- Test: `internal/db/recall_eval_ingest_test.go`

- Test: `internal/db/read_only_test.go`

- [ ] Add failing tests for the trust matrix.

    Cover all four review states with otherwise identical accepted, transferable,
    provenance-valid entries. `TrustedOnly` must return only `human_reviewed`.
    Also assert that an unknown non-empty state is rejected by
    `InsertRecallEntry` and `SupersedeRecallEntry`.

- [ ] Add a failing migration test.

    Create a database with reviewed, `recall-eval-ingest`, and
    `recall-import`-placeholder rows, remove the new column to simulate the old
    schema, reopen through `Open`, and assert:

    - ordinary rows become `human_reviewed`;
    - eval rows become `eval_raw`;
    - placeholder rows become `human_reviewed` with `provenance_ok = false`;
    - no session, recall, or evidence row is deleted.

- [ ] Add failing importer and eval-ingest tests.

    Assert that JSONL containing a top-level `review_state` member is rejected,
    verified imports receive `human_reviewed`, imports admitted through the
    placeholder option have provenance revoked, and raw eval chunks receive
    `eval_raw`.

- [ ] Run the red tests in an isolated environment.

    ```bash
    AGENTSVIEW_DATA_DIR="$(mktemp -d)" AGENTSVIEW_NO_DAEMON=1 \
      go test -tags 'fts5,evalingest' ./internal/db -run \
      'Test.*(ReviewState|TrustedOnly|Placeholder|EvalTrajectory)' -count=1
    ```

- [ ] Implement review-state constants and validation.

    Add `human_reviewed`, `unreviewed_auto`, `calibrated_auto`, and `eval_raw` in
    `internal/recall/types.go`. Empty state remains a trusted-code compatibility
    default to `human_reviewed`; any other unknown value fails before a
    transaction starts. Import and eval paths set their states explicitly.

- [ ] Add the additive schema and migration.

    Add `recall_entries.review_state TEXT NOT NULL DEFAULT 'human_reviewed'` with
    a `CHECK` over the four values. Add the column through `migrateColumns`,
    then run an idempotent data backfill that classifies eval rows and revokes
    placeholder provenance. Include recall tables in read-only schema
    compatibility so an unmigrated archive asks for a writable upgrade instead
    of failing during a query.

- [ ] Thread `review_state` through every recall row shape.

    Update `RecallEntry`, selected column constants, scanners, inserts,
    supersession, and full-resync copying. Add `review_state = human_reviewed`
    to the trusted predicate in `buildRecallEntryWhere`.

- [ ] Run the focused package tests and commit.

    ```bash
    AGENTSVIEW_DATA_DIR="$(mktemp -d)" AGENTSVIEW_NO_DAEMON=1 \
      go test -tags 'fts5,evalingest' ./internal/db -count=1
    ```

    Commit message: `feat(recall): enforce review state trust`

## Task 2: Build and bind canonical evidence windows

**Files:**

- Create: `internal/db/recall_evidence_window.go`

- Modify: `internal/db/schema.sql`

- Modify: `internal/db/db.go`

- Modify: `internal/db/recall.go`

- Modify: `internal/db/recall_import.go`

- Test: `internal/db/recall_evidence_window_test.go`

- Test: `internal/db/recall_import_test.go`

- [ ] Write failing window-construction tests over real temporary SQLite rows.

    Seed ordered messages with source UUIDs and multiple tool calls. Assert the
    built window contains the requested session/range, ordered messages, stable
    endpoint IDs, a sorted unique allowed-tool list, and a deterministic SHA-256
    authorization digest. Missing ordinals and reversed ranges must fail.

- [ ] Write failing containment tests.

    A narrowed selection inside the window succeeds. A start/end outside the
    window, a missing ordinal, or a tool-use ID not present in the window fails.
    The returned evidence metadata must contain endpoint source UUIDs and a
    selected-content digest.

- [ ] Pin digest semantics with behavior tests.

    The versioned canonical representation contains ordered role/content plus the
    exact supplied tool-call fields, but excludes database row IDs, timestamps,
    absolute ordinals, and source UUIDs. Therefore shifting otherwise identical
    messages changes the authorization coordinates but not the selected-content
    digest; changing visible content or a referenced tool call changes it.

- [ ] Run the red tests.

    ```bash
    AGENTSVIEW_DATA_DIR="$(mktemp -d)" AGENTSVIEW_NO_DAEMON=1 \
      go test -tags fts5 ./internal/db -run 'TestRecallEvidenceWindow' -count=1
    ```

- [ ] Implement the host-owned window types and builder.

    `RecallEvidenceWindow` carries session/range, ordered model-visible messages,
    allowed tool-use IDs, and authorization digest. `RecallEvidenceSelection`
    carries only narrowed ordinals and tool-use IDs; it cannot replace the host
    session. Use a transaction-compatible query interface so the same canonical
    builder works during import, migration, message replacement, and resync.

- [ ] Add evidence metadata columns additively.

    Add `message_start_source_uuid`, `message_end_source_uuid`, and
    `content_digest` to `recall_evidence`, its Go struct/scanners/inserts/copy
    queries, and `migrateColumns`. Existing evidence remains intact.

- [ ] Bind verified imports to host metadata.

    After the existing range/tool checks succeed, build the canonical selection
    from the database and overwrite source UUID/digest metadata from host state.
    Placeholder or otherwise unverified imports keep empty metadata and false
    provenance. The JSONL shape must not accept these host-owned fields.

- [ ] Run focused tests and commit.

    ```bash
    AGENTSVIEW_DATA_DIR="$(mktemp -d)" AGENTSVIEW_NO_DAEMON=1 \
      go test -tags 'fts5,evalingest' ./internal/db -run \
      'Test(RecallEvidenceWindow|ImportAcceptedRecallEntries)' -count=1
    ```

    Commit message: `feat(recall): authorize evidence windows`

## Task 3: Reconcile evidence whenever transcripts are rewritten

**Files:**

- Modify: `internal/db/recall_evidence_window.go`

- Modify: `internal/db/messages.go`

- Modify: `internal/db/session_batch.go`

- Modify: `internal/db/recall.go`

- Test: `internal/db/recall_evidence_window_test.go`

- Test: `internal/db/messages_test.go`

- Test: `internal/db/recall_test.go`

- [ ] Add failing routine-rewrite tests.

    Exercise both the in-place diff and full replacement paths:

    - stable endpoint UUIDs remap a shifted range and preserve provenance when the
      selected digest still matches;
    - no-UUID evidence stays at its ordinals only when the digest matches;
    - changed content, ambiguous/missing endpoints, or a missing referenced tool
      call sets the parent entry's provenance false without deleting it;
    - an append strictly after all cited ordinals preserves metadata.

- [ ] Add a failing transactional test.

    Cause reconciliation to fail structurally and assert the message replacement
    rolls back with the original transcript and recall metadata both intact.

- [ ] Run the red tests.

    ```bash
    AGENTSVIEW_DATA_DIR="$(mktemp -d)" AGENTSVIEW_NO_DAEMON=1 \
      go test -tags fts5 ./internal/db -run \
      'Test.*RecallEvidence.*(Replace|Diff|Remap|Revoke|Rollback)' -count=1
    ```

- [ ] Implement transaction-local reconciliation.

    Re-resolve both non-empty endpoint UUIDs only when each is unique in the same
    session, rebuild the selected digest, and validate the referenced tool call.
    Never turn a previously false `provenance_ok` back on implicitly. Invalid
    evidence revokes the parent entry; valid evidence may update shifted
    ordinals/source IDs.

- [ ] Wire every mutating transcript path.

    Call reconciliation after message/tool rows are complete but before commit in
    `ReplaceSessionMessages`, `ReplaceSessionContent`, and replacement branches
    of `writeOneSessionBatchTx`. The helper should no-op cheaply when the
    session has no recall evidence.

- [ ] Extend full-resync recall copying.

    Copy the new evidence metadata, then reconcile against messages in the new
    database before committing `CopyRecallEntriesFrom`. Legacy verified evidence
    without a digest is fingerprinted only if its current range/tool references
    verify; placeholder/unverifiable evidence remains revoked.

- [ ] Run focused tests and commit.

    ```bash
    AGENTSVIEW_DATA_DIR="$(mktemp -d)" AGENTSVIEW_NO_DAEMON=1 \
      go test -tags fts5 ./internal/db -run \
      'Test(ReplaceSession|WriteSessionBatch|CopyRecall|RecallEvidence)' -count=1
    ```

    Commit message: `feat(recall): reconcile evidence on transcript changes`

## Task 4: Add the append-only query and exposure ledger

**Files:**

- Create: `internal/db/recall_query_events.go`

- Modify: `internal/db/schema.sql`

- Modify: `internal/db/db.go`

- Modify: `internal/db/store.go`

- Modify: `internal/db/recall.go`

- Modify: `internal/postgres/store.go`

- Modify: `internal/duckdb/store.go`

- Test: `internal/db/recall_query_events_test.go`

- Test: `internal/db/recall_test.go`

- Test: `internal/db/store_contract_test.go`

- [ ] Write failing storage tests.

    Record an event with ranked packed and unpacked exposures, then read it back
    and assert every literal field: opaque ID, query, surface, serialized
    filters, trusted flag, score-policy version, counts, top score, miss reason,
    ranks, scores, and packed flags.

- [ ] Write failing atomicity and lifecycle tests.

    A duplicate exposure rank must roll back both the event and all exposures.
    Deleting the underlying recall entry/session must leave the event and
    exposure snapshot intact; `entry_id` therefore has no recall-entry foreign
    key.

- [ ] Write a failing full-resync preservation test.

    Copy an old database containing an event whose exposed entry is intentionally
    absent from the new database. Assert the event and exposure still survive.

- [ ] Run the red tests.

    ```bash
    AGENTSVIEW_DATA_DIR="$(mktemp -d)" AGENTSVIEW_NO_DAEMON=1 \
      go test -tags fts5 ./internal/db -run 'TestRecallQueryEvent' -count=1
    ```

- [ ] Add the schema and DB API.

    `recall_query_events` stores the completed request and
    `recall_query_exposures` uses `(query_id, rank)` as its key with only the
    event foreign key. `RecordRecallQueryEvent` creates a cryptographically
    random ID when absent and inserts the event/exposures in one transaction.
    `GetRecallQueryEvent` supports tests and the later proposal lookup. Add a
    stable lexical score-policy version constant.

- [ ] Extend store contracts and read-only providers.

    Add the recording method to `db.Store`. PostgreSQL and DuckDB return
    `ErrReadOnly`. SQLite read-only opens do the same at the DB method;
    suppression belongs to service orchestration, not storage.

- [ ] Preserve both ledger tables during full resync.

    Copy them independently of surviving recall entries and tolerate an old
    archive that predates the tables. Include the tables in read-only schema
    compatibility for archives created by the new binary.

- [ ] Run focused tests and commit.

    ```bash
    AGENTSVIEW_DATA_DIR="$(mktemp -d)" AGENTSVIEW_NO_DAEMON=1 \
      go test -tags fts5 ./internal/db ./internal/postgres ./internal/duckdb -count=1
    ```

    Commit message: `feat(recall): persist query exposure ledger`

## Task 5: Classify and record outcomes in the service and HTTP paths

**Files:**

- Modify: `internal/service/service.go`

- Modify: `internal/service/recall.go`

- Modify: `internal/service/direct.go`

- Modify: `internal/service/http.go`

- Modify: `internal/server/recall.go`

- Test: `internal/service/recall_test.go`

- Test: `internal/service/http_test.go`

- Test: `internal/server/recall_test.go`

- [ ] Add failing direct-service behavior tests.

    Verify:

    - no ranked entries records `no_results` with zero exposures;
    - ranked entries plus a requested context too small for any entry records
      `context_empty`;
    - a normal result records every rank and marks only IDs in
      `context_meta.included_ids` packed;
    - no-context queries record results with an empty miss reason;
    - ordinary recording failure logs/returns recall with empty `query_id`;
    - strict recording failure returns an error;
    - a read-only SQLite open returns recall with no event and no error.

- [ ] Add failing server and HTTP-backend round-trip tests.

    `POST /api/v1/recall/query` and the HTTP service backend must preserve
    `query_id` and `miss_reason`. Assert the server-created ledger row matches
    the response rather than merely checking JSON key presence.

- [ ] Run the red tests.

    ```bash
    AGENTSVIEW_DATA_DIR="$(mktemp -d)" AGENTSVIEW_NO_DAEMON=1 \
      go test -tags fts5 ./internal/service ./internal/server -run \
      'Test.*Recall.*(QueryID|MissReason|Recording|Exposure|ReadOnly)' -count=1
    ```

- [ ] Implement shared outcome construction.

    Add `QueryID` and `MissReason` to `RecallQueryResult`. Add an audited surface
    (`query`, `brief`, or `calibration`) and an internal strict-recording option
    to `RecallQuery`. Build the event only after context assembly, deriving
    packed IDs from `RecallContextMeta`. The service helper checks
    `Store.ReadOnly()` before ordinary recording, logs best-effort failures, and
    propagates strict failures.

- [ ] Use the same helper in both transports.

    Refactor only enough to keep direct-service and HTTP-server classification
    byte-for-byte equivalent. The HTTP client transports the fields; it never
    records a second event.

- [ ] Run focused tests and commit.

    ```bash
    AGENTSVIEW_DATA_DIR="$(mktemp -d)" AGENTSVIEW_NO_DAEMON=1 \
      go test -tags fts5 ./internal/service ./internal/server -count=1
    ```

    Commit message: `feat(recall): measure query misses and exposure`

## Task 6: Expose review and measurement contracts in the CLI

**Files:**

- Modify: `cmd/agentsview/recall.go`

- Test: `cmd/agentsview/recall_test.go`

- [ ] Add failing CLI tests.

    Assert JSON from `recall query` and `recall brief` contains the exact query ID
    and miss reason returned by the service. Assert brief records surface
    `brief`, query records `query`, and trusted-only CLI output excludes
    `unreviewed_auto`, `calibrated_auto`, and `eval_raw` entries. Human review
    lines must display `review_state` so quarantined rows are auditable.

- [ ] Run the red tests.

    ```bash
    AGENTSVIEW_DATA_DIR="$(mktemp -d)" AGENTSVIEW_NO_DAEMON=1 \
      go test -tags fts5 ./cmd/agentsview -run 'TestRecall(Query|Brief|Trusted|Review)' -count=1
    ```

- [ ] Thread the fields through query and brief output.

    Set the surface before invoking the service. Add `query_id` and `miss_reason`
    to the brief wrapper instead of dropping them when it reshapes the service
    result. Keep human output compact; the ID contract is required in JSON.

- [ ] Run focused tests and commit.

    ```bash
    AGENTSVIEW_DATA_DIR="$(mktemp -d)" AGENTSVIEW_NO_DAEMON=1 \
      go test -tags fts5 ./cmd/agentsview -run 'TestRecall' -count=1
    ```

    Commit message: `feat(cli): expose recall measurement identity`

## Task 7: Verify the complete foundation

**Files:**

- Modify only if verification exposes a real defect; return to the relevant red
  test before changing implementation.

- [ ] Format all Go code.

    ```bash
    go fmt ./...
    ```

- [ ] Run focused recall packages with eval ingest enabled.

    ```bash
    AGENTSVIEW_DATA_DIR="$(mktemp -d)" AGENTSVIEW_NO_DAEMON=1 \
      go test -tags 'fts5,evalingest' \
      ./internal/db ./internal/service ./internal/server ./internal/mcp \
      ./cmd/agentsview -count=1
    ```

- [ ] Run repository-required static validation.

    ```bash
    AGENTSVIEW_DATA_DIR="$(mktemp -d)" AGENTSVIEW_NO_DAEMON=1 go vet ./...
    AGENTSVIEW_DATA_DIR="$(mktemp -d)" AGENTSVIEW_NO_DAEMON=1 make lint
    ```

- [ ] Run the repository-wide Go suite.

    ```bash
    AGENTSVIEW_DATA_DIR="$(mktemp -d)" AGENTSVIEW_NO_DAEMON=1 make test
    ```

- [ ] Inspect final scope and history.

    ```bash
    git status --short
    git diff --check HEAD~6..HEAD
    git log --oneline -10
    ```

- [ ] If verification required tracked fixes, commit them with a focused
  conventional message after using `kenn:commit`. Otherwise do not create an
  empty verification commit.

## Done criteria

- Trusted recall is mechanically limited to accepted, human-reviewed,
  transferable, provenance-valid entries.
- Placeholder and eval data cannot enter trusted context.
- Every verified evidence selection is host-fingerprinted and becomes untrusted
  if routine sync or full resync can no longer reproduce it.
- Every ordinary writable recall query attempts one atomic event with ranked
  exposures; query/brief JSON returns its ID and mechanical miss reason.
- Read-only SQLite recall remains usable without writes.
- Query history and orphaned exposure snapshots survive full resync.
- No model invocation, automatic population, remote recall claim, or production
  archive mutation is introduced.
