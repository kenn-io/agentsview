# AGENTS.md

Instructions for autonomous coding agents working in this repository.

## Scope

- Applies to all agent-driven work in this repo.
- If multiple instruction files exist, follow the most specific one for the
  files you are editing.

## Required Git Rules

1. Commit every turn that changes tracked files.
1. Do not make empty commits. If a turn is read-only or only changes ignored
   files, state that no commit was made.
1. Do not amend commits.
1. Do not change branches without explicit user permission.

## Commit Expectations

- Keep commits focused and related to the requested task.
- Use clear conventional commit messages.
- Do not push, pull, or rebase unless explicitly requested.
- Do not include generated-with lines, attribution blocks, validation footers,
  or command transcripts in commit messages.

## Validation

- Run relevant tests before committing when practical.
- If tests cannot be run, state that clearly in the handoff.
- After Go code changes, run `go fmt ./...` and `go vet ./...` before
  committing.

## Backend Parity

- Preserve behavior and query-shape parity between supported storage backends
  whenever practical. SQLite and PostgreSQL/Cockroach queries, indexes,
  aggregations, filtering, and ordering should match until there is a
  concrete, documented reason for them to differ.
- Do not implement a performance or correctness fix for only one backend and
  call the problem solved unless the user explicitly scopes the work to that
  backend, for example "this is only for PostgreSQL". If one backend needs a
  different implementation, explain why and keep the observable behavior the
  same.

## Test Style

- Go tests use `github.com/stretchr/testify` for assertions. Use `require.X`
  when a failed check should abort the test (setup, nil receivers, length
  checks before indexing) and `assert.X` for independent checks that should
  keep running. Don't write `if got != want { t.Fatalf(...) }` in new tests.
- Domain-specific helpers are fine, but they must use testify internally rather
  than stdlib comparisons.

## Safety

- Do not revert user-authored or unrelated local changes unless explicitly
  requested.
- Avoid destructive git commands unless explicitly requested.
- The SQLite database is a persistent archive. Never delete, drop, truncate, or
  recreate it to handle data version changes. Schema changes use
  non-destructive migrations such as `ALTER TABLE` and `UPDATE`; parser
  changes trigger a full resync that builds a fresh DB, syncs files, copies
  orphaned sessions from the old DB, and swaps atomically. Existing session
  data must be preserved even when source files no longer exist on disk.

## Project Overview

agentsview is a local web viewer for AI agent sessions. It syncs session data
from disk into SQLite with FTS5 full-text search, serves a Svelte 5 SPA via an
embedded Go HTTP server, and provides real-time updates via SSE. See
`internal/parser/types.go` for the full list of supported agents.

## Architecture

```text
CLI (agentsview) -> Config -> DB (SQLite/FTS5)
                  |           |
                  v           v
              File Watcher -> Sync Engine -> Parsers (per agent)
                  |           |
                  v           v
              HTTP Server -> REST API + SSE + Embedded SPA
                              |
                              v
                           PG Push Sync -> PostgreSQL (optional)
                              ^
                              |
              HTTP Server (pg serve) <- PostgreSQL
```

- Server: HTTP server with auto-port discovery, defaulting to 8080.
- Storage: SQLite with WAL mode, FTS5 for full-text search, and optional
  PostgreSQL for multi-machine shared access.
- Sync: file watcher plus periodic sync every 15 minutes for session
  directories.
- PG sync: on-demand push sync from SQLite to PostgreSQL via `pg push`.
- Frontend: Svelte 5 SPA embedded in the Go binary at build time.
- Config: `AGENTSVIEW_DATA_DIR` plus per-agent directory overrides and CLI
  flags. Per-agent env vars are listed on each entry in
  `internal/parser/types.go`.

## Project Structure

- `cmd/agentsview/` - Go server entrypoint.
- `cmd/testfixture/` - Test data generator for E2E tests.
- `internal/config/` - Config loading, JSON migration, and flag registration.
- `internal/db/` - SQLite sessions, messages, search, analytics, and schema.
- `internal/postgres/` - PostgreSQL push sync, read-only store, schema, and
  connection helpers.
- `internal/parser/` - Per-agent session file parsers and content extraction.
- `internal/server/` - HTTP handlers, SSE, middleware, search, and export.
- `internal/sync/` - Sync engine, file watcher, discovery, and hashing.
- `internal/timeutil/` - Time parsing utilities.
- `internal/web/` - Embedded frontend copied from `frontend/dist/` at build
  time.
- `frontend/` - Svelte 5 SPA with Vite and TypeScript.
- `scripts/` - Utility scripts for E2E server setup and changelog work.

## Key Files

| Path                             | Purpose                                       |
| -------------------------------- | --------------------------------------------- |
| `cmd/agentsview/main.go`         | CLI entry point, server startup, file watcher |
| `cmd/agentsview/pg.go`           | `pg` command group: push, status, serve       |
| `internal/server/server.go`      | HTTP router and handler setup                 |
| `internal/server/sessions.go`    | Session list/detail API handlers              |
| `internal/server/search.go`      | Full-text search API                          |
| `internal/server/events.go`      | SSE event streaming                           |
| `internal/db/db.go`              | Database open, migrations, schema             |
| `internal/db/sessions.go`        | Session CRUD queries                          |
| `internal/db/search.go`          | FTS5 search queries                           |
| `internal/sync/engine.go`        | Sync orchestration                            |
| `internal/parser/types.go`       | Agent registry with one `AgentDef` per agent  |
| `internal/parser/*.go`           | Per-agent session parsers                     |
| `internal/postgres/connect.go`   | Connection setup, SSL checks, DSN helpers     |
| `internal/postgres/schema.go`    | PG DDL and schema management                  |
| `internal/postgres/push.go`      | Push logic and fingerprinting                 |
| `internal/postgres/sync.go`      | Push sync lifecycle                           |
| `internal/postgres/store.go`     | PostgreSQL read-only store                    |
| `internal/postgres/sessions.go`  | PG session queries on the read side           |
| `internal/postgres/messages.go`  | PG message queries and ILIKE search           |
| `internal/postgres/analytics.go` | PG analytics queries                          |
| `internal/postgres/time.go`      | Timestamp conversion helpers                  |
| `internal/config/config.go`      | Config loading and flag registration          |

## Development

```bash
make build          # Build binary with embedded frontend
make dev            # Run Go server in dev mode
make frontend       # Build frontend SPA only
make frontend-dev   # Run Vite dev server, use alongside make dev
make install        # Build and install to ~/.local/bin or GOPATH
make install-hooks  # Install pre-commit and pre-push git hooks
```

## Testing

All new features and bug fixes must include unit tests. Run tests before
committing:

```bash
make test       # Go tests with CGO_ENABLED=1 and -tags "fts5"
make test-short # Fast tests only with -short
make e2e        # Playwright E2E tests
make lint       # golangci-lint plus NilAway
make vet        # go vet
```

## Test Style

- Prefer table-driven tests for Go code.
- Go tests use `github.com/stretchr/testify` for assertions.
- Use `require.X` when a failed check should abort the test, including setup
  errors, nil receivers, and length checks before indexing.
- Use `assert.X` for independent checks that should keep running.
- Do not write `if got != want { t.Fatalf(...) }` in new tests.
- Domain-specific helpers are fine, but they must use testify internally rather
  than stdlib comparisons.
- Use the existing `testDB(t)` helper for database tests.
- Frontend tests are colocated `*.test.ts` files, with Playwright specs in
  `frontend/e2e/`.
- All tests use `t.TempDir()` for temp directories.
- Shell script tests must exercise observable behavior by running the script
  against controlled inputs and asserting outputs, side effects, or exit
  codes. Do not write tautological tests that read a shell script and assert
  that it contains a specific implementation line, flag, or snippet.

## PostgreSQL Integration Tests

PG integration tests require a real PostgreSQL instance and the `pgtest` build
tag. The easiest way to run them is with docker-compose:

```bash
make test-postgres   # Starts PG container, runs tests, leaves container running
make postgres-down   # Stop the test container when done
```

Or manually with an existing PostgreSQL instance:

```bash
TEST_PG_URL="postgres://user:pass@host:5432/dbname?sslmode=disable" \
  CGO_ENABLED=1 go test -tags "fts5,pgtest" ./internal/postgres/... -v
```

Tests create and drop the `agentsview` schema, so use a dedicated database or
one where schema changes are acceptable. The CI pipeline runs these tests via a
GitHub Actions service container in `.github/workflows/ci.yml`.

## Build Requirements

- `CGO_ENABLED=1` is required for the sqlite3 driver.
- The `fts5` build tag is required for full-text search.
- `go test` does not need kit's `kit_posthog_disabled` build tag. The telemetry
  reporter already short-circuits to a disabled no-op under
  `testing.Testing()`, so tests never send PostHog events. Binaries built for
  e2e tests (`make e2e`, the CI pre-build of `agentsview`/`testfixture`) do
  use the tag, because they run as real processes where the test guard does
  not apply.
- Node.js and npm are required to build the Svelte frontend embedded under
  `internal/web/dist/`.

## Conventions

- Prefer stdlib over external dependencies.
- Tests should be fast and isolated.
- No emojis in code or output.
- For frontend UI work, read `DESIGN.md` before adding or changing controls,
  styling, or reusable components.
- Use `mdformat --wrap 80` to format Markdown files when mdformat and
  `mdformat-tables` are available.

## Pull Requests

- PR descriptions should be summaries only, with no test plans or checklists. Do
  not add a "Tests", "Testing", "Verification", or "Test plan" section. CI
  runs the tests, so the description must not restate the suite, list test
  commands, or describe how the change was verified.
- Describe what the code does now, why it changed, tradeoffs, limitations, and
  where reviewers should look.
