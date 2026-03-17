# Claude Code Instructions

## Project Overview

agentsview is a local web viewer for AI agent sessions (Claude Code, Codex, Copilot CLI, Gemini CLI, OpenCode, Amp). It syncs session data from disk into SQLite (with FTS5 full-text search), serves a Svelte 5 SPA via an embedded Go HTTP server, and provides real-time updates via SSE.

## Architecture

```
CLI (agentsview) → Config → DB (SQLite/FTS5)
                  ↓              ↓
              File Watcher → Sync Engine → Parser (Claude, Codex, Copilot, Gemini, OpenCode, Amp)
                  ↓              ↓
              HTTP Server → REST API + SSE + Embedded SPA
                                 ↓
                           PG Push Sync → PostgreSQL (optional)
                                 ↑
              HTTP Server (pg-read mode) ← PostgreSQL
```

- **Server**: HTTP server with auto-port discovery (default 8080)
- **Storage**: SQLite with WAL mode, FTS5 for full-text search; optional PostgreSQL for multi-machine shared access
- **Sync**: File watcher + periodic sync (15min) for session directories
- **PG Sync**: Push-only sync from SQLite to PostgreSQL (configurable interval)
- **Frontend**: Svelte 5 SPA embedded in the Go binary at build time
- **Config**: Env vars (`AGENT_VIEWER_DATA_DIR`, `CLAUDE_PROJECTS_DIR`, `CODEX_SESSIONS_DIR`, `COPILOT_DIR`, `GEMINI_DIR`, `OPENCODE_DIR`, `AMP_DIR`) and CLI flags

## Project Structure

- `cmd/agentsview/` - Go server entrypoint
- `cmd/testfixture/` - Test data generator for E2E tests
- `internal/config/` - Config loading, flag registration, legacy migration
- `internal/db/` - SQLite operations (sessions, messages, search, analytics)
- `internal/pgdb/` - PostgreSQL read-only store (implements `db.Store` for PG-backed serving)
- `internal/pgsync/` - Push sync from local SQLite to PostgreSQL
- `internal/parser/` - Session file parsers (Claude, Codex, Copilot, Gemini, OpenCode, Amp, content extraction)
- `internal/server/` - HTTP handlers, SSE, middleware, search, export
- `internal/sync/` - Sync engine, file watcher, discovery, hashing
- `internal/timeutil/` - Time parsing utilities
- `internal/web/` - Embedded frontend (dist/ copied at build time)
- `frontend/` - Svelte 5 SPA (Vite, TypeScript)
- `scripts/` - Utility scripts (E2E server, changelog)

## Key Files

| Path | Purpose |
|------|---------|
| `cmd/agentsview/main.go` | CLI entry point, server startup, file watcher |
| `internal/server/server.go` | HTTP router and handler setup |
| `internal/server/sessions.go` | Session list/detail API handlers |
| `internal/server/search.go` | Full-text search API |
| `internal/server/events.go` | SSE event streaming |
| `internal/db/db.go` | Database open, migrations, schema |
| `internal/db/sessions.go` | Session CRUD queries |
| `internal/db/search.go` | FTS5 search queries |
| `internal/sync/engine.go` | Sync orchestration |
| `internal/parser/claude.go` | Claude Code session parser |
| `internal/parser/codex.go` | Codex session parser |
| `internal/parser/copilot.go` | Copilot CLI session parser |
| `internal/parser/amp.go` | Amp session parser |
| `internal/pgdb/pgdb.go` | PostgreSQL read-only store |
| `internal/pgdb/sessions.go` | PG session list/detail queries |
| `internal/pgdb/messages.go` | PG message queries, ILIKE search |
| `internal/pgdb/analytics.go` | PG analytics queries |
| `internal/pgsync/pgsync.go` | PG push sync lifecycle |
| `internal/pgsync/push.go` | Push logic (sessions, messages, tool calls) |
| `internal/pgsync/schema.go` | PG schema DDL and migrations |
| `internal/config/config.go` | Config loading, flag registration |

## Development

```bash
make build          # Build binary with embedded frontend
make dev            # Run Go server in dev mode
make frontend       # Build frontend SPA only
make frontend-dev   # Run Vite dev server (use alongside make dev)
make install        # Build and install to ~/.local/bin or GOPATH
make install-hooks  # Install pre-commit git hooks
```

After making Go code changes, always run `go fmt ./...` and `go vet ./...` before committing.

## Testing

**All new features and bug fixes must include unit tests.** Run tests before committing:

```bash
make test       # Go tests (CGO_ENABLED=1 -tags fts5)
make test-short # Fast tests only (-short flag)
make e2e        # Playwright E2E tests
make lint       # golangci-lint
make vet        # go vet
```

### PostgreSQL Integration Tests

PG integration tests require a real PostgreSQL instance and the `pgtest`
build tag. Set `TEST_PG_URL` to a valid connection string:

```bash
TEST_PG_URL="postgres://user:pass@host:5432/dbname?sslmode=disable" \
  CGO_ENABLED=1 go test -tags "fts5,pgtest" ./internal/pgsync/... -v
```

Tests create and drop the `agentsview` schema, so use a dedicated
database or one where schema changes are acceptable.

### Test Guidelines

- Table-driven tests for Go code
- Use `testDB(t)` helper for database tests
- Frontend: colocated `*.test.ts` files, Playwright specs in `frontend/e2e/`
- All tests use `t.TempDir()` for temp directories

## Build Requirements

- **CGO_ENABLED=1** required (sqlite3 driver)
- **Build tag**: `-tags fts5` required for full-text search
- **Frontend**: Node.js + npm for Svelte build, embedded via `internal/web/dist/`

## Conventions

- Prefer stdlib over external dependencies
- Tests should be fast and isolated
- No emojis in code or output
- **The database is a persistent archive** — never drop, truncate, or recreate the database to handle data version changes. Use non-destructive migrations (ALTER TABLE, UPDATE) and full resync (build fresh DB, copy orphaned data, swap) instead. Session data must survive even when the original source files are gone.
- **Markdown formatting**: Use `mdformat --wrap 80` to format Markdown files. Requires the `mdformat-tables` plugin (`uv tool install mdformat --with mdformat-tables`).

## Git Workflow

- **Commit every turn** - always commit your work at the end of each turn, no exceptions
- **Never amend commits** - always create new commits for fixes, never use `--amend`
- **Never change branches** - don't create, switch, or delete branches without explicit permission
- Use conventional commit messages
- Run tests before committing when applicable
- Never push or pull unless explicitly asked
