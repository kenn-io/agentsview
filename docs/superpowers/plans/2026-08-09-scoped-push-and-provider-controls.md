# Scoped Push and Session Provider Controls Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans
> to implement this plan directly in the current agent, task-by-task. Never use
> subagent-driven development. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep watch-triggered PostgreSQL pushes proportional to their changed
batch and let users disable unused session providers from config or Settings.

**Approved spec/design:**
`docs/superpowers/specs/2026-08-09-scoped-push-and-provider-controls-design.md`

**Architecture:** Config owns a normalized disabled-provider set and exposes
filtered ingestion roots while retaining raw roots for presentation. Watcher
batches reuse existing bounds, travel through the push loop and daemon request,
and are applied by one engine operation that keeps the PostgreSQL push
serialized. Gemini project metadata initializes lazily only after discovery
finds a session file.

**Tech Stack:** Go 1.26.3, Cobra, Huma/OpenAPI, SQLite with CGO/FTS5, Testify,
Svelte 5, TypeScript, Paraglide JS, kit-ui, Vite+/Vitest.

## Global Constraints

- `disabled_agents` changes session ingestion only; Recall execution is
  unchanged.
- Disabling never deletes or hides archived sessions.
- Provider changes require daemon restart; the UI never restarts it.
- Ordinary batches remain bounded; ambiguous events remain authoritative.
- Manual pushes and clients omitting the optional batch preserve current
  behavior.
- No database migration, dependency, or persistent Gemini project cache.
- Localize new copy in `en`, `zh-CN`, `zh-TW`, `ko`, and `fr`.
- Run Go with `CGO_ENABLED=1` and `-tags fts5`.
- Use Testify and observable behavior, including cardinality-scaling
  regressions.
- Keep paths, projects, and session IDs out of metric labels and routine logs.
- Do not change `docs/internal/session-format-sources.md`; no provider format
  changes.
- Do not modify the running daemon or watcher processes.

______________________________________________________________________

### Task 1: Disabled-provider config and effective roots

**Files:**

- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `internal/config/persistence_test.go`
- Modify production engine construction in `cmd/agentsview/main.go`, `sync.go`,
  `sync_worker.go`, `session_sync.go`, `usage.go`, `parse_diff.go`,
  `archive_write_backend.go`, and `archive_query_backend.go`
- Modify: `internal/server/huma_routes_sync.go`
- Modify: `internal/server/huma_routes_sync_internal_test.go`
- Modify: `internal/remotesync/import.go`
- Modify: `internal/remotesync/import_test.go`
- Create: `internal/sync/disabled_provider_test.go`
- Modify: `cmd/agentsview/main_test.go`
- Modify: `cmd/agentsview/remote_source_sync_test.go`
- Modify: `docs/configuration.md`

**Interfaces:**

- Produces `Config.DisabledAgents []parser.AgentType`.

- Produces `NormalizeDisabledAgents([]string) ([]parser.AgentType, error)`.

- Produces `AgentDisabled`, `ConfiguredDirs`, `ResolveDirs`, `SyncAgentDirs`,
  and `SyncSourceMachines`.

- [ ] **Step 1: Write failing config/filter tests**

Load:

```toml
disabled_agents = [" gemini ", "claude", "gemini"]
```

Assert:

```go
assert.Equal(t,
    []parser.AgentType{parser.AgentClaude, parser.AgentGemini},
    cfg.DisabledAgents,
)
assert.Empty(t, cfg.ResolveDirs(parser.AgentGemini))
assert.NotEmpty(t, cfg.ConfiguredDirs(parser.AgentGemini))
assert.NotContains(t, cfg.SyncAgentDirs(), parser.AgentGemini)
```

Reject unknown `not-an-agent` and import-only `chatgpt` with errors naming the
ID. Prove disabled Gemini creates no watcher/polling plan and no remote upload
root. Exercise session-usage refresh, the no-sync/on-demand server engine, and
the receiving side of HTTP/SSH remote import so none can ingest Gemini when
disabled. Seed an archived Gemini session, run ordinary sync and authoritative
reconciliation with Gemini disabled, and assert the stored session remains
unchanged and queryable.

- [ ] **Step 2: Run and verify failure**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/config ./internal/remotesync \
  ./internal/sync ./internal/server ./cmd/agentsview \
  -run 'DisabledAgents|DisabledProvider|DisabledRemoteSource' -count=1
```

- [ ] **Step 3: Implement normalization and filtered views**

Add the field and signatures:

```go
func NormalizeDisabledAgents(values []string) ([]parser.AgentType, error)
func (c Config) AgentDisabled(agent parser.AgentType) bool
func (c Config) ConfiguredDirs(agent parser.AgentType) []string
func (c Config) ResolveDirs(agent parser.AgentType) []string
func (c Config) SyncAgentDirs() map[parser.AgentType][]string
func (c Config) SyncSourceMachines() map[parser.AgentType]map[string]string
```

Trim/lowercase, accept only registry entries matching
`FileBased || EnvVar != ""`, dedupe, and output registry order. Honor an
explicit empty TOML array. Make `SaveSettings` accept a typed provider slice,
validate before locking, persist, then update memory. Use the `Sync*` maps at
every production engine constructor, including session-usage refresh and the
server's on-demand/no-sync engine; existing `ResolveDirs` callers then omit
disabled watch, poll, live-activity, diagnostic, and remote-source work. Filter
the receiving-side remote `TargetSet` against the local disabled set before
constructing its import engine, so a sending host cannot re-enable a locally
disabled provider.

- [ ] **Step 4: Document the config**

Add a “Disabling Session Providers” subsection:

```toml
disabled_agents = ["gemini"]
```

State restart, archived-session, and Recall boundaries.

- [ ] **Step 5: Test, format, and commit**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/config ./internal/remotesync \
  ./internal/sync ./internal/server ./cmd/agentsview \
  -run 'DisabledAgents|DisabledProvider|DisabledRemoteSource' -count=1
go fmt ./internal/config ./internal/remotesync ./internal/sync \
  ./internal/server ./cmd/agentsview
git diff --check
git add internal/config internal/remotesync internal/sync \
  internal/server/huma_routes_sync.go \
  internal/server/huma_routes_sync_internal_test.go cmd/agentsview \
  docs/configuration.md
git commit -m "feat(sync): allow session providers to be disabled"
```

### Task 2: Settings API provider model

**Files:**

- Modify: `internal/server/settings.go`
- Modify: `internal/server/huma_routes_settings.go`
- Modify: `internal/server/server_test.go`

**Interfaces:**

- Produces ordered `session_providers` with `id`, `display_name`, and `dirs`.

- Produces `disabled_agents` on GET and accepts the complete string array on
  PUT.

- [ ] **Step 1: Write failing API tests**

PUT `{"disabled_agents":["gemini","claude","gemini"]}`. Assert response/TOML
normalize to Claude then Gemini, ordered provider metadata still includes
disabled Gemini and its dirs, invalid `nope` returns 400 without mutation, and
read-only mode rejects mutation.

- [ ] **Step 2: Run and verify failure**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/server \
  -run 'Settings.*(Disabled|Provider)|SettingsRemainLocked' -count=1
```

- [ ] **Step 3: Implement metadata and mutation**

Define:

```go
type sessionProviderResponse struct {
    ID          parser.AgentType
    DisplayName string
    Dirs        []string
}
```

Add the required JSON tags in code. Build the slice from registry order with the
same eligibility predicate as Task 1. Keep `agent_dirs` for compatibility.
Normalize before adding a typed `disabled_agents` value to the settings patch;
return HTTP 400 on error.

- [ ] **Step 4: Test, format, and commit**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/server \
  -run 'Settings.*(Disabled|Provider)|SettingsRemainLocked' -count=1
go fmt ./internal/server
git add internal/server
git commit -m "feat(settings): expose session provider controls"
```

### Task 3: Localized Session Providers UI

**Files:**

- Modify: `frontend/src/lib/stores/settings.svelte.ts` and `settings.test.ts`
- Modify: `frontend/src/lib/components/settings/AgentDirSettings.svelte`
- Create: `frontend/src/lib/components/settings/AgentDirSettings.test.ts`
- Modify: `SettingsPage.svelte`, `SettingsPage.test.ts`, and `settingsPanels.ts`
- Modify all five `frontend/messages/*.json` locale files
- Regenerate: `frontend/src/lib/api/generated/`

**Interfaces:**

- Produces `settings.sessionProviders`, `settings.disabledAgents`, and
  boolean-returning `settings.save`.

- [ ] **Step 1: Regenerate API and write failing store tests**

Run `(cd frontend && npm run generate:api)`. Use:

```ts
session_providers: [
  { id: "claude", display_name: "Claude Code", dirs: ["/sessions/claude"] },
  { id: "gemini", display_name: "Gemini", dirs: ["/sessions/gemini"] },
],
disabled_agents: ["gemini"],
```

Assert load records both, successful save returns true, and failure returns
false without replacing confirmed state.

- [ ] **Step 2: Write failing component tests**

Assert Gemini’s “Enable Gemini session sync” switch is off; enabling PUTs
`{ disabled_agents: [] }`; a rejected Claude change rolls back; read-only
disables switches; a successful save displays localized `role="status"` restart
text.

- [ ] **Step 3: Run and verify failure**

```bash
(cd frontend && vp test run src/lib/stores/settings.test.ts \
  src/lib/components/settings/AgentDirSettings.test.ts \
  src/lib/components/settings/SettingsPage.test.ts)
```

- [ ] **Step 4: Implement compact rows**

Use kit-ui `Toggle` and server-ordered provider metadata. Show dirs or localized
not-configured text. Keep confirmed and row-pending state separate, disable
pending/read-only rows, rollback on false, and show restart status on true.
Rename visible panel copy to “Session Providers” but retain internal ID
`agent-directories`. Add localized title, description, aria label, state labels,
restart notice, and failure copy in all locales.

- [ ] **Step 5: Validate and commit**

```bash
(cd frontend && npm run i18n:compile && npm run check && vp check && \
  vp test run src/lib/stores/settings.test.ts \
  src/lib/components/settings/AgentDirSettings.test.ts \
  src/lib/components/settings/SettingsPage.test.ts)
git add frontend/messages frontend/src/lib/api/generated \
  frontend/src/lib/stores frontend/src/lib/components/settings
git commit -m "feat(settings): configure session providers in the UI"
```

### Task 4: Gemini zero-session fast path

**Files:**

- Modify: `internal/parser/gemini_provider.go`

- Modify: `internal/parser/gemini_copilot_provider_test.go`

- Modify: `internal/parser/discovery_test.go`

- [ ] **Step 1: Write failing slice/stream tests**

Create project metadata/chat dirs without `session-*.json[l]`. Stub
`buildGeminiProjectMap` and assert zero calls. Indirect
`newDiscoveryDiskMapForContext` through `newGeminiDiscoveryMap`, stub it, and
assert streaming makes zero maps and yields zero sources. Retain populated-root
one-initialization/project-hint assertions.

- [ ] **Step 2: Run and verify failure**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/parser \
  -run 'Gemini.*(Empty|ProjectMap|SourceMethods)' -count=1
```

- [ ] **Step 3: Implement lazy work**

For slice discovery:

```go
paths := s.discoverSessionPaths(root)
if len(paths) == 0 {
    return nil, nil
}
projectMap := buildGeminiProjectMap(root)
```

For streaming, scan chats before resolving a project; construct/load the map
immediately before first matching yield, reuse per root, and close once if
created. Preserve cancellation and joined cleanup errors.

- [ ] **Step 4: Test, format, and commit**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/parser \
  -run 'Gemini.*(Empty|ProjectMap|SourceMethods)' -count=1
go fmt ./internal/parser
git add internal/parser/gemini_provider.go \
  internal/parser/gemini_copilot_provider_test.go internal/parser/discovery_test.go
git commit -m "perf(gemini): skip metadata work without sessions"
```

### Task 5: Bounded public WatchBatch transport

**Files:**

- Create: `internal/sync/watch_batch_accumulator.go`
- Modify: `internal/sync/watcher.go` and `watcher_test.go`
- Modify: `cmd/agentsview/daemon_push.go` and `daemon_push_test.go`
- Modify: `internal/server/huma_routes_push.go` and
  `huma_routes_push_internal_test.go`
- Regenerate: `frontend/src/lib/api/generated/`

**Interfaces:**

- Produces `WatchBatchAccumulator` with `Add`, `Take`, and `Empty`.

- Produces JSON-safe `WatchBatch`/`WatchRename` excluding lifecycle tokens.

- Produces optional daemon `WatchBatch` and `WatchRecovery` fields.

- [ ] **Step 1: Write failing accumulator/wire tests**

Merge duplicates, preserve `LostEvents`, assert sorted output, and separately
exceed existing count and byte limits to get `FullSync+LostEvents`. Capture the
promotion callback and assert it reports `entry_limit` or `byte_limit` without
any path. JSON round-trip a batch with a lifecycle token and assert only public
data survives. Assert CLI/server request shapes match and omission remains
compatible.

- [ ] **Step 2: Run and verify failure**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/sync ./cmd/agentsview ./internal/server \
  -run 'WatchBatchAccumulator|WatchBatchJSON|DaemonPush.*Watch' -count=1
```

- [ ] **Step 3: Implement using existing limits**

Wrap `pendingWatchBatch`:

```go
type WatchBatchAccumulator struct {
    pending *pendingWatchBatch
}
func NewWatchBatchAccumulator(onPromote func(WatchBatchPromotionReason)) *WatchBatchAccumulator
func (a *WatchBatchAccumulator) Add(batch WatchBatch)
func (a *WatchBatchAccumulator) Take() (WatchBatch, bool)
func (a *WatchBatchAccumulator) Empty() bool
```

`Add` copies only public work and calls existing bounded methods. Thread a typed
promotion reason through `pendingWatchBatch.onOverflow`; callers log only that
reason and aggregate pending counts. Add JSON tags and exclude lifecycle tokens.
Define `WatchRecoveryScope` with available/deferred roots. Add optional pointer
fields with JSON names `watch_batch` and `watch_recovery` to both request
structs. Regenerate the frontend API after these final Huma schema fields land.

- [ ] **Step 4: Test, format, and commit**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/sync ./cmd/agentsview ./internal/server \
  -run 'WatchBatchAccumulator|WatchBatchJSON|DaemonPush.*Watch' -count=1
go fmt ./internal/sync ./cmd/agentsview ./internal/server
(cd frontend && npm run generate:api)
git add internal/sync cmd/agentsview/daemon_push.go frontend/src/lib/api/generated \
  cmd/agentsview/daemon_push_test.go internal/server/huma_routes_push.go \
  internal/server/huma_routes_push_internal_test.go
git commit -m "feat(sync): add bounded watcher batch transport"
```

### Task 6: Preserve batches through push-loop retry

**Files:**

- Modify: `cmd/agentsview/pg_watch_loop.go` and `pg_watch_loop_test.go`
- Modify: `cmd/agentsview/archive_write_backend.go` and related PG/DuckDB tests

**Interfaces:**

- Produces `NotifyBatch` and `NotifyBatchWithAck`.

- Changes push callback to receive `*sync.WatchBatch`.

- Produces `PGPushConfig.WatchBatch` and `WatchRecovery`.

- [ ] **Step 1: Write failing loop tests**

Prove union coalescing, unscoped supersession, floor claiming pending scope,
exact failure restoration plus concurrent merge, overflow retry, waiter
ordering, shutdown batch flush, and aggregate promotion logs containing a
reason/count but no paths.

- [ ] **Step 2: Write failing daemon watch tests**

Ordinary callback sends its path. Full/rename sends a recovery snapshot from
`probeWatchRecoveryScope`. Startup/interval with no pending event omits scope.

- [ ] **Step 3: Run and verify failure**

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview \
  -run 'PushLoop.*Batch|PGPushWatch.*Batch|Daemon.*WatchBatch' -count=1
```

- [ ] **Step 4: Implement pending ownership**

Use:

```go
pendingUnscoped bool
pendingBatch    *sync.WatchBatchAccumulator
waiters         []chan error
```

Dirty marks unscoped; batch methods add bounded scope unless superseded. Claim
returns nil for unscoped or a copied batch. Restore merges failed scope with
arrivals and preserves waiter order. Pass the claim for every push reason.

- [ ] **Step 5: Wire daemon PG**

Use batch notification from `notifyPushForWatchBatch`. Attach claimed scope to
`PGPushConfig`. For full/rename, probe recovery immediately before HTTP. Local
PG/DuckDB callbacks accept and ignore their already-applied batches.

- [ ] **Step 6: Test, format, and commit**

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview \
  -run 'PushLoop|PGPushWatch|Daemon.*WatchBatch' -count=1
go fmt ./cmd/agentsview
git add cmd/agentsview
git commit -m "fix(pg): preserve watcher scope through daemon pushes"
```

### Task 7: Apply WatchBatch and push under one engine lock

**Files:**

- Create: `internal/sync/watch_batch_sync.go` and `watch_batch_sync_test.go`
- Modify: `internal/sync/engine.go` and `engine_test.go`
- Modify: `cmd/agentsview/main.go` and `main_test.go`
- Modify: `cmd/agentsview/archive_write_backend.go` and
  `archive_write_backend_test.go`

**Interfaces:**

- Produces `ValidateWatchBatch`, `ApplyWatchBatch`, and
  `Engine.SyncWatchBatchThenRun`.

- [ ] **Step 1: Drive planning tests through shared API**

Preserve existing rename, missing-descendant, recovery, lost-event, and retry
cases. Reject empty work, invalid item type, full batch retaining any paths,
roots, or renames, full/rename without recovery, blank recovery roots, and any
available/deferred roots that are equal or overlap by ancestry.

- [ ] **Step 2: Write synchronized cardinality tests**

Use one changed source with one versus 10,000 unrelated stored sources. Assert
callback observes updated archive, provider-wide discovery is zero, and
classification count is equal. Repeat at both cardinalities after deleting the
changed source; assert tombstoning work remains equal and only that session is
tombstoned. Block callback and prove another sync cannot enter. Add root/lost
cases proving authoritative discovery and retry markers.

- [ ] **Step 3: Run and verify failure**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/sync ./cmd/agentsview \
  -run 'WatchBatch|SyncWatchBatchThenRun|ChangedBatchCardinality' -count=1
```

- [ ] **Step 4: Extract planning/recovery**

Move command-local watcher interface, rename classification, retry errors, and
recovery overlap into `internal/sync/watch_batch_sync.go`. Keep probing in
command code but return shared `WatchRecoveryScope`.

- [ ] **Step 5: Split path prepare/apply and add serialized execution**

Add:

```go
func (e *Engine) prepareChangedPathSync(
    ctx context.Context, paths []string,
) preparedChangedPathSync
func (e *Engine) applyChangedPathSyncLocked(
    ctx context.Context, prepared preparedChangedPathSync,
) (SyncStats, int, error)
```

Keep public `SyncPathsContext` behavior. The new method plans/prepares, locks
once, applies paths, invokes `reconcileScopedWatchRootsLocked`, flushes signals,
runs work, unlocks, and emits once. Failures skip work.

- [ ] **Step 6: Use shared application in watcher callbacks**

Call `sync.ApplyWatchBatch` from serve/archive watcher callbacks with the probed
recovery scope. Preserve `WatchRetryError` acknowledgements.

- [ ] **Step 7: Test, race-test, format, and commit**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/sync ./cmd/agentsview \
  -run 'WatchBatch|SyncWatchBatchThenRun|ChangedBatchCardinality' -count=1
CGO_ENABLED=1 go test -race -tags fts5 ./internal/sync \
  -run 'SyncWatchBatchThenRun' -count=1
go fmt ./internal/sync ./cmd/agentsview
git add internal/sync cmd/agentsview/main.go cmd/agentsview/main_test.go \
  cmd/agentsview/archive_write_backend.go \
  cmd/agentsview/archive_write_backend_test.go
git commit -m "feat(sync): run scoped batches with serialized work"
```

### Task 8: Route daemon PG pushes through scoped sync

**Files:**

- Modify: `internal/server/huma_routes_push.go`

- Modify: `internal/server/huma_routes_push_internal_test.go`

- Modify: `docs/pg-sync.md`

- [ ] **Step 1: Write failing route tests**

Cover one-path update, one versus 10,000 unrelated-source cardinality,
full/lost/rename/root authoritative behavior, omitted-field compatibility,
stale-archive worker fallback, and pre-SSE HTTP 400 for malformed scope.

- [ ] **Step 2: Run and verify failure**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/server \
  -run 'Push.*(WatchBatch|Scoped|Stale)|SyncThenRunForPush' -count=1
```

- [ ] **Step 3: Implement validation and selection**

Extend `syncThenRunForPush` with batch/recovery pointers. Current archive plus
batch uses `SyncWatchBatchThenRun` and `work(false)`. Stale/explicit-full keeps
worker/rebuild and `work(true)`. Omitted batch keeps `SyncThenRun`. DuckDB
supplies nil. Validate before stream response creation.

- [ ] **Step 4: Document PG watch behavior**

State `pg push --watch` sends bounded changed paths to the daemon;
overflow/lost/renames reconcile authoritatively. Do not document internal
request fields.

- [ ] **Step 5: Test, format, and commit**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/server \
  -run 'Push.*(WatchBatch|Scoped|Stale)|SyncThenRunForPush' -count=1
go fmt ./internal/server
git diff --check
git add internal/server docs/pg-sync.md
git commit -m "fix(pg): scope daemon syncs to watcher batches"
```

### Task 9: Full verification and PR

- [ ] **Step 1: Format, diff-check, and scrub private data**

```bash
go fmt ./...
git diff --check
rg -n '/Users/|marius|private-host|internal-host' \
  --glob='!docs/superpowers/**' --glob='!*.sum' \
  internal cmd frontend docs/configuration.md docs/pg-sync.md || true
```

Inspect matches and remove only newly introduced private data.

- [ ] **Step 2: Run Go gates**

```bash
CGO_ENABLED=1 go vet -tags fts5 ./...
CGO_ENABLED=1 go test -tags fts5 ./...
```

- [ ] **Step 3: Run frontend gates**

```bash
(cd frontend && npm run generate:api)
git diff --exit-code -- frontend/src/lib/api/generated
(cd frontend && npm run i18n:compile && npm run check && vp check && vp test)
```

- [ ] **Step 4: Audit spec coverage**

Confirm disabled roots, retained archives, zero-session Gemini work, bounded
ordinary pushes, authoritative ambiguity, UI rollback/read-only/restart
behavior, and absence of migrations/dependencies/high-cardinality labels/process
mutation.

- [ ] **Step 5: Commit only genuine verification fixes**

If tracked files changed, stage exact files and commit
`fix: address scoped push verification findings`. Otherwise do not create an
empty commit.

- [ ] **Step 6: Push and open PR**

Use commit-push-PR. Push the existing branch without rebase/amend. Create a
summary-only PR explaining measured rediscovery, bounded batches, controls,
Gemini fast path, restart requirement, and review hotspots. Include no test-plan
section, checklist, transcript, private path, or merge.
