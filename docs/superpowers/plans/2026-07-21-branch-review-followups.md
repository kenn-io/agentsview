# Branch Review Follow-ups Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Preserve exact branch identity across private projects while making
branch-heavy Usage summaries opt-in and keeping every returned attribution row
usable at large cardinalities.

**Architecture:** Activity and Usage responses carry an opaque `project_key`
beside the sanitized project label, while storage aggregation stays keyed by raw
`(project, branch)` values. Usage requests add an independent
`branch_breakdowns` capability that defaults off; the frontend requests it only
for an active Branch grouping and retains a branch-rich response until another
date or filter refresh. The existing TanStack wrapper virtualizes the
attribution side rail and list without changing row behavior or copy.

**Tech Stack:** Go 1.24, SQLite, PostgreSQL, DuckDB, Huma/OpenAPI, Svelte 5,
TypeScript, Vitest, `@tanstack/virtual-core`, Paraglide JS.

## Global Constraints

- Preserve SQLite, PostgreSQL, and DuckDB behavior and query-shape parity.
- Derive opaque project keys before replacing raw project labels with safe
  display labels.
- Keep raw path-like project labels inside the local service/storage boundary.
- Do not add a compatibility endpoint, versioned response, dual read/write path,
  or fallback response shape.
- Keep plain branch-name filters and current raw project-qualified branch tokens
  working.
- Do not truncate the branch catalog or hide low-cost branches.
- Keep the treemap cap at exactly 40 tiles.
- Preserve sorting, percentages, tooltips, click behavior, keyboard focus, and
  localized visible copy in virtualized rows.
- Use `github.com/stretchr/testify`; use `require.X` for setup and length gates
  and `assert.X` for independent checks.
- Do not add tests for framework behavior, generated-file absence, or source
  snippets. Every new test must assert an observable contract.
- Run `go fmt ./...` and `go vet ./...` after Go changes.
- Never use `--no-verify`, amend, squash, rebase, change branches, or merge the
  pull request during this plan.
- Commit every task that changes tracked files with a focused conventional
  commit message.

______________________________________________________________________

## File Structure

### Backend identity and filtering

- `internal/activity/activity.go`: add and populate Activity branch
  `project_key` values before label sanitization.
- `internal/activity/sessions_test.go`: prove two unsafe labels remain distinct
  after sanitization.
- `internal/db/query_dialect.go`: rewrite only the project component of
  qualified branch filter tokens while preserving plain names and sentinels.
- `internal/db/usage.go`: add Usage branch `project_key`, separate branch
  aggregation from ordinary breakdowns, and sanitize branch rows with the
  project catalog.
- `internal/db/branch_filter_test.go`: cover SQLite identity, default omission,
  opt-in inclusion, and exact filtering.
- `internal/postgres/usage.go`: mirror SQLite scan, accumulator, and output
  behavior.
- `internal/postgres/usage_pgtest_test.go`: protect PostgreSQL parity.
- `internal/duckdb/analytics_usage.go`: independently gate branch grouping and
  output in DuckDB.
- `internal/duckdb/store_test.go`: protect DuckDB parity.
- `internal/service/usage.go`: expose `branch_breakdowns`, resolve opaque
  project-key branch tokens, and fold totals by `(project_key, branch)`.
- `internal/service/usage_internal_test.go`: test range-wide branch identity.
- `internal/service/usage_test.go`: test request defaults and exact
  project-key-qualified filtering.
- `internal/service/http.go`: forward `branch_breakdowns` through the remote
  service client.
- `internal/server/huma_routes_usage.go`: publish the query parameter in Huma.
- `internal/server/usage_internal_test.go`: test transport defaults and opt-in
  mapping.

### Frontend API, state, and rendering

- `frontend/src/lib/api/types/usage.ts`: type `project_key` on daily and
  range-wide branch rows.
- `frontend/src/lib/api/generated/`: regenerate OpenAPI models and Usage service
  parameters after backend contract changes.
- `frontend/src/lib/components/activity/Breakdowns.svelte`: use the project key
  for row identity while displaying only the safe label.
- `frontend/src/lib/components/activity/Breakdowns.test.ts`: prove colliding
  display labels keep independent keys.
- `frontend/src/lib/components/usage/CostTimeSeriesChart.svelte`: key branch
  series with `project_key` while keeping safe labels in the legend.
- `frontend/src/lib/components/usage/CostTimeSeriesChart.test.ts`: protect
  distinct series identity.
- `frontend/src/lib/components/usage/AttributionPanel.svelte`: key branch rows
  with `project_key`, request Branch data through the store, and virtualize
  the side rail and list.
- `frontend/src/lib/components/usage/AttributionPanel.test.ts`: protect exact
  branch selection and bounded DOM rendering.
- `frontend/src/lib/stores/usage.svelte.ts`: track whether the displayed summary
  contains branch data and perform the first Branch-only refresh.
- `frontend/src/lib/stores/usage.test.ts`: test lazy fetch, cached reuse, and
  invalidation after date/filter refreshes.

### Documentation

- `docs/session-api.md`: replace the obsolete opaque-token description with the
  bounded branch-name search contract.

______________________________________________________________________

### Task 1: Preserve Activity branch identity through sanitization

**Files:**

- Modify: `internal/activity/activity.go`
- Test: `internal/activity/sessions_test.go`

**Interfaces:**

- Consumes: `export.ProjectKeyForEntry(export.ProjectMapEntry) string` and
  `export.SafeProjectDisplayLabel(string) string`.

- Produces: `activity.BranchKeyMinutes.ProjectKey string` serialized as
  `project_key`.

- [ ] **Step 1: Replace the single-row sanitization test with a collision
  regression**

    Add this observable test to `internal/activity/sessions_test.go`:

    ```go
    func TestSanitizeProjectLabelsKeepsCollidingBranchProjectsDistinct(t *testing.T) {
        first := "/Users/example/one/private/repo"
        second := "/Users/example/two/private/repo"
        report := Report{ByBranch: []BranchKeyMinutes{
            {Project: first, Branch: "main", Cost: 1},
            {Project: second, Branch: "main", Cost: 2},
        }}
        projects := map[string]export.ProjectMapEntry{
            first:  {ProjectKey: "pl1:sha256:first"},
            second: {ProjectKey: "pl1:sha256:second"},
        }

        SanitizeProjectLabels(&report, projects)

        require.Len(t, report.ByBranch, 2)
        assert.Equal(t, "pl1:sha256:first", report.ByBranch[0].ProjectKey)
        assert.Equal(t, "pl1:sha256:second", report.ByBranch[1].ProjectKey)
        assert.Empty(t, report.ByBranch[0].Project)
        assert.Empty(t, report.ByBranch[1].Project)
        assert.Equal(t, "main", report.ByBranch[0].Branch)
        assert.Equal(t, "main", report.ByBranch[1].Branch)
    }
    ```

- [ ] **Step 2: Run the Activity regression and verify it fails**

    Run:

    ```bash
    go test ./internal/activity -run TestSanitizeProjectLabelsKeepsCollidingBranchProjectsDistinct -count=1
    ```

    Expected: compile failure because `BranchKeyMinutes` has no `ProjectKey`
    field, or assertion failure because the field is not populated.

- [ ] **Step 3: Add the branch project key and populate it before sanitizing**

    Update `BranchKeyMinutes` and the `ByBranch` loop in
    `internal/activity/activity.go` to this shape:

    ```go
    type BranchKeyMinutes struct {
        ProjectKey             string  `json:"project_key"`
        Project                string  `json:"project"`
        Branch                 string  `json:"branch"`
        AgentMinutes           float64 `json:"agent_minutes"`
        Cost                   float64 `json:"cost"`
        AutomatedAgentMinutes  float64 `json:"automated_agent_minutes"`
        InteractiveAgentMinutes float64 `json:"interactive_agent_minutes"`
        AutomatedCost          float64 `json:"automated_cost"`
        InteractiveCost        float64 `json:"interactive_cost"`
    }
    ```

    Keep the existing field alignment after `gofmt`, and change sanitization to:

    ```go
    for i := range report.ByBranch {
        raw := report.ByBranch[i].Project
        report.ByBranch[i].ProjectKey = export.ProjectKeyForEntry(projects[raw])
        report.ByBranch[i].Project = export.SafeProjectDisplayLabel(raw)
    }
    ```

- [ ] **Step 4: Run the focused Activity package tests**

    Run:

    ```bash
    go test ./internal/activity -run 'TestSanitizeProjectLabels|TestSessionsTable_ByBranch' -count=1
    ```

    Expected: PASS.

- [ ] **Step 5: Commit the Activity identity change**

    ```bash
    git add internal/activity/activity.go internal/activity/sessions_test.go
    git commit -m "fix: preserve activity branch identity"
    ```

______________________________________________________________________

### Task 2: Preserve Usage branch identity and resolve opaque branch filters

**Files:**

- Modify: `internal/db/query_dialect.go`
- Modify: `internal/db/usage.go`
- Modify: `internal/service/usage.go`
- Test: `internal/db/branch_filter_test.go`
- Test: `internal/service/usage_internal_test.go`
- Test: `internal/service/usage_test.go`

**Interfaces:**

- Consumes: branch filter values separated by the existing private
  `branchListSep`, with qualified values encoded by
  `db.EncodeBranchFilterToken(project, branch)`.

- Produces:
  `db.RewriteQualifiedBranchFilterProjects(tokens string, rewrite func(string) (string, error)) (string, error)`.

- Produces: `db.BranchBreakdown.ProjectKey string` and
  `service.BranchTotal.ProjectKey string`, both serialized as `project_key`.

- Produces: `ResolveUsageProjectKeys` rewriting opaque project-key components in
  both `GitBranch` and `ExcludeGitBranch` before `BuildUsageFilter` runs.

- [ ] **Step 1: Add failing tests for sanitized identity and range-wide
  folding**

    In `internal/db/branch_filter_test.go`, replace the existing branch
    sanitization assertion with:

    ```go
    func TestSanitizeDailyUsageProjectLabelsKeepsBranchIdentity(t *testing.T) {
        first := "/Users/example/one/private/repo"
        second := "/Users/example/two/private/repo"
        result := DailyUsageResult{Daily: []DailyUsageEntry{{
            BranchBreakdowns: []BranchBreakdown{
                {Project: first, Branch: "main", Cost: 1},
                {Project: second, Branch: "main", Cost: 2},
            },
        }}}
        projects := map[string]export.ProjectMapEntry{
            first:  {ProjectKey: "pl1:sha256:first"},
            second: {ProjectKey: "pl1:sha256:second"},
        }

        SanitizeDailyUsageProjectLabelsWithCatalog(&result, projects)

        require.Len(t, result.Daily, 1)
        require.Len(t, result.Daily[0].BranchBreakdowns, 2)
        assert.Equal(t, "pl1:sha256:first", result.Daily[0].BranchBreakdowns[0].ProjectKey)
        assert.Equal(t, "pl1:sha256:second", result.Daily[0].BranchBreakdowns[1].ProjectKey)
        assert.Empty(t, result.Daily[0].BranchBreakdowns[0].Project)
        assert.Empty(t, result.Daily[0].BranchBreakdowns[1].Project)
    }
    ```

    In `internal/service/usage_internal_test.go`, make the folding test contain
    two rows with the same safe `Project` and `Branch` but different
    `ProjectKey` values, and assert two `BranchTotal` rows:

    ```go
    assert.Equal(t, []BranchTotal{
        {
            ProjectKey: "pl1:sha256:second", Project: "", Branch: "main",
            InputTokens: 20, OutputTokens: 8, Cost: 2,
        },
        {
            ProjectKey: "pl1:sha256:first", Project: "", Branch: "main",
            InputTokens: 10, OutputTokens: 4, Cost: 1,
        },
    }, got)
    ```

- [ ] **Step 2: Add a failing direct-service filter test**

    In `internal/service/usage_test.go`, seed two sessions whose raw project
    labels sanitize to the same empty display label, assign both branch `main`,
    obtain their opaque project keys from an unfiltered summary, and request one
    exact token:

    ```go
    filtered, err := be.UsageSummary(context.Background(), service.UsageRequest{
        From:             "2026-05-14",
        To:               "2026-05-14",
        Timezone:         "UTC",
        IncludeOneShot:   true,
        IncludeAutomated: true,
        GitBranch: db.EncodeBranchFilterToken(
            summary.BranchTotals[0].ProjectKey,
            summary.BranchTotals[0].Branch,
        ),
    })
    require.NoError(t, err)
    require.Len(t, filtered.BranchTotals, 1)
    assert.Equal(t, summary.BranchTotals[0].ProjectKey,
        filtered.BranchTotals[0].ProjectKey)
    assert.Equal(t, summary.BranchTotals[0].Cost,
        filtered.Totals.TotalCost)
    ```

    Add a second request with `pl1:sha256:missing` and assert a
    `*service.UsageInputError` with code
    `service.UsageErrorCodeUnknownProjectKey`.

- [ ] **Step 3: Run the focused tests and verify they fail**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/service \
      -run 'TestSanitizeDailyUsageProjectLabelsKeepsBranchIdentity|TestFoldBranchTotals|TestDirectBackend_UsageSummary_.*ProjectKey.*Branch' \
      -count=1
    ```

    Expected: compile failures for the missing `ProjectKey` fields and filtering
    failure because opaque keys are still compared directly with raw labels.

- [ ] **Step 4: Add a shared qualified-token rewrite helper**

    Add this function beside `SplitBranchFilterTokens` in
    `internal/db/query_dialect.go`:

    ```go
    func RewriteQualifiedBranchFilterProjects(
        tokens string,
        rewrite func(string) (string, error),
    ) (string, error) {
        parts := strings.Split(tokens, branchListSep)
        for i, token := range parts {
            project, branch, qualified := strings.Cut(token, branchFilterSep)
            if !qualified {
                continue
            }
            rewritten, err := rewrite(project)
            if err != nil {
                return "", err
            }
            parts[i] = EncodeBranchFilterToken(rewritten, branch)
        }
        return strings.Join(parts, branchListSep), nil
    }
    ```

    This preserves plain names, `NoBranchFilterToken`, `NoBranchMatchToken`, empty
    list elements, and raw project-qualified tokens unless the callback chooses
    to replace their project component.

- [ ] **Step 5: Add project keys to daily and range-wide Usage rows**

    Update `internal/db/usage.go`:

    ```go
    type BranchBreakdown struct {
        ProjectKey          string  `json:"project_key"`
        Project             string  `json:"project"`
        Branch              string  `json:"branch"`
        InputTokens         int     `json:"inputTokens"`
        OutputTokens        int     `json:"outputTokens"`
        CacheCreationTokens int     `json:"cacheCreationTokens"`
        CacheReadTokens     int     `json:"cacheReadTokens"`
        Cost                float64 `json:"cost"`
    }
    ```

    In `SanitizeDailyUsageProjectLabelsWithCatalog`, capture `raw` before
    sanitization and set both fields:

    ```go
    for j := range result.Daily[i].BranchBreakdowns {
        raw := result.Daily[i].BranchBreakdowns[j].Project
        result.Daily[i].BranchBreakdowns[j].ProjectKey =
            export.ProjectKeyForEntry(projects[raw])
        result.Daily[i].BranchBreakdowns[j].Project =
            export.SafeProjectDisplayLabel(raw)
    }
    ```

    Update `internal/service/usage.go`:

    ```go
    type BranchTotal struct {
        ProjectKey          string  `json:"project_key"`
        Project             string  `json:"project"`
        Branch              string  `json:"branch"`
        InputTokens         int     `json:"inputTokens"`
        OutputTokens        int     `json:"outputTokens"`
        CacheCreationTokens int     `json:"cacheCreationTokens"`
        CacheReadTokens     int     `json:"cacheReadTokens"`
        Cost                float64 `json:"cost"`
    }
    ```

    Fold by `ProjectKey` and `Branch`, initialize all three identity fields, and
    use `ProjectKey` before the safe label in the stable tie-break comparator.

- [ ] **Step 6: Resolve opaque project components inside the service boundary**

    Refactor `ResolveUsageProjectKeys` so it loads the project catalog once when
    any of these inputs need it: `ExcludeProjectKey`, an opaque qualified token
    in `GitBranch`, or an opaque qualified token in `ExcludeGitBranch`.

    Use a callback with this behavior:

    ```go
    rewriteProject := func(project string) (string, error) {
        if !strings.HasPrefix(project, "pl1:sha256:") {
            return project, nil
        }
        label, ok := byKey[project]
        if !ok {
            return "", &UsageInputError{
                Code: UsageErrorCodeUnknownProjectKey,
                Msg:  "unknown project key",
            }
        }
        return label, nil
    }
    ```

    Apply it through `db.RewriteQualifiedBranchFilterProjects` to both branch
    fields. Leave plain branch names and current raw qualified values unchanged.

- [ ] **Step 7: Run the focused identity and filtering tests**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/service \
      -run 'TestSanitizeDailyUsageProjectLabelsKeepsBranchIdentity|TestFoldBranchTotals|TestDirectBackend_UsageSummary_.*ProjectKey.*Branch' \
      -count=1
    ```

    Expected: PASS.

- [ ] **Step 8: Commit the Usage identity change**

    ```bash
    git add internal/db/query_dialect.go internal/db/usage.go \
      internal/db/branch_filter_test.go internal/service/usage.go \
      internal/service/usage_internal_test.go internal/service/usage_test.go
    git commit -m "fix: preserve usage branch identity"
    ```

______________________________________________________________________

### Task 3: Make branch breakdown aggregation independently opt-in

**Files:**

- Modify: `internal/db/usage.go`
- Modify: `internal/postgres/usage.go`
- Modify: `internal/duckdb/analytics_usage.go`
- Modify: `internal/service/usage.go`
- Modify: `internal/service/http.go`
- Modify: `internal/server/huma_routes_usage.go`
- Test: `internal/db/branch_filter_test.go`
- Test: `internal/postgres/usage_pgtest_test.go`
- Test: `internal/duckdb/store_test.go`
- Test: `internal/service/usage_test.go`
- Test: `internal/server/usage_internal_test.go`

**Interfaces:**

- Produces: `db.UsageFilter.BranchBreakdowns bool`.

- Produces: `service.UsageRequest.BranchBreakdowns *bool` serialized as
  `branch_breakdowns`.

- Produces: Huma query field `branch_breakdowns`, default `false`.

- Invariant: `Breakdowns` controls project, model, agent, and machine data;
  `BranchBreakdowns` alone controls branch scan columns, accumulator identity,
  daily branch rows, and service `BranchTotals`.

- [ ] **Step 1: Add SQLite default-omission and opt-in tests**

    Extend `internal/db/branch_filter_test.go` with a table-driven test that runs
    the same fixture twice:

    ```go
    tests := []struct {
        name              string
        branchBreakdowns  bool
        wantBranchRows    int
    }{
        {name: "default omission", branchBreakdowns: false, wantBranchRows: 0},
        {name: "opt in", branchBreakdowns: true, wantBranchRows: 3},
    }
    ```

    Use
    `UsageFilter{From: ..., To: ..., Timezone: "UTC", Breakdowns: true, BranchBreakdowns: tc.branchBreakdowns}`.
    Assert total cost and ordinary project/model/agent/machine breakdowns are
    identical in both cases, while `BranchBreakdowns` alone changes from empty
    to the literal expected rows.

- [ ] **Step 2: Add matching PostgreSQL and DuckDB parity tests**

    In `internal/postgres/usage_pgtest_test.go` and
    `internal/duckdb/store_test.go`, use each package's existing branch usage
    fixture and assert:

    ```go
    withoutBranches, err := store.GetDailyUsage(ctx, db.UsageFilter{
        From: "2026-05-14", To: "2026-05-14", Timezone: "UTC",
        Breakdowns: true,
    })
    require.NoError(t, err)
    require.Len(t, withoutBranches.Daily, 1)
    assert.Empty(t, withoutBranches.Daily[0].BranchBreakdowns)
    assert.NotEmpty(t, withoutBranches.Daily[0].ProjectBreakdowns)

    withBranches, err := store.GetDailyUsage(ctx, db.UsageFilter{
        From: "2026-05-14", To: "2026-05-14", Timezone: "UTC",
        Breakdowns: true, BranchBreakdowns: true,
    })
    require.NoError(t, err)
    require.Len(t, withBranches.Daily, 1)
    assert.Equal(t, withoutBranches.Totals, withBranches.Totals)
    assert.NotEmpty(t, withBranches.Daily[0].BranchBreakdowns)
    ```

    Also assert every returned branch row has the expected non-empty `ProjectKey`
    for a safe project catalog entry.

- [ ] **Step 3: Add request and transport default tests**

    In `internal/service/usage_test.go`, extend
    `TestBuildUsageFilter_ValidMapping` to assert:

    ```go
    assert.False(t, f.BranchBreakdowns,
        "branch aggregation is opt-in independently of ordinary breakdowns")
    ```

    Add a case with `BranchBreakdowns: ptr(true)` and assert true. In
    `internal/server/usage_internal_test.go`, add tests proving an omitted query
    field maps to false and `branch_breakdowns=true` maps to true while
    `breakdowns` remains true. Update every pre-existing branch-specific Usage
    request in `internal/db/branch_filter_test.go`,
    `internal/postgres/usage_pgtest_test.go`, `internal/duckdb/store_test.go`,
    and `internal/service/usage_test.go` to set `BranchBreakdowns: true`; tests
    whose subject is ordinary project/model/agent/machine data must leave it
    false.

- [ ] **Step 4: Run the new tests and verify they fail**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/duckdb \
      ./internal/service ./internal/server \
      -run 'BranchBreakdowns|BuildUsageFilter_ValidMapping' -count=1
    ```

    Expected: compile failure for missing `BranchBreakdowns`, or branch rows are
    still emitted whenever `Breakdowns` is true.

- [ ] **Step 5: Add the independent request and filter fields**

    Add these exact fields:

    ```go
    // internal/db/usage.go
    BranchBreakdowns bool // populate BranchBreakdowns per day

    // internal/service/usage.go
    BranchBreakdowns *bool `json:"branch_breakdowns,omitempty"`

    // internal/server/huma_routes_usage.go
    BranchBreakdowns bool `query:"branch_breakdowns" default:"false" doc:"Include per-project branch breakdowns"`
    ```

    In `BuildUsageFilter`, default the pointer to false and map it to
    `db.UsageFilter.BranchBreakdowns`. Thread the value through
    `usageRequestFromInput` and through `internal/service/http.go` as:

    ```go
    if req.BranchBreakdowns != nil {
        q.Set("branch_breakdowns", strconv.FormatBool(*req.BranchBreakdowns))
    }
    ```

- [ ] **Step 6: Split SQLite scan and aggregation gates**

    Change `dailyUsageRowSelectFromRowsWithBreakdowns` and
    `scanDailyUsageRowWithBreakdowns` to accept
    `(includeBreakdowns, includeBranchBreakdowns bool)`. Select/scan `machine`
    only for ordinary breakdowns and `git_branch` only for branch breakdowns.

    In `GetDailyUsage`:

    ```go
    branchAttributed := f.BranchBreakdowns && r.usageSource != "cursor"
    ```

    Keep `gitBranch` out of `accumKey` when false. Build project, agent, and
    machine maps only when `f.Breakdowns`; build the branch map and serialize
    `entry.BranchBreakdowns` only when `f.BranchBreakdowns`. The no-breakdown
    fast return applies only when both flags are false:

    ```go
    if !f.Breakdowns && !f.BranchBreakdowns {
        // existing date/model fast path
    }
    ```

- [ ] **Step 7: Mirror the split in PostgreSQL**

    Apply the same two booleans to `pgDailyUsageRowSelectFromRowsWithBreakdowns`
    and `scanPGDailyUsageRowWithBreakdowns`. Gate `machine` with `f.Breakdowns`,
    `git_branch` and `branchAttributed` with `f.BranchBreakdowns`, and daily
    branch maps/output with `f.BranchBreakdowns`. Preserve the existing ordering
    and cursor unattributed behavior.

- [ ] **Step 8: Mirror the split in DuckDB**

    In `dailyUsageAggregateRows`, leave the machine select/group/order controlled
    by `f.Breakdowns`, and move the branch select, `branch_attributed`, group,
    and order fragments under `f.BranchBreakdowns`. In `GetDailyUsage`, populate
    the ordinary maps only under `f.Breakdowns` and the branch map/output only
    under `f.BranchBreakdowns`.

- [ ] **Step 9: Fold branch totals only for branch-rich responses**

    In `buildUsageSummary`, keep project/model/agent totals under `f.Breakdowns`
    and set branch totals independently:

    ```go
    if f.BranchBreakdowns {
        out.BranchTotals = foldBranchTotals(result.Daily)
    } else {
        out.BranchTotals = []BranchTotal{}
    }
    ```

- [ ] **Step 10: Run SQLite, service, server, and DuckDB tests**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/duckdb \
      ./internal/service ./internal/server \
      -run 'BranchBreakdowns|BuildUsageFilter|UsageSummary' -count=1
    ```

    Expected: PASS.

- [ ] **Step 11: Run PostgreSQL parity tests**

    Run:

    ```bash
    make test-postgres
    ```

    Expected: PASS, including the branch breakdown parity test. If this task
    started the repository's test PostgreSQL container, stop that exact
    container with `make postgres-down` after the test run.

- [ ] **Step 12: Commit the lazy backend aggregation change**

    ```bash
    git add internal/db/usage.go internal/db/branch_filter_test.go \
      internal/postgres/usage.go internal/postgres/usage_pgtest_test.go \
      internal/duckdb/analytics_usage.go internal/duckdb/store_test.go \
      internal/service/usage.go internal/service/usage_test.go \
      internal/service/http.go internal/server/huma_routes_usage.go \
      internal/server/usage_internal_test.go
    git commit -m "perf: make usage branch breakdowns opt in"
    ```

______________________________________________________________________

### Task 4: Regenerate the API and use project-key branch identities

**Files:**

- Modify by generation: `frontend/src/lib/api/generated/`
- Modify: `frontend/src/lib/api/types/usage.ts`
- Modify: `frontend/src/lib/components/activity/Breakdowns.svelte`
- Modify: `frontend/src/lib/components/usage/CostTimeSeriesChart.svelte`
- Modify: `frontend/src/lib/components/usage/AttributionPanel.svelte`
- Test: `frontend/src/lib/components/activity/Breakdowns.test.ts`
- Test: `frontend/src/lib/components/usage/CostTimeSeriesChart.test.ts`
- Test: `frontend/src/lib/components/usage/AttributionPanel.test.ts`

**Interfaces:**

- Consumes: generated `ActivityBranchKeyMinutes.project_key`,
  `DbBranchBreakdown.project_key`, `ServiceBranchTotal.project_key`, and
  `UsageService.getApiV1UsageSummary({ branchBreakdowns?: boolean })`.

- Produces: branch filter tokens whose project component is always the opaque
  project key for newly rendered Activity and Usage branch rows.

- Invariant: labels are built from the sanitized `project` plus `branch`; no
  opaque project key is rendered as visible text.

- [ ] **Step 1: Add frontend collision regressions**

    In all three component test files, construct two branch rows with:

    ```ts
    {
      project_key: "pl1:sha256:first",
      project: "",
      branch: "main",
      cost: 8,
    }
    {
      project_key: "pl1:sha256:second",
      project: "",
      branch: "main",
      cost: 4,
    }
    ```

    Supply the remaining required numeric fields as literals. Assert the two rows
    or series remain independently keyed, and in `AttributionPanel.test.ts`
    click the second row and assert:

    ```ts
    expect(spy).toHaveBeenCalledWith(
      branchFilterToken("pl1:sha256:second", "main"),
    );
    ```

    Also assert neither rendered label contains `pl1:sha256:`.

- [ ] **Step 2: Run the component tests and verify they fail**

    Run from `frontend/`:

    ```bash
    npm test -- src/lib/components/activity/Breakdowns.test.ts \
      src/lib/components/usage/CostTimeSeriesChart.test.ts \
      src/lib/components/usage/AttributionPanel.test.ts
    ```

    Expected: type failures before generation, or identity assertions fail because
    the safe display label is still used in the token.

- [ ] **Step 3: Regenerate the OpenAPI client**

    Run from `frontend/`:

    ```bash
    npm run generate:api
    ```

    Confirm generated models contain `project_key: string` for Activity branch,
    daily Usage branch, and range-wide Usage branch rows, and that Usage summary
    accepts `branchBreakdowns?: boolean` with default false.

- [ ] **Step 4: Update the hand-written Usage types**

    Add `project_key: string` to both interfaces in
    `frontend/src/lib/api/types/usage.ts`:

    ```ts
    export interface BranchBreakdown {
      project_key: string;
      project: string;
      branch: string;
      inputTokens: number;
      outputTokens: number;
      cacheCreationTokens: number;
      cacheReadTokens: number;
      cost: number;
    }

    export interface BranchTotal {
      project_key: string;
      project: string;
      branch: string;
      inputTokens: number;
      outputTokens: number;
      cacheCreationTokens: number;
      cacheReadTokens: number;
      cost: number;
    }
    ```

- [ ] **Step 5: Separate branch identity from visible labels**

    In `CostTimeSeriesChart.svelte` and `AttributionPanel.svelte`, use:

    ```ts
    const token = branchFilterToken(b.project_key, b.branch);
    return {
      key: token,
      label: branchLabel(b.project, b.branch, noBranchLabel),
      cost: b.cost,
    };
    ```

    Use `id` instead of `key` in the attribution row shape. Import `branchLabel`
    directly; do not call `branchTokenLabel` on a project-key-qualified token.

    In `activity/Breakdowns.svelte`, map each branch row to an object with:

    ```ts
    type BreakdownRow = KeyMinutes & { displayLabel?: string };

    {
      ...b,
      key: branchFilterToken(b.project_key, b.branch),
      displayLabel: branchLabel(b.project, b.branch, m.shared_no_branch()),
    }
    ```

    Change `rankedRows`, `Panel.rows`, and the local panel label callback to use
    `BreakdownRow`; define `label?: (row: BreakdownRow) => string`. The branch
    panel returns `row.displayLabel ?? row.key`, while keyed rendering continues
    to use `row.key`.

- [ ] **Step 6: Run the identity component tests**

    Run from `frontend/`:

    ```bash
    npm test -- src/lib/components/activity/Breakdowns.test.ts \
      src/lib/components/usage/CostTimeSeriesChart.test.ts \
      src/lib/components/usage/AttributionPanel.test.ts
    ```

    Expected: PASS.

- [ ] **Step 7: Commit the generated contract and frontend identity wiring**

    ```bash
    git add frontend/src/lib/api/generated frontend/src/lib/api/types/usage.ts \
      frontend/src/lib/components/activity/Breakdowns.svelte \
      frontend/src/lib/components/activity/Breakdowns.test.ts \
      frontend/src/lib/components/usage/CostTimeSeriesChart.svelte \
      frontend/src/lib/components/usage/CostTimeSeriesChart.test.ts \
      frontend/src/lib/components/usage/AttributionPanel.svelte \
      frontend/src/lib/components/usage/AttributionPanel.test.ts
    git commit -m "fix: key branch rows by project identity"
    ```

______________________________________________________________________

### Task 5: Fetch branch-rich Usage summaries only when needed

**Files:**

- Modify: `frontend/src/lib/stores/usage.svelte.ts`
- Test: `frontend/src/lib/stores/usage.test.ts`

**Interfaces:**

- Produces: private state `summaryHasBranchBreakdowns: boolean`.

- Produces: `baseParams()` setting `branchBreakdowns: true` only while the
  shared active grouping is `branch`.

- Produces: `ensureBranchBreakdowns(): Promise<void>` for a focused first-Branch
  refresh that reuses summary abort/version/error handling and preserves
  comparison, pairwise, and top-session state.

- Invariant: a branch-rich response stays displayed across grouping switches,
  but any later date/filter `fetchAll()` requests branch data only when Branch
  is active.

- [ ] **Step 1: Add lazy-fetch tests to the Usage store**

    Add a `describe("UsageStore lazy branch breakdowns", ...)` block with these
    three observable cases:

    ```ts
    it("omits branch breakdowns from the ordinary summary", async () => {
      const { usage } = await loadStore();
      await usage.fetchSummary();
      expect(usageServiceMocks.getApiV1UsageSummary).toHaveBeenLastCalledWith(
        expect.not.objectContaining({ branchBreakdowns: true }),
      );
    });

    it("fetches once when Branch is first selected and reuses the rich summary", async () => {
      const { usage } = await loadStore();
      await usage.fetchSummary();
      const before = usageServiceMocks.getApiV1UsageSummary.mock.calls.length;

      usage.setAttributionGroupBy("branch");
      await vi.waitFor(() =>
        expect(usageServiceMocks.getApiV1UsageSummary).toHaveBeenLastCalledWith(
          expect.objectContaining({ branchBreakdowns: true }),
        ),
      );
      expect(usageServiceMocks.getApiV1UsageSummary).toHaveBeenCalledTimes(before + 1);

      usage.setTimeSeriesGroupBy("model");
      usage.setTimeSeriesGroupBy("branch");
      await Promise.resolve();
      expect(usageServiceMocks.getApiV1UsageSummary).toHaveBeenCalledTimes(before + 1);
    });

    it("drops retained branch data on a non-Branch filter refresh", async () => {
      const { usage } = await loadStore();
      usage.setAttributionGroupBy("branch");
      await usage.fetchSummary();
      usage.setAttributionGroupBy("model");

      await usage.fetchAll();
      expect(usageServiceMocks.getApiV1UsageSummary).toHaveBeenLastCalledWith(
        expect.not.objectContaining({ branchBreakdowns: true }),
      );

      const before = usageServiceMocks.getApiV1UsageSummary.mock.calls.length;
      usage.setAttributionGroupBy("branch");
      await vi.waitFor(() =>
        expect(usageServiceMocks.getApiV1UsageSummary).toHaveBeenCalledTimes(before + 1),
      );
    });
    ```

    For the focused refresh case, also snapshot the comparison, pairwise, and top
    sessions mock call counts before switching and assert they do not increase.

- [ ] **Step 2: Run the Usage store tests and verify they fail**

    Run from `frontend/`:

    ```bash
    npm test -- src/lib/stores/usage.test.ts
    ```

    Expected: the first Branch switch does not request new data, and ordinary
    requests cannot express `branchBreakdowns` yet.

- [ ] **Step 3: Track whether the accepted summary is branch-rich**

    Add private state in `UsageStore`:

    ```ts
    private summaryHasBranchBreakdowns = $state(false);
    ```

    In the successful, current-version branch of `fetchSummary`, assign:

    ```ts
    this.summaryHasBranchBreakdowns = params.branchBreakdowns === true;
    ```

    Do not change the flag for aborted, stale, or failed requests.

- [ ] **Step 4: Make normal request construction grouping-aware**

    Add this field to the object returned by `baseParams()`:

    ```ts
    branchBreakdowns:
      this.toggles.attribution.groupBy === "branch" ? true : undefined,
    ```

    The time-series and attribution grouping are synchronized, so reading one is
    sufficient.

- [ ] **Step 5: Add a focused branch refresh path**

    Extend `fetchSummary` options with:

    ```ts
    preserveRelatedState?: boolean;
    ```

    When true, accept the new summary without resetting pairwise selection/state
    and without launching comparison or pairwise requests. Keep the existing
    default behavior for all current callers. Concretely, default the option to
    false and wrap the existing `ensurePairwiseSelection()` and
    `clearPairwiseComparisonState()` calls in:

    ```ts
    if (!preserveRelatedState) {
      this.ensurePairwiseSelection();
      this.clearPairwiseComparisonState();
    }
    ```

    Add:

    ```ts
    private async ensureBranchBreakdowns(): Promise<void> {
      if (this.summary === null || this.summaryHasBranchBreakdowns) return;
      await this.fetchSummary({
        loadComparison: false,
        preserveRelatedState: true,
        params: { ...this.baseParams(), branchBreakdowns: true },
      });
    }
    ```

    Call `void this.ensureBranchBreakdowns()` from both group-by setters after
    synchronizing the two toggle values and saving them, but only when
    `g === "branch"`.

- [ ] **Step 6: Run the Usage store tests**

    Run from `frontend/`:

    ```bash
    npm test -- src/lib/stores/usage.test.ts
    ```

    Expected: PASS, including abort/version tests already in the file.

- [ ] **Step 7: Commit the lazy frontend fetch behavior**

    ```bash
    git add frontend/src/lib/stores/usage.svelte.ts \
      frontend/src/lib/stores/usage.test.ts
    git commit -m "perf: load usage branch data lazily"
    ```

______________________________________________________________________

### Task 6: Virtualize the attribution side rail and list

**Files:**

- Modify: `frontend/src/lib/components/usage/AttributionPanel.svelte`
- Test: `frontend/src/lib/components/usage/AttributionPanel.test.ts`

**Interfaces:**

- Consumes: `createVirtualizer(() => ElementOpts)` from
  `frontend/src/lib/virtual/createVirtualizer.svelte.ts`.

- Produces: separate fixed-row virtualizers for the treemap side rail and list
  view, each owning its current scroll element.

- Constants: `RAIL_ROW_HEIGHT = 24`, `LIST_ROW_HEIGHT = 42`, `overscan = 6`, and
  the existing `TREEMAP_MAX_TILES = 40` remains unchanged.

- [ ] **Step 1: Mock the virtualizer at the component boundary**

    Add this hoisted mock before importing `AttributionPanel` in its test file:

    ```ts
    vi.mock("../../virtual/createVirtualizer.svelte.js", () => ({
      createVirtualizer: (
        options: () => {
          count: number;
          estimateSize: (index: number) => number;
          getItemKey?: (index: number) => string;
        },
      ) => ({
        get instance() {
          const opts = options();
          const count = Math.min(opts.count, 12);
          const size = opts.estimateSize(0);
          return {
            getTotalSize: () => opts.count * size,
            getVirtualItems: () => Array.from({ length: count }, (_, index) => ({
              index,
              key: opts.getItemKey?.(index) ?? index,
              start: index * size,
              size,
              end: (index + 1) * size,
            })),
          };
        },
      }),
    }));
    ```

- [ ] **Step 2: Add large-list rendering tests**

    Create 1,000 literal branch totals with stable project keys and distinct
    branch names. In list view, assert exactly 12 `.list-row` elements render,
    `.list-virtual-spacer` has height `42000px`, and clicking a visible row
    passes its exact project-key token to `usage.toggleBranch`. In treemap view,
    assert the SVG still has at most 40 `.tile` groups, the rail renders 12
    `.rail-row` elements, and `.rail-virtual-spacer` has height `24000px`.

- [ ] **Step 3: Run the component test and verify it fails**

    Run from `frontend/`:

    ```bash
    npm test -- src/lib/components/usage/AttributionPanel.test.ts
    ```

    Expected: all 1,000 eager rows render and no virtual spacer exists.

- [ ] **Step 4: Add the two fixed-height virtualizers**

    In `AttributionPanel.svelte`, import the wrapper and add:

    ```ts
    import { createVirtualizer } from "../../virtual/createVirtualizer.svelte.js";

    const RAIL_ROW_HEIGHT = 24;
    const LIST_ROW_HEIGHT = 42;
    let railScrollElement: HTMLDivElement | undefined = $state();
    let listScrollElement: HTMLDivElement | undefined = $state();

    const railVirtualizer = createVirtualizer(() => ({
      count: rows.length,
      getScrollElement: () => railScrollElement ?? null,
      estimateSize: () => RAIL_ROW_HEIGHT,
      overscan: 6,
      getItemKey: (index) => rows[index]?.id ?? index,
    }));

    const listVirtualizer = createVirtualizer(() => ({
      count: rows.length,
      getScrollElement: () => listScrollElement ?? null,
      estimateSize: () => LIST_ROW_HEIGHT,
      overscan: 6,
      getItemKey: (index) => rows[index]?.id ?? index,
    }));
    ```

- [ ] **Step 5: Replace eager loops with virtual spacers**

    Bind each existing scroll container, add an inner relative spacer using
    `getTotalSize()`, naming the spacers `.rail-virtual-spacer` and
    `.list-virtual-spacer`, and loop over `getVirtualItems()`. Resolve the real
    row with `rows[virtualRow.index]`, keep the existing title/click/copy
    markup, and place each rendered row with:

    ```svelte
    style="position: absolute; top: 0; left: 0; width: 100%;
           height: {virtualRow.size}px;
           transform: translateY({virtualRow.start}px);"
    ```

    Use `virtualRow.index + 1` for rank text so scrolling does not renumber rows.

- [ ] **Step 6: Make both views bounded scroll regions without changing their
  visual vocabulary**

    Keep the side rail at `max-height: 280px`. Give `.list-view` a bounded
    `max-height: 420px` and `overflow-y: auto`. Set `.rail-row` and `.list-row`
    to `box-sizing: border-box` and their respective fixed heights. Do not add
    new controls, copy, custom scrollbar styling, animation, or color rules.

- [ ] **Step 7: Run the attribution component tests**

    Run from `frontend/`:

    ```bash
    npm test -- src/lib/components/usage/AttributionPanel.test.ts
    ```

    Expected: PASS for both existing interaction/copy tests and the new 1,000-row
    virtualization cases.

- [ ] **Step 8: Commit the virtualization change**

    ```bash
    git add frontend/src/lib/components/usage/AttributionPanel.svelte \
      frontend/src/lib/components/usage/AttributionPanel.test.ts
    git commit -m "perf: virtualize usage attribution rows"
    ```

______________________________________________________________________

### Task 7: Correct the branch API documentation and verify the branch

**Files:**

- Modify: `docs/session-api.md`
- Verify: all files changed by Tasks 1 through 6

**Interfaces:**

- Documents: `GET /api/v1/branches` with `scope=roots|all`, repeated `projects`,
  case-insensitive `search`, `limit` from 1 through 100, unique branch-name
  rows, and `has_more`.

- Removes: instructions to obtain or replay opaque project-qualified tokens from
  this endpoint.

- [ ] **Step 1: Replace the stale branch endpoint section**

    Replace the current token-based text and JSON with this contract:

    ````markdown
    `GET /api/v1/branches` returns a bounded list of distinct branch names for
    filter pickers. Results are sorted by session count and then branch name.
    The search is case-insensitive and matches branch-name substrings.

    | Query parameter | Meaning |
    |-----------------|---------|
    | `search` | Optional case-insensitive branch-name substring |
    | `projects` | Optional repeated project filter applied before branch-name deduplication |
    | `scope` | `roots` by default; `all` also includes subagent and fork sessions |
    | `limit` | Maximum branch names, from 1 through 100; default 100 |

    ```json
    {
        "branches": [
            {
                "branch": "main",
                "session_count": 42
            }
        ],
        "has_more": false
    }
    ```

    `has_more` is true when additional matching branch names exist beyond the
    requested limit. Branch-aware endpoints accept branch names directly in
    `git_branch`; clients do not obtain opaque filter tokens from this metadata
    endpoint.
    ````

    Update the later `--branch` table row so it says the CLI requires `--project`,
    while direct HTTP callers may pass branch names and the service applies any
    separately supplied project scope.

- [ ] **Step 2: Format the Markdown**

    Run:

    ```bash
    mdformat --wrap 80 docs/session-api.md \
      docs/superpowers/plans/2026-07-21-branch-review-followups.md
    ```

    Expected: exit 0. If `mdformat` or `mdformat-tables` is unavailable, leave the
    hand-wrapped Markdown unchanged and record that limitation in the handoff.

- [ ] **Step 3: Run Go formatting and static checks**

    Run:

    ```bash
    go fmt ./...
    go vet ./...
    ```

    Expected: both exit 0, with no uncommitted formatting surprises outside the
    files in this plan.

- [ ] **Step 4: Run the full Go and backend parity suites**

    Run:

    ```bash
    make test
    make test-postgres
    ```

    Expected: PASS. If this task started the repository's test PostgreSQL
    container, stop that exact container with `make postgres-down` after the
    run.

- [ ] **Step 5: Run frontend generation and verification**

    Run from `frontend/`:

    ```bash
    npm run generate:api
    npm run i18n:compile
    npm run check:kit-ui
    npm run check
    npm test
    ```

    Expected: API generation produces no additional diff; kit-ui check reports 0
    findings; Svelte check reports 0 errors; all frontend tests pass. Existing
    unrelated unused-CSS warnings may remain if their count and locations are
    unchanged.

- [ ] **Step 6: Re-run the affected benchmark gate locally**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db \
      -run '^$' -bench '^BenchmarkGetDailyUsage$' -benchmem \
      -count 6 -benchtime 20x
    ```

    Expected: ordinary `Breakdowns: true, BranchBreakdowns: false` allocations do
    not regress from the established roughly 143 MiB/op local baseline. Record
    the median `B/op` and `allocs/op` in the handoff.

- [ ] **Step 7: Inspect the final diff and private-data hygiene**

    Run:

    ```bash
    git diff --check
    git status --short
    git diff origin/main...HEAD --stat
    rg -n '/Users/|/home/|localhost:[0-9]+|generated by|Co-Authored-By' \
      docs/session-api.md internal/activity internal/db/branch_filter_test.go \
      internal/service/usage_test.go internal/postgres/usage_pgtest_test.go \
      internal/duckdb/store_test.go frontend/src/lib/components/usage \
      frontend/src/lib/components/activity frontend/src/lib/stores/usage.test.ts
    ```

    Expected: `git diff --check` is clean; only intended files are modified; any
    path-like strings are controlled test fixtures already permitted by existing
    project tests, not personal paths or infrastructure details introduced by
    this work.

- [ ] **Step 8: Commit documentation and any verification-only formatting**

    ```bash
    git add docs/session-api.md
    git commit -m "docs: correct branch search contract"
    ```

    If `go fmt ./...`, API regeneration, or Markdown formatting changed tracked
    implementation files, include only the files belonging to this plan in the
    same commit and explain the mechanical formatting in the final handoff. Do
    not create an empty commit.

- [ ] **Step 9: Final branch state check**

    Run:

    ```bash
    git status --short --branch
    git log --oneline --decorate -8
    ```

    Expected: a clean worktree on `feat/branch-filtering`, ahead of the remote by
    the design, plan, and implementation commits. Do not push until the user
    asks for or confirms the publish step.

______________________________________________________________________

## Self-Review Results

- **Spec coverage:** Tasks 1, 2, and 4 cover project-key identity from raw
  backend rows through rendered frontend tokens. Task 2 covers exact service
  resolution and unknown keys. Tasks 3 and 5 cover default omission, opt-in
  aggregation, and retained branch-rich state. Task 6 covers both virtualized
  row surfaces while preserving the 40-tile treemap cap. Task 7 corrects the
  metadata documentation and runs parity, frontend, benchmark, and hygiene
  verification.
- **No compatibility scaffolding:** The plan keeps current plain and raw
  qualified filters, but adds no new endpoint, version field, response alias,
  or dual path.
- **Placeholder scan:** The plan contains no placeholder markers, deferred
  implementation, unnamed error handling, or references to undefined
  interfaces.
- **Type consistency:** `project_key` is spelled identically in Go JSON tags,
  generated models, hand-written TypeScript types, and component fixtures.
  `BranchBreakdowns` maps to `branch_breakdowns` and generated
  `branchBreakdowns`. Frontend branch tokens consistently use
  `(project_key, branch)` while labels use `(project, branch)`.
- **Test quality:** New tests assert returned rows, totals, outgoing request
  parameters, rendered DOM counts, exact click tokens, and stable errors. None
  inspect implementation source or re-test TanStack, Huma, JSON, or Svelte
  behavior in isolation.
