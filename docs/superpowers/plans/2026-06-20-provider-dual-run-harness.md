# Provider Dual-Run Harness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the root-level provider migration harness so provider branches
must opt into shadow comparison instead of only adding parallel provider
implementations.

**Architecture:** The parser package owns the per-`AgentType` migration manifest
because provider branches already change parser factories. The sync package owns
the source-level observation helper because it converts provider `Fingerprint`
and `Parse` calls into engine-shaped planned effects without touching the live
database.

**Tech Stack:** Go 1.26, `testing`, `github.com/stretchr/testify`, git-spice
stacked branches.

**Migration mode semantics:**

- `legacy-only`: only the legacy parser/sync path runs and writes. This is the
  default for legacy adapter providers and is allowed for concrete providers
  only with an explicit rollback note and open follow-up task.
- `shadow-compare`: the legacy path remains authoritative for DB writes,
  skip-cache persistence, data-version rows, source metadata, diagnostics,
  SSE, and return values. The provider path runs through the shared provider
  runner and produces normalized in-memory planned effects. Tests compare
  those planned effects against the legacy outcome; runtime mismatches are
  developer diagnostics only and must not create user-visible parse
  diagnostics.
- `provider-authoritative`: the provider path owns writes and return values and
  the old provider-specific legacy branch is gone. This mode is reserved for
  the stack tip after every parse-capable provider has passed shadow
  comparison.
- `import-only`: the provider is intentionally excluded from filesystem parse
  comparison because it represents non-filesystem import/export metadata
  rather than a parser replacement.

Promotion requires fixture evidence for parsed sessions, exclusions, skip-cache
keys, data-version state, source metadata, diagnostics, retry state, and
source-key/session-ID compatibility. Rollback means moving the manifest entry
back to `legacy-only`, recording the reason in kata/review notes, and leaving
the legacy path authoritative until the mismatch is fixed.

Provider observations must reject cross-provider output before planning effects
and before any remote machine prefix is applied. `ParseResult.Session.Agent`
must equal the provider `AgentType`. Persisted session IDs in the result graph
must use the provider's ID prefix when one exists; this includes result IDs,
parent IDs, usage-event session IDs, subagent links, exclusions, and diagnostic
session IDs. Diagnostic `SourceError.SourceKey` values are required and must be
the provider fingerprint key, `SourceRef.FingerprintKey`, `SourceRef.Key`, or a
virtual key derived from one of those candidates by appending `#`, `::`, or `|`.

`ProviderPlannedEffects` is an engine-shaped comparison model, not a second
writer. Its source key is the fingerprint key when available, then
`SourceRef.FingerprintKey`, then `SourceRef.Key`. Its skip-cache key follows the
same engine order used for persisted skip decisions. Its data-version entries
match the rows the legacy engine would stamp after successful writes, including
retry state from `DataVersionNeedsRetry`. Its diagnostics mirror parse
diagnostics without inserting them into the live store. Provider retry-reason
text and SSE scopes are outside the root process-result comparison until a later
caller task exposes equivalent legacy data.

Performance rule: shadow comparison may double-parse a source only while that
provider is actively migrating. Large roots and shared database providers need
fixture or benchmark coverage before promotion, and caller-level shadow wiring
must keep provider failures from blocking legacy writes unless a test is
explicitly asserting the mismatch.

______________________________________________________________________

### Task 1: Provider Migration Manifest

**Files:**

- Create: `internal/parser/provider_migration.go`

- Modify: `internal/parser/provider_test.go`

- [ ] **Step 1: Write the failing manifest tests**

Add tests that prove the manifest covers the registry and rejects a concrete
provider left in `legacy-only` mode:

```go
func TestProviderMigrationModesCoverRegistry(t *testing.T) {
	err := ValidateProviderMigrationModes(ProviderFactories(), ProviderMigrationModes())
	require.NoError(t, err)
}

func TestProviderMigrationModesRejectConcreteProviderLeftLegacyOnly(t *testing.T) {
	factory := testProviderFactory{def: AgentDef{Type: AgentCodex, DisplayName: "Codex"}}
	modes := map[AgentType]ProviderMigrationMode{
		AgentCodex: ProviderMigrationLegacyOnly,
	}

	err := ValidateProviderMigrationModes([]ProviderFactory{factory}, modes)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "codex")
	assert.Contains(t, err.Error(), "shadow-compare")
}
```

- [ ] **Step 2: Run the parser tests and verify RED**

Run:

```bash
go test -tags "fts5" ./internal/parser -run TestProviderMigrationModes -count=1
```

Expected: FAIL because `ProviderMigrationMode`, `ProviderMigrationModes`, and
`ValidateProviderMigrationModes` do not exist yet.

- [ ] **Step 3: Implement the manifest types and validation**

Create `internal/parser/provider_migration.go` with:

```go
type ProviderMigrationMode string

const (
	ProviderMigrationLegacyOnly          ProviderMigrationMode = "legacy-only"
	ProviderMigrationShadowCompare       ProviderMigrationMode = "shadow-compare"
	ProviderMigrationProviderAuthoritative ProviderMigrationMode = "provider-authoritative"
	ProviderMigrationImportOnly          ProviderMigrationMode = "import-only"
)
```

Add a registry-covering manifest initialized to `legacy-only`, return copies to
callers, and validate:

- every provider factory has one mode;

- no extra manifest entry points at an unknown agent;

- concrete non-legacy factories cannot remain `legacy-only`;

- `shadow-compare`, `provider-authoritative`, and `import-only` require a
  concrete factory;

- `import-only` is allowed only for Claude.ai and ChatGPT.

- [ ] **Step 4: Run the parser tests and verify GREEN**

Run:

```bash
go test -tags "fts5" ./internal/parser -run TestProviderMigrationModes -count=1
```

Expected: PASS.

### Task 2: Source-Level Provider Observation

**Files:**

- Create: `internal/sync/provider_shadow.go`

- Create: `internal/sync/provider_shadow_test.go`

- [ ] **Step 1: Write failing observation tests**

Add tests that use a fake provider to prove the helper:

- calls `Fingerprint` before `Parse`;
- converts `ParseOutcome` into an observation;
- records planned data-version/source/diagnostic effects in memory;
- never accepts a mismatched `SourceRef.Provider`;
- rejects provider results, exclusions, and diagnostics whose agent or persisted
  session-ID namespace belongs to another provider.

The main test should assert:

```go
assert.Equal(t, []string{"fingerprint", "parse"}, provider.calls)
assert.Equal(t, []string{"codex:one"}, observation.Planned.DataVersionSessionIDs())
assert.Equal(t, []string{"codex:two"}, observation.Planned.RetrySessionIDs())
assert.Equal(t, []string{"source-key"}, observation.Planned.SourceKeys)
assert.Len(t, observation.Planned.Diagnostics, 1)
```

- [ ] **Step 2: Run the sync tests and verify RED**

Run:

```bash
go test -tags "fts5" ./internal/sync -run TestObserveProviderSource -count=1
```

Expected: FAIL because `ObserveProviderSource` and observation types do not
exist.

- [ ] **Step 3: Implement the minimal observation helper**

Create `internal/sync/provider_shadow.go` with:

```go
type ProviderObserveRequest struct {
	Source     parser.SourceRef
	Machine    string
	ForceParse bool
}

type ProviderObservation struct {
	Results            []parser.ParseResult
	ExcludedSessionIDs []string
	SourceErrors       []parser.SourceError
	SkipReason         parser.SkipReason
	ForceReplace       bool
	Planned            ProviderPlannedEffects
}
```

`ObserveProviderSource` checks the source/provider type match, calls
`Fingerprint`, calls `Parse`, validates provider output invariants, and builds
in-memory planned effects. It must not accept a `db.DB`, `Engine`, writer
callback, or mutable skip-cache reference.

- [ ] **Step 4: Run the sync tests and verify GREEN**

Run:

```bash
go test -tags "fts5" ./internal/sync -run TestObserveProviderSource -count=1
```

Expected: PASS.

### Follow-Up: Caller-Level Wiring

**Files:**

The root harness branch wires the shared `processFile` shadow comparison. The
remaining caller families below stay as later sync migration work so provider
branches can add caller-specific source selection, hint lookup, and acceptance
coverage one behavior group at a time.

**Step 1: Wire remaining source-processing callers into shadow comparison**

Move changed-path sync and `SyncSingleSession` semantics into the caller-level
dual-run wrapper without adding a duplicate `processFile` hook. These callers
reuse the shared `processFile` observation for parse comparison, then add
caller-specific source selection, stored-source hints, and acceptance assertions
around that observation. They must leave live DB/diagnostic/SSE state driven
only by the legacy result.

**Step 2: Add lookup/watch/diagnostic caller coverage**

Move session watch flows, export/source lookup, source mtime, token-usage raw
source probing, parse-diff, and parse diagnostics through the same provider
runner. Tests must cover source lookup freshness, virtual paths, source mtime,
raw probing behavior, report shape, and source-error behavior.

**Step 3: Define runtime mismatch reporting**

Mismatches are test failures in shared parity tests. Runtime mismatch reporting
is developer-only logging or debug diagnostics and must include provider, source
key, fingerprint key, mode, field path, legacy value summary, provider value
summary, and whether fingerprinting or parsing failed. It must not persist
user-visible parse diagnostics while `shadow-compare` is active.

### Task 3: Validation And Commit

**Files:**

- Modify as needed from Tasks 1-2.

- [ ] **Step 1: Format and verify**

Run:

```bash
go fmt ./...
go test -tags "fts5" ./internal/parser -run TestProviderMigrationModes -count=1
go test -tags "fts5" ./internal/sync -run TestObserveProviderSource -count=1
go test -tags "fts5" ./internal/parser -count=1
go test -tags "fts5" ./internal/sync -count=1
go vet ./...
git diff --check
```

Expected: all commands pass. If `go fmt ./...` rewrites unrelated comments,
restore only unrelated user-owned changes before committing.

- [ ] **Step 2: Commit on `provider-facade-core`**

Commit the root harness slice with a conventional message:

```bash
git add docs/superpowers/plans/2026-06-20-provider-dual-run-harness.md internal/parser/provider_migration.go internal/parser/provider_test.go internal/sync/provider_shadow.go internal/sync/provider_shadow_test.go
git commit -m "feat(parser): add provider migration harness"
```

- [ ] **Step 3: Restack locally when explicitly authorized**

If the user has explicitly authorized branch changes and restacking for this
session, run:

```bash
git-spice upstack restack
```

Expected: dependent provider branches are replayed on the harness branch and
conflicts are resolved provider by provider. Do not push, submit, or update PRs
unless the user has separately authorized that network operation.
