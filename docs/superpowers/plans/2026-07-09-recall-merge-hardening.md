# Recall Merge Hardening Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development
> (if subagents available) or superpowers:executing-plans to implement this
> plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the unreleased Recall substrate fail closed, observable,
idempotent, covered under its lab build tag, and accurately documented as active
research before PR #1046 is considered mergeable.

**Architecture:** Keep SQLite as the sole Recall authority and preserve the
existing shared import, query, and evidence-reconciliation funnels. Tighten
those funnels at their current boundaries instead of adding migrations or new
write paths; keep query recording synchronous while batching its child rows.
Publish a small experimental Zensical surface and leave model execution and
LongMemEval-v2 for later branches.

**Tech Stack:** Go 1.26, SQLite/FTS5, Cobra CLI, `net/http`, testify, Make,
GitHub Actions, Zensical/Markdown.

______________________________________________________________________

## Inputs and constraints

- Design: `docs/superpowers/specs/2026-07-09-recall-merge-hardening-design.md`
- Population direction:
  `docs/superpowers/specs/2026-07-09-recall-population-write-through.md`
- Repository rules: `AGENTS.md`
- Required implementation disciplines: `@superpowers:test-driven-development`,
  `@testing-without-tautologies`, `@kenn:isolate-prod`, `@kenn:commit`, and
  `@superpowers:verification-before-completion`.
- Recall tables are new and unreleased. Edit their canonical definitions in
  `internal/db/schema.sql`; do not add recall-table migration code.
- Never run a branch binary against the user's default AgentsView data
  directory. Tests and runtime checks use a temporary `HOME` and explicit
  scratch `AGENTSVIEW_DATA_DIR`.
- Preserve the pre-existing untracked `.golangci-cache-wt2/` directory and do
  not stage it.

## File map

- `internal/recall/types.go`: review-state normalization constants.
- `internal/db/schema.sql`: canonical Recall default and existing message
  indexes.
- `internal/db/recall.go`: entry insertion invariants and query validation.
- `internal/db/recall_import.go`: dry-run and real import ordering.
- `internal/db/recall_evidence_window.go`: endpoint reconciliation, reason
  selection, revocation logging, and source-UUID lookup.
- `internal/db/recall_query_events.go`: atomic event/exposure persistence.
- `internal/service/recall.go`: query completion and validation propagation.
- `internal/server/recall.go`: HTTP classification of invalid Recall queries.
- `cmd/agentsview/recall.go`: CLI-facing review-state fallback and Recall
  command behavior.
- `Makefile`, `.github/workflows/ci.yml`: lab build-tag coverage.
- `docs/recall.md`, `docs/zensical.toml`, `docs/commands.md`: public
  experimental documentation.
- Colocated `*_test.go` files: observable regression coverage.

### Task 1: Fail closed on implicit review state and evidence ownership

**Files:**

- Modify: `internal/recall/types.go:42-58`

- Modify: `internal/db/schema.sql:317-352`

- Modify: `internal/db/recall.go:166-456`

- Modify: `cmd/agentsview/recall.go:1386-1398,1574-1578`

- Create: `internal/recall/types_test.go`

- Modify: `internal/db/recall_test.go`

- Modify: `cmd/agentsview/recall_test.go`

- [ ] **Step 1: Write failing normalization and persistence tests**

    Add a table-driven `TestNormalizeReviewState` in
    `internal/recall/types_test.go` with literal expectations:

    ```go
    tests := []struct {
        name  string
        input string
        want  string
        ok    bool
    }{
        {"empty is automatic", "", ReviewStateUnreviewedAuto, true},
        {"whitespace is automatic", "  ", ReviewStateUnreviewedAuto, true},
        {"human reviewed remains explicit", ReviewStateHumanReviewed, ReviewStateHumanReviewed, true},
        {"unknown is rejected", "self_approved", "", false},
    }
    ```

    Add SQLite tests that:

    - call `InsertRecallEntry` with an empty review state and assert the stored
      row is `unreviewed_auto`;
    - execute a raw `INSERT` that omits `review_state` and assert the schema
      default is `unreviewed_auto`;
    - insert an entry whose evidence names a different session and assert an error
      containing both session IDs, no entry row, and no evidence rows.

    Add focused formatter tests using a `bytes.Buffer` to assert empty remote
    review state renders as `unreviewed_auto`, never `human_reviewed`.

- [ ] **Step 2: Run the tests and verify the expected red failures**

    Run:

    ```bash
    env -u AGENTSVIEW_DATA_DIR -u AGENT_VIEWER_DATA_DIR -u AGENTSVIEW_NO_DAEMON \
      HOME="$(mktemp -d)" GOCACHE="$(go env GOCACHE)" \
      GOMODCACHE="$(go env GOMODCACHE)" GOPATH="$(go env GOPATH)" \
      go test -tags fts5 ./internal/recall ./internal/db ./cmd/agentsview \
      -run 'Test(NormalizeReviewState|InsertRecallEntryDefaults|RecallSchemaDefaults|InsertRecallEntryRejectsEvidence|PrintRecall.*Review)' \
      -count=1
    ```

    Expected: failures show empty state becoming `human_reviewed`, the schema
    defaulting to `human_reviewed`, mismatched evidence being accepted, and CLI
    output displaying `human_reviewed`.

- [ ] **Step 3: Implement the fail-closed default and shared invariant**

    Change `NormalizeReviewState("")` and the schema default to
    `ReviewStateUnreviewedAuto` / `'unreviewed_auto'`.

    Add a validator called by `insertRecallEntryTx` before either the entry or
    evidence is inserted:

    ```go
    func validateRecallEvidenceSessions(m RecallEntry) error {
        for _, evidence := range m.Evidence {
            if evidence.SessionID != m.SourceSessionID {
                return fmt.Errorf(
                    "recall evidence session %q does not match source session %q",
                    evidence.SessionID, m.SourceSessionID,
                )
            }
        }
        return nil
    }
    ```

    Keep the reviewed JSONL importer explicitly assigning
    `ReviewStateHumanReviewed` and eval ingest explicitly assigning
    `ReviewStateEvalRaw`. Change both CLI empty-state fallbacks to
    `unreviewed_auto`.

- [ ] **Step 4: Make existing trusted test fixtures explicit**

    Run `internal/recall`, `internal/db`, `internal/service`, `internal/server`,
    and `cmd/agentsview` without `-run`. Existing trusted-only service and
    server tests also create entries with empty review states. For every fixture
    intended to represent a human-reviewed trusted entry, set
    `ReviewState: corerecall.ReviewStateHumanReviewed` explicitly. Do not add a
    test helper that globally converts empty state back to trusted; fixtures
    that are testing automatic or default behavior must retain the empty state.

    `TestCopyRecallEntriesFromRevokesDroppedEvidenceSession` intentionally needs a
    legacy-invalid cross-session evidence row to exercise full-resync repair.
    Its current `InsertRecallEntry` setup will correctly fail under the new
    invariant. Convert only that fixture to insert the entry and mismatched
    evidence with explicit raw SQL, and comment that the bypass models corrupt
    or pre-invariant data. Preserve the test's later revocation assertions; do
    not weaken the production insertion gate for the fixture.

- [ ] **Step 5: Verify green and run required Go checks**

    Run the focused command from Step 2, then run all five packages from Step 4
    without `-run`. Finally run:

    ```bash
    go fmt ./...
    go vet ./...
    ```

    Expected: all commands exit 0.

- [ ] **Step 6: Commit the focused trust-default change**

    Use `@kenn:commit` and commit only the files from this task with subject:

    ```text
    fix(recall): fail closed on implicit trust
    ```

### Task 2: Restore archive-idempotent imports

**Files:**

- Modify: `internal/db/recall_import.go:75-350`

- Modify: `internal/db/recall_import_test.go:498-735`

- [ ] **Step 1: Replace the validator-first duplicate regression with
  idempotency regressions**

    Replace `TestImportAcceptedRecallEntriesJSONLValidatesEvidenceBeforeDuplicate`
    with a table-driven test over real and dry-run imports:

    1. seed and import a valid `candidate_id` with verified evidence;
    1. rewrite or remove the cited transcript rows;
    1. re-import the same line with `RequireExistingSessions: true`;
    1. assert no error, `Skipped == 1`, reason `duplicate`, and no mutation of the
       stored entry or evidence digest.

    Add a second case proving a nonduplicate candidate against the same rewritten
    transcript still fails evidence validation. This prevents duplicate-first
    from weakening validation for new identities.

- [ ] **Step 2: Run the import tests and verify red**

    Run:

    ```bash
    env -u AGENTSVIEW_DATA_DIR -u AGENT_VIEWER_DATA_DIR -u AGENTSVIEW_NO_DAEMON \
      HOME="$(mktemp -d)" GOCACHE="$(go env GOCACHE)" \
      GOMODCACHE="$(go env GOMODCACHE)" GOPATH="$(go env GOPATH)" \
      go test -tags fts5 ./internal/db \
      -run 'TestImportAcceptedRecallEntriesJSONL.*(Duplicate|Idempot|Evidence)' \
      -count=1
    ```

    Expected: the duplicate case returns the current missing-evidence error.

- [ ] **Step 3: Split duplicate detection from remaining identity validation**

    Introduce focused queryer helpers:

    ```go
    func recallImportEntryExistsWithQueryer(
        ctx context.Context, queryer recallImportQueryer, id string,
    ) (bool, error)

    func validateRecallImportSupersessionWithQueryer(
        ctx context.Context, queryer recallImportQueryer, recall RecallEntry,
    ) error
    ```

    In both `validateRecallImportDryRun` and `importAcceptedRecallEntry`:

    1. begin the existing transaction;
    1. check the stored candidate ID and return duplicate immediately;
    1. validate required session/evidence and bind its digest;
    1. validate any supersession target;
    1. perform the dry-run projection or write.

    Keep JSON parsing, host-controlled-field rejection, candidate-shape
    validation, and same-stream duplicate handling in their current pre-
    transaction positions.

- [ ] **Step 4: Verify real/dry-run parity and required Go checks**

    Run the command from Step 2 and the complete import test file:

    ```bash
    go test -tags fts5 ./internal/db -run 'TestImportAcceptedRecallEntriesJSONL' -count=1
    go fmt ./...
    go vet ./...
    ```

    Expected: all commands exit 0.

- [ ] **Step 5: Commit the idempotency change**

    Use `@kenn:commit` with subject:

    ```text
    fix(recall): preserve import idempotency
    ```

### Task 3: Reject contradictory trusted-status filters

**Files:**

- Modify: `internal/db/recall.go:85-101,480-900,1041-1110`

- Modify: `internal/db/recall_test.go`

- Modify: `internal/service/recall.go:35-75`

- Modify: `internal/service/recall_test.go`

- Modify: `internal/server/recall.go:25-155`

- Modify: `internal/server/recall_test.go`

- Modify: `cmd/agentsview/recall_test.go`

- [ ] **Step 1: Write observable rejection tests at each boundary**

    Add tests that pass `TrustedOnly: true` with `Status: "archived"` and assert:

    - `DB.ListRecallEntries` and `DB.QueryRecallEntries` return an error matching
      a new `ErrInvalidRecallQuery` sentinel;
    - `service.QueryRecallStore` propagates the same error without recording a
      query event;
    - `GET /api/v1/recall/entries?trusted_only=true&status=archived` and
      `POST /api/v1/recall/query` return HTTP 400 with a literal explanatory
      message;
    - `agentsview recall list --trusted-only --status archived` and the equivalent
      `query` invocation return an error rather than successful empty output.

    Include accepted/empty status control cases that continue to succeed.

- [ ] **Step 2: Run boundary tests and verify red**

    Run:

    ```bash
    env -u AGENTSVIEW_DATA_DIR -u AGENT_VIEWER_DATA_DIR -u AGENTSVIEW_NO_DAEMON \
      HOME="$(mktemp -d)" GOCACHE="$(go env GOCACHE)" \
      GOMODCACHE="$(go env GOMODCACHE)" GOPATH="$(go env GOPATH)" \
      go test -tags fts5 ./internal/db ./internal/service ./internal/server ./cmd/agentsview \
      -run 'Test.*Recall.*Trusted.*(Archived|Status)' -count=1
    ```

    Expected: archived requests currently succeed with empty results or return
    HTTP 200.

- [ ] **Step 3: Add one shared DB validation contract**

    Define:

    ```go
    var ErrInvalidRecallQuery = errors.New("invalid recall query")

    func ValidateRecallQuery(q RecallQuery) error {
        q = NormalizeRecallQuery(q)
        if q.TrustedOnly && q.Status != "" && q.Status != corerecall.StatusAccepted {
            return fmt.Errorf(
                "%w: trusted_only requires status %q",
                ErrInvalidRecallQuery, corerecall.StatusAccepted,
            )
        }
        return nil
    }
    ```

    Call it at every public DB Recall list/query entry point before SQL
    construction. Call it in `QueryRecallStore` before ranking so non-DB stores
    share the contract. In HTTP handlers, map `ErrInvalidRecallQuery` to 400;
    preserve current context/read-only mappings. CLI and the HTTP service
    backend then surface the server/direct errors without transport-specific
    policy.

    Remove the redundant second `status = accepted` predicate from
    `buildRecallEntryWhere`; after validation the normalized primary status
    predicate already enforces accepted status for trusted queries.

- [ ] **Step 4: Verify green and required Go checks**

    Run the command from Step 2, then:

    ```bash
    go fmt ./...
    go vet ./...
    ```

    Expected: all commands exit 0 and HTTP tests observe 400.

- [ ] **Step 5: Commit the validation change**

    Use `@kenn:commit` with subject:

    ```text
    fix(recall): reject contradictory trust filters
    ```

### Task 4: Make evidence revocation deterministic and observable

**Files:**

- Modify: `internal/db/recall_evidence_window.go:500-780`

- Modify: `internal/db/recall_evidence_window_test.go:194-490`

- Modify: `internal/db/recall.go:196-320`

- Modify: `internal/db/recall_test.go:1569-1730`

- [ ] **Step 1: Add mixed-endpoint and deterministic-reason regressions**

    Extend reconciliation tests with two literal mixed cases:

    - stable start UUID plus legacy end ordinal;
    - legacy start ordinal plus stable end UUID.

    Shift the anchored message while preserving selected content and assert the
    anchored endpoint remaps independently. Then remove or duplicate the
    anchored source UUID and assert provenance is revoked rather than falling
    back to its old ordinal.

    Capture the package logger in nonparallel tests and cover every reason code:

    ```text
    start_endpoint_unresolved
    end_endpoint_unresolved
    invalid_resolved_range
    window_invalid
    selection_invalid
    missing_digest
    content_digest_mismatch
    evidence_dropped_during_resync
    ```

    For a case where both endpoints are unresolved, assert only
    `start_endpoint_unresolved` is logged. For a later failure on another
    evidence group belonging to the same entry, assert no second revocation log.
    Include entry ID and session ID assertions in every log check.

- [ ] **Step 2: Extract the current endpoint SQL without changing behavior**

    Run the existing evidence tests once as a green characterization, then move
    the inline source-UUID lookup SQL from `uniqueRecallEvidenceOrdinalTx` to a
    package constant without adding the partial-index predicate. Rerun the same
    tests and keep them green. This is a behavior-preserving preparatory
    refactor that lets the performance integration test execute the exact
    production SQL.

- [ ] **Step 3: Add a red query-plan integration check**

    Add a narrow test around the production source-UUID lookup SQL. Run
    `EXPLAIN QUERY PLAN` against `testDB(t)` and assert the detail names the
    existing `idx_messages_source_uuid`. The test must use the same SQL constant
    as `uniqueRecallEvidenceOrdinalTx`, so removing the partial-index predicate
    breaks the integration assumption.

- [ ] **Step 4: Run evidence tests and verify red**

    Run:

    ```bash
    env -u AGENTSVIEW_DATA_DIR -u AGENT_VIEWER_DATA_DIR -u AGENTSVIEW_NO_DAEMON \
      HOME="$(mktemp -d)" GOCACHE="$(go env GOCACHE)" \
      GOMODCACHE="$(go env GOMODCACHE)" GOPATH="$(go env GOPATH)" \
      go test -tags fts5 ./internal/db \
      -run 'Test(RecallEvidence|CopyRecallEntriesFrom).*' -count=1
    ```

    Expected: mixed endpoints retain ordinal-only behavior, no stable reasons are
    logged, and the query-plan test reports a table scan.

- [ ] **Step 5: Implement independent endpoint resolution and reason
  precedence**

    Add typed string constants for the eight reason codes and a helper that:

    - updates `provenance_ok` only when it is currently true;
    - checks `RowsAffected` so only the first transition logs;
    - logs `recall: revoked provenance entry=<id> session=<id> reason=<code>`.

    Resolve start and end UUIDs independently in this exact order: start, end,
    resolved-range validity, window build, selection binding, stored-digest
    presence, digest equality. Use the stored ordinal only when that endpoint's
    stored source UUID was empty. Keep `loadTrustedRecallEvidenceGroupsTx`
    restricted to `provenance_ok = 1`, making revocation sticky.

    Before full-resync reconciliation, select entries whose evidence-row count
    changed in deterministic ID order and revoke them individually with
    `evidence_dropped_during_resync`; do not leave the existing unlogged bulk
    update in place.

- [ ] **Step 6: Reuse the existing partial index**

    Add `AND source_uuid != ''` to the shared endpoint lookup SQL constant created
    in Step 2. Do not create a new index or migration; the existing
    `idx_messages_source_uuid` is sufficient.

- [ ] **Step 7: Verify green, rollback behavior, and required Go checks**

    Run the command from Step 4 plus the existing rollback test explicitly:

    ```bash
    go test -tags fts5 ./internal/db -run 'TestRecallEvidenceReplaceRollbackOnReconcileFailure' -count=1
    go fmt ./...
    go vet ./...
    ```

    Expected: all commands exit 0, and the rollback test still leaves transcript
    and Recall state unchanged.

- [ ] **Step 8: Commit the reconciliation change**

    Use `@kenn:commit` with subject:

    ```text
    fix(recall): explain provenance revocations
    ```

### Task 5: Batch atomic query-exposure writes

**Files:**

- Modify: `internal/db/recall_query_events.go:45-125`

- Modify: `internal/db/recall_query_events_test.go`

- [ ] **Step 1: Write a multi-batch persistence regression**

    Insert an event with 205 literal/generated exposures whose ranks are 1..205,
    distinct entry IDs, alternating packed state, and known scores. Read the
    event back and assert all 205 rows, boundary ranks 1/100/101/200/201/205,
    entry IDs, scores, and packed flags are preserved in order.

    Keep the existing first-batch atomic-failure test. Add a second atomicity case
    with at least 101 exposures: make exposure 101 duplicate rank 100 so the
    constraint failure occurs in the second batch after the first 100-row insert
    succeeded inside the transaction. Assert `RecordRecallQueryEvent` errors,
    `GetRecallQueryEvent` returns nil, and a direct exposure count for the query
    ID is zero. This proves a later batch cannot commit the event or any earlier
    exposure batch.

- [ ] **Step 2: Run the ledger tests and establish the characterization**

    Run:

    ```bash
    env -u AGENTSVIEW_DATA_DIR -u AGENT_VIEWER_DATA_DIR -u AGENTSVIEW_NO_DAEMON \
      HOME="$(mktemp -d)" GOCACHE="$(go env GOCACHE)" \
      GOMODCACHE="$(go env GOMODCACHE)" GOPATH="$(go env GOPATH)" \
      go test -tags fts5 ./internal/db -run 'TestRecallQueryEvent' -count=1
    ```

    Expected: the new behavioral test passes against the old implementation; this
    is a characterization test for the refactor, so perform the required
    mutation check in Step 4 rather than claiming a red functional gap.

- [ ] **Step 3: Implement bounded multi-row inserts**

    Add `const recallExposureInsertBatchSize = 100` and a helper that builds one
    `INSERT ... VALUES (?, ?, ?, ?, ?), ...` per slice of at most 100 exposures.
    Keep all batches inside the existing event transaction and writer lock. Skip
    the helper for zero exposures. Return an error that identifies the first and
    last rank in the failed batch.

- [ ] **Step 4: Prove the regression test detects a broken batch boundary**

    Temporarily mutate the batch loop to skip the first row of the second batch,
    run the 205-row test, and observe a length/rank failure. Restore the correct
    loop and rerun to green. Do not commit the temporary mutation.

- [ ] **Step 5: Run all ledger/service recording tests and required Go checks**

    Run:

    ```bash
    go test -tags fts5 ./internal/db ./internal/service \
      -run 'Test.*Recall.*(QueryEvent|Recording|Exposure)' -count=1
    go fmt ./...
    go vet ./...
    ```

    Expected: all commands exit 0.

- [ ] **Step 6: Commit the batching change**

    Use `@kenn:commit` with subject:

    ```text
    perf(recall): batch query exposure writes
    ```

### Task 6: Put eval-ingest quarantine coverage in CI

**Files:**

- Modify: `Makefile:36,274-285`

- Modify: `.github/workflows/ci.yml:160-200`

- [ ] **Step 1: Verify the missing target fails**

    Run:

    ```bash
    make test-evalingest
    ```

    Expected: Make exits nonzero with “No rule to make target”.

- [ ] **Step 2: Add the dedicated tagged test target**

    Add `test-evalingest` to `.PHONY` and define:

    ```make
    test-evalingest: pricing-snapshot ensure-embed-dir
        CGO_ENABLED=1 go test -tags "fts5,evalingest" \
            ./internal/db ./internal/server -v -count=1
    ```

    Use a real Make recipe tab, not spaces, for both displayed recipe lines when
    editing `Makefile`.

    In the Go test job, add a Linux-only step after the ordinary Go tests:

    ```yaml
    - name: Run eval-ingest build-tag tests
      if: runner.os == 'Linux'
      run: make test-evalingest
    ```

    Do not add a source-grepping test for the Makefile or workflow; executing the
    target is the observable contract.

- [ ] **Step 3: Run the new target and workflow lint**

    Run:

    ```bash
    env -u AGENTSVIEW_DATA_DIR -u AGENT_VIEWER_DATA_DIR -u AGENTSVIEW_NO_DAEMON \
      HOME="$(mktemp -d)" GOCACHE="$(go env GOCACHE)" \
      GOMODCACHE="$(go env GOMODCACHE)" GOPATH="$(go env GOPATH)" \
      make test-evalingest
    make lint
    ```

    Expected: the tagged DB/server suites pass and lint reports zero issues.

- [ ] **Step 4: Commit the CI coverage change**

    Use `@kenn:commit` with subject:

    ```text
    test(recall): run eval ingest coverage in CI
    ```

### Task 7: Publish the experimental Recall documentation

**Files:**

- Create: `docs/recall.md`

- Modify: `docs/zensical.toml:10-35`

- Modify: `docs/commands.md:880-910`

- [ ] **Step 1: Write the concise experimental page**

    Create `docs/recall.md` with frontmatter and these sections:

    ```markdown
    ---
    title: Recall (Experimental)
    description: Experimental, provenance-linked durable knowledge over the local session archive
    ---

    !!! warning "Active research"

        Recall's schema, scoring, trust policy, and workflows may change. Treat
        its entries and measurement rows as a rebuildable research corpus. The
        session archive remains authoritative and must not be deleted to reset
        Recall.
    ```

    Cover, without presenting import as a stable recommended workflow:

    - Recall versus semantic transcript search;
    - current SQLite-only lexical query/brief/list/get/stats surfaces;
    - evidence windows, sticky provenance revocation, review states, and the
      trusted-only predicate;
    - query ledger and dry-run extraction;
    - reviewed JSONL import as the current lab inlet;
    - `eval_raw` quarantine and `trusted_only=false` for eval harness inspection;
    - no automatic distillation, semantic entry retrieval, web UI, PostgreSQL, or
      DuckDB support;
    - local model extraction/judging by default and explicit per-run disclosure
      before remote frontier judging;
    - LongMemEval-v2 as later benchmark work.

- [ ] **Step 2: Add navigation and the compact CLI reference**

    Add `{"Recall (Experimental)" = "recall.md"}` after semantic-search pages in
    `docs/zensical.toml`. Add a short `agentsview recall` command block after
    the embeddings section in `docs/commands.md`, linking to `/recall/` and
    listing `list`, `get`, `query`, `brief`, `stats`, `extract --dry-run`, and
    `import --dry-run`.

- [ ] **Step 3: Format, privacy-scan, and build the docs**

    Run:

    ```bash
    mdformat docs/recall.md docs/commands.md
    AGENTSVIEW_DOCS_USE_LOCAL_ASSET_BRANCHES=1 make docs-check
    ```

    Apply `@kenn:scrub-private-data` to the page, nav/reference diff, and proposed
    commit message. Expected: zero private-term/structural matches and a
    successful Zensical build/site check.

- [ ] **Step 4: Commit the public research preview**

    Use `@kenn:commit` with subject:

    ```text
    docs(recall): publish experimental research preview
    ```

### Task 8: Run the merge-readiness gate and close review debt

**Files:**

- Review only: all files changed from `git merge-base origin/main HEAD` to
  `HEAD`, including `ad644c28`

- Potentially modify: only files required by failures discovered here

- [ ] **Step 1: Run focused Recall and tagged suites in scratch state**

    Run:

    ```bash
    env -u AGENTSVIEW_DATA_DIR -u AGENT_VIEWER_DATA_DIR -u AGENTSVIEW_NO_DAEMON \
      HOME="$(mktemp -d)" GOCACHE="$(go env GOCACHE)" \
      GOMODCACHE="$(go env GOMODCACHE)" GOPATH="$(go env GOPATH)" \
      go test -tags fts5 ./internal/recall ./internal/db ./internal/service \
        ./internal/server ./cmd/agentsview -run 'TestRecall|Test.*Recall' -count=1

    env -u AGENTSVIEW_DATA_DIR -u AGENT_VIEWER_DATA_DIR -u AGENTSVIEW_NO_DAEMON \
      HOME="$(mktemp -d)" GOCACHE="$(go env GOCACHE)" \
      GOMODCACHE="$(go env GOMODCACHE)" GOPATH="$(go env GOPATH)" \
      make test-evalingest
    ```

    Expected: all focused and build-tag tests pass.

- [ ] **Step 2: Run repository-wide required checks**

    Run each command separately and inspect its exit code:

    ```bash
    go fmt ./...
    go vet ./...

    env -u AGENTSVIEW_DATA_DIR -u AGENT_VIEWER_DATA_DIR -u AGENTSVIEW_NO_DAEMON \
      HOME="$(mktemp -d)" GOCACHE="$(go env GOCACHE)" \
      GOMODCACHE="$(go env GOMODCACHE)" GOPATH="$(go env GOPATH)" \
      make test

    AGENTSVIEW_DATA_DIR="$(mktemp -d)" AGENTSVIEW_NO_DAEMON=1 make lint

    AGENTSVIEW_DOCS_USE_LOCAL_ASSET_BRANCHES=1 make docs-check
    ```

    Expected: every command exits 0, lint reports zero issues, and docs validation
    reports no source, build, or redirect failures.

- [ ] **Step 3: Drive the CLI and HTTP failure paths**

    Build the branch binary to a temporary path, start it only against a scratch
    `AGENTSVIEW_DATA_DIR`, and verify:

    - `recall list --trusted-only --status archived` exits nonzero with the
      validation message;
    - an HTTP Recall query with the same combination returns 400;
    - `recall --help` includes the experimental surfaces and existing explicit
      server-token flag;
    - no files appear in the user's default data directory.

- [ ] **Step 4: Review the final branch diff and outstanding reviews**

    Run:

    ```bash
    base="$(git merge-base origin/main HEAD)"
    git diff --check "$base"..HEAD
    git diff --stat "$base"..HEAD
    git log --oneline "$base"..HEAD
    roborev fix --list
    ```

    Inspect every post-review commit, explicitly including the credential change
    in `ad644c28`. For each remaining stale/invalid review, draft a technical
    dismissal; for each fixed actionable review, use the applicable roborev
    response workflow to comment and close the original job. Do not close a
    review that cannot be matched confidently.

- [ ] **Step 5: Request a final branch review**

    Apply `@superpowers:requesting-code-review` to the complete branch diff. If it
    finds a confirmed blocker, fix it with a fresh red-green cycle, repeat the
    relevant verification, use `@kenn:commit` for the focused fix, and request a
    new final branch review against the resulting HEAD. Repeat until the current
    HEAD has no confirmed merge blocker; never base readiness on a review of a
    superseded commit.

- [ ] **Step 6: Report merge readiness without merging or pushing**

    Report the exact verification evidence, commits created, remaining known
    limitations, review status, and whether the branch is merge-ready. Do not
    push, update the public PR, or merge without separate user authorization.
