# Project Listing and PG Push Filtering

## Overview

Add a CLI command to list projects and the ability to filter `pg push` by
project name, configured via CLI flags or TOML config.

## 1. `agentsview projects` Command

New top-level subcommand that queries SQLite and prints all known projects with
session counts.

### Output

Default (table):

```
PROJECT                  SESSIONS
my-app                   42
research                 15
agentsview               8
```

JSON (`--json` flag):

```json
[
  {"name": "my-app", "session_count": 42},
  {"name": "research", "session_count": 15},
  {"name": "agentsview", "session_count": 8}
]
```

### Implementation

- New file: `cmd/agentsview/projects.go`
- Reuses existing `db.GetProjects()` which returns `[]ProjectInfo{Name,
  SessionCount}`
- Flags: `--json` for machine-readable output
- No new DB code required

## 2. `pg push` Project Filtering

### CLI Flags

Two new flags on `agentsview pg push`:

- `--projects=foo,bar` — only push sessions belonging to listed projects
  (inclusive)
- `--exclude-projects=baz,qux` — push all sessions except those in listed
  projects (exclusive)

These flags are **mutually exclusive**. Specifying both returns an error before
any work begins.

### TOML Config

Under the existing `[pg]` section:

```toml
[pg]
url = "postgres://..."
projects = ["foo", "bar"]
exclude_projects = ["baz", "qux"]
```

Same mutual exclusivity rule applies in config. Validation happens at command
startup.

### CLI Override Behavior

CLI flags override their TOML config counterparts entirely (not merged). If
`--projects` is set on the CLI, `pg.projects` from TOML is ignored. Same for
`--exclude-projects` and `pg.exclude_projects`.

### Filter Implementation (Approach A: Filter at Push Boundary)

The `Push()` method passes the project filter down to
`ListSessionsModifiedBetween`, which adds a WHERE clause:

- Include: `AND project IN (?, ?, ...)`
- Exclude: `AND project NOT IN (?, ?, ...)`

Filtering happens at the SQL level so no unnecessary sessions are fetched or
fingerprinted.

### Config Changes

`PGConfig` struct gains two new fields:

```go
type PGConfig struct {
    URL              string   // existing
    Schema           string   // existing
    MachineName      string   // existing
    AllowInsecure    bool     // existing
    Projects         []string // new: inclusive project filter
    ExcludeProjects  []string // new: exclusive project filter
}
```

### Data Flow

1. Command handler reads CLI flags; if set, overrides config values
2. Validates mutual exclusivity of projects/exclude_projects
3. Passes filter to `Push()` method
4. `Push()` passes filter to `ListSessionsModifiedBetween()`
5. SQL query applies WHERE clause before returning candidates

## 3. Error Handling

- **No matching projects**: If `--projects` specifies project names with no
  modified sessions, zero sessions are pushed. This is not an error — the
  `projects` command lets users discover valid names beforehand.
- **Empty string projects**: Sessions with empty project strings are only
  included if `""` is explicitly in the filter list.
- **`--full` + `--projects`**: Works as expected — full resync scoped to the
  specified projects only.
- **Mutual exclusivity violation**: Returns error immediately with a message
  like `--projects and --exclude-projects are mutually exclusive`.

## 4. Testing

- **`projects` command**: Table-driven test verifying output format (text and
  JSON) against a test DB seeded with known sessions across multiple projects.
- **Push filtering**: Unit test for `ListSessionsModifiedBetween` with project
  include/exclude params, verifying correct sessions are returned.
- **Config parsing**: Test that TOML parsing populates `PGConfig.Projects` and
  `ExcludeProjects` correctly.
- **Mutual exclusivity**: Test that specifying both flags returns an error.
- **CLI override**: Test that CLI flags override TOML config values.

All tests use existing `testDB(t)` helper and table-driven patterns.

## Files to Change

| File | Change |
| ---- | ------ |
| `cmd/agentsview/projects.go` | New file: `projects` subcommand |
| `cmd/agentsview/pg.go` | Add `--projects` and `--exclude-projects` flags to `pg push` |
| `internal/config/config.go` | Add `Projects` and `ExcludeProjects` to `PGConfig` |
| `internal/db/sessions.go` | Add project filter params to `ListSessionsModifiedBetween` |
| `internal/postgres/push.go` | Thread project filter through `Push()` to DB query |
| `internal/postgres/sync.go` | Pass filter from sync entry point to push |
