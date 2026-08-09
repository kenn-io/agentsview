# Recall Entry Review Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:executing-plans to implement this plan directly in the current
> agent, task-by-task. Keep execution inline; do not dispatch subagents. Steps
> use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let users approve or durably reject one machine-extracted Recall entry
from its expanded Corpus row.

**Approved spec/design:**
`docs/superpowers/specs/2026-08-08-recall-entry-review-design.md`

**Architecture:** A one-time SQLite table migration removes the review-state
`CHECK`, while shared Go validation adds `human_rejected`. A transactional
store operation owns the legal transitions, a dedicated HTTP action exposes
it, and the Corpus panel updates the affected row locally.

**Tech Stack:** Go, SQLite, `net/http`, testify, Svelte 5, TypeScript,
Paraglide, kit-ui, Vite+/Vitest.

## Global Constraints

- Use red/green TDD for every behavior change.
- Use `kenn:db-migration-discipline` before editing schema or migration code.
  This PR contains exactly one immutable migration.
- The migration rebuilds only `recall_entries`, preserves `rowid`, and passes
  `PRAGMA foreign_key_check` before commit.
- Do not bump parser `dataVersion`; this schema-only migration must not force a
  session resync.
- Validate allowed review states in Go, not with a SQLite `CHECK`.
- Approval is `accepted/unreviewed_auto` to `accepted/human_reviewed` and
  requires valid provenance.
- Archive is `accepted/unreviewed_auto` to `archived/human_rejected` and
  remains available with revoked provenance.
- Treat stale, repeated, or otherwise illegal transitions as conflicts.
- Do not modify content, evidence, transferability, source identity, or
  extraction metadata.
- Notify the Recall embedding scheduler only after a committed decision.
- Keep controls in the expanded row. Approve is immediate; Archive uses the
  shared confirmation modal.
- Add no notes, audit table, bulk action, undo, CLI, MCP, PostgreSQL mutation,
  DuckDB mutation, or generic patch endpoint.
- Use `localization-paraglide` before editing message catalogues and keep all
  five locales synchronized.
- Use `kenn:commit` before every commit. Do not add attribution trailers.

---

### Task 1: Move review-state validation to Go and migrate SQLite

**Files:**

- Modify: `internal/recall/types.go`
- Modify: `internal/recall/types_test.go`
- Modify: `internal/db/schema.sql`
- Create: `internal/db/recall_review_migration.go`
- Create: `internal/db/recall_review_migration_test.go`
- Modify: `internal/db/db.go`

**Interface:** `recall.ReviewStateHumanRejected` is an allowed state. Writable
open upgrades the legacy constrained table before schema initialization; later
opens are no-ops.

- [ ] **Step 1: Load migration and testing guidance**

Read and follow `kenn:db-migration-discipline`,
`kenn:test-scope-discipline`, and `testing-without-tautologies`. Confirm this
branch contains no other schema migration.

- [ ] **Step 2: Write the failing state-normalization test**

Extend `TestNormalizeReviewState` in `internal/recall/types_test.go`:

```go
{
    name:      "human rejected",
    value:     ReviewStateHumanRejected,
    wantState: ReviewStateHumanRejected,
    wantOK:    true,
},
```

- [ ] **Step 3: Verify RED**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/recall \
  -run '^TestNormalizeReviewState$' -count=1
```

Expected: compilation fails because `ReviewStateHumanRejected` is undefined.

- [ ] **Step 4: Add the Go-owned state**

Add the constant and accept it in `NormalizeReviewState`:

```go
const (
    ReviewStateHumanReviewed  = "human_reviewed"
    ReviewStateHumanRejected  = "human_rejected"
    ReviewStateUnreviewedAuto = "unreviewed_auto"
    ReviewStateCalibratedAuto = "calibrated_auto"
    ReviewStateEvalRaw        = "eval_raw"
)

switch value {
case ReviewStateHumanReviewed,
    ReviewStateHumanRejected,
    ReviewStateUnreviewedAuto,
    ReviewStateCalibratedAuto,
    ReviewStateEvalRaw:
    return value, true
default:
    return "", false
}
```

Run the focused test again and expect PASS.

- [ ] **Step 5: Write the failing legacy migration tests**

Create `internal/db/recall_review_migration_test.go`. Build a temporary archive,
seed a session, two entries with explicit rowids, evidence, and a supersession
link, then replace only `recall_entries` with the legacy definition containing:

```sql
review_state TEXT NOT NULL DEFAULT 'unreviewed_auto'
    CHECK (review_state IN (
        'human_reviewed', 'unreviewed_auto', 'calibrated_auto', 'eval_raw'
    ))
```

Close the fixture, reopen with `Open(path)`, and assert rowids, entries,
evidence, supersession, FTS search, and foreign keys survive. Query
`sqlite_master.sql` and assert the migrated table contains no
`CHECK (review_state IN`. Then prove the new Go write boundary accepts
`human_rejected`:

```go
_, err = reopened.InsertRecallEntry(RecallEntry{
    ID: "rejected", Type: "fact", Scope: "project",
    Status: corerecall.StatusArchived,
    ReviewState: corerecall.ReviewStateHumanRejected,
    Title: "Rejected", Body: "Rejected after review.",
    SourceSessionID: "session-1",
})
require.NoError(t, err)
```

Add an idempotence test that calls the migration twice and proves row count,
rowids, and schema remain unchanged. Use only `t.TempDir()` archives.

- [ ] **Step 6: Verify migration RED**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db \
  -run 'Test(OpenMigratesLegacyRecallReviewConstraint|MigrateRecallReviewStateConstraintIsIdempotent)' \
  -count=1
```

Expected: inserting `human_rejected` still fails the legacy SQL constraint.

- [ ] **Step 7: Remove the canonical SQL constraint**

Change only the `review_state` column in `internal/db/schema.sql`:

```sql
review_state TEXT NOT NULL DEFAULT 'unreviewed_auto',
```

Keep the default. Do not replace the constraint or bump `dataVersion`.

- [ ] **Step 8: Implement the pre-init migration**

Create `migrateRecallReviewStateConstraintLocked`. It must inspect
`sqlite_master.sql`, return when the table is missing or unconstrained, pin one
writer connection, save and disable `PRAGMA foreign_keys`, and restore the
setting on every exit:

```go
func migrateRecallReviewStateConstraintLocked(w *writerHandle) (retErr error) {
    var tableSQL string
    err := w.QueryRow(`SELECT sql FROM sqlite_master
        WHERE type = 'table' AND name = 'recall_entries'`).Scan(&tableSQL)
    if errors.Is(err, sql.ErrNoRows) {
        return nil
    }
    if err != nil {
        return fmt.Errorf("probing recall_entries review constraint: %w", err)
    }
    if !strings.Contains(tableSQL, "CHECK (review_state IN") {
        return nil
    }

    ctx := context.Background()
    conn, err := w.Conn(ctx)
    if err != nil {
        return fmt.Errorf("acquiring recall review migration connection: %w", err)
    }
    defer func() { retErr = errors.Join(retErr, conn.Close()) }()

    var foreignKeys int
    if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
        return fmt.Errorf("reading foreign-key mode: %w", err)
    }
    if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
        return fmt.Errorf("disabling foreign keys: %w", err)
    }
    defer func() {
        if foreignKeys != 0 {
            _, restoreErr := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`)
            retErr = errors.Join(retErr, restoreErr)
        }
    }()

    tx, err := conn.BeginTx(ctx, nil)
    if err != nil {
        return fmt.Errorf("beginning recall review migration: %w", err)
    }
    defer func() { _ = tx.Rollback() }()
    if _, err := tx.ExecContext(ctx, recallReviewStateMigrationSQL); err != nil {
        return fmt.Errorf("migrating recall review state: %w", err)
    }
    if err := verifyRecallReviewMigrationTx(ctx, tx); err != nil {
        return err
    }
    if err := tx.Commit(); err != nil {
        return fmt.Errorf("committing recall review migration: %w", err)
    }
    return nil
}
```

The prepare SQL drops entry corpus/query/FTS triggers, creates
`recall_entries_review_state_v2` with the complete canonical columns but no
review-state `CHECK`, and copies every column plus `rowid`. Before dropping the
old table, query both counts through the transaction and fail if they differ.
Then execute a separate swap statement that drops the old table and renames the
replacement. After the swap, fail if `PRAGMA foreign_key_check` yields any row.
Do not recreate indexes or triggers inside the migration: the immediately
following `db.init()` owns canonical creation, while preserved rowids keep
external-content FTS attached.

Call it in `openAndInit`, after legacy-column repair and before `db.init()`:

```go
db.mu.Lock()
err = migrateRecallReviewStateConstraintLocked(db.getWriter())
db.mu.Unlock()
if err != nil {
    db.Close()
    return nil, fmt.Errorf("migrating recall review state: %w", err)
}
```

- [ ] **Step 9: Verify GREEN**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/recall ./internal/db -count=1
```

Expected: PASS, including migration preservation, FTS, foreign-key, and
idempotence assertions.

- [ ] **Step 10: Commit the schema checkpoint**

Use `kenn:commit`; stage only Task 1 files and commit:

```text
feat(recall): add durable rejected review state
```

---

### Task 2: Add the transactional store review operation

**Files:**

- Create: `internal/db/recall_review.go`
- Create: `internal/db/recall_review_test.go`
- Modify: `internal/db/store.go`
- Modify: `internal/postgres/store.go`
- Modify: `internal/duckdb/store.go`
- Modify: `internal/db/recall_extract_test.go`

**Interface:** `ReviewRecallEntry(ctx, id, action)` returns the committed entry
with evidence or one typed validation, not-found, provenance, or conflict error.

- [ ] **Step 1: Write failing transition tests**

Create table-driven tests covering:

```go
tests := []struct {
    name       string
    action     RecallReviewAction
    provenance bool
    wantStatus string
    wantReview string
    wantErr    error
}{
    {"approve", RecallReviewApprove, true,
        corerecall.StatusAccepted, corerecall.ReviewStateHumanReviewed, nil},
    {"approve revoked", RecallReviewApprove, false,
        corerecall.StatusAccepted, corerecall.ReviewStateUnreviewedAuto,
        ErrRecallReviewProvenance},
    {"archive", RecallReviewArchive, true,
        corerecall.StatusArchived, corerecall.ReviewStateHumanRejected, nil},
    {"archive revoked", RecallReviewArchive, false,
        corerecall.StatusArchived, corerecall.ReviewStateHumanRejected, nil},
}
```

For success, assert unchanged content, evidence, transferability, source and
extractor fields, and `created_at`, plus a newer `updated_at`. Add cases for a
missing ID, unknown action, already reviewed/rejected, and initially archived
entry.

- [ ] **Step 2: Add failing freshness and lifecycle coverage**

Assert approval advances only `RecallQueryRevision`; archive advances query and
corpus revisions. Extend `TestActivateExtractGenerationSwitchesServedEntries`
with an archived `human_rejected` entry and assert activation leaves it
archived. Exercise digest-reset cleanup and assert it does not delete the row.

- [ ] **Step 3: Verify RED**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db \
  -run 'Test(ReviewRecallEntry|ActivateExtractGenerationSwitchesServedEntries)' \
  -count=1
```

Expected: compilation fails because review action types are missing.

- [ ] **Step 4: Implement typed actions, errors, and transaction**

Create `internal/db/recall_review.go`:

```go
type RecallReviewAction string

const (
    RecallReviewApprove RecallReviewAction = "approve"
    RecallReviewArchive RecallReviewAction = "archive"
)

var (
    ErrInvalidRecallReviewAction = errors.New("invalid recall review action")
    ErrRecallEntryNotFound       = errors.New("recall entry not found")
    ErrRecallReviewConflict      = errors.New("recall review conflict")
    ErrRecallReviewProvenance    = errors.New("recall provenance is revoked")
)

func (a RecallReviewAction) Validate() error {
    switch a {
    case RecallReviewApprove, RecallReviewArchive:
        return nil
    default:
        return fmt.Errorf("%w: %q", ErrInvalidRecallReviewAction, a)
    }
}
```

`ReviewRecallEntry` trims and validates inputs, takes `db.mu`, opens one writer
transaction, reads current status/review/provenance, classifies precondition
errors, and executes a guarded update repeating
`status='accepted' AND review_state='unreviewed_auto'`. Approval additionally
guards `provenance_ok != 0`. Assign:

```go
nextStatus := corerecall.StatusAccepted
nextReview := corerecall.ReviewStateHumanReviewed
if action == RecallReviewArchive {
    nextStatus = corerecall.StatusArchived
    nextReview = corerecall.ReviewStateHumanRejected
}
```

Read the updated base row and its evidence through the same transaction, then
commit and return it. Do not use a post-commit lookup: a response-read failure
after commit would make the caller believe a durable mutation failed.

- [ ] **Step 5: Extend the store contract and read-only stores**

Add to `internal/db/store.go`:

```go
ReviewRecallEntry(
    ctx context.Context,
    id string,
    action RecallReviewAction,
) (RecallEntry, error)
```

Add matching PostgreSQL and DuckDB methods returning an empty entry and
`db.ErrReadOnly`.

- [ ] **Step 6: Verify GREEN**

```bash
CGO_ENABLED=1 go test -tags fts5 \
  ./internal/db ./internal/postgres ./internal/duckdb -count=1
```

- [ ] **Step 7: Commit the storage checkpoint**

Use `kenn:commit`; stage only Task 2 files and commit:

```text
feat(recall): persist individual review decisions
```

---

### Task 3: Expose a dedicated review HTTP action

**Files:**

- Modify: `internal/server/server.go`
- Modify: `internal/server/recall.go`
- Modify: `internal/server/recall_test.go`

**Interface:** `POST /api/v1/recall/entries/{id}/review` accepts one JSON action
and returns the updated entry.

- [ ] **Step 1: Write failing success tests**

Use the real SQLite server test environment:

```go
func TestReviewRecallEntryApproveAndArchive(t *testing.T) {
    for _, tc := range []struct {
        name       string
        action     string
        wantStatus string
        wantReview string
    }{
        {"approve", "approve", "accepted", "human_reviewed"},
        {"archive", "archive", "archived", "human_rejected"},
    } {
        t.Run(tc.name, func(t *testing.T) {
            te := setup(t)
            seedReviewableRecallEntry(t, te, "review-me", true)
            w := te.post(t, "/api/v1/recall/entries/review-me/review",
                `{"action":"`+tc.action+`"}`)
            assertStatus(t, w, http.StatusOK)
            got := decode[db.RecallEntry](t, w)
            assert.Equal(t, tc.wantStatus, got.Status)
            assert.Equal(t, tc.wantReview, got.ReviewState)
            require.Len(t, got.Evidence, 1)
        })
    }
}
```

Install `WithRecallCorpusMutationNotifier` and assert one notification after
each committed action.

- [ ] **Step 2: Write failing error-mapping tests**

Add cases for malformed JSON, unknown action, missing entry, already reviewed,
already rejected, initially archived, revoked approval, and revoked archive.
Assert 400 for malformed/unknown, 404 for missing, 409 for stale/repeated and
revoked approval, and 200 for revoked archive. Close the writer before a
request and assert 503 plus `Retry-After`. Use `setupPGMode` and assert the
established 501 response. Every non-2xx case must leave the notifier count zero.

- [ ] **Step 3: Verify RED**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/server \
  -run 'TestReviewRecallEntry' -count=1
```

Expected: 404 because the route is not registered.

- [ ] **Step 4: Register and implement the handler**

Register beside the existing entry routes:

```go
s.mux.Handle("POST /api/v1/recall/entries/{id}/review", s.withTimeout(
    "POST /api/v1/recall/entries/{id}/review",
    s.handleReviewRecallEntry,
))
```

Use a strict request body and the typed store action:

```go
type reviewRecallEntryRequest struct {
    Action db.RecallReviewAction `json:"action"`
}

func (s *Server) handleReviewRecallEntry(w http.ResponseWriter, r *http.Request) {
    var req reviewRecallEntryRequest
    decoder := json.NewDecoder(r.Body)
    decoder.DisallowUnknownFields()
    if err := decoder.Decode(&req); err != nil {
        writeError(w, http.StatusBadRequest, "invalid JSON body")
        return
    }
    if err := req.Action.Validate(); err != nil {
        writeError(w, http.StatusBadRequest, err.Error())
        return
    }

    entry, err := s.db.ReviewRecallEntry(
        r.Context(), strings.TrimSpace(r.PathValue("id")), req.Action,
    )
    if err != nil {
        s.handleRecallReviewError(w, err)
        return
    }
    s.notifyRecallCorpusMutation()
    writeJSON(w, http.StatusOK, entry)
}
```

`handleRecallReviewError` must delegate context/read-only handling first, then
map `ErrRecallEntryNotFound` to 404 and the two review-precondition errors to
409. Reuse `handleReadOnly` so `ErrWriterClosed` produces 503 with
`Retry-After`; unclassified storage failures remain 500.

- [ ] **Step 5: Verify GREEN**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/server -count=1
```

- [ ] **Step 6: Commit the HTTP checkpoint**

Use `kenn:commit`; stage only Task 3 files and commit:

```text
feat(recall): expose entry review action
```

---

### Task 4: Add the typed frontend API and localized contract

**Files:**

- Modify: `frontend/src/lib/api/types/recall.ts`
- Modify: `frontend/src/lib/api/recall.ts`
- Modify: `frontend/src/lib/api/recall.test.ts`
- Modify: `frontend/messages/en.json`
- Modify: `frontend/messages/de.json`
- Modify: `frontend/messages/es.json`
- Modify: `frontend/messages/fr.json`
- Modify: `frontend/messages/ja.json`

**Interface:** `reviewRecallEntry(id, action)` posts a decision and returns a
typed `RecallEntry`; every review state and action has localized copy.

- [ ] **Step 1: Load frontend guidance and dependencies**

Read and follow `localization-paraglide`, `kenn:test-scope-discipline`, and
`testing-without-tautologies`. Run:

```bash
cd frontend
vp install
```

- [ ] **Step 2: Write the failing API test**

Add to `frontend/src/lib/api/recall.test.ts`:

```ts
describe("reviewRecallEntry", () => {
  it("posts one encoded review action and returns the updated entry", async () => {
    const updated = {
      id: "entry one",
      status: "archived",
      review_state: "human_rejected",
    };
    const fetchMock = vi.fn().mockResolvedValue(new Response(
      JSON.stringify(updated),
      { status: 200, headers: { "Content-Type": "application/json" } },
    ));
    vi.stubGlobal("fetch", fetchMock);

    await expect(reviewRecallEntry("entry one", "archive"))
      .resolves.toEqual(updated);
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/v1/recall/entries/entry%20one/review",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({ action: "archive" }),
      }),
    );
  });
});
```

- [ ] **Step 3: Verify RED**

```bash
cd frontend
vp test run src/lib/api/recall.test.ts
```

Expected: compilation fails because `reviewRecallEntry` is absent.

- [ ] **Step 4: Add the typed action and API function**

In `frontend/src/lib/api/types/recall.ts`:

```ts
export type RecallReviewAction = "approve" | "archive";
```

In `frontend/src/lib/api/recall.ts`:

```ts
export async function reviewRecallEntry(
  id: string,
  action: RecallReviewAction,
): Promise<RecallEntry> {
  const response = await fetch(
    `${getBase()}/recall/entries/${encodeURIComponent(id)}/review`,
    authHeaders({
      method: "POST",
      body: JSON.stringify({ action }),
    }),
  );
  if (!response.ok) {
    throw new ApiError(response.status, await responseErrorMessage(response));
  }
  return (await response.json()) as RecallEntry;
}
```

Import the new type with `import type`. Run the API test and expect PASS.

- [ ] **Step 5: Add synchronized localized copy**

Add these keys with real translations to all five locale files:

```text
recall_page_review_state_human_reviewed
recall_page_review_state_human_rejected
recall_page_review_state_unreviewed_auto
recall_page_review_state_calibrated_auto
recall_page_review_state_eval_raw
recall_page_review_approve
recall_page_review_archive
recall_page_review_approve_disabled
recall_page_review_archive_title
recall_page_review_archive_message
recall_page_review_cancel
recall_page_review_close
recall_page_review_error
```

English source copy:

```json
"recall_page_review_state_human_reviewed": "Human approved",
"recall_page_review_state_human_rejected": "Human rejected",
"recall_page_review_state_unreviewed_auto": "Unreviewed automatic",
"recall_page_review_state_calibrated_auto": "Calibrated automatic",
"recall_page_review_state_eval_raw": "Evaluation raw",
"recall_page_review_approve": "Approve",
"recall_page_review_archive": "Archive",
"recall_page_review_approve_disabled": "Approval is unavailable because the source evidence was revoked.",
"recall_page_review_archive_title": "Archive Recall entry",
"recall_page_review_archive_message": "Archive “{title}” as rejected? It will remain outside the served Recall corpus.",
"recall_page_review_cancel": "Cancel",
"recall_page_review_close": "Close archive confirmation",
"recall_page_review_error": "Could not review this Recall entry: {error}"
```

Compile and type-check:

```bash
cd frontend
vp run i18n:compile
vp check
```

- [ ] **Step 6: Commit the frontend contract checkpoint**

Use `kenn:commit`; stage only Task 4 files and commit:

```text
feat(recall): add localized entry review contract
```

---

### Task 5: Wire review controls into the expanded Corpus row

**Files:**

- Modify: `frontend/src/lib/components/recall/RecallCorpusPanel.svelte`
- Modify: `frontend/src/lib/components/recall/RecallCorpusPanel.test.ts`

**Interface:** Expanded accepted `unreviewed_auto` rows offer Approve and
Archive; the response updates or removes only that local row.

- [ ] **Step 1: Write failing visibility and provenance tests**

Extend the component fixture with a valid reviewable entry, a revoked
reviewable entry, and a `human_reviewed` entry. Expand each and assert:

- the valid automatic row has enabled Approve and Archive buttons;
- the revoked automatic row has disabled Approve, the localized explanation,
  and enabled Archive; and
- the human-reviewed row has neither action.

Select controls by accessible name rather than CSS implementation details.

- [ ] **Step 2: Write failing approve interaction tests**

Click Approve and assert the exact POST body, retained expanded row, localized
`human_reviewed` label, and unchanged scroll position. Activate the
`unreviewed_auto` filter in a second test, approve, and assert the row disappears
without another list request.

- [ ] **Step 3: Write failing archive, busy, and error tests**

Assert the first Archive click opens the shared modal without sending a
request, Cancel closes it, and confirmation sends `{"action":"archive"}`.
Success removes the row because the browser defaults to accepted entries.
While the promise is pending, assert both row actions are disabled. Return a
409 and assert the expanded row remains with a localized inline error.

- [ ] **Step 4: Verify RED**

```bash
cd frontend
vp test run src/lib/components/recall/RecallCorpusPanel.test.ts
```

Expected: review controls and confirmation are absent.

- [ ] **Step 5: Add localized review-state labels**

Include `human_rejected` in `REVIEW_STATES` and add:

```ts
function reviewStateLabel(state: string): string {
  switch (state) {
    case "human_reviewed":
      return m.recall_page_review_state_human_reviewed();
    case "human_rejected":
      return m.recall_page_review_state_human_rejected();
    case "unreviewed_auto":
      return m.recall_page_review_state_unreviewed_auto();
    case "calibrated_auto":
      return m.recall_page_review_state_calibrated_auto();
    case "eval_raw":
      return m.recall_page_review_state_eval_raw();
    default:
      return state;
  }
}
```

Use it in filter options and table cells.

- [ ] **Step 6: Implement local review state and mutation flow**

Import `reviewRecallEntry` and `RecallReviewAction`. Add row-scoped state:

```ts
let reviewingEntryIds = $state<string[]>([]);
let reviewErrors = $state<Record<string, string>>({});
let archiveEntry = $state<RecallEntry | null>(null);

function isReviewable(entry: RecallEntry): boolean {
  return entry.status === "accepted" &&
    entry.review_state === "unreviewed_auto";
}

function keepAfterReview(entry: RecallEntry): boolean {
  return entry.status === "accepted" &&
    (!reviewState || entry.review_state === reviewState);
}

async function submitReview(
  entry: RecallEntry,
  action: RecallReviewAction,
) {
  if (reviewingEntryIds.includes(entry.id)) return;
  reviewingEntryIds = [...reviewingEntryIds, entry.id];
  reviewErrors = { ...reviewErrors, [entry.id]: "" };
  try {
    const updated = await reviewRecallEntry(entry.id, action);
    const keep = keepAfterReview(updated);
    entries = keep
      ? entries.map((item) => item.id === updated.id ? updated : item)
      : entries.filter((item) => item.id !== updated.id);
    if (!keep) {
      expandedEntryIds = expandedEntryIds.filter((id) => id !== updated.id);
    }
    archiveEntry = null;
  } catch (error) {
    const detail = error instanceof Error ? error.message : String(error);
    reviewErrors = {
      ...reviewErrors,
      [entry.id]: m.recall_page_review_error({ error: detail }),
    };
  } finally {
    reviewingEntryIds = reviewingEntryIds.filter((id) => id !== entry.id);
  }
}
```

Do not call `loadEntries` after success. Local replacement/removal preserves
pagination and scroll.

- [ ] **Step 7: Render actions and the archive modal**

Inside `.entry-detail`, after evidence, render actions only when
`isReviewable(entry)`. Approve calls `submitReview` immediately and is disabled
when provenance is invalid or `reviewingEntryIds` contains the row. Archive
sets `archiveEntry` and remains enabled for revoked provenance. Render the
localized provenance reason and row error beside the controls.

Add one `Modal` outside the table following the existing generation modal. Its
confirmation calls:

```ts
if (archiveEntry) void submitReview(archiveEntry, "archive");
```

Use kit-ui `Button` and `Modal`. Add only layout CSS using existing spacing,
border, and danger tokens; add no native control or one-off button chrome.

- [ ] **Step 8: Verify GREEN**

```bash
cd frontend
vp test run src/lib/api/recall.test.ts \
  src/lib/components/recall/RecallCorpusPanel.test.ts
vp check
```

- [ ] **Step 9: Commit the UI checkpoint**

Use `kenn:commit`; stage only Task 5 files and commit:

```text
feat(recall): review entries from the corpus table
```

---

### Task 6: Document the workflow and run final verification

**Files:**

- Modify: `docs/recall.md`
- Modify: `docs/internal/recall-extraction.md`
- Modify as required by formatters: touched Go and frontend files only

- [ ] **Step 1: Update public Recall documentation**

In `docs/recall.md`, add `human_rejected` and correct the state meanings:

```markdown
| `human_reviewed`  | Explicitly approved by a human                   |
| `human_rejected`  | Explicitly rejected and archived by a human      |
| `unreviewed_auto` | Generated or omitted review decision             |
| `calibrated_auto` | Automated output from a calibrated future policy |
| `eval_raw`        | Quarantined evaluation material                  |
```

Add a concise “Review extracted entries” subsection explaining expanded-row
Approve/Archive behavior, provenance gating, confirmation, and the absence of
editing, bulk actions, and undo.

- [ ] **Step 2: Update extraction lifecycle documentation**

In `docs/internal/recall-extraction.md`, state that `human_reviewed` and
`human_rejected` are human-touched states excluded from activation, retirement,
retraction, and digest-reset cleanup. Rejected entries stay archived across
later generation activation.

- [ ] **Step 3: Format and run focused verification**

```bash
mdformat --wrap 80 docs/recall.md docs/internal/recall-extraction.md \
  docs/superpowers/specs/2026-08-08-recall-entry-review-design.md \
  docs/superpowers/plans/2026-08-08-recall-entry-review.md
go fmt ./...
CGO_ENABLED=1 go test -tags fts5 \
  ./internal/recall ./internal/db ./internal/postgres \
  ./internal/duckdb ./internal/server -count=1
go vet ./...
make lint-golangci-ci
cd frontend
vp run i18n:compile
vp test run src/lib/api/recall.test.ts \
  src/lib/components/recall/RecallCorpusPanel.test.ts
vp check
```

Expected: every command exits zero. If `mdformat` is unavailable, preserve the
80-column style manually and report that limitation.

- [ ] **Step 4: Inspect the final diff and public-data boundary**

```bash
git diff --check
git status --short
git diff --stat origin/main...HEAD
git diff origin/main...HEAD
```

Confirm there are no private hostnames, personal paths, lab data, generated site
output, `.superpowers/` files, unrelated changes, or attribution trailers.

- [ ] **Step 5: Commit the documentation checkpoint**

Use `kenn:commit`; commit only documentation or formatter changes not already
committed:

```text
docs(recall): explain entry review decisions
```

- [ ] **Step 6: Record kata evidence**

Comment on `9bds` with focused commands, commit hashes, and migration
preservation evidence. Keep it open until a pull request exists and final
verification has passed.

- [ ] **Step 7: Prepare delivery without watching CI**

Use `superpowers:verification-before-completion`, then
`superpowers:finishing-a-development-branch`. Do not push or open a pull request
unless requested. Never poll GitHub Actions or use `gh api` to watch jobs unless
explicitly requested.
