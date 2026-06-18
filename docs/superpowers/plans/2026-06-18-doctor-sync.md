# Doctor Sync Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development
> (if subagents available) or superpowers:executing-plans to implement this
> plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a read-only `agentsview doctor sync` command for human-readable
startup sync diagnostics.

**Architecture:** Add a small Cobra command group in `cmd/agentsview` and keep
diagnostic collection in a focused file. Reuse existing config loading and
SQLite access, but inspect `PRAGMA user_version` with a read-only raw connection
so diagnostics do not trigger a sync.

**Tech Stack:** Go, Cobra, SQLite via `github.com/mattn/go-sqlite3`, existing
testify test style.

______________________________________________________________________

### Task 1: CLI Command And Report

**Files:**

- Create: `cmd/agentsview/doctor.go`

- Test: `cmd/agentsview/doctor_test.go`

- Modify: `cmd/agentsview/cli.go`

- Modify: `internal/db/db.go`

- [ ] **Step 1: Write failing CLI tests**

Add tests for:

- `agentsview doctor sync` on a current DB reports no data-version resync.

- `agentsview doctor sync` on a stale `PRAGMA user_version` reports full resync
  and likely aborted prior resync when debug log contains `resync aborted`.

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
go test -tags "fts5" ./cmd/agentsview -run 'TestDoctorSync' -count=1
```

Expected: fail because the command does not exist.

- [ ] **Step 3: Implement minimal command**

Create `doctor.go` with a `doctor` command and `sync` subcommand. Add an
exported DB data-version accessor in `internal/db/db.go`, then collect the
report:

- config paths.
- raw SQLite `PRAGMA user_version`.
- grouped session data-version counts.
- temp resync files.
- configured root existence.
- filtered tail of `debug.log`.
- likely-cause text.

Wire `doctor` into the root command.

- [ ] **Step 4: Run focused tests**

Run:

```bash
go test -tags "fts5" ./cmd/agentsview -run 'TestDoctorSync' -count=1
```

Expected: pass.

- [ ] **Step 5: Run Go formatting and vet**

Run:

```bash
go fmt ./...
go vet ./...
```

Expected: both succeed.

- [ ] **Step 6: Commit**

Commit docs and code with a focused conventional commit.
