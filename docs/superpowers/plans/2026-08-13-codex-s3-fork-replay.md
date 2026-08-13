# Codex S3 Fork Replay Repair Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans
> to implement this plan directly in the current agent, task-by-task. Never use
> subagent-driven development. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Exclude replayed parent messages and usage from Codex forks imported
through S3, while keeping unresolved children eligible for a later corrective
sync.

**Approved spec/design:**
`docs/superpowers/specs/2026-08-13-codex-s3-fork-replay-design.md`

**Architecture:** The parser package identifies the canonical Codex S3 root and
locates a parent rollout by opaque session ID. The sync S3 seam fetches only
that parent and materializes it beside the child, so the existing file-backed
turn-membership resolver remains authoritative. The Codex provider reports an
unresolved parent as `DataVersionNeedsRetry`, which the existing sync write path
persists below the current version.

**Tech Stack:** Go 1.26, JSONL, S3-compatible object storage, Testify, SQLite
sync integration tests.

## Global Constraints

- Compare Codex `turn_context.turn_id` values only as opaque equality keys.
- Fetch at most the one named parent rollout per child parse; never materialize
  an S3 archive wholesale.
- Fail open for content so unresolved parents cannot erase child work, but mark
  the result retryable so the overcount is not accepted as current.
- Do not add a persisted dependency graph or timestamp fallback.
- Keep production relationship classification unchanged.
- Update the Codex format provenance entry with the supported S3 behavior.

______________________________________________________________________

### Task 1: Locate a Codex Parent Within Its S3 Root

**Files:**

- Modify: `internal/parser/s3source.go`
- Test: `internal/parser/s3source_test.go`

**Interfaces:**

- Consumes: a child rollout URI and opaque parent session ID.

- Produces:
  `FindCodexS3ParentSessionURI(childURI, parentID string) (string, bool)`.

- [x] **Step 1: Write the failing URI lookup tests**

    Add a table-driven test that stubs `listS3Objects` and covers:

    - a dated parent under `s3://bucket/machine/raw/codex/2026/08/12/`;
    - a parent under `sessions/YYYY/MM/DD`;
    - a flat parent under `archived_sessions`;
    - a live and archived duplicate, where the live rollout must win;
    - a supported configured root without the `raw/codex` convention;
    - an unrelated object whose filename does not carry the exact parent ID;
    - an empty, shortened, or path-like parent ID, which must not list or match
      anything.

    Assert the exact matching URI and that listing is scoped to the canonical
    `s3://bucket/machine/raw/codex` root.

- [x] **Step 2: Run the focused test and verify RED**

    Run:

    ```bash
    go test ./internal/parser -run TestFindCodexS3ParentSessionURI -count=1
    ```

    Expected: FAIL because `FindCodexS3ParentSessionURI` does not exist.

- [x] **Step 3: Implement the bounded metadata lookup**

    Add a private helper that derives the canonical Codex root from the child URI,
    accepting the existing raw, `sessions`, `archived_sessions`, and dated
    layouts. Reject parent IDs containing `/`, `\\`, or surrounding whitespace.
    List that root through `listS3Objects`, keep only valid Codex rollout
    filenames, and return the first object whose
    `CodexSessionUUIDFromFilename(path.Base(uri))` equals `parentID` exactly.
    Return `("", false)` on invalid input, listing failure, or no match.

- [x] **Step 4: Run the focused test and verify GREEN**

    Run the command from Step 2. Expected: PASS.

### Task 2: Hydrate the Named Parent and Retry Unresolved Children

**Files:**

- Modify: `internal/parser/codex.go`
- Modify: `internal/parser/codex_provider.go`
- Modify: `internal/sync/s3.go`
- Test: `internal/parser/codex_provider_test.go`
- Test: `internal/sync/s3_test.go`

**Interfaces:**

- Consumes: `FindCodexS3ParentSessionURI`, the materialized child path, and the
  existing `ParseResultOutcome.DataVersion` contract.

- Produces: a private `hydrateS3CodexParent(childPath, childURI string) bool` in
  `internal/sync/s3.go`, and a Codex parse result whose data-version state is
  `DataVersionNeedsRetry` when an explicit parent cannot provide any turn IDs.

- [x] **Step 1: Write the failing provider retry test**

    Add a provider test with a fork child whose `forked_from_id` names no file in
    the provider root. Assert that parsing still returns the child's visible
    messages, but the sole `ParseResultOutcome` has `DataVersionNeedsRetry` and
    a non-empty retry reason naming unresolved parent turns. Repeat with valid
    final-line metadata that has no trailing newline.

- [x] **Step 2: Write the failing S3 replay and eventual-retry test**

    In `internal/sync/s3_test.go`, hand-build a parent rollout with one parent
    turn and a child rollout that replays that turn before one child-owned turn.
    Stub `listS3Objects` and `fetchS3Object` for the child, parent, and optional
    session index.

    First process the child with parent lookup unavailable. Write it through
    `pendingWrite{needsRetry: res.needsRetryForSession(childID)}` and assert:

    - replayed and child messages remain visible (fail-open);
    - stored data version is below `db.CurrentDataVersion()`.

    Then expose the parent without changing child metadata, process and write the
    same child again, and assert:

    - only the two child-owned messages remain;
    - replayed token usage is absent;
    - stored data version equals `db.CurrentDataVersion()`;
    - only the named parent object was fetched in addition to the child and
      session index.

- [x] **Step 3: Run both focused tests and verify RED**

    Run:

    ```bash
    go test ./internal/parser ./internal/sync \
      -run 'TestCodexProviderUnresolvedParentNeedsRetry|TestProcessS3CodexForkRetriesUntilParentAvailable' \
      -count=1
    ```

    Expected: FAIL because unresolved Codex parents are reported current and the
    S3 seam never hydrates the parent.

- [x] **Step 4: Preflight parent resolution in the Codex provider**

    Add a private
    `codexParentResolution(path string) (parentID string, resolved bool)`
    method. It scans the first valid session metadata, extracts `forked_from_id`
    or the existing subagent parent field, and uses `parentTurnResolver(path)`
    to require a non-empty parent turn set. In `codexProvider.Parse`, keep the
    existing fail-open parse result, but set its data-version state to
    `DataVersionNeedsRetry` with a concrete retry reason when explicit lineage
    is present and the parent is unresolved. Preserve `DataVersionCurrent` for
    non-derived sessions and resolved parents. Do not change `parseSession`
    signatures or production relationship classification.

- [x] **Step 5: Hydrate one named parent in the S3 parse seam**

    After writing the child temp file and before provider parsing, read the first
    valid `session_meta` to obtain `forked_from_id` or the existing subagent
    parent field. Call `FindCodexS3ParentSessionURI`. When it returns a URI,
    fetch that object and stream it to a path under the same temporary root
    using `safeS3TempRelPath` with an `AgentCodex` discovered file. Treat a
    missing or unreadable parent as unresolved rather than as a fatal child
    parse error. Return whether hydration succeeded so tests can distinguish
    current from retryable parsing without introducing a second parent parser.

- [x] **Step 6: Preserve per-result retry state through S3 conversion**

    Change `parseMaterializedS3Source` to translate each
    `ParseResultOutcome.DataVersionNeedsRetry` into
    `processResult.retrySessionIDs` keyed by the unprefixed parser session ID.
    After applying the S3 machine prefix, rewrite those map keys with the same
    prefix. Keep `ForceReplace` and excluded IDs unchanged.

- [x] **Step 7: Run focused tests and verify GREEN**

    Run the command from Step 3. Expected: PASS.

### Task 3: Include Explicit Forks in Captured Full Replay Validation

**Files:**

- Modify: `internal/parser/codex_replay_simulator_test.go`

**Interfaces:**

- Consumes: the first `session_meta` record in each captured rollout.

- Produces: full-replay totals over exactly the five explicit fork children,
  independent of production `RelationshipType` classification.

- [x] **Step 1: Change full-replay selection to source metadata**

    Before parsing totals, partition the six captured files by non-empty
    `payload.forked_from_id` in their first line, as the line-by-line companion
    test already does. Assert `require.Len(t, children, 5)` and iterate only
    those children. Remove the `RelationshipType` filter.

- [x] **Step 2: Run captured validation when evidence is available**

    Run:

    ```bash
    go test ./internal/parser -run 'TestCodexCapturedFork(Replay|LineReplay)Totals' -count=1
    ```

    Expected without `AGENTSVIEW_CODEX_REPLAY_ROOT`: both tests SKIP cleanly. When
    the configured evidence root is available, both tests must select five
    children and retain the existing literal message, token, and cost totals.

### Task 4: Record Provenance and Verify the Approved Batch

**Files:**

- Modify: `docs/internal/session-format-sources.md`
- Modify: `docs/superpowers/plans/2026-08-13-codex-s3-fork-replay.md`

**Interfaces:**

- Consumes: the approved design and passing behavioral tests.

- Produces: an updated Codex evidence entry and completed implementation record.

- [x] **Step 1: Update Codex format provenance**

    State that S3 forks hydrate only their explicitly named parent into the
    temporary parse tree, unresolved parents fail open with a retryable data
    version, and later unchanged-object syncs can correct the stored overcount.
    Record the 2026-08-13 re-verification against the materialized-S3 tests.

- [x] **Step 2: Run focused and repository verification**

    Run in isolated scratch HOME/XDG state with the pinned Go 1.26.5 binary:

    ```bash
    go fmt ./...
    go test -tags fts5 ./internal/parser ./internal/sync ./internal/db -count=1
    go vet ./...
    git diff --check
    ```

    Expected: all commands pass with no warnings or formatting errors.

- [x] **Step 3: Review scope and private-data safety**

    Inspect `git status --short`, `git diff HEAD`, and every introduced blob.
    Confirm only the approved S3 lookup, hydration, retry propagation, captured
    test selection, provenance, and plan changed. Block publication on real
    credentials, private paths, or internal endpoints.

- [x] **Step 4: Mark this plan complete and commit the implementation**

    Change every task checkbox to `[x]`. Stage only the intended files and use the
    mandatory commit skill. Do not bypass hooks and do not amend existing
    commits.

### Task 5: Push and Record Triage Outcomes

**Files:** None.

**Interfaces:**

- Consumes: the verified implementation commit and triage ledger decisions.

- Produces: a pushed exact head, local roborev outcome records when same-head
  jobs exist, and resolved minimization of the fully addressed trusted
  roborev-ci comment.

- [ ] **Step 1: Inspect the complete base-to-head history**

    Fetch `origin/main`, inspect diff, commit metadata, patches, and introduced
    blobs, and verify the PR title/body still describe the current result.

- [ ] **Step 2: Push the branch normally**

    Run `git push origin HEAD:fix/codex-fork-parent-membership`. Wait until the
    GitHub head OID matches local HEAD.

- [ ] **Step 3: Reinspect every exact-head evidence surface**

    Wait for all selected non-review checks, trusted same-head roborev-ci, and
    enabled local roborev jobs. Do not create a review. If new findings appear,
    return to one-at-a-time user triage before editing.

- [ ] **Step 4: Record approved outcomes**

    For existing local roborev jobs covering these findings, add one idempotent
    `triage-pr` comment per finding and close a job only when all its findings
    are fixed or recorded not-an-issue. When every finding in the current
    roborev-ci comment is verified absent, minimize that comment as `RESOLVED`.
    Do not post, edit, resolve, or delete human review comments or threads.

- [ ] **Step 5: Apply the exact-head completion gate**

    Confirm local, remote, and GitHub heads match; the worktree is clean; merge
    state is clean; CI passes; all review findings are decided and verified; and
    no deferred finding or changes-requested review remains.
