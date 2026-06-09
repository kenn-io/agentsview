# Usage Dashboard Performance — Phase 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop the usage dashboard from hammering a CPU and re-fetching a 30 MB
sidebar index on every live update, give it manual-refresh + staleness UX, and
add a covering index + gzip — without changing any usage/cost/timezone semantics
or SQLite↔PostgreSQL behavior parity.

**Architecture:** Phase 1 from the approved spec
(`.superpowers/specs/2026-06-09-usage-dashboard-performance.md`). Backend: widen
the existing partial usage index into a covering index (SQLite includes
`token_usage`; PG/Cockroach omits it because a btree tuple cannot safely key an
unbounded TEXT blob — documented divergence, identical results) and add a gzip
response middleware that bypasses SSE streams. Frontend: gate the global
`sessions.load()` (the 30 MB fetch + live-refresh arming) so it only runs on the
sessions route, coalesce/cancel in-flight sidebar requests, make the usage page
manual-refresh only, parallelize its two heavy fetches, and add "Updated X ago"
\+ a "new data" hint.

**Tech Stack:** Go (`net/http`, `database/sql`, sqlite3 `fts5`, testify),
PostgreSQL/Cockroach (`pgx`), Svelte 5 runes, TypeScript, Vitest.

______________________________________________________________________

## File Structure

Backend (Go):

- `internal/db/db.go` — MODIFY `createPartialIndexesLocked` (SQLite covering
  index + drop legacy).
- `internal/db/usage_covering_index_test.go` — CREATE (SQLite index/migration
  test).
- `internal/postgres/schema.go` — MODIFY `createPartialIndexesPG` (PG/Cockroach
  covering index + drop legacy).
- `internal/postgres/usage_covering_index_pgtest_test.go` — CREATE (PG parity
  test, `pgtest` tag).
- `internal/server/compress.go` — CREATE (gzip middleware).
- `internal/server/compress_test.go` — CREATE.
- `internal/server/server.go` — MODIFY `Handler()` (insert gzip into the chain).

Frontend (Svelte/TS):

- `frontend/src/lib/stores/sessions.svelte.ts` — MODIFY (route gate
  `setSidebarActive`; coalesce + cancel in `load()`).
- `frontend/src/lib/stores/sessions.test.ts` — MODIFY (gate + coalesce/cancel
  tests).
- `frontend/src/App.svelte` — MODIFY route effect (call `setSidebarActive`
  instead of unconditional `load()`).
- `frontend/src/lib/stores/usage.svelte.ts` — MODIFY (`lastUpdated`,
  `hasNewData`, `busy`, `markStale()`; parallel `fetchAll`).
- `frontend/src/lib/stores/usage.test.ts` — CREATE.
- `frontend/src/lib/components/usage/UsagePage.svelte` — MODIFY (remove
  interval; SSE → `markStale` not fetch; staleness + busy UI).

Each task is independently committable. Backend tasks (1–3) and frontend tasks
(4–8) have no cross-dependencies; within frontend, do 4 before 5 (both edit
`load()`), and 6 before 8 (8 uses `markStale`/`lastUpdated`/`busy`).

______________________________________________________________________

## Task 1: SQLite covering index (C1)

Widen the existing partial usage index into a covering index so the messages
branch of the usage query reads every column it needs from the index. The
existing index keeps its name reserved; create the wider one under a new name
and drop the old one so existing databases migrate (a same-name
`CREATE INDEX IF NOT EXISTS` would be a silent no-op on an existing index).

**Files:**

- Modify: `internal/db/db.go` (`createPartialIndexesLocked`, ~lines 763-787)

- Create: `internal/db/usage_covering_index_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/db/usage_covering_index_test.go`:

```go
package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUsageCoveringIndexMigration verifies that a fresh DB has the
// widened covering index and not the legacy narrow one, and that a
// pre-upgrade DB (legacy index present, covering absent) is migrated
// on reopen without losing data.
func TestUsageCoveringIndexMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	d, err := Open(path)
	requireNoError(t, err, "initial open")
	insertSession(t, d, "s1", "proj")
	d.Close()

	requireIndex(t, path, "idx_messages_usage_covering", 1)
	requireIndex(t, path, "idx_messages_usage_timestamp", 0)

	// Simulate a pre-upgrade DB: drop the covering index, recreate
	// the legacy narrow one.
	conn, err := sql.Open("sqlite3", path)
	requireNoError(t, err, "raw open")
	_, err = conn.Exec(`DROP INDEX IF EXISTS idx_messages_usage_covering`)
	requireNoError(t, err, "drop covering")
	_, err = conn.Exec(`CREATE INDEX idx_messages_usage_timestamp
		ON messages(timestamp, session_id, ordinal)
		WHERE token_usage != '' AND model != '' AND model != '<synthetic>'`)
	requireNoError(t, err, "recreate legacy index")
	conn.Close()

	// Reopen runs createPartialIndexesLocked, which must migrate.
	d, err = Open(path)
	requireNoError(t, err, "reopen")
	defer d.Close()

	requireIndex(t, path, "idx_messages_usage_covering", 1)
	requireIndex(t, path, "idx_messages_usage_timestamp", 0)

	var sessions int
	row := d.reader.Load().QueryRow(`SELECT count(*) FROM sessions`)
	requireNoError(t, row.Scan(&sessions), "count sessions")
	require.Equal(t, 1, sessions, "session row must survive migration")
}

func requireIndex(t *testing.T, path, name string, want int) {
	t.Helper()
	conn, err := sql.Open("sqlite3", path)
	requireNoError(t, err, "raw open for index check")
	defer conn.Close()
	var n int
	err = conn.QueryRow(
		`SELECT count(*) FROM sqlite_master
		 WHERE type='index' AND name=?`, name,
	).Scan(&n)
	requireNoError(t, err, "query sqlite_master")
	require.Equal(t, want, n, "index %s presence", name)
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run:
`CGO_ENABLED=1 go test -tags fts5 -run TestUsageCoveringIndexMigration -v ./internal/db/`
Expected: FAIL — `idx_messages_usage_covering` does not exist (count 0, want 1).

- [ ] **Step 3: Implement the covering index + legacy drop**

In `internal/db/db.go`, in `createPartialIndexesLocked`, replace the
`idx_messages_usage_timestamp` entry in the `indexes` slice with the covering
index, then drop the legacy index after the create loop. The function becomes:

```go
func (db *DB) createPartialIndexesLocked(w *sql.DB) error {
	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_sessions_cwd
		 ON sessions(cwd) WHERE cwd != ''`,
		`CREATE INDEX IF NOT EXISTS idx_messages_compact_boundary
		 ON messages(session_id, ordinal) WHERE is_compact_boundary = 1`,
		`CREATE INDEX IF NOT EXISTS idx_messages_sidechain
		 ON messages(session_id) WHERE is_sidechain = 1`,
		`CREATE INDEX IF NOT EXISTS idx_messages_source_uuid
		 ON messages(source_uuid) WHERE source_uuid != ''`,
		// Covering index for the usage query's messages branch: the
		// scan reads timestamp, session_id, ordinal, model,
		// claude_message_id, claude_request_id, and token_usage, so
		// keying all of them lets SQLite serve the scan from the index
		// without touching the content-heavy table rows.
		`CREATE INDEX IF NOT EXISTS idx_messages_usage_covering
		 ON messages(timestamp, session_id, ordinal, model,
		             claude_message_id, claude_request_id, token_usage)
		 WHERE token_usage != ''
		   AND model != ''
		   AND model != '<synthetic>'`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_has_secret
		 ON sessions(secret_leak_count) WHERE secret_leak_count > 0`,
	}
	for _, ddl := range indexes {
		if _, err := w.Exec(ddl); err != nil {
			return fmt.Errorf("creating index: %w", err)
		}
	}
	// Drop the legacy narrow index superseded by the covering one.
	// A same-name CREATE IF NOT EXISTS cannot widen an existing index,
	// so the new index uses a new name and the old one is removed here.
	if _, err := w.Exec(
		`DROP INDEX IF EXISTS idx_messages_usage_timestamp`,
	); err != nil {
		return fmt.Errorf("dropping legacy usage index: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run:
`CGO_ENABLED=1 go test -tags fts5 -run TestUsageCoveringIndexMigration -v ./internal/db/`
Expected: PASS.

- [ ] **Step 5: Run the db package tests + format/vet**

Run: `CGO_ENABLED=1 go test -tags fts5 ./internal/db/` Then:
`go fmt ./internal/db/... && go vet ./internal/db/...` Expected: PASS, no diffs,
no vet warnings.

- [ ] **Step 6: Commit**

```bash
git add internal/db/db.go internal/db/usage_covering_index_test.go
git commit -m "perf(db): widen usage index into a covering index (sqlite)"
```

______________________________________________________________________

## Task 2: PostgreSQL/Cockroach covering index (C1 parity)

Mirror Task 1 on the PG schema. PG/Cockroach currently has **no** usage index on
`messages` at all (only `idx_messages_velocity`), plus the legacy
`idx_messages_usage_timestamp` created by `createPartialIndexesPG`. The covering
index **omits `token_usage`** because PG/Cockroach btree index tuples have a
size limit that an unbounded TEXT blob can exceed; all other read columns are
bounded and safe to key. Observable results are identical (`token_usage` is
still read from the heap for the messages branch).

**Files:**

- Modify: `internal/postgres/schema.go` (`createPartialIndexesPG`, ~lines
  647-671)

- Create: `internal/postgres/usage_covering_index_pgtest_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/postgres/usage_covering_index_pgtest_test.go`:

```go
//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

const usageIdxTestSchema = "agentsview_usage_idx_test"

func cleanUsageIdxTestPG(t *testing.T, pgURL string) {
	t.Helper()
	pg, err := sql.Open("pgx", pgURL)
	require.NoError(t, err, "connecting to pg")
	defer pg.Close()
	_, _ = pg.Exec("DROP SCHEMA IF EXISTS " + usageIdxTestSchema + " CASCADE")
}

// TestUsageCoveringIndexPG verifies EnsureSchema creates the covering
// index and drops the legacy narrow index, idempotently.
func TestUsageCoveringIndexPG(t *testing.T) {
	pgURL := testPGURL(t)
	cleanUsageIdxTestPG(t, pgURL)
	t.Cleanup(func() { cleanUsageIdxTestPG(t, pgURL) })

	pg, err := Open(pgURL, usageIdxTestSchema, true)
	require.NoError(t, err, "connecting to pg")
	defer pg.Close()

	ctx := context.Background()
	require.NoError(t, EnsureSchema(ctx, pg, usageIdxTestSchema),
		"EnsureSchema (first)")
	require.NoError(t, EnsureSchema(ctx, pg, usageIdxTestSchema),
		"EnsureSchema (idempotency)")

	var covering bool
	err = pg.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE schemaname = $1
			  AND indexname = 'idx_messages_usage_covering'
		)`, usageIdxTestSchema).Scan(&covering)
	require.NoError(t, err, "checking covering index")
	require.True(t, covering, "covering index must exist")

	var legacy bool
	err = pg.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE schemaname = $1
			  AND indexname = 'idx_messages_usage_timestamp'
		)`, usageIdxTestSchema).Scan(&legacy)
	require.NoError(t, err, "checking legacy index")
	require.False(t, legacy, "legacy narrow index must be dropped")
}
```

- [ ] **Step 2: Run the test to verify it fails**

Start a test PG (`make test-postgres` leaves one running) or set `TEST_PG_URL`,
then run:
`TEST_PG_URL="$TEST_PG_URL" CGO_ENABLED=1 go test -tags "fts5,pgtest" -run TestUsageCoveringIndexPG -v ./internal/postgres/`
Expected: FAIL — covering index absent (and/or legacy still present).

- [ ] **Step 3: Implement the covering index + legacy drop**

In `internal/postgres/schema.go`, in `createPartialIndexesPG`, replace the
`idx_messages_usage_timestamp` entry with the covering index (no `token_usage`),
and drop the legacy index after the loop:

```go
func createPartialIndexesPG(ctx context.Context, db *sql.DB) error {
	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_sessions_cwd
		 ON sessions(cwd) WHERE cwd != ''`,
		`CREATE INDEX IF NOT EXISTS idx_messages_compact_boundary
		 ON messages(session_id, ordinal) WHERE is_compact_boundary = TRUE`,
		`CREATE INDEX IF NOT EXISTS idx_messages_sidechain
		 ON messages(session_id) WHERE is_sidechain = TRUE`,
		`CREATE INDEX IF NOT EXISTS idx_messages_source_uuid
		 ON messages(source_uuid) WHERE source_uuid != ''`,
		// Covering index for the usage query's messages branch.
		// token_usage is intentionally omitted from the key: a
		// PG/Cockroach btree tuple cannot safely hold an unbounded
		// TEXT blob, so it stays a heap read. Results are identical
		// to SQLite; only the index shape differs (see AGENTS.md
		// Backend Parity).
		`CREATE INDEX IF NOT EXISTS idx_messages_usage_covering
		 ON messages(timestamp, session_id, ordinal, model,
		             claude_message_id, claude_request_id)
		 WHERE token_usage != ''
		   AND model != ''
		   AND model != '<synthetic>'`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_has_secret
		 ON sessions(secret_leak_count) WHERE secret_leak_count > 0`,
	}
	for _, ddl := range indexes {
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("creating PG index: %w", err)
		}
	}
	// Drop the legacy narrow index superseded by the covering one.
	// `DROP INDEX IF EXISTS <name>` is accepted by both PostgreSQL and
	// CockroachDB.
	if _, err := db.ExecContext(ctx,
		`DROP INDEX IF EXISTS idx_messages_usage_timestamp`,
	); err != nil {
		return fmt.Errorf("dropping legacy PG usage index: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run:
`TEST_PG_URL="$TEST_PG_URL" CGO_ENABLED=1 go test -tags "fts5,pgtest" -run TestUsageCoveringIndexPG -v ./internal/postgres/`
Expected: PASS.

- [ ] **Step 5: Format/vet**

Run:
`go fmt ./internal/postgres/... && go vet -tags "fts5,pgtest" ./internal/postgres/...`
Expected: no diffs, no warnings.

- [ ] **Step 6: Commit**

```bash
git add internal/postgres/schema.go internal/postgres/usage_covering_index_pgtest_test.go
git commit -m "perf(postgres): add usage covering index, drop legacy (parity)"
```

______________________________________________________________________

## Task 3: Gzip response middleware (W1)

Compress responses for clients that send `Accept-Encoding: gzip`, skipping
streaming (SSE) responses by inspecting the `Content-Type` set by the handler,
and skipping already-encoded bodies. Implement `http.Flusher` passthrough so SSE
keeps streaming even though it flows through the wrapper.

**Files:**

- Create: `internal/server/compress.go`

- Create: `internal/server/compress_test.go`

- Modify: `internal/server/server.go` (`Handler()`, ~line 354)

- [ ] **Step 1: Write the failing test**

Create `internal/server/compress_test.go`:

```go
package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGzipMiddlewareCompressesJSON(t *testing.T) {
	body := bytes.Repeat([]byte(`{"k":"v"},`), 500)
	h := gzipMiddleware(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/usage/summary", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, "gzip", rec.Header().Get("Content-Encoding"))
	require.Empty(t, rec.Header().Get("Content-Length"))
	gr, err := gzip.NewReader(rec.Body)
	require.NoError(t, err, "new gzip reader")
	got, err := io.ReadAll(gr)
	require.NoError(t, err, "read gzip body")
	require.Equal(t, body, got, "decompressed body must match")
}

func TestGzipMiddlewareSkipsEventStream(t *testing.T) {
	h := gzipMiddleware(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: hi\n\n"))
		}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Empty(t, rec.Header().Get("Content-Encoding"),
		"SSE must not be gzipped")
	require.Equal(t, "data: hi\n\n", rec.Body.String())
}

func TestGzipMiddlewareSkipsWithoutAcceptEncoding(t *testing.T) {
	h := gzipMiddleware(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"k":"v"}`))
		}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Empty(t, rec.Header().Get("Content-Encoding"))
	require.Equal(t, `{"k":"v"}`, rec.Body.String())
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run:
`CGO_ENABLED=1 go test -tags fts5 -run TestGzipMiddleware -v ./internal/server/`
Expected: FAIL — `undefined: gzipMiddleware`.

- [ ] **Step 3: Implement the middleware**

Create `internal/server/compress.go`:

```go
package server

import (
	"compress/gzip"
	"net/http"
	"strings"
)

// gzipMiddleware compresses responses for clients that accept gzip.
// The decision to compress is made lazily on the first WriteHeader/
// Write, once the handler has set Content-Type, so streaming (SSE)
// and already-encoded responses pass through untouched. It implements
// http.Flusher so SSE handlers keep streaming through the wrapper.
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Accept-Encoding")
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
	compress    bool
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	if w.ResponseWriter.Header().Get("Content-Encoding") == "" &&
		isCompressibleType(w.ResponseWriter.Header().Get("Content-Type")) {
		w.compress = true
		w.ResponseWriter.Header().Del("Content-Length")
		w.ResponseWriter.Header().Set("Content-Encoding", "gzip")
		w.gz = gzip.NewWriter(w.ResponseWriter)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.compress {
		return w.gz.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

// Flush passes through so SSE keeps streaming. When compressing, the
// gzip buffer is flushed first so framed events reach the client.
func (w *gzipResponseWriter) Flush() {
	if w.compress && w.gz != nil {
		_ = w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *gzipResponseWriter) close() {
	if w.gz != nil {
		_ = w.gz.Close()
	}
}

func isCompressibleType(contentType string) bool {
	ct := strings.ToLower(contentType)
	if strings.HasPrefix(ct, "text/event-stream") {
		return false
	}
	return strings.HasPrefix(ct, "application/json") ||
		strings.HasPrefix(ct, "text/") ||
		strings.HasPrefix(ct, "application/javascript") ||
		strings.HasPrefix(ct, "image/svg+xml")
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run:
`CGO_ENABLED=1 go test -tags fts5 -run TestGzipMiddleware -v ./internal/server/`
Expected: PASS (all three).

- [ ] **Step 5: Wire the middleware into the chain**

In `internal/server/server.go`, in `Handler()`, wrap `s.mux` with
`gzipMiddleware` inside `logMiddleware`. Change:

```go
				corsMiddleware(
					allowedOrigins, bindAll, s.cfg.Port, bindAllIPs, logMiddleware(s.mux),
				),
```

to:

```go
				corsMiddleware(
					allowedOrigins, bindAll, s.cfg.Port, bindAllIPs,
					logMiddleware(gzipMiddleware(s.mux)),
				),
```

- [ ] **Step 6: Run server tests + format/vet**

Run: `CGO_ENABLED=1 go test -tags fts5 ./internal/server/` Then:
`go fmt ./internal/server/... && go vet ./internal/server/...` Expected: PASS,
no diffs, no warnings. (If any existing test asserts an exact `Content-Length`
on an API response while sending `Accept-Encoding: gzip`, update it to gunzip
first — note it in the commit.)

- [ ] **Step 7: Commit**

```bash
git add internal/server/compress.go internal/server/compress_test.go internal/server/server.go
git commit -m "perf(server): gzip API responses, bypassing SSE streams"
```

______________________________________________________________________

## Task 4: Gate the global sessions load to the sessions route (A2/A3)

`App.svelte` currently calls `sessions.load()` on **every** route change, and
`load()` both fetches the 30 MB sidebar index and arms a permanent live-refresh
(SSE subscription + 5-minute `setInterval`). The gate lives in the store so it
also covers `sync.onSyncComplete` and the internal callers, not just the route
effect: `load()` no-ops unless the sidebar is active, and `setSidebarActive`
arms (on enter) / disposes (on leave) the background work.

**Files:**

- Modify: `frontend/src/lib/stores/sessions.svelte.ts` (state field, `load()`,
  new `setSidebarActive`)

- Modify: `frontend/src/App.svelte` (route effect, ~lines 238-250)

- Modify: `frontend/src/lib/stores/sessions.test.ts`

- [ ] **Step 1: Write the failing tests**

Append to `frontend/src/lib/stores/sessions.test.ts` (the file already mocks the
generated client, runtime, and `watchEvents` — reuse
`api.getSidebarSessionIndex` and `api.watchEvents` from its `vi.hoisted` block):

```ts
describe("sidebar route gating", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    storageData.clear();
    api.getSidebarSessionIndex.mockResolvedValue({ sessions: [], total: 0 });
  });

  it("does not fetch the sidebar index while inactive", async () => {
    const store = createSessionsStore();
    await store.load();
    expect(api.getSidebarSessionIndex).not.toHaveBeenCalled();
  });

  it("fetches and arms live refresh when activated", async () => {
    const store = createSessionsStore();
    store.setSidebarActive(true);
    await Promise.resolve();
    await Promise.resolve();
    expect(api.getSidebarSessionIndex).toHaveBeenCalledTimes(1);
    expect(api.watchEvents).toHaveBeenCalledTimes(1);
  });

  it("stops live refresh when deactivated", async () => {
    const close = vi.fn();
    api.watchEvents.mockReturnValue({ close });
    const store = createSessionsStore();
    store.setSidebarActive(true);
    await Promise.resolve();
    store.setSidebarActive(false);
    expect(close).toHaveBeenCalledTimes(1);
  });
});
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd frontend && npx vitest run src/lib/stores/sessions.test.ts` Expected:
FAIL — `store.setSidebarActive is not a function`; the first test fails because
`load()` currently fetches unconditionally.

- [ ] **Step 3: Add the `sidebarActive` field**

In `frontend/src/lib/stores/sessions.svelte.ts`, add the field next to the
live-refresh fields (after line 259, `private safetyNetTimer ...`):

```ts
  private sidebarActive = false;
```

- [ ] **Step 4: Guard `load()` and add `setSidebarActive`**

Add the guard as the first line of `load()` (before `saveFilters`):

```ts
  async load() {
    if (!this.sidebarActive) return;
    saveFilters(this.filters);
    this.startLiveRefresh();
    // ...unchanged...
  }
```

Add the public method (place it right after `load()`):

```ts
  // setSidebarActive gates all sidebar-index work to the route that
  // actually renders the list. Activating loads + arms live refresh;
  // deactivating tears down the SSE subscription and timers so no
  // background fetching happens off-route. Because load() self-guards
  // on sidebarActive, the sync.onSyncComplete and internal callers
  // also become no-ops while inactive.
  setSidebarActive(active: boolean) {
    if (this.sidebarActive === active) return;
    this.sidebarActive = active;
    if (active) {
      this.load();
    } else {
      this.dispose();
    }
  }
```

- [ ] **Step 5: Update the App route effect**

In `frontend/src/App.svelte`, in the route `$effect` (~lines 238-250), replace
the unconditional `sessions.load()` with the gate. Keep the cheap
`loadProjects()`/`loadAgents()` (the usage filters need them):

```svelte
  $effect(() => {
    const route = router.route;
    const params = router.params;
    untrack(() => {
      const sid = router.sessionId;
      if (!sid && route === "sessions" && hasFilterParams(params)) {
        sessions.initFromParams(params);
      }
      sessions.setSidebarActive(route === "sessions");
      sessions.loadProjects();
      sessions.loadAgents();
    });
  });
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `cd frontend && npx vitest run src/lib/stores/sessions.test.ts` Expected:
PASS (existing tests too — existing tests that call `load()` directly must first
call `setSidebarActive(true)`; update any that now no-op, and note it in the
commit).

- [ ] **Step 7: Typecheck + lint**

Run: `cd frontend && npx tsc --noEmit && npx oxlint src/` Expected: no errors.

- [ ] **Step 8: Commit**

```bash
git add frontend/src/lib/stores/sessions.svelte.ts frontend/src/App.svelte frontend/src/lib/stores/sessions.test.ts
git commit -m "perf(web): gate sidebar index load+live-refresh to sessions route"
```

______________________________________________________________________

## Task 5: Coalesce and cancel sidebar-index requests (A4)

`load()` guards stale results with `loadVersion` **after** the await, so a
superseded request still downloads and parses the full 30 MB. Add: (a) coalesce
— skip a new `load()` when an identical request (same filter signature) is
already in flight; (b) cancel — abort a superseded in-flight request via
`AbortController` routed through `callGenerated(fn, signal)`.

**Files:**

- Modify: `frontend/src/lib/stores/sessions.svelte.ts` (imports, fields,
  `load()`)

- Modify: `frontend/src/lib/stores/sessions.test.ts`

- [ ] **Step 1: Write the failing tests**

Append to `frontend/src/lib/stores/sessions.test.ts`:

```ts
describe("sidebar request coalescing and cancellation", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    storageData.clear();
  });

  it("coalesces identical concurrent loads into one request", async () => {
    let resolve!: (v: unknown) => void;
    api.getSidebarSessionIndex.mockReturnValue(
      new Promise((r) => { resolve = r; }),
    );
    const store = createSessionsStore();
    store.setSidebarActive(true); // first load (in flight)
    store.load();                 // identical signature -> coalesced
    expect(api.getSidebarSessionIndex).toHaveBeenCalledTimes(1);
    resolve({ sessions: [], total: 0 });
    await Promise.resolve();
  });

  it("aborts a superseded in-flight load when filters change", async () => {
    const signals: (AbortSignal | undefined)[] = [];
    vi.mocked(callGenerated).mockImplementation(
      (request: () => Promise<unknown>, signal?: AbortSignal) => {
        signals.push(signal);
        return request();
      },
    );
    api.getSidebarSessionIndex.mockReturnValue(new Promise(() => {})); // never
    const store = createSessionsStore();
    store.setSidebarActive(true);     // load with signature A
    store.setProject("other-proj");   // changes filters -> load signature B
    expect(signals[0]?.aborted).toBe(true);
  });
});
```

Add `callGenerated` to the imports at the top of the test file
(`import { callGenerated } from "../api/runtime.js";`) — the existing
`vi.mock("../api/runtime.js", ...)` already exports it as a passthrough mock;
the second test overrides it. If `setProject` is not the exact filter-setter
name, use any setter that calls `this.load()` with changed `apiParams`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd frontend && npx vitest run src/lib/stores/sessions.test.ts` Expected:
FAIL — coalescing not implemented (called twice); no signal recorded / not
aborted.

- [ ] **Step 3: Add imports and fields**

In `frontend/src/lib/stores/sessions.svelte.ts`, extend the runtime import (it
currently imports only `configureGeneratedClient`):

```ts
import {
  configureGeneratedClient,
  callGenerated,
  isAbortError,
} from "../api/runtime.js";
```

Add fields next to `sidebarActive`:

```ts
  private sidebarAbort: AbortController | null = null;
  private inflightSignature: string | null = null;
```

- [ ] **Step 4: Rewrite `load()` to coalesce, cancel, and route through
  callGenerated**

Replace the body of `load()` with (guard from Task 4 retained at top):

```ts
  async load() {
    if (!this.sidebarActive) return;
    saveFilters(this.filters);
    this.startLiveRefresh();

    const signature = JSON.stringify(this.apiParams);
    // Coalesce: an identical request is already in flight and will
    // populate the same state, so skip this duplicate.
    if (this.inflightSignature === signature) return;
    // Cancel a superseded in-flight request so its (up to 30 MB)
    // payload stops downloading/parsing instead of being fetched and
    // then discarded by the version guard below.
    this.sidebarAbort?.abort();
    const controller = new AbortController();
    this.sidebarAbort = controller;
    this.inflightSignature = signature;

    const version = ++this.loadVersion;
    const indexVersion = this.sidebarIndexVersion + 1;
    this.loading = true;
    const prev = {
      sessions: this.sessions,
      nextCursor: this.nextCursor,
      total: this.total,
    };
    try {
      const index = await callGenerated(
        () => SessionsService.getApiV1SessionsSidebarIndex(this.apiParams),
        controller.signal,
      ) as unknown as SidebarSessionIndexResponse;
      if (this.loadVersion !== version) return;

      this.sidebarIndexVersion = indexVersion;
      this.hydratedSessionsByVersion.set(indexVersion, new Map());
      this.sidebarHydrationEpochByVersion.set(indexVersion, 0);
      this.pruneSidebarHydrationVersions(indexVersion);
      const previousById = new Map(prev.sessions.map((s) => [s.id, s]));
      this.sessions = index.sessions.map((row) =>
        sidebarIndexRowToSession(row, previousById.get(row.id))
      );
      this.nextCursor = null;
      this.total = index.total;
    } catch (e) {
      if (isAbortError(e)) return;
      // Restore previous state so a transient failure doesn't wipe
      // the visible session list.
      if (this.loadVersion === version) {
        this.sessions = prev.sessions;
        this.nextCursor = prev.nextCursor;
        this.total = prev.total;
      }
    } finally {
      if (this.inflightSignature === signature) {
        this.inflightSignature = null;
      }
      if (this.sidebarAbort === controller) {
        this.sidebarAbort = null;
      }
      if (this.loadVersion === version) {
        this.loading = false;
      }
    }
  }
```

Note: `callGenerated` calls `configureGeneratedClient()` internally, so the
prior explicit `configureGeneratedClient()` line inside `load()` is removed. The
import stays — other methods still call it directly.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `cd frontend && npx vitest run src/lib/stores/sessions.test.ts` Expected:
PASS.

- [ ] **Step 6: Typecheck + lint**

Run: `cd frontend && npx tsc --noEmit && npx oxlint src/` Expected: no errors.

- [ ] **Step 7: Commit**

```bash
git add frontend/src/lib/stores/sessions.svelte.ts frontend/src/lib/stores/sessions.test.ts
git commit -m "perf(web): coalesce and cancel in-flight sidebar index requests"
```

______________________________________________________________________

## Task 6: Usage store — staleness, busy, and new-data state (D1/D3)

Add the state the manual-refresh UX needs: `lastUpdated` (set after a successful
refresh), `hasNewData` (set by an SSE event indicating sessions changed, cleared
on refresh), `busy` (true while a refresh is running, for the spinner), and
`markStale()`.

**Files:**

- Modify: `frontend/src/lib/stores/usage.svelte.ts`

- Create: `frontend/src/lib/stores/usage.test.ts`

- [ ] **Step 1: Write the failing test**

Create `frontend/src/lib/stores/usage.test.ts`:

```ts
import { describe, it, expect, vi, beforeEach } from "vitest";

const storageData = new Map<string, string>();
Object.defineProperty(globalThis, "localStorage", {
  value: {
    getItem: (k: string) => storageData.get(k) ?? null,
    setItem: (k: string, v: string) => { storageData.set(k, v); },
    removeItem: (k: string) => { storageData.delete(k); },
    clear: () => { storageData.clear(); },
  },
  configurable: true,
  writable: true,
});

vi.mock("../api/runtime.js", () => ({
  configureGeneratedClient: vi.fn(),
  callGenerated: vi.fn((request: () => Promise<unknown>) => request()),
  isAbortError: vi.fn(() => false),
}));

const usageApi = vi.hoisted(() => ({
  summary: vi.fn(),
  topSessions: vi.fn(),
  comparison: vi.fn(),
}));

vi.mock("../api/generated/index", () => ({
  UsageService: {
    getApiV1UsageSummary: vi.fn((p) => usageApi.summary(p)),
    getApiV1UsageTopSessions: vi.fn((p) => usageApi.topSessions(p)),
    getApiV1UsageComparison: vi.fn((p) => usageApi.comparison(p)),
  },
}));

import { usage } from "./usage.svelte.js";

function emptySummary() {
  return { totals: { totalCost: 0 }, daily: [], modelTotals: [] };
}

describe("usage staleness state", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    usage.hasNewData = false;
    usage.lastUpdated = null;
    usageApi.summary.mockResolvedValue(emptySummary());
    usageApi.topSessions.mockResolvedValue({ sessions: [] });
    usageApi.comparison.mockResolvedValue({});
  });

  it("markStale sets hasNewData", () => {
    usage.markStale();
    expect(usage.hasNewData).toBe(true);
  });

  it("fetchAll records lastUpdated and clears hasNewData", async () => {
    usage.markStale();
    await usage.fetchAll();
    expect(usage.hasNewData).toBe(false);
    expect(usage.lastUpdated).not.toBeNull();
  });

  it("fetchAll toggles busy off when done", async () => {
    await usage.fetchAll();
    expect(usage.busy).toBe(false);
  });
});
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd frontend && npx vitest run src/lib/stores/usage.test.ts` Expected: FAIL
— `usage.markStale is not a function` / `lastUpdated`/`busy` undefined.

- [ ] **Step 3: Add the state fields and `markStale`**

In `frontend/src/lib/stores/usage.svelte.ts`, add fields next to the existing
`loading` declaration (after line 193):

```ts
  lastUpdated = $state<number | null>(null);
  hasNewData = $state(false);
  busy = $state(false);
```

Add the method (place it near `fetchAll`):

```ts
  // markStale flags that sessions changed since the last refresh.
  // The usage page calls this from an SSE subscription instead of
  // auto-fetching, so the user can refresh on demand.
  markStale(): void {
    this.hasNewData = true;
  }
```

(The actual setting of `lastUpdated`/`hasNewData=false`/`busy` happens in
`fetchAll`, implemented in Task 7. To make this task's test pass on its own,
also set them in the current `fetchAll` now; Task 7 replaces that body.)

In the current `fetchAll`, set `busy` and record freshness:

```ts
  async fetchAll() {
    const fetchVersion = ++this.fetchAllVersion;
    this.busy = true;
    this.invalidatePanel("topSessions");
    this.rollDates();
    saveUsageFilters(this);
    const loadedSummary = await this.fetchSummary({ loadComparison: false });
    if (fetchVersion !== this.fetchAllVersion || !loadedSummary) {
      if (fetchVersion === this.fetchAllVersion) this.busy = false;
      return;
    }
    await this.fetchTopSessions(loadedSummary.params);
    if (fetchVersion !== this.fetchAllVersion) return;
    void this.fetchComparison(
      loadedSummary.version,
      loadedSummary.summary,
      loadedSummary.params,
    );
    this.lastUpdated = Date.now();
    this.hasNewData = false;
    this.busy = false;
  }
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd frontend && npx vitest run src/lib/stores/usage.test.ts` Expected:
PASS.

- [ ] **Step 5: Typecheck + lint**

Run: `cd frontend && npx tsc --noEmit && npx oxlint src/` Expected: no errors.

- [ ] **Step 6: Commit**

```bash
git add frontend/src/lib/stores/usage.svelte.ts frontend/src/lib/stores/usage.test.ts
git commit -m "feat(web): add usage staleness, busy, and new-data state"
```

______________________________________________________________________

## Task 7: Parallelize the usage fetch (D2)

`fetchAll` runs `fetchSummary` then `fetchTopSessions` serially (~6–7s
all-time), even though top-sessions needs only the filter params, not the
summary result. Run both concurrently against the same `baseParams()` so panels
fill independently (~3–3.6s).

**Files:**

- Modify: `frontend/src/lib/stores/usage.svelte.ts` (`fetchAll`)

- Modify: `frontend/src/lib/stores/usage.test.ts`

- [ ] **Step 1: Write the failing test**

Append to `frontend/src/lib/stores/usage.test.ts`:

```ts
describe("usage parallel fetch", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    usageApi.summary.mockResolvedValue(emptySummary());
    usageApi.topSessions.mockResolvedValue({ sessions: [] });
    usageApi.comparison.mockResolvedValue({});
  });

  it("starts summary and top-sessions without awaiting summary first", async () => {
    const order: string[] = [];
    let releaseSummary!: () => void;
    usageApi.summary.mockImplementation(
      () =>
        new Promise((resolve) => {
          order.push("summary-start");
          releaseSummary = () => resolve(emptySummary());
        }),
    );
    usageApi.topSessions.mockImplementation(() => {
      order.push("top-start");
      return Promise.resolve({ sessions: [] });
    });

    const done = usage.fetchAll();
    await Promise.resolve();
    await Promise.resolve();
    // top-sessions started even though summary is still pending.
    expect(order).toContain("top-start");
    releaseSummary();
    await done;
    expect(usageApi.summary).toHaveBeenCalledTimes(1);
    expect(usageApi.topSessions).toHaveBeenCalledTimes(1);
  });
});
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd frontend && npx vitest run src/lib/stores/usage.test.ts` Expected: FAIL
— `top-start` is not present while summary is pending (serial).

- [ ] **Step 3: Implement the parallel fetchAll**

Replace `fetchAll` with the concurrent version. `fetchSummary` computes its own
`baseParams()` and triggers its comparison internally when `loadComparison` is
true, so we let it own the comparison and pass the same params to
`fetchTopSessions`:

```ts
  async fetchAll() {
    const fetchVersion = ++this.fetchAllVersion;
    this.busy = true;
    this.rollDates();
    saveUsageFilters(this);
    const params = this.baseParams();
    await Promise.allSettled([
      this.fetchSummary({ loadComparison: true }),
      this.fetchTopSessions(params),
    ]);
    if (fetchVersion !== this.fetchAllVersion) return;
    this.lastUpdated = Date.now();
    this.hasNewData = false;
    this.busy = false;
  }
```

Notes: `fetchSummary` and `fetchTopSessions` each manage their own version
counter and `AbortController`, so dropping the explicit `invalidatePanel` is
safe — `fetchTopSessions` aborts any prior in-flight top request via
`nextAbortSignal`. `Promise.allSettled` ensures `busy`/`lastUpdated` are set
even if one panel errors (each method swallows its own non-abort errors).

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd frontend && npx vitest run src/lib/stores/usage.test.ts` Expected: PASS
(all usage tests, including Task 6's).

- [ ] **Step 5: Typecheck + lint**

Run: `cd frontend && npx tsc --noEmit && npx oxlint src/` Expected: no errors.

- [ ] **Step 6: Commit**

```bash
git add frontend/src/lib/stores/usage.svelte.ts frontend/src/lib/stores/usage.test.ts
git commit -m "perf(web): run usage summary and top-sessions concurrently"
```

______________________________________________________________________

## Task 8: Usage page — manual refresh + staleness UI (A1/D1/D3)

Remove the 5-minute interval and the SSE-driven auto-fetch; subscribe to SSE
only to flag new data (`markStale`). Show "Updated X ago", a "new data" dot on
the refresh button, and a busy spinner while refreshing. Per-panel first-load
skeletons already exist in `UsageSummaryCards`/`TopSessionsTable` (they read
`usage.loading`), so D1 here is refresh feedback, not a new skeleton component.

**Files:**

- Modify: `frontend/src/lib/components/usage/UsagePage.svelte`

- [ ] **Step 1: Remove the interval and rewire SSE to markStale**

In the `<script>`, delete `const REFRESH_MS = 5 * 60 * 1000;` and the
`refreshTimer` declaration. Replace the `onMount`/`onDestroy` bodies:

```svelte
  let unsubEvents: (() => void) | undefined;
  let mounted = false;
  let nowTs = $state(Date.now());
  let staleTicker: ReturnType<typeof setInterval> | undefined;

  onMount(() => {
    mounted = true;
    tick().then(() => {
      urlWritebackReady = true;
    });
    // Manual refresh only: an SSE event flags new data instead of
    // re-running the (CPU-bound) usage scans on every session write.
    unsubEvents = events.subscribe((e) => {
      if (e.scope === "sessions" || e.scope === "sync") {
        usage.markStale();
      }
    });
    staleTicker = setInterval(() => { nowTs = Date.now(); }, 30_000);
  });

  onDestroy(() => {
    unsubEvents?.();
    if (staleTicker !== undefined) clearInterval(staleTicker);
  });
```

- [ ] **Step 2: Add the relative-time label**

In the `<script>`, add a helper and a derived label:

```ts
  function formatAgo(from: number | null, now: number): string {
    if (from === null) return "not yet loaded";
    const secs = Math.max(0, Math.round((now - from) / 1000));
    if (secs < 5) return "just now";
    if (secs < 60) return `${secs}s ago`;
    const mins = Math.round(secs / 60);
    if (mins < 60) return `${mins}m ago`;
    const hrs = Math.round(mins / 60);
    return `${hrs}h ago`;
  }

  const updatedLabel = $derived(formatAgo(usage.lastUpdated, nowTs));
```

- [ ] **Step 3: Update the refresh control markup**

Replace the refresh button (~lines 305-312) with the staleness control:

```svelte
      <div class="usage-refresh">
        <span class="updated-label" title="Last refreshed">
          Updated {updatedLabel}
        </span>
        <button
          class="refresh-btn"
          class:has-new={usage.hasNewData}
          class:busy={usage.busy}
          onclick={() => usage.fetchAll()}
          disabled={usage.busy}
          title={usage.hasNewData ? "New data available — refresh" : "Refresh"}
          aria-label="Refresh usage data"
        >
          <RefreshCwIcon size="14" strokeWidth="2" aria-hidden="true" />
          {#if usage.hasNewData}
            <span class="new-dot" aria-hidden="true"></span>
          {/if}
        </button>
      </div>
```

Add styles in the component's `<style>` block:

```svelte
  .usage-refresh {
    display: inline-flex;
    align-items: center;
    gap: 0.5rem;
  }
  .updated-label {
    font-size: 0.75rem;
    color: var(--text-secondary, #888);
  }
  .refresh-btn.busy :global(svg) {
    animation: usage-spin 0.8s linear infinite;
  }
  .refresh-btn {
    position: relative;
  }
  .new-dot {
    position: absolute;
    top: 2px;
    right: 2px;
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--accent, #3b82f6);
  }
  @keyframes usage-spin {
    to { transform: rotate(360deg); }
  }
```

(If the component uses CSS variables under different names, match the existing
ones in this file.)

- [ ] **Step 4: Verify no other references to the removed symbols**

Run:
`cd frontend && rg -n "REFRESH_MS|refreshTimer|subscribeDebounced" src/lib/components/usage/UsagePage.svelte`
Expected: no matches.

- [ ] **Step 5: Typecheck, lint, build**

Run: `cd frontend && npx tsc --noEmit && npx oxlint src/ && npm run build`
Expected: no errors; build succeeds.

- [ ] **Step 6: Commit**

```bash
git add frontend/src/lib/components/usage/UsagePage.svelte
git commit -m "feat(web): manual-refresh usage page with staleness indicator"
```

______________________________________________________________________

## Task 9: Verification (re-measure)

No code changes — run the harnesses from the spec's §9 and confirm the targets.
This produces the data for the D-3 decision (whether C2 / sidebar pagination is
still worth it).

- [ ] **Step 1: Backend query timings + EXPLAIN (covering index)**

Run the read-only harness against a **copy** of the prod DB (never the live
archive):

```bash
REAL_DB=/tmp/agytest/sessions.db CGO_ENABLED=1 \
  go test -tags fts5 -run TestRealDBUsagePerf -v -timeout 1200s ./internal/db/
```

Capture `EXPLAIN QUERY PLAN` for the usage query before/after and confirm the
covering index (`idx_messages_usage_covering`) is used and the messages-branch
table lookups become index-only reads. Do **not** require the temp B-tree sort
to disappear (the `UNION` + `COALESCE(u.ts, ...)` shape may keep it; removing it
is out of scope).

- [ ] **Step 2: Payload + gzip**

Confirm the sidebar/summary payloads and that gzip is applied on the wire (run a
server on the copy and check `Content-Encoding: gzip` on `/api/v1/usage/summary`
with `Accept-Encoding: gzip`):

```bash
REAL_DB=/tmp/agytest/sessions.db CGO_ENABLED=1 \
  go test -tags fts5 -run TestRealDBUsagePayload -v -timeout 600s ./internal/db/
```

- [ ] **Step 3: Browser behavior (Playwright)**

With a server on the copy, run the capture script and assert: idle usage tab = 0
background fetches; usage tab issues **0** sidebar-index requests; the sessions
route issues exactly one sidebar request per filter change; manual refresh fills
panels progressively.

- [ ] **Step 4: Full test + lint sweep**

Run:

```bash
make test
make lint
make vet
cd frontend && npx vitest run && npx tsc --noEmit && npx oxlint src/
```

Expected: all green.

- [ ] **Step 5: Record results**

Write the before/after numbers into the spec's §2 (or a short results note) so
the D-3 decision (C2 vs sidebar pagination) is made on measured data. No commit
needed if only the gitignored spec changed; otherwise commit any tracked
updates.

______________________________________________________________________

## Self-Review

**Spec coverage** (each Phase-1 item → task):

- A1 manual refresh → Task 8 (interval removed; SSE → markStale).
- A2/A3 gate global load in store → Task 4.
- A4 coalesce + cancel → Task 5.
- D1 loading feedback → Task 6 (`busy`) + Task 8 (spinner). Per-panel first-load
  skeletons already exist; not re-implemented (documented).
- D2 parallel + progressive → Task 7.
- D3 staleness UI → Task 6 (`lastUpdated`/`hasNewData`/`markStale`) + Task 8 UI.
- C1 widened covering index, SQLite + PG/Cockroach parity → Tasks 1 + 2.
- W1 gzip responses (SSE-bypass) → Task 3.
- Verification/re-measure → Task 9.

**Type/name consistency:** `setSidebarActive`, `sidebarActive`, `sidebarAbort`,
`inflightSignature` (sessions store); `lastUpdated`, `hasNewData`, `busy`,
`markStale` (usage store); `gzipMiddleware`/`gzipResponseWriter`/
`isCompressibleType` (server); `idx_messages_usage_covering` (both backends)
used identically across tasks. `callGenerated(fn, signal)` / `isAbortError`
signatures match `internal/.../runtime.ts`.

**Placeholder scan:** no TBD/TODO; every code step shows complete code; every
test step shows the assertion and the run command with expected result.

**Known divergence (intentional, documented):** the PG/Cockroach covering index
omits `token_usage` (btree tuple-size limit); observable results are identical
to SQLite. Recorded in Task 2 and the spec §5/§8.

**Risk to verify during execution:** the Task 4 `load()` guard makes off-route
`load()` calls no-ops. Existing `sessions.test.ts` tests that call `load()`
directly must call `setSidebarActive(true)` first; the e2e sweep (Task 9) must
confirm the command palette and session navigation still populate the list on
the sessions route.
