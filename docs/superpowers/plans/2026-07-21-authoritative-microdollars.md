# Authoritative Microdollars Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace every floating-point monetary value with exact signed `int64`
microdollars while preserving ordinary dollar rendering in human-facing CLI and
UI output.

**Architecture:** Add one shared `internal/money.Money` value object and make it
the only monetary domain type. Convert source decimals at ingestion, migrate
SQLite and PostgreSQL forward to integer columns, rebuild DuckDB at schema v4,
and replace machine JSON money with `{"microdollars": int64}` objects. All
calculation, sorting, comparison, hashing, and synchronization use integer
values; only dimensionless ratios remain floating point.

**Tech Stack:** Go 1.24, SQLite/FTS5, PostgreSQL/pgx, DuckDB, Svelte 5,
TypeScript, Huma/OpenAPI, testify, Vitest, and Playwright.

## Global Constraints

- One US dollar equals exactly 1,000,000 microdollars.
- `Money` serializes only as `{"microdollars": int64}`.
- CLI tables and UI labels render ordinary dollars, never raw microdollars.
- Round imported or calculated values to the nearest microdollar with exact
  halves away from zero.
- Round computed pricing once per independently priced usage row after summing
  its unrounded token-category products.
- Reject malformed, non-finite, negative source money, and overflow.
- Do not retain float money fields, legacy aliases, dual reads, dual writes, or
  compatibility adapters.
- Preserve SQLite/PostgreSQL behavior and query-shape parity.
- DuckDB is rebuilt through schema version 4; it receives no in-place migration.
- Use testify assertions and behavior-focused tests.
- Run `go fmt ./...` and `go vet ./...` after Go changes.

______________________________________________________________________

### Task 1: Exact Money Value and Arithmetic

**Files:**

- Create: `internal/money/money.go`
- Create: `internal/money/decimal.go`
- Create: `internal/money/format.go`
- Create: `internal/money/money_test.go`
- Create: `internal/money/decimal_test.go`
- Create: `internal/money/format_test.go`

**Interfaces:**

- Produces: `money.Money{Microdollars int64}`.

- Produces: `money.ParseScaledDecimal(string, uint) (int64, error)`.

- Produces: `money.ParseDollars(string) (Money, error)` and
  `money.ParseCents(string) (Money, error)`.

- Produces: `money.Add`, `money.Sub`, `money.Sum`, and `money.CostPerMillion`
  checked arithmetic.

- Produces: `money.FormatUSD(Money, money.DisplayPrecision) string`.

- [ ] **Step 1: Write failing behavior tests**

    Cover literal expected values for zero, signed values, exponents, half-away
    rounding, malformed numbers, `int64` boundaries, checked sum overflow,
    128-bit token-rate multiplication, combined-component rounding, and
    `math.MinInt64` formatting. A representative cost assertion is:

    ```go
    got, err := money.CostPerMillion([]money.RatedTokens{
        {Tokens: 1, Rate: money.Money{Microdollars: 500_000}},
        {Tokens: 1, Rate: money.Money{Microdollars: 500_000}},
    })
    require.NoError(t, err)
    assert.Equal(t, money.Money{Microdollars: 1}, got)
    ```

- [ ] **Step 2: Run the new tests and verify RED**

    Run:

    ```bash
    go test ./internal/money -run 'Test(Parse|Cost|Add|Format)' -count=1
    ```

    Expected: compilation fails because `internal/money` does not yet exist.

- [ ] **Step 3: Implement the minimal money package**

    Use checked base-10 digit accumulation for decimal parsing. Use
    `math/bits.Mul64`, `bits.Add64`, and `bits.Div64` for nonnegative pricing
    products, rejecting a quotient outside `int64`. Do not use `float64`,
    `math/big`, or decimal dependencies. `FormatUSD` must divide the magnitude
    as unsigned arithmetic so `math.MinInt64` is safe.

- [ ] **Step 4: Run focused and package tests and verify GREEN**

    ```bash
    go test ./internal/money -count=1
    ```

    Expected: PASS.

- [ ] **Step 5: Commit the focused money package**

    ```bash
    git add internal/money
    git commit -m "feat: add exact microdollar money"
    ```

### Task 2: Source, Catalog, and Configuration Boundaries

**Files:**

- Modify: `internal/parser/types.go`
- Modify: `internal/parser/grok.go`
- Modify: `internal/parser/hermes.go`
- Modify: `internal/parser/roocode.go`
- Modify: `internal/parser/shelley.go`
- Modify: `internal/parser/vibe.go`
- Modify: relevant parser `*_test.go` files returned by
  `rg -l 'CostUSD|TotalCost' internal/parser`.
- Modify: `internal/cursorusage/client.go`
- Modify: `internal/cursorusage/client_test.go`
- Modify: `internal/pricing/catalog/litellm.go`
- Modify: `internal/pricing/litellm_test.go`
- Modify: `internal/pricing/fallback.go`
- Modify: `internal/pricing/fallback_test.go`
- Modify: `internal/pricing/cmd/litellm-snapshot/main.go`
- Modify: `internal/pricing/cmd/litellm-snapshot/main_test.go`
- Modify: `internal/config/config.go`
- Modify: custom-pricing config tests under `internal/config` and
  `cmd/agentsview`.

**Interfaces:**

- Consumes: Task 1 `money.Money` and decimal parsers.

- Produces: `parser.ParsedUsageEvent.Cost *money.Money`.

- Produces: Cursor `Charged` and `CursorTokenFee` as `money.Money`.

- Produces: catalog and config rates as `money.Money` per million tokens.

- [ ] **Step 1: Replace parser expectations with exact Money values**

    Update parser fixtures to assert literals such as:

    ```go
    require.NotNil(t, event.Cost)
    assert.Equal(t, money.Money{Microdollars: 42_413}, *event.Cost)
    ```

    Include explicit zero versus absent cost and Grok tick rounding.

- [ ] **Step 2: Run parser tests and verify RED**

    ```bash
    go test ./internal/parser ./internal/cursorusage ./internal/pricing/... -count=1
    ```

    Expected: compilation failures on the old float fields.

- [ ] **Step 3: Convert input boundaries**

    Rename `CostUSD` to `Cost`, decode JSON decimals as `json.Number` or raw JSON,
    convert Grok ticks with integer arithmetic, and convert unavoidable upstream
    SQLite `REAL` values immediately with the one explicitly named boundary
    converter. Change Cursor cents and LiteLLM per-token decimals to exact
    scaled parsing. Replace custom rate keys with `*_microdollars_per_mtok`
    integers.

- [ ] **Step 4: Regenerate the embedded pricing snapshot**

    Run the repository's existing pricing snapshot generator so the embedded
    snapshot stores integer rate values and its version/digest reflects the new
    canonical representation.

- [ ] **Step 5: Run focused tests and verify GREEN**

    ```bash
    go test ./internal/parser ./internal/cursorusage ./internal/pricing/... ./internal/config -count=1
    ```

    Expected: PASS.

### Task 3: SQLite Integer Schema and Forward Migration

**Files:**

- Modify: `internal/db/schema.sql`
- Modify: `internal/db/db.go`
- Create: `internal/db/money_migration.go`
- Create: `internal/db/money_migration_test.go`
- Modify: `internal/db/legacy_schema_test.go`
- Modify: `internal/db/usage_events.go`
- Modify: `internal/db/cursor_usage_events.go`
- Modify: `internal/db/pricing.go`
- Modify: `internal/db/pricing_list.go`
- Modify: `internal/db/orphaned.go`
- Modify: `internal/db/db_test.go`
- Modify: `internal/db/pricing_test.go`
- Modify: `internal/db/usage_test.go`

**Interfaces:**

- Consumes: Task 1 `Money`, Task 2 parsed and catalog money.

- Produces: final SQLite columns named in the design specification.

- Produces: one idempotent transactional forward migration from the released
  float schema.

- Produces: `db.UsageEvent.Cost *money.Money`, integer `ModelPricing` rates, and
  integer `CursorUsageEvent` money.

- [ ] **Step 1: Write the legacy migration test**

    Create a released-schema database with explicit zero, null cost, fractional
    reported cost, fractional Cursor cents, pricing rates, preserved IDs, and
    deduplication keys. Open it through `db.Open` and assert exact integer
    values, final declared column types, preserved rows/indexes, and no old
    money columns.

- [ ] **Step 2: Verify the migration test fails**

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db -run 'TestMoneyMigration' -count=1
    ```

    Expected: FAIL because the new integer columns and migration are absent.

- [ ] **Step 3: Implement fresh schemas and the transactional migration**

    Rebuild `usage_events`, `cursor_usage_events`, and `model_pricing` inside one
    SQLite transaction. Preflight invalid/range values, copy with nearest-micro
    rounding, preserve keys and foreign keys, replace the tables, and recreate
    their indexes. Detect fresh, released, completed, and invalid mixed schemas
    explicitly.

- [ ] **Step 4: Convert SQLite reads, writes, copies, and fingerprints**

    Bind and scan `int64` microdollars. Remove `sql.NullFloat64`, `/ 100.0`,
    `strconv.FormatFloat`, and float pricing comparisons from owned money paths.

- [ ] **Step 5: Run SQLite tests and verify GREEN**

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db -count=1
    ```

    Expected: PASS.

### Task 4: PostgreSQL Integer Schema and Push Parity

**Files:**

- Modify: `internal/postgres/schema.go`
- Modify: `internal/postgres/pricing.go`
- Modify: `internal/postgres/push.go`
- Modify: `internal/postgres/push_fingerprint.go`
- Modify: `internal/postgres/usage.go`
- Modify: `internal/postgres/activityreport.go`
- Modify: `internal/postgres/schema_test.go`
- Modify: `internal/postgres/pricing_unit_test.go`
- Modify: `internal/postgres/push_test.go`
- Modify: all affected `internal/postgres/*_pgtest_test.go` files.

**Interfaces:**

- Consumes: final SQLite money records.

- Produces: PostgreSQL `BIGINT` money columns with names and semantics identical
  to SQLite.

- Produces: transactional legacy `DOUBLE PRECISION` to `BIGINT` migration.

- [ ] **Step 1: Write failing PostgreSQL schema and parity tests**

    Seed the released float schema, run `EnsureSchema`, and assert exact converted
    values, final `BIGINT` types, old-column removal, preserved deduplication,
    and parity with the SQLite fixture.

- [ ] **Step 2: Verify RED with the real PostgreSQL suite**

    ```bash
    make test-postgres
    ```

    Expected: new schema/migration assertions fail.

- [ ] **Step 3: Implement the PostgreSQL forward migration**

    Run it before queries expect new names. Preflight `NaN`, infinities,
    negatives, and scaled range. Convert and rename in one transaction; reject
    mixed schemas. Update core DDL for fresh databases.

- [ ] **Step 4: Convert pricing, push, fingerprint, usage, and activity paths**

    Replace double casts, nullable floats, float expressions, and cents division
    with `BIGINT` values and shared Go integer calculation.

- [ ] **Step 5: Run PostgreSQL unit and integration tests and verify GREEN**

    ```bash
    go test ./internal/postgres -count=1
    make test-postgres
    ```

    Expected: PASS.

### Task 5: DuckDB v4 Rebuild and Integer Analytics

**Files:**

- Modify: `internal/duckdb/schema.go`
- Modify: `internal/duckdb/push.go`
- Modify: `internal/duckdb/analytics_usage.go`
- Modify: `internal/duckdb/activityreport.go`
- Modify: `internal/duckdb/schema_test.go`
- Modify: `internal/duckdb/rebuild_test.go`
- Modify: `internal/duckdb/sync_test.go`
- Modify: `internal/duckdb/store_test.go`
- Modify: `internal/duckdb/analytics_usage_test.go`
- Modify: `internal/duckdb/activityreport_test.go`

**Interfaces:**

- Consumes: SQLite integer money.

- Produces: DuckDB schema version 4 with `BIGINT` money.

- Produces: Quack results using the same `money.Money` contracts as SQLite and
  PostgreSQL.

- [ ] **Step 1: Write failing v4 rebuild and integer parity tests**

    Assert a v3 mirror is rebuilt, integer values are pushed exactly, existing
    non-AgentsView files still fail closed, and usage/activity results match the
    SQLite fixture.

- [ ] **Step 2: Verify RED**

    ```bash
    go test ./internal/duckdb -count=1
    ```

    Expected: failures on schema version and float column/value assertions.

- [ ] **Step 3: Implement schema v4 and integer push/analytics**

    Change only create-time schema, bump `SchemaVersion` to 4, remove float cost
    SQL and `roundCost`, and use shared integer row pricing where SQL cannot
    preserve the exact rounding contract.

- [ ] **Step 4: Run DuckDB tests and verify GREEN**

    ```bash
    go test ./internal/duckdb -count=1
    ```

    Expected: PASS.

### Task 6: Shared Usage, Activity, Export, and Insight Domain Types

**Files:**

- Modify: `internal/export/pricing.go`
- Modify: `internal/export/types.go`
- Modify: `internal/export/canonical_json.go`
- Modify: `internal/db/usage.go`
- Modify: `internal/db/activityreport.go`
- Modify: `internal/db/session_export.go`
- Modify: `internal/db/session_stats.go`
- Modify: `internal/db/session_stats_types.go`
- Modify: `internal/activity/activity.go`
- Modify: `internal/service/usage.go`
- Modify: `internal/service/session_usage_rollup.go`
- Modify: `internal/insight/canned.go`
- Modify: `internal/insight/summary.go`
- Modify: `internal/insight/prompt.go`
- Modify: all colocated affected Go tests.

**Interfaces:**

- Consumes: backend `Money` rows and integer model rates.

- Produces: all monetary fields as `money.Money` or `*money.Money`.

- Produces: dimensionless ratios as floats calculated from microdollar integers.

- Produces: usage/activity/session export schema version increments.

- [ ] **Step 1: Change behavior tests to literal Money values**

    Replace epsilon assertions with exact equality. Add a fixture whose sub-micro
    components round only after their row sum and assert breakdowns sum exactly
    to totals.

- [ ] **Step 2: Verify RED across shared packages**

    ```bash
    go test ./internal/export ./internal/activity ./internal/service ./internal/insight -count=1
    ```

- [ ] **Step 3: Replace float aggregation and domain fields**

    Use checked money sums for daily, project, model, agent, session, rollup,
    savings, activity, and insight totals. Cost-per-session is a rounded `Money`
    division; only `*Ratio` fields remain floats. Canonical pricing JSON hashes
    integer money objects.

- [ ] **Step 4: Run shared package tests and verify GREEN**

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/export ./internal/activity ./internal/service ./internal/insight ./internal/db -count=1
    ```

### Task 7: REST, CLI JSON, and Human Dollar Output

**Files:**

- Modify: `internal/server/huma_routes_sessions.go`
- Modify: `internal/server/huma_routes_usage.go`
- Modify: `internal/server/usage.go`
- Modify: `internal/server/insights.go`
- Modify: affected `internal/server/*_test.go` files.
- Modify: `cmd/agentsview/session_usage.go`
- Modify: `cmd/agentsview/usage.go`
- Modify: `cmd/agentsview/usage_cursor.go`
- Modify: `cmd/agentsview/stats.go`
- Modify: affected `cmd/agentsview/*_test.go` files.
- Modify: `cmd/testfixture/main.go`

**Interfaces:**

- Consumes: Task 6 Money response types.

- Produces: OpenAPI money schema `{microdollars: int64}`.

- Produces: machine JSON with no float monetary fields.

- Produces: unchanged human-facing dollar conventions from integer formatters.

- [ ] **Step 1: Write failing HTTP and CLI contract tests**

    Decode JSON into maps and assert each money member equals exactly
    `map[string]any{"microdollars": float64(420000)}` at the generic decoder
    boundary, while old `cost_usd` fields are absent. Assert human output
    remains `$0.42`, `<$0.01`, and grouped-dollar output.

- [ ] **Step 2: Verify RED**

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/server ./cmd/agentsview -count=1
    ```

- [ ] **Step 3: Replace transport DTOs and formatters**

    Rename semantic parents such as `cost_usd` to `cost` and `rollup_cost_usd` to
    `rollup_cost`; keep established casing on each surface. Use integer request
    values where cost is an input. Generate human dollar text directly from
    `Money`.

- [ ] **Step 4: Regenerate OpenAPI clients**

    Run the existing frontend API generation command and verify one reusable
    generated `Money` model is referenced by every monetary response field.

- [ ] **Step 5: Run HTTP and CLI tests and verify GREEN**

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/server ./cmd/agentsview -count=1
    ```

### Task 8: Frontend Integer Money and Dollar Presentation

**Files:**

- Modify: `frontend/src/lib/api/types/usage.ts`
- Modify: generated models under `frontend/src/lib/api/generated/models/`.
- Create: `frontend/src/lib/utils/money.ts`
- Create: `frontend/src/lib/utils/money.test.ts`
- Modify: `frontend/src/lib/stores/usage.svelte.ts`
- Modify: `frontend/src/lib/stores/usage.test.ts`
- Modify: usage components under `frontend/src/lib/components/usage/`.
- Modify: activity components under `frontend/src/lib/components/activity/`.
- Modify: `frontend/src/lib/components/layout/SessionBreadcrumb.svelte`
- Modify: affected colocated frontend tests and `frontend/e2e/usage.spec.ts`.

**Interfaces:**

- Consumes: `{microdollars: number}` Money API objects.

- Produces: integer add/subtract/compare helpers and dimensionless ratio
  helpers.

- Produces: `formatMoneyUSD(Money)` for all human cost labels.

- [ ] **Step 1: Write failing frontend money tests**

    Cover exact integer addition/comparison, negative values, `$0.42`, `<$0.01`,
    `$1,234.56`, and preservation of existing chart/table display precision.

- [ ] **Step 2: Verify RED**

    ```bash
    cd frontend && npm test -- --run src/lib/utils/money.test.ts
    ```

- [ ] **Step 3: Convert frontend domain calculations**

    Replace scalar money numbers with `Money` objects. Sort and aggregate by
    `.microdollars`; convert to chart-axis dollar numbers only inside visual
    presentation adapters. Keep ratios numeric.

- [ ] **Step 4: Convert all human renderers**

    Route summary cards, charts, attribution, pairwise comparisons, activity
    tables, top sessions, and session breadcrumbs through `formatMoneyUSD`.

- [ ] **Step 5: Run frontend tests and checks and verify GREEN**

    ```bash
    cd frontend && npm test -- --run && npm run check
    ```

    Expected: PASS with no new warnings.

### Task 9: Documentation and Contract Examples

**Files:**

- Modify: `README.md`
- Modify: `docs/token-usage.md`
- Modify: `docs/session-api.md`
- Modify: `docs/session-export.md`
- Modify: `docs/commands.md`
- Modify: `docs/configuration.md`
- Modify: any additional documentation returned by
  `rg -l 'cost_usd|totalCost|input_cost_per_mtok|chargedCents' README.md docs`.

**Interfaces:**

- Consumes: final API, CLI, and configuration names.

- Produces: examples with only `{"microdollars": ...}` machine money and normal
  dollar human output.

- [ ] **Step 1: Update documentation examples and prose**

    Remove float monetary JSON examples, document integer custom-pricing keys,
    explain microdollar rounding/range, and retain `$` examples for UI and human
    CLI output.

- [ ] **Step 2: Format and inspect documentation**

    ```bash
    mdformat --wrap 80 README.md docs/token-usage.md docs/session-api.md docs/session-export.md docs/commands.md docs/configuration.md
    rg -n 'cost_usd|charged_cents|input_per_mtok' README.md docs internal cmd frontend/src
    ```

    Expected: remaining matches are explicitly nonmonetary, upstream source names,
    or migration fixtures; no old public or storage contract remains.

### Task 10: Full Verification and Final Commit

**Files:**

- Verify all changed files from Tasks 1-9.

**Interfaces:**

- Produces: one coherent implementation with no float monetary authority.

- [ ] **Step 1: Run formatting and static checks**

    ```bash
    go fmt ./...
    go vet ./...
    make lint
    cd frontend && npm run check
    ```

- [ ] **Step 2: Run backend and frontend suites**

    ```bash
    make test
    make test-postgres
    go test ./internal/duckdb -count=1
    cd frontend && npm test -- --run
    ```

- [ ] **Step 3: Run relevant end-to-end coverage**

    ```bash
    cd frontend && npx playwright test e2e/usage.spec.ts
    ```

- [ ] **Step 4: Audit monetary floats and legacy names**

    ```bash
    rg -n --glob '!**/*_test.go' --glob '!docs/superpowers/**' \
      'CostUSD|cost_usd|charged_cents|input_per_mtok|TotalCost[[:space:]]+float64|Cost[[:space:]]+float64' \
      internal cmd frontend/src
    ```

    Inspect every remaining match. Only upstream wire names, migration fixtures,
    or nonmonetary dimensionless values may remain.

- [ ] **Step 5: Review the final diff and commit**

    ```bash
    git status --short
    git diff --stat
    git diff --check
    git add cmd frontend internal README.md docs
    git commit -m "refactor: make microdollars authoritative"
    ```
