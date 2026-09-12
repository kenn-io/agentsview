# Audit current project-classification capabilities

Type: task

Status: resolved

## Question

What does the current system actually support before prototypes make new
promises? Trace candidate selection, machine and path identity, normalization,
nested-rule precedence, impact preview and apply atomicity, reversal behavior,
read-only presentation, authentication and origin enforcement, and the SQLite,
PostgreSQL/CockroachDB, and DuckDB boundaries. Record facts and constraints
only; leave product choices to the dependent decision tickets.

## Answer

### Candidate selection

- The selected inventory row is identified by both its display label and opaque
  project key. Candidate selection rejects a mismatched pair, then includes
  raw project labels that resolve to the same project identity. It considers
  every non-deleted session in the archive, without Activity filters or date
  bounds.
- Candidate grouping is shared by SQLite, PostgreSQL/CockroachDB, and DuckDB.
  Groups are machine-scoped and prefer session identity snapshots, then
  compatible aggregate identity observations, then an exact stored `cwd`.
  Sessions with none of those become an unavailable group.
- A snapshot or aggregate group suggests the longest common directory prefix of
  its stored working directories. A fallback group suggests its exact `cwd`.
  Filesystem roots are shown but marked unavailable. Each candidate carries at
  most ten session examples.
- Candidate availability is lexical. The builder checks that the suggested
  prefix is non-empty and is not a filesystem root; it does not check whether
  the directory currently exists or can be reached from the server.

Sources: `internal/db/worktree_candidates.go`,
`internal/postgres/worktree_candidates.go`, and
`internal/duckdb/worktree_candidates.go`.

### Machine, path, and rule identity

- SQLite stores one rule per `(machine, path_prefix)`. Machine names are trimmed
  and compared exactly. A rule can target any non-empty machine name; the UI
  suggests machines represented by live sessions or stored rules.
- Paths are trimmed, backslashes become forward slashes, and `path.Clean`
  removes redundant separators and dot segments. UNC roots and Windows drive
  roots retain distinct portable forms. Matching is lexical and requires an
  exact path or directory boundary, so `/work/app` does not match
  `/work/app-old`.
- Explicit target names are trimmed and normalized by replacing `-` with `_`.
  The preview returns that normalized target. `original_project` is trimmed by
  the reclassification path and is set once on a rule; later edits do not
  replace a non-empty value.
- Numeric rule IDs are local SQLite identifiers. Replicated rule identity is
  `(source_archive_id, machine, path_prefix)`, because PostgreSQL and DuckDB
  can contain rules from several source archives and do not publish SQLite
  IDs.

Sources: `internal/db/schema.sql`, `internal/db/worktree_mappings.go`,
`internal/db/worktree_reclassification.go`, and `internal/db/project_rules.go`.

### Matching and precedence

- Only enabled rules participate. Rules are evaluated per machine, with the
  longest path prefix first. The first matching rule that successfully
  resolves a project wins. A nested dynamic rule that cannot derive a project
  falls through to broader matching rules.
- An `explicit` rule returns its stored project. A `repo_dot_worktrees` rule
  derives the project from the first `<repo>.worktrees` directory beneath the
  configured parent and normalizes the repository name.
- A session with an empty `cwd` can borrow a matching `cwd` from sessions that
  share its source file, but only when every non-empty sibling is mapped and
  all mapped siblings agree on one project. Conflicting or unmapped siblings
  leave it unchanged.
- Sync applies active mappings to newly written sessions. The manual apply path
  can reevaluate every visible session for one machine. Neither path requires
  the stored directory to exist on the current host.

Sources: `internal/db/worktree_mappings.go` and `internal/sync/engine.go`.

### Preview and apply

- Preview overlays the draft onto the current rule set, resolves an exact
  normalized prefix collision on the server, and evaluates every non-deleted
  session for the selected machine. It reports matched sessions, sessions that
  would change, distinct current project labels, and up to ten project and
  session samples. Already-targeted sessions count as matched but not updated.
- The preview token binds the normalized draft, the complete rule set for that
  machine, the exact collision ID, and the affected session state. A changed
  rule, changed draft, new affected session, or changed affected project makes
  apply return HTTP 409. The UI then discards the accepted preview and loads a
  fresh one.
- Apply runs under the sync engine's exclusive lock. SQLite reevaluates the
  draft and token inside a write transaction, then writes the rule, all
  session project changes, `local_modified_at`, and project-identity aggregate
  changes in that transaction. Failure at the rule, session, or identity stage
  rolls all of it back. After commit, the server emits a `sessions` event when
  rows changed.
- The inventory refresh is not part of that transaction. If apply commits but
  the subsequent frontend refresh fails, the UI reports that the mapping was
  saved and offers a refresh-only retry. It does not send apply again.
- Preview and apply scan the selected machine's visible sessions and materialize
  their evaluation in memory. Candidate selection begins with all visible
  sessions. Neither API has pagination or a declared performance budget.

Sources: `internal/db/worktree_reclassification.go`, `internal/sync/engine.go`,
`internal/server/huma_routes_settings.go`, and
`frontend/src/lib/components/data/ProjectReclassificationEditor.svelte`.

### Reversal and history

- The current API has no reversal or change-history operation.
  `original_project` records one rule-level source label for display and
  inventory annotation; it is not a per-session before-image.
- Deleting or disabling a rule changes the rule only. It does not restore
  session projects. Running the generic apply operation afterward reevaluates
  active rules, but a session that no longer matches any active rule keeps its
  current project.
- A user can edit or create a rule that maps the same prefix to another target
  and apply it, but that is a new forward reclassification rather than an undo
  of the earlier session set.

Sources: `internal/db/worktree_mappings.go`,
`internal/server/huma_routes_settings.go`, and
`frontend/src/lib/components/data/WorktreeMappingRules.svelte`.

### Read-only presentation and backend boundaries

- SQLite is the only backend that serves rule mutations, preview, and apply.
  These routes require a writable `*db.DB` and a sync engine. PostgreSQL,
  CockroachDB, DuckDB, and any other remote-mode store receive HTTP 501 from
  those settings routes.
- All three store implementations support project inventory, project-rule reads,
  and archive-wide candidate reads. The Data UI still shows observed folders
  and rules in read-only mode, but it hides mapping fields and mutation
  actions and explains that changes must be made in the writable archive.
- PostgreSQL/CockroachDB and DuckDB receive rule metadata through push. An
  unfiltered push publishes the full archive-scoped rule set or a tombstoned
  delta in one transaction. A filtered push republishes only explicit rules
  whose target is in scope, removes an out-of-scope `original_project`, and
  omits dynamic `repo_dot_worktrees` rules because they have no fixed target.
- Mirror reads distinguish rules from different source archives. They expose
  governed-session counts and candidates, but they cannot mutate the source
  archive or reclassify mirrored session rows through the UI.

Sources: `internal/server/huma_routes_settings.go`, `internal/db/store.go`,
`internal/postgres/store.go`, `internal/duckdb/store.go`,
`internal/postgres/worktree_mappings_push.go`, and
`internal/duckdb/worktree_mappings_push.go`.

### Authentication and origin enforcement

- Every `/api/` route, including the classification reads and writes, passes
  through the shared authentication, Host-header, and CORS middleware. The
  classification routes add no separate authorization layer.
- When `require_auth` is enabled, non-SSE API requests require the configured
  bearer token. An authenticated remote request may use any Host and Origin.
  Query-string tokens are accepted only for SSE endpoints.
- Without bearer authentication, protected routes still require an allowed Host.
  State-changing requests also require a recognized `Origin`; a missing or
  foreign Origin receives HTTP 403. Configured public origins and local
  interface origins extend the allowlists according to server configuration.

Sources: `internal/server/auth.go` and `internal/server/server.go`.

### Verification

Focused Go checks passed for the SQLite classification, server route and
middleware, and DuckDB parity behavior:

```text
go test ./internal/db ./internal/server ./internal/duckdb \
  -run 'Test(Worktree|ListArchiveWorktree|BuildWorktree|ResolveWorktree|ApplyWorktree|DataProject|DuckWorktree|DuckProjectRules|CORS|HostHeader|AuthRequired)' \
  -count=1
```

The focused frontend component tests could not start because this worktree has
no installed frontend dependencies. Vite reported unresolved `vite-plus`,
`@sveltejs/vite-plugin-svelte`, and `@inlang/paraglide-js` imports.
