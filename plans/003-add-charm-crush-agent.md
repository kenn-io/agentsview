# Plan 003: Add Charm Crush agent harness support to agentsview

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md` — unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**:
> `git diff --stat 64e5359f..HEAD -- internal/parser/ internal/config/ frontend/src/lib/utils/ docs/ README.md`
> If any in-scope file changed since this plan was written, compare the
> "Current state" excerpts against the live code before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P1
- **Effort**: L
- **Risk**: MED
- **Depends on**: none
- **Category**: direction
- **Planned at**: commit `64e5359f`, 2026-09-11

## Why this matters

agentsview currently does not support Charm Crush, the CLI agent harness from
Charm (upstream `github.com/charmbracelet/crush`; provider field in its data
is `hyper` from the bundled "Charm Hyper" endpoint). Users running Crush get
no session catalog, no usage or cost analytics, and no transcript view. Adding
a Crush provider brings it to parity with the other supported agents and
follows the exact 12-layer integration pattern every other agent uses.

## Current state

### Crush's on-disk format (verified against a live install)

All findings below were confirmed by reading a real Crush database. The
executor should re-verify against their own install if one exists.

**Storage layout — per-project SQLite, not a home-directory file tree:**

- Each project gets its own database at `<project>/.crush/crush.db` plus
  `crush.db-wal` and `crush.db-shm` sidecars, and a `logs/` subdirectory.
- A registry file maps project paths to their data dirs:
  `~/.local/share/crush/projects.json` (macOS/Linux, XDG data dir). Shape —
  note it is an **array**, unlike Gemini's map:
  ```json
  {"projects":[{"path":"/abs/project","data_dir":"/abs/project/.crush","last_accessed":"2026-09-11T02:25:17Z"}]}
  ```
- `~/.config/crush/` holds `commands/` and `skills/` only — no sessions.
- The database was created by goose migrations (there is a
  `goose_db_version` table) because Crush vendored goose's migration tool.
  This does **not** make it the Goose agent's format; do not confuse the two.

**Schema (as observed):**

```sql
sessions (
  id TEXT PRIMARY KEY,            -- UUID
  parent_session_id TEXT,         -- subagent relationships (nullable, saw NULL in sample data)
  title TEXT NOT NULL,
  message_count INTEGER NOT NULL DEFAULT 0,
  prompt_tokens INTEGER NOT NULL DEFAULT 0,    -- cumulative session total
  completion_tokens INTEGER NOT NULL DEFAULT 0, -- cumulative session total
  cost REAL NOT NULL DEFAULT 0.0,               -- recorded cost, e.g. 0.0126
  updated_at INTEGER NOT NULL,  -- comment says "ms" — see timestamp warning below
  created_at INTEGER NOT NULL,
  summary_message_id TEXT,
  todos TEXT
);
messages (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  role TEXT NOT NULL,             -- observed: 'user', 'assistant', 'tool'
  parts TEXT NOT NULL default '[]',  -- JSON array, schema below
  model TEXT,                     -- e.g. "glm-5.3-flash", set on assistant rows only
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  finished_at INTEGER,
  provider TEXT,                  -- observed: "hyper"
  is_summary_message INTEGER DEFAULT 0 NOT NULL,
  FOREIGN KEY (session_id) REFERENCES sessions (id) ON DELETE CASCADE
);
-- plus files, read_files, goose_db_version tables
```

**CRITICAL timestamp discrepancy:** schema comments say "Unix timestamp in
milliseconds", but live values are **Unix seconds** (`created_at` ≈
`1789093626`, i.e. ~1.79e9). The `update_sessions_updated_at` trigger writes
`strftime('%s','now')` — seconds — proving seconds is what Crush actually
writes. Decode as seconds; if a future build emits ms, a value > 1e12
disambiguates. Add a test asserting timestamps land in the correct era
(year 2026 after decoding, not 1970).

**`parts` JSON shapes (verified from real rows):**

- user row:
  ```json
  [{"type":"text","data":{"text":"..."}},
   {"type":"finish","data":{"reason":"stop","time":0}}]
  ```
- assistant row:
  ```json
  [{"type":"reasoning","data":{"thinking":"...","signature":"","thought_signature":"","tool_id":"","started_at":1789093629,"finished_at":1789093629}},
   {"type":"tool_call","data":{"id":"chatcmpl-tool-af5cd1a21895b142","name":"view","input":"{\"file_path\":\"...\"}","finished":true,"provider_executed":false}},
   {"type":"finish","data":{"reason":"tool_use","time":1789093630}}]
  ```
  `input` is a **JSON-encoded string** — decode it before storing in
  `InputJSON`. `reasoning` parts carry thinking text (accumulate, do not
  overwrite). Multiple `tool_call` parts per row were observed.
- tool row:
  ```json
  [{"type":"tool_result","data":{"tool_call_id":"chatcmpl-tool-af5cd1a21895b142","name":"view","content":"..."}}]
  ```
  Tool results are **separate role='tool' rows** keyed by `tool_call_id` —
  pair them into the originating assistant tool call's `ResultEvents` (or
  emit as system tool-result messages), matching how a sibling parser pairs
  results. Decide pairing vs. standalone emission after reading an existing
  exemplar; if results are paired into earlier messages, the agent needs
  `shouldReplaceFullParseMessages` (sync/engine.go) and
  `ForceReplaceOnParse`.

**Tool names:** Crush tool names are lowercase (`view` was observed). Enumerate
the full tool family from real data or the upstream repo and add exact-case
entries to `taxonomy.go` (Read/Edit/Write/Bash/Grep/Glob/Task/MCP/Skill as
appropriate — do not dump into `Tool`).

**Usage/cost model:** session-level cumulative `prompt_tokens` /
`completion_tokens` / `cost`; per-message rows have model but no token
counts. So: `Usage: UsageCapabilities{NoPerMessageTokenData: true}`,
declare the aggregate-usage capability per how Goose does it, and emit usage
events gated on having token or cost data (cost alone still counts). Cost
`0.0` may be a genuine zero — decode numeric fields with presence in mind
(skill checklist: present-positive / present-zero / absent).

**Compaction:** `is_summary_message` on messages plus
`sessions.summary_message_id` exist for condensed summaries. No live samples
were available (count was 0). Treat summary rows as compact-boundary system
messages when `is_summary_message=1`; add a synthetic test since no real
fixture exists. There is no error variant observed — if the executor finds
one upstream, note the lesson that error variants are not boundaries.

**Subagents:** `sessions.parent_session_id` is the only observed linkage;
wire `ParentSessionID` and `InferRelationshipTypes`. Sample data had none —
cover with synthetic fixtures.

**Format detection marker:** the DB must be identified as Crush, not Goose.
Goose uses a `sessions.db` with a different schema (no `parts` column).
Require presence of the `messages` table with a `parts` column AND the
`sessions` table with a `title` column before treating a file as a Crush
store. Do not use `goose_db_version` as a marker — it belongs to the
migration tool, not the agent.

### Exemplars in this repo (the executor's reference code)

- `internal/parser/types.go:690-707` — Goose registry entry. Model for the
  Crush entry (FileBased: false, NoPerMessageTokenData, PeriodicReconcile).
- `internal/parser/goose.go` — SQLite-backed parser exemplar (row structs,
  gjson over content JSON, NULL-safe cost via `sql.NullFloat64`).
- `internal/parser/goose_provider.go` and `internal/parser/db_backed_provider.go`
  — Pattern C provider (dbBackedProvider). `internal/parser/sqlite_dsn.go` and
  `internal/parser/sqlite_container_state.go` handle read-only DSN opening and
  snapshot consistency — reuse them.
- `internal/parser/discovery.go:971-1010` — `buildGeminiProjectMap`, the
  precedent for reading a `projects.json` registry. Crush's variant is an
  array of `{path, data_dir, last_accessed}` and yields absolute data-dir
  paths instead of directory names.
- `internal/parser/kiro_sqlite.go` — second SQLite provider example.

### Repo conventions that apply

- Read `docs/agents/testing.md` and `docs/agents/storage.md` before editing —
  this task routes to both.
- Registry entries are alphabetical by Type in `types.go`; provider factory
  cases alphabetical in `provider.go` (Goose case sits at
  `internal/parser/provider.go:1172`).
- Conventional commits, e.g. `feat(parser): add charm crush agent support`.
- After Go changes: `go fmt ./...` and `go vet ./...`.

## Commands you will need

| Purpose | Command | Expected on success |
|---------|---------|---------------------|
| Parser tests | `go test ./internal/parser/ -run TestParseCrush` | all pass |
| Registry test | `go test ./internal/parser/ -run TestRegistryCompleteness` | pass |
| Full parser suite | `go test ./internal/parser/` | all pass |
| Format | `go fmt ./...` | no diff |
| Vet | `go vet ./...` | no output |
| Frontend check | `cd frontend && vp check` | exit 0 |
| Frontend tests | `cd frontend && vp test` | all pass |

## Suggested executor toolkit

- Skill: `agentsview-add-new-agent` — the full 12-layer integration
  checklist, parser correctness lessons, and roborev-ci flag list this plan
  follows.
- `docs/agents/testing.md` and `docs/agents/storage.md` — required reading
  per the repo's task routes.

## Scope

**In scope (the only files you should modify/create):**

- `internal/parser/types.go` — `AgentCrush AgentType = "crush"` constant +
  registry entry (alphabetical position; keep `types_test.go` in sync)
- `internal/parser/crush.go` — parser (create)
- `internal/parser/crush_provider.go` — Pattern C provider (create)
- `internal/parser/crush_test.go` — tests (create)
- `internal/parser/provider.go` — factory case
- `internal/parser/provider_migration.go` — `ProviderMigrationProviderAuthoritative`
- `internal/parser/taxonomy.go` — Crush tool-name cases + taxonomy test
- `internal/parser/types_test.go` — add `AgentCrush` to `TestRegistryCompleteness` `allTypes`
- `internal/config/config.go` — `CRUSH_DIR` env var / `crush_dirs` config key wiring
- `internal/sync/engine.go` — only if the parser pairs results into earlier messages (see Current state)
- `frontend/src/lib/utils/agents.ts` and `agents.test.ts` — `{ name: "crush", color: <unused accent>, label: "Charm Crush" }`
- `docs/configuration.md`, `README.md` — discovery table + supported-agents rows
- `docs/internal/session-format-sources.md` — evidence entry (see below)
- `plans/README.md` — status row

**Out of scope (do NOT touch):**

- Any other agent's parser or shared pairing/termination helpers — if a fix
  there seems needed, STOP and report.
- PostgreSQL/DuckDB mirrors: no new session columns are introduced (this plan
  reuses existing ParsedSession fields), so no PG schema work.
- Existing plans 001/002 and their files.

## Git workflow

- Work on a feature branch off the current default, e.g. `crush-agent-support`;
  branch creation is normally the user's call — confirm if working on the
  user's checkout.
- Commit per logical unit; message style: conventional commits
  (`feat(parser): ...`). No attribution footers (repo rule).

## Steps

### Step 1: Register the agent type and registry entry

In `internal/parser/types.go`: add `AgentCrush AgentType = "crush"` (alphabetical
by value) and a registry entry modeled on the Goose entry at
`internal/parser/types.go:690`:

- `DisplayName: "Charm Crush"`, `IDPrefix: "crush:"`, `FileBased: false`
- `EnvVar: "CRUSH_DIR"`, `ConfigKey: "crush_dirs"` — these point at directories
  **containing a `crush.db` file** (i.e. project `.crush` dirs), matching how
  DB-backed providers take roots.
- `DefaultDirs`: the platform dir that holds `projects.json`
  (`".local/share/crush"` for macOS/Linux). Windows path is unverified — see
  STOP conditions.
- `Usage: UsageCapabilities{NoPerMessageTokenData: true}` and
  `PeriodicReconcile: true` (SQLite WAL churn makes fsnotify non-authoritative).

Add `AgentCrush` to `TestRegistryCompleteness`'s `allTypes` in
`internal/parser/types_test.go`.

Wire `CRUSH_DIR`/`crush_dirs` in `internal/config/config.go` following the
`GOOSE_PATH_ROOT`/`goose_dirs` pattern (`internal/config/config.go:1913` area).

**Verify**: `go test ./internal/parser/ -run TestRegistryCompleteness` → pass.

### Step 2: Discovery of per-project databases

Implement discovery of Crush databases following `goose_provider.go`:

1. Roots from config are directories that directly contain `crush.db`.
2. Default discovery reads `~/.local/share/crush/projects.json` (array of
   `{path, data_dir, last_accessed}` — see Current state for exact shape) and
   yields each `data_dir` as a root. Precedent: `buildGeminiProjectMap` in
   `internal/parser/discovery.go:984`. Guard against `data_dir` entries that
   no longer exist (skip, do not error).
3. Deduplicate roots when `CRUSH_DIR`/`crush_dirs` overrides overlap defaults
   (skill lesson 52).

Session IDs: `crush:<uuid>`. UUIDs make cross-root collision unlikely, but the
project registry can contain the same data_dir twice — deduplicate roots, not IDs.

**Verify**: `go build ./...` → exit 0; `go test ./internal/parser/` → pass.

### Step 3: Write the parser (`internal/parser/crush.go`)

Implement `parseCrushSession` following `goose.go`:

1. Open read-only via the shared SQLite DSN helpers
   (`internal/parser/sqlite_dsn.go`); parse and fingerprint from a single read
   transaction (lesson 57).
2. Decode timestamps as Unix **seconds** (Current state warning). Assert in
   tests that a known `created_at` decodes to a 2026-era time.
3. Per session row: session ID `crush:<id>`, name from `title`, started/ended
   from `created_at`/`updated_at`, `ParentSessionID` from
   `parent_session_id`, project from the project registry mapping or the
   `.crush` parent directory (use `ExtractProjectFromCwd` conventions — check
   how Goose sets project for DB-backed sessions and match it).
4. Per message row: decode `parts` JSON:
   - `text` parts → message content (user or assistant by row role)
   - `reasoning` parts → HasThinking/ThinkingText, **append** multiple parts
   - `tool_call` parts → ParsedToolCall with `ToolUseID: "crush:<row-id>:<ordinal>"`
     (per-call uniqueness), ToolName from `data.name`, `InputJSON` from
     decoding the JSON-encoded `data.input` string
   - `tool_result` rows (role='tool') → pair into the matching assistant call
     by `tool_call_id`, set ResultEvents with Status and the row's timestamp;
     or emit as system tool-result messages if that matches the exemplar's
     pairing style — pick one approach, declare capabilities honestly, and if
     results are paired into earlier messages add Crush to
     `shouldReplaceFullParseMessages` in `internal/sync/engine.go`
   - `is_summary_message=1` rows → compact-boundary system messages
   - `finish` parts → internal metadata, never user content
5. Usage: one aggregate usage event per session from
   `prompt_tokens`/`completion_tokens`/`cost` with the most recent assistant
   row's `model` (fall back to empty model rather than dropping the event).
   Include cache-token fields in peak context when Crush exposes them; today
   it does not — leave zero.
6. Fingerprint: include DB file identity plus WAL mtime/size per
   `db_backed_provider.go` conventions (lesson 59: replacement detection).

**Verify**: `go test ./internal/parser/ -run TestParseCrush` → pass (tests
written in Step 4; write them alongside this step if easier).

### Step 4: Tests (`internal/parser/crush_test.go`)

Model after `goose_test.go` and `db_backed_provider_test.go`. Build fixture
databases in `t.TempDir()` with the schema from Current state (a small
`CREATE TABLE` fixture set is fine; do not copy the real database into the
repo). Required cases, per the add-new-agent skill's test matrix:

- basic parse: session ID, name, timestamps in the correct era, message count
- role attribution for user/assistant/tool rows
- tool call extraction incl. per-call unique ToolUseID and InputJSON decoding
- tool result pairing incl. empty-but-present results completing a call
- thinking accumulation across multiple reasoning parts
- usage: aggregate session tokens/cost → exactly one usage event (no double
  emission); cost present-zero vs absent
- timestamp unit regression (seconds, not ms)
- summary/compact boundary synthetic fixture
- parent session relationship synthetic fixture
- malformed JSON in `parts` → MalformedLines counted, no crash

**Verify**: `go test ./internal/parser/` → all pass.

### Step 5: Provider, migration mode, taxonomy

- `internal/parser/crush_provider.go`: Pattern C `dbBackedProvider` modeled on
  `goose_provider.go`; declare only capabilities the parser actually sets.
- `internal/parser/provider.go`: add `case AgentCrush:` in alphabetical order
  near line 1172.
- `internal/parser/provider_migration.go`: `AgentCrush:
  ProviderMigrationProviderAuthoritative`.
- `internal/parser/taxonomy.go`: add Crush tool names with exact lowercase
  casing, mapped to real categories; add a taxonomy test asserting each name.

**Verify**: `go vet ./...` → clean; `go test ./internal/parser/` → pass.

### Step 6: Frontend, docs, provenance

- `frontend/src/lib/utils/agents.ts`: add `{ name: "crush", color: <an accent
  var not yet used in the list>, label: "Charm Crush" }`; update the
  hardcoded `toEqual([...])` in `agents.test.ts` and add color/label
  assertions.
- `docs/configuration.md`: session discovery table row (alphabetical),
  platform directory section (`~/.local/share/crush/` registry, per-project
  `.crush/crush.db`), `CRUSH_DIR` env var and `crush_dirs` config key.
- `README.md`: Supported Agents table row.
- `docs/internal/session-format-sources.md`: evidence entry, class
  `source`, upstream `https://github.com/charmbracelet/crush` pinned to a
  specific commit (check the latest at execution time), format description
  (per-project SQLite, parts JSON, session-level usage), and implementation
  references (`crush.go`, `crush_test.go`).

**Verify**: `cd frontend && vp check && vp test` → pass.

### Step 7: Full validation and resync check

- `go fmt ./...`, `go vet ./...`, `go test ./...`
- If a live Crush install with sessions is available: run a sync against a
  scratch database (never the live archive) and confirm sessions, transcripts,
  tool calls, and usage appear correctly in the UI. Note the repo safety rule:
  isolated scratch data only.

## Done criteria

- [ ] `go test ./internal/parser/` passes including new `TestParseCrush*` tests
- [ ] `go test ./internal/parser/ -run TestRegistryCompleteness` passes
- [ ] `go vet ./...` clean, `go fmt ./...` produces no diff
- [ ] `cd frontend && vp check && vp test` pass
- [ ] `grep -n "AgentCrush" internal/parser/types.go internal/parser/types_test.go internal/parser/provider.go internal/parser/provider_migration.go` shows all four wired
- [ ] `docs/configuration.md`, `README.md`, and
      `docs/internal/session-format-sources.md` each contain a Crush entry
- [ ] No files outside the in-scope list modified (`git status`)
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- The schema of a real `crush.db` at execution time no longer matches the
  Current state schema (Crush shipped a migration; e.g. `parts` split or
  renamed).
- Live data shows timestamps are milliseconds, not seconds — the decode
  strategy and tests must be re-specified rather than improvised.
- The Windows data-dir location cannot be confirmed from the upstream repo —
  ship without a Windows `DefaultDirs` entry and report, rather than guessing
  a path (wrong paths are harmless but the provenance doc must not record
  guesses).
- Pairing tool results requires changes to shared pairing/termination helpers
  in `termination.go` or other agents' files.
- `is_summary_message` semantics in the upstream source contradict the
  compact-boundary assumption.

## Maintenance notes

- Crush is under active development; the vendored goose migrations mean new
  columns may appear. The parser should tolerate unknown columns and unknown
  `parts` types by skipping them, so minor format additions do not break sync.
- If Crush ever grows a global session store, discovery via `projects.json`
  can be replaced without touching the parser.
- Reviewers should scrutinize: timestamp decoding, ToolUseID uniqueness,
  capability honesty in `crush_provider.go`, and that usage events are
  emitted exactly once per session.
- Deferred: per-request usage events (Crush stores none), Windows path
  confirmation, live-subagent fixtures if real subagent data surfaces later.
