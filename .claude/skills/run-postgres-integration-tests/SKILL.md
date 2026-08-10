---
name: run-postgres-integration-tests
description: Run AgentsView PostgreSQL integration and backend-parity tests against a dedicated disposable local database. Use for pgtest failures, PostgreSQL storage changes, or release gates that require TEST_PG_URL.
---

# Run PostgreSQL integration tests

1. Read `docs/agents/testing.md`, `docs/agents/storage.md`, and
   `docs/agents/build.md`.
2. Never use production, shared, or persistent archive databases. The tests
   drop and recreate test schemas.
3. Create a unique cluster below `$env:TEMP`, bind it only to `127.0.0.1`, and
   use a free non-default port. Initialize it with PostgreSQL 17 `initdb`, UTF-8,
   locale `C`, user `postgres`, and local trust authentication.
4. Start with `pg_ctl -w`, create a dedicated `agentsview_test` database, and
   verify the database and server version with `psql`.
5. Set `TEST_PG_URL` only in the test process. Set `CGO_ENABLED=1` and verify
   the compiler target is `x86_64-w64-mingw32` before running Go.
6. Run the smallest gate first:

   ```powershell
   go test -tags 'fts5,pgtest' ./internal/postgres/... -run '^TestIssueReviewRowsConditionallyLoadsResultTail$' -v -count=1
   ```

7. Run the full canonical gate only after the focused test passes:

   ```powershell
   go test -tags 'fts5,pgtest' ./internal/postgres/... -json -count=1
   ```

   For large output, retain only failed test events and nearby output in the
   conversation. Keep the unfiltered JSON outside Git if exact diagnosis is
   needed.
8. Do not repeat a failure unchanged. Identify the failing test, then rerun
   that test with `-run '^ExactTestName$'` before another full suite.
9. Stop the exact scratch server with `pg_ctl -w stop -t 360`. A full suite can
   leave a large checkpoint that legitimately exceeds 30 seconds; while the
   log shows checkpoint progress, wait instead of killing the process. Remove
   the cluster only after resolving the absolute path and proving it is a child
   named `agentsview-pgtest-*` below `$env:TEMP`. Preserve logs on failure until
   the cause is recorded.
