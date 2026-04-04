# Project Listing and PG Push Filtering Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an `agentsview projects` command to list projects from SQLite, and
`--projects`/`--exclude-projects` flags to `pg push` for filtering which
sessions are synced to PostgreSQL.

**Architecture:** Extend `PGConfig` with project filter fields, add project
filter parameters to `ListSessionsModifiedBetween`, thread them through the
`Push()` call path, and add a new `projects` CLI subcommand that reuses
`db.GetProjects()`.

**Tech Stack:** Go, SQLite, TOML config (BurntSushi/toml)

---

## File Map

| File                                    | Action | Purpose                                        |
| --------------------------------------- | ------ | ---------------------------------------------- |
| `internal/config/config.go`             | Modify | Add `Projects`, `ExcludeProjects` to `PGConfig` |
| `internal/config/config_test.go`        | Modify | Test TOML parsing and mutual exclusivity        |
| `internal/db/sessions.go`               | Modify | Add project filter to `ListSessionsModifiedBetween` |
| `internal/db/db_test.go`                | Modify | Test project filtering in `ListSessionsModifiedBetween` |
| `internal/postgres/push.go`             | Modify | Thread project filter through `Push()`          |
| `internal/postgres/sync.go`             | Modify | Add filter fields to `Sync` struct              |
| `cmd/agentsview/pg.go`                  | Modify | Add CLI flags, validation, pass to `Sync`       |
| `cmd/agentsview/projects.go`            | Create | New `projects` subcommand                       |
| `cmd/agentsview/main.go`                | Modify | Register `projects` in command dispatch          |

---

### Task 1: Add project filter fields to PGConfig

**Files:**
- Modify: `internal/config/config.go:58-63`
- Modify: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test for TOML parsing**

Add to `internal/config/config_test.go`:

```go
func TestPGConfig_ProjectFilter(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	os.WriteFile(tomlPath, []byte(`
[pg]
url = "postgres://localhost/test"
projects = ["alpha", "beta"]
`), 0o644)

	cfg, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = dir
	if err := cfg.loadFile(); err != nil {
		t.Fatalf("loadFile: %v", err)
	}

	if len(cfg.PG.Projects) != 2 {
		t.Fatalf("Projects = %v, want [alpha beta]", cfg.PG.Projects)
	}
	if cfg.PG.Projects[0] != "alpha" || cfg.PG.Projects[1] != "beta" {
		t.Errorf("Projects = %v, want [alpha beta]", cfg.PG.Projects)
	}
}

func TestPGConfig_ExcludeProjectFilter(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	os.WriteFile(tomlPath, []byte(`
[pg]
url = "postgres://localhost/test"
exclude_projects = ["gamma"]
`), 0o644)

	cfg, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = dir
	if err := cfg.loadFile(); err != nil {
		t.Fatalf("loadFile: %v", err)
	}

	if len(cfg.PG.ExcludeProjects) != 1 {
		t.Fatalf("ExcludeProjects = %v, want [gamma]", cfg.PG.ExcludeProjects)
	}
	if cfg.PG.ExcludeProjects[0] != "gamma" {
		t.Errorf("ExcludeProjects = %v, want [gamma]", cfg.PG.ExcludeProjects)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /home/tmaloney/gitdev/com.github/tlmaloney/agentsview && CGO_ENABLED=1 go test -tags fts5 -run 'TestPGConfig_ProjectFilter|TestPGConfig_ExcludeProjectFilter' ./internal/config/ -v`

Expected: FAIL — `PGConfig` has no `Projects` or `ExcludeProjects` fields.

- [ ] **Step 3: Add fields to PGConfig**

In `internal/config/config.go`, update the `PGConfig` struct:

```go
// PGConfig holds PostgreSQL connection settings.
type PGConfig struct {
	URL             string   `toml:"url" json:"url"`
	Schema          string   `toml:"schema" json:"schema"`
	MachineName     string   `toml:"machine_name" json:"machine_name"`
	AllowInsecure   bool     `toml:"allow_insecure" json:"allow_insecure"`
	Projects        []string `toml:"projects" json:"projects,omitempty"`
	ExcludeProjects []string `toml:"exclude_projects" json:"exclude_projects,omitempty"`
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /home/tmaloney/gitdev/com.github/tlmaloney/agentsview && CGO_ENABLED=1 go test -tags fts5 -run 'TestPGConfig_ProjectFilter|TestPGConfig_ExcludeProjectFilter' ./internal/config/ -v`

Expected: PASS

- [ ] **Step 5: Run go fmt and go vet**

Run: `cd /home/tmaloney/gitdev/com.github/tlmaloney/agentsview && go fmt ./internal/config/ && go vet -tags fts5 ./internal/config/`

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat: add Projects and ExcludeProjects fields to PGConfig"
```

---

### Task 2: Add project filter to ListSessionsModifiedBetween

**Files:**
- Modify: `internal/db/sessions.go:1331-1408`
- Modify: `internal/db/db_test.go`

- [ ] **Step 1: Write the failing test for project include filter**

Add to `internal/db/db_test.go`:

```go
func TestListSessionsModifiedBetween_ProjectFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	sessions := []Session{
		{ID: "s1", Project: "alpha", Machine: "local", Agent: "claude", CreatedAt: "2026-03-10T12:00:00.000Z"},
		{ID: "s2", Project: "beta", Machine: "local", Agent: "claude", CreatedAt: "2026-03-10T12:00:00.000Z"},
		{ID: "s3", Project: "gamma", Machine: "local", Agent: "claude", CreatedAt: "2026-03-10T12:00:00.000Z"},
	}
	for _, s := range sessions {
		if err := d.UpsertSession(s); err != nil {
			t.Fatalf("upsert %s: %v", s.ID, err)
		}
	}
	for _, s := range sessions {
		_, err := d.getWriter().Exec(
			"UPDATE sessions SET created_at = ? WHERE id = ?",
			s.CreatedAt, s.ID,
		)
		if err != nil {
			t.Fatalf("backdate %s: %v", s.ID, err)
		}
	}

	tests := []struct {
		name            string
		projects        []string
		excludeProjects []string
		wantIDs         []string
	}{
		{
			name:    "no filter returns all",
			wantIDs: []string{"s1", "s2", "s3"},
		},
		{
			name:     "include alpha only",
			projects: []string{"alpha"},
			wantIDs:  []string{"s1"},
		},
		{
			name:     "include alpha and gamma",
			projects: []string{"alpha", "gamma"},
			wantIDs:  []string{"s1", "s3"},
		},
		{
			name:            "exclude beta",
			excludeProjects: []string{"beta"},
			wantIDs:         []string{"s1", "s3"},
		},
		{
			name:     "include nonexistent project",
			projects: []string{"nope"},
			wantIDs:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := d.ListSessionsModifiedBetween(
				ctx, "", "", tt.projects, tt.excludeProjects,
			)
			if err != nil {
				t.Fatalf("ListSessionsModifiedBetween: %v", err)
			}
			var gotIDs []string
			for _, s := range got {
				gotIDs = append(gotIDs, s.ID)
			}
			if len(gotIDs) != len(tt.wantIDs) {
				t.Fatalf("got %v, want %v", gotIDs, tt.wantIDs)
			}
			for i, id := range tt.wantIDs {
				if gotIDs[i] != id {
					t.Errorf("got[%d] = %q, want %q", i, gotIDs[i], id)
				}
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/tmaloney/gitdev/com.github/tlmaloney/agentsview && CGO_ENABLED=1 go test -tags fts5 -run TestListSessionsModifiedBetween_ProjectFilter ./internal/db/ -v`

Expected: FAIL — `ListSessionsModifiedBetween` doesn't accept project filter parameters.

- [ ] **Step 3: Update ListSessionsModifiedBetween signature and implementation**

In `internal/db/sessions.go`, change the function signature and add project
filtering logic. The new signature adds `projects []string` and
`excludeProjects []string` parameters after the existing `since, until string`
parameters.

```go
func (db *DB) ListSessionsModifiedBetween(
	ctx context.Context, since, until string,
	projects, excludeProjects []string,
) ([]Session, error) {
	query := "SELECT " + sessionFullCols + " FROM sessions"
	var (
		args  []any
		where []string
	)
	if since != "" {
		sinceTime, err := time.Parse(time.RFC3339Nano, since)
		if err != nil {
			return nil, fmt.Errorf(
				"parsing since timestamp %q: %w", since, err,
			)
		}
		sinceText := sinceTime.UTC().Format("2006-01-02T15:04:05.000Z")
		sinceNano := sinceTime.UnixNano()
		where = append(where, `(file_mtime > ?
			OR `+sqliteSyncTimestampExpr(colLocalModifiedAt)+` > ?
			OR `+sqliteSyncTimestampExpr(colBestTimestamp)+` > ?
			OR `+sqliteSyncTimestampExpr(colCreatedAt)+` > ?)`)
		args = append(args, sinceNano, sinceText, sinceText, sinceText)
	}
	if until != "" {
		untilTime, err := time.Parse(time.RFC3339Nano, until)
		if err != nil {
			return nil, fmt.Errorf(
				"parsing until timestamp %q: %w", until, err,
			)
		}
		untilText := untilTime.UTC().Format("2006-01-02T15:04:05.000Z")
		untilNano := untilTime.UnixNano()
		where = append(where, `(COALESCE(file_mtime, -1) <= ?
			AND COALESCE(`+sqliteSyncTimestampExpr(colLocalModifiedAt)+`, '') <= ?
			AND `+sqliteSyncTimestampExpr(colBestTimestamp)+` <= ?
			AND `+sqliteSyncTimestampExpr(colCreatedAt)+` <= ?)`)
		args = append(args, untilNano, untilText, untilText, untilText)
	}
	if len(projects) > 0 {
		placeholders := make([]string, len(projects))
		for i, p := range projects {
			placeholders[i] = "?"
			args = append(args, p)
		}
		where = append(where, "project IN ("+strings.Join(placeholders, ", ")+")")
	}
	if len(excludeProjects) > 0 {
		placeholders := make([]string, len(excludeProjects))
		for i, p := range excludeProjects {
			placeholders[i] = "?"
			args = append(args, p)
		}
		where = append(where, "project NOT IN ("+strings.Join(placeholders, ", ")+")")
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += ` ORDER BY created_at`

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf(
			"listing sessions modified since %s: %w",
			since, err,
		)
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		var s Session
		err := rows.Scan(
			&s.ID, &s.Project, &s.Machine, &s.Agent,
			&s.FirstMessage, &s.DisplayName, &s.StartedAt, &s.EndedAt,
			&s.MessageCount, &s.UserMessageCount,
			&s.ParentSessionID, &s.RelationshipType,
			&s.TotalOutputTokens, &s.PeakContextTokens,
			&s.HasTotalOutputTokens, &s.HasPeakContextTokens,
			&s.IsAutomated,
			&s.DeletedAt, &s.FilePath, &s.FileSize,
			&s.FileMtime, &s.FileHash, &s.LocalModifiedAt, &s.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scanning session: %w", err)
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}
```

- [ ] **Step 4: Fix all callers of ListSessionsModifiedBetween**

There are two call sites in `internal/postgres/push.go` (lines 114 and 154).
Update both to pass `nil, nil` for the new parameters:

At line 114:
```go
	allSessions, err := s.local.ListSessionsModifiedBetween(
		ctx, lastPush, cutoff, nil, nil,
	)
```

At line 154:
```go
		boundarySessions, err := s.local.ListSessionsModifiedBetween(
			ctx, windowStart, lastPush, nil, nil,
		)
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd /home/tmaloney/gitdev/com.github/tlmaloney/agentsview && CGO_ENABLED=1 go test -tags fts5 -run TestListSessionsModifiedBetween ./internal/db/ -v`

Expected: PASS (both old and new tests)

- [ ] **Step 6: Run go fmt and go vet**

Run: `cd /home/tmaloney/gitdev/com.github/tlmaloney/agentsview && go fmt ./internal/db/ ./internal/postgres/ && go vet -tags fts5 ./internal/db/ ./internal/postgres/`

- [ ] **Step 7: Commit**

```bash
git add internal/db/sessions.go internal/db/db_test.go internal/postgres/push.go
git commit -m "feat: add project filter params to ListSessionsModifiedBetween"
```

---

### Task 3: Thread project filter through Push()

**Files:**
- Modify: `internal/postgres/push.go:51-53`
- Modify: `internal/postgres/sync.go:27-38`

- [ ] **Step 1: Add filter fields to Sync struct and New constructor**

In `internal/postgres/sync.go`, add fields and update `New`:

```go
// Sync manages push-only sync from local SQLite to a remote
// PostgreSQL database.
type Sync struct {
	pg      *sql.DB
	local   *db.DB
	machine string
	schema  string

	// Project filtering for push scope.
	projects        []string
	excludeProjects []string

	closeOnce sync.Once
	closeErr  error

	schemaMu   sync.Mutex
	schemaDone bool
}

// New creates a Sync instance and verifies the PG connection.
// The machine name must not be "local", which is reserved as the
// SQLite sentinel for sessions that originated on this machine.
// When allowInsecure is true, non-loopback connections without TLS
// produce a warning instead of failing.
func New(
	pgURL, schema string, local *db.DB,
	machine string, allowInsecure bool,
	projects, excludeProjects []string,
) (*Sync, error) {
	if pgURL == "" {
		return nil, fmt.Errorf("postgres URL is required")
	}
	if machine == "" {
		return nil, fmt.Errorf(
			"machine name must not be empty",
		)
	}
	if machine == "local" {
		return nil, fmt.Errorf(
			"machine name %q is reserved; "+
				"choose a different pg.machine_name",
			machine,
		)
	}
	if local == nil {
		return nil, fmt.Errorf("local db is required")
	}

	pg, err := Open(pgURL, schema, allowInsecure)
	if err != nil {
		return nil, err
	}

	return &Sync{
		pg:              pg,
		local:           local,
		machine:         machine,
		schema:          schema,
		projects:        projects,
		excludeProjects: excludeProjects,
	}, nil
}
```

- [ ] **Step 2: Update Push() to use filter fields**

In `internal/postgres/push.go`, update the two
`ListSessionsModifiedBetween` calls to pass the filter fields:

At line 114 (the main session fetch):
```go
	allSessions, err := s.local.ListSessionsModifiedBetween(
		ctx, lastPush, cutoff, s.projects, s.excludeProjects,
	)
```

At line 154 (the boundary session fetch):
```go
		boundarySessions, err := s.local.ListSessionsModifiedBetween(
			ctx, windowStart, lastPush, s.projects, s.excludeProjects,
		)
```

- [ ] **Step 3: Fix all callers of postgres.New**

Find all callers of `postgres.New` and add `nil, nil` for the new parameters.

In `cmd/agentsview/pg.go` (line 89):
```go
	ps, err := postgres.New(
		pgCfg.URL, pgCfg.Schema, database,
		pgCfg.MachineName, pgCfg.AllowInsecure,
		nil, nil,
	)
```

Search for any other callers (test files, etc.) and update them similarly.

- [ ] **Step 4: Run full test suite**

Run: `cd /home/tmaloney/gitdev/com.github/tlmaloney/agentsview && CGO_ENABLED=1 go test -tags fts5 ./internal/postgres/ ./internal/db/ ./cmd/agentsview/ -v -short`

Expected: PASS

- [ ] **Step 5: Run go fmt and go vet**

Run: `cd /home/tmaloney/gitdev/com.github/tlmaloney/agentsview && go fmt ./... && go vet -tags fts5 ./...`

- [ ] **Step 6: Commit**

```bash
git add internal/postgres/sync.go internal/postgres/push.go cmd/agentsview/pg.go
git commit -m "feat: thread project filter through Sync and Push"
```

---

### Task 4: Add CLI flags and validation to pg push

**Files:**
- Modify: `cmd/agentsview/pg.go:41-120`

- [ ] **Step 1: Add --projects and --exclude-projects flags with validation**

Update `runPGPush` in `cmd/agentsview/pg.go`:

```go
func runPGPush(args []string) {
	fs := flag.NewFlagSet("pg push", flag.ExitOnError)
	full := fs.Bool("full", false,
		"Force full local resync and PG push")
	projectsFlag := fs.String("projects", "",
		"Comma-separated list of projects to push (inclusive)")
	excludeProjectsFlag := fs.String("exclude-projects", "",
		"Comma-separated list of projects to exclude from push")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if *projectsFlag != "" && *excludeProjectsFlag != "" {
		fatal("pg push: --projects and --exclude-projects are mutually exclusive")
	}

	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		log.Fatalf("creating data dir: %v", err)
	}
	setupLogFile(appCfg.DataDir)

	pgCfg, err := appCfg.ResolvePG()
	if err != nil {
		fatal("pg push: %v", err)
	}
	if pgCfg.URL == "" {
		fatal("pg push: url not configured")
	}

	// CLI flags override config values entirely.
	projects := pgCfg.Projects
	excludeProjects := pgCfg.ExcludeProjects
	if *projectsFlag != "" {
		projects = strings.Split(*projectsFlag, ",")
	}
	if *excludeProjectsFlag != "" {
		excludeProjects = strings.Split(*excludeProjectsFlag, ",")
	}

	// Validate mutual exclusivity (config values may conflict too).
	if len(projects) > 0 && len(excludeProjects) > 0 {
		fatal("pg push: projects and exclude_projects are mutually exclusive")
	}

	database, err := db.Open(appCfg.DBPath)
	if err != nil {
		fatal("opening database: %v", err)
	}
	defer database.Close()

	if appCfg.CursorSecret != "" {
		secret, decErr := base64.StdEncoding.DecodeString(
			appCfg.CursorSecret,
		)
		if decErr != nil {
			fatal("invalid cursor secret: %v", decErr)
		}
		database.SetCursorSecret(secret)
	}

	didResync := runLocalSync(appCfg, database, *full)
	forceFull := *full || didResync

	ps, err := postgres.New(
		pgCfg.URL, pgCfg.Schema, database,
		pgCfg.MachineName, pgCfg.AllowInsecure,
		projects, excludeProjects,
	)
	if err != nil {
		fatal("pg push: %v", err)
	}
	defer ps.Close()

	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt,
	)
	defer stop()

	if err := ps.EnsureSchema(ctx); err != nil {
		fatal("pg push schema: %v", err)
	}
	result, err := ps.Push(ctx, forceFull)
	if err != nil {
		fatal("pg push: %v", err)
	}
	fmt.Printf(
		"Pushed %d sessions, %d messages in %s\n",
		result.SessionsPushed,
		result.MessagesPushed,
		result.Duration.Round(time.Millisecond),
	)
	if result.Errors > 0 {
		fatal("pg push: %d session(s) failed",
			result.Errors)
	}
}
```

Add `"strings"` to the import block in `cmd/agentsview/pg.go` if not already
present.

- [ ] **Step 2: Run go fmt and go vet**

Run: `cd /home/tmaloney/gitdev/com.github/tlmaloney/agentsview && go fmt ./cmd/agentsview/ && go vet -tags fts5 ./cmd/agentsview/`

- [ ] **Step 3: Run existing tests**

Run: `cd /home/tmaloney/gitdev/com.github/tlmaloney/agentsview && CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview/ -v -short`

Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add cmd/agentsview/pg.go
git commit -m "feat: add --projects and --exclude-projects flags to pg push"
```

---

### Task 5: Update help text

**Files:**
- Modify: `cmd/agentsview/main.go:73-130`

- [ ] **Step 1: Add projects command and push flags to help text**

In `cmd/agentsview/main.go`, update `printUsage()` to add the `projects`
command and the new `pg push` flags:

Add to the usage block after the `import` line:
```
  agentsview projects [flags] List projects with session counts
```

Add to the PG push flags section:
```
  -projects string   Comma-separated projects to push (inclusive)
  -exclude-projects  Comma-separated projects to exclude from push
```

- [ ] **Step 2: Run go fmt and go vet**

Run: `cd /home/tmaloney/gitdev/com.github/tlmaloney/agentsview && go fmt ./cmd/agentsview/ && go vet -tags fts5 ./cmd/agentsview/`

- [ ] **Step 3: Commit**

```bash
git add cmd/agentsview/main.go
git commit -m "docs: add projects command and push filter flags to help text"
```

---

### Task 6: Add agentsview projects command

**Files:**
- Create: `cmd/agentsview/projects.go`
- Modify: `cmd/agentsview/main.go:37-68`

- [ ] **Step 1: Create projects.go with the subcommand**

Create `cmd/agentsview/projects.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/db"
)

func runProjects(args []string) {
	fs := flag.NewFlagSet("projects", flag.ExitOnError)
	jsonOutput := fs.Bool("json", false,
		"Output as JSON array")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	database, err := db.Open(appCfg.DBPath)
	if err != nil {
		fatal("opening database: %v", err)
	}
	defer database.Close()

	ctx := context.Background()
	projects, err := database.GetProjects(ctx, false, false)
	if err != nil {
		fatal("listing projects: %v", err)
	}

	if *jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(projects); err != nil {
			fatal("encoding json: %v", err)
		}
		return
	}

	if len(projects) == 0 {
		fmt.Println("No projects found.")
		return
	}

	fmt.Printf("%-40s %s\n", "PROJECT", "SESSIONS")
	for _, p := range projects {
		name := p.Name
		if name == "" {
			name = "(none)"
		}
		fmt.Printf("%-40s %d\n", name, p.SessionCount)
	}
}
```

- [ ] **Step 2: Register the command in main.go**

In `cmd/agentsview/main.go`, add a case to the command dispatch switch:

```go
		case "projects":
			runProjects(os.Args[2:])
			return
```

Add it after the `"import"` case (before the `"version"` case).

- [ ] **Step 3: Run go fmt and go vet**

Run: `cd /home/tmaloney/gitdev/com.github/tlmaloney/agentsview && go fmt ./cmd/agentsview/ && go vet -tags fts5 ./cmd/agentsview/`

- [ ] **Step 4: Build to verify compilation**

Run: `cd /home/tmaloney/gitdev/com.github/tlmaloney/agentsview && CGO_ENABLED=1 go build -tags fts5 ./cmd/agentsview/`

Expected: Successful build with no errors.

- [ ] **Step 5: Commit**

```bash
git add cmd/agentsview/projects.go cmd/agentsview/main.go
git commit -m "feat: add agentsview projects command"
```

---

### Task 7: Run full test suite and verify

**Files:** None (verification only)

- [ ] **Step 1: Run all Go tests**

Run: `cd /home/tmaloney/gitdev/com.github/tlmaloney/agentsview && CGO_ENABLED=1 go test -tags fts5 ./... -short`

Expected: All tests PASS.

- [ ] **Step 2: Run go vet across entire project**

Run: `cd /home/tmaloney/gitdev/com.github/tlmaloney/agentsview && go vet -tags fts5 ./...`

Expected: No issues.

- [ ] **Step 3: Build the binary**

Run: `cd /home/tmaloney/gitdev/com.github/tlmaloney/agentsview && CGO_ENABLED=1 go build -tags fts5 -o /tmp/agentsview-test ./cmd/agentsview/`

Expected: Successful build.

- [ ] **Step 4: Smoke test the projects command**

Run: `/tmp/agentsview-test projects`

Expected: Either a list of projects or "No projects found." depending on
existing data.

Run: `/tmp/agentsview-test projects --json`

Expected: JSON array output.

- [ ] **Step 5: Smoke test mutual exclusivity validation**

Run: `/tmp/agentsview-test pg push --projects=a --exclude-projects=b 2>&1 || true`

Expected: Error message containing "mutually exclusive".
