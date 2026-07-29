# Request Pricing Bands Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans
> to implement this plan directly in the current agent, task-by-task. Never use
> subagent-driven development. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Preserve LiteLLM whole-request context pricing bands and apply the
correct band to each normalized usage request before aggregation.

**Approved spec/design:**
`docs/superpowers/specs/2026-07-29-request-pricing-bands-design.md`

**Architecture:** The LiteLLM adapter converts anchored threshold field names
into complete provider-neutral rate bands. Bands travel with base rates through
fallback snapshots and SQLite/PostgreSQL/DuckDB catalogs. `export.ModelRates`
owns exclusive per-request selection for every usage surface.

**Tech Stack:** Go, exact microdollar arithmetic, SQLite, PostgreSQL, DuckDB,
testify.

## Global Constraints

- Recognize only `input_cost_per_token_above_<N>k_tokens` and
  `input_cost_per_token_above_<N>_tokens` at the LiteLLM boundary.
- Select the highest band where total request input (uncached + cache read +
  cache creation) is strictly greater than `AboveInputTokens`.
- Treat message rows and ordinal-bound usage events as request-scoped. Force
  unbound aggregate/unknown usage events to base pricing.
- Missing companion rates inherit base; explicit zero remains zero.
- Ignore Priority/Flex/Batch/regional/one-hour fields, `tiered_pricing`, model
  names, and `max_input_tokens` as threshold signals.
- Custom rates stay flat and suppress catalog bands; reported costs remain
  authoritative.
- Fallback artifacts are generated from, embed, and validate one immutable
  LiteLLM commit. Read-only offline fallback rows retain bands; actual custom
  overrides remain flat.
- SQLite/PostgreSQL behavior must match. DuckDB uses a schema-version rebuild,
  never an in-place migration.
- Add one conceptual forward catalog-schema change; do not edit shipped
  migrations or add compatibility scaffolding.
- Follow strict red/green TDD with literal testify expectations.

______________________________________________________________________

### Task 1: Normalize LiteLLM bands and snapshot transport

**Files:**

- Modify: `internal/pricing/catalog/litellm.go`
- Modify: `internal/pricing/catalog/litellm_test.go`
- Modify: `internal/pricing/litellm.go`
- Modify: `internal/pricing/fallback.go`
- Modify: `internal/pricing/fallback_test.go`
- Modify: `internal/pricing/cmd/litellm-snapshot/main.go`
- Modify: `internal/pricing/cmd/litellm-snapshot/main_test.go`

**Interfaces:** Produces `catalog.PricingBand`, its `pricing.PricingBand` alias,
and `ModelPricing.Bands []PricingBand` sorted ascending.

- [ ] **Step 1: Write failing parsing tests**

Use hand-written 200K and 272K JSON entries. Assert literal rate tuples, nonzero
inherited cache creation, explicit cache-read zero, both threshold spellings,
ascending multiple bands, zero/overflow/duplicate-threshold errors, and ignored
`_priority`, `_batch`, one-hour, and `tiered_pricing` fields.

```go
type PricingBand struct {
    AboveInputTokens     int         `json:"above_input_tokens"`
    InputPerMTok         money.Money `json:"input_per_mtok"`
    OutputPerMTok        money.Money `json:"output_per_mtok"`
    CacheCreationPerMTok money.Money `json:"cache_creation_per_mtok"`
    CacheReadPerMTok     money.Money `json:"cache_read_per_mtok"`
}
```

- [ ] **Step 2: Verify RED**

```bash
go test ./internal/pricing/catalog -run 'TestParseLiteLLMPricing.*Band' -count=1
```

Expected: missing `PricingBand`/`Bands` compilation failure.

- [ ] **Step 3: Implement adapter parsing**

Decode each model object as raw fields, match only:

```go
regexp.MustCompile(`^input_cost_per_token_above_([0-9]+)(k?)_tokens$`)
```

Use checked `int` conversion and checked `k * 1000`, construct companion keys
from the exact matched suffix, ignore a null anchor, treat a null companion as
missing/inherited, preserve numeric zero, reject duplicate normalized
thresholds, inherit missing components, and sort bands.

- [ ] **Step 4: Verify GREEN**

```bash
go test ./internal/pricing/catalog -run 'TestParseLiteLLMPricing' -count=1
```

- [ ] **Step 5: Write failing fallback tests**

Using in-memory gzip fixtures, assert band decode/sort, absent-band backward
compatibility, positive/unique validation, and nested-slice caller safety from
`FallbackPricing()`.

```bash
go test ./internal/pricing ./internal/pricing/cmd/litellm-snapshot -run 'Test.*(Fallback|Snapshot).*Band' -count=1
```

Expected: band validation/deep cloning assertions fail.

- [ ] **Step 6: Implement fallback validation/deep cloning and verify**

```bash
go test ./internal/pricing/... -count=1
```

Transport is first protected with synthetic snapshot data; do not hard-code
model overlays for threshold pricing.

- [ ] **Step 7: Publish and pin a band-bearing fallback artifact**

This step requires the user's explicit authorization to push the dedicated
artifact branch. Generate from the immutable LiteLLM commit recorded as format
evidence, embed that full commit in the bundle, and reject validation when it is
absent or mutable:

```bash
artifact_dir=$(mktemp -d /tmp/agentsview-pricing-artifact.XXXXXX)
go run ./internal/pricing/cmd/litellm-snapshot \
  -litellm-ref 551e5d097c11f08fd2400a25a651b1844fcf89c2 \
  -out "$artifact_dir/litellm_snapshot.json.gz"
go run ./internal/pricing/cmd/litellm-snapshot -validate "$artifact_dir/litellm_snapshot.json.gz"
shasum -a 256 "$artifact_dir/litellm_snapshot.json.gz"
```

Use a separate temporary worktree for `origin/litellm-pricing-snapshot`, copy
only the generated gzip into it, use the mandatory commit skill, and push that
artifact commit to the dedicated remote branch. Update `defaultSnapshotRef` and
`defaultSnapshotSHA256` to the published commit/hash, then remove the local
ignored snapshot and prove the normal restore path fetches and validates the new
artifact:

```bash
go run ./internal/pricing/cmd/litellm-snapshot -restore
go test ./internal/pricing/... -count=1
```

- [ ] **Step 8: Commit the catalog checkpoint**

Use the mandatory commit skill and commit only catalog/fallback changes.

______________________________________________________________________

### Task 2: Select bands per request and expose provenance

**Files:**

- Modify: `internal/export/pricing.go`
- Modify: `internal/export/pricing_test.go`
- Modify: `internal/export/types.go`
- Modify: `internal/export/types_test.go`
- Modify: `internal/export/canonical_json.go`
- Modify: `internal/export/canonical_json_test.go`

**Interfaces:** Produces `export.PricingBand`,
`ModelRates.RatesForTokens(input, cacheWrite, cacheRead int) ModelRates`, and
band-bearing `EffectiveModelRate` JSON/digests with base-request, aggregate-row,
and per-band request counts.

- [ ] **Step 1: Write failing cost-selection tests**

Test base at exactly 200,000 total input, band at 200,001, highest satisfied
band, and two separately priced 150K requests. Use base rates $1/$2/$0.50/$0.10
and band rates $2/$3/$1/$0.20; expected costs are 150,000 microdollars at the
boundary and 290,002 above it for 100K-ish input + 10K output + 50K/50K cache.

```bash
go test ./internal/export -run 'TestModelRates.*(Band|Request)' -count=1
```

Expected: missing band interface compilation failure.

- [ ] **Step 2: Implement centralized selection and verify**

```go
type PricingBand struct {
    AboveInputTokens  int         `json:"above_input_tokens"`
    InputPerMTok      money.Money `json:"input_cost_per_mtok"`
    OutputPerMTok     money.Money `json:"output_cost_per_mtok"`
    CacheWritePerMTok money.Money `json:"cache_write_cost_per_mtok"`
    CacheReadPerMTok  money.Money `json:"cache_read_cost_per_mtok"`
    UpdatedAt         *time.Time  `json:"-"`
}
```

`RatesForTokens` selects and returns a complete tuple; `CostForTokens` calls it
before billing output/reasoning/cache categories.

```bash
go test ./internal/export -run 'TestModelRates' -count=1
```

- [ ] **Step 3: Write failing provenance tests**

Assert `BuildBlock` emits bands, band timestamps participate in
`LatestRowUpdatedAt`, changing only a band changes the digest, and a mixed set
of base/banded/aggregate computations emits literal application counts. Assert
reported-only rows do not increment computed counts.

```bash
go test ./internal/export -run 'Test(PricingResolver|EffectivePricingDigest|PricingBlockJSONShape).*Band' -count=1
```

Expected: bands are absent from block/digest/timestamp.

- [ ] **Step 4: Implement provenance and verify**

Add deterministic threshold/rates/timestamp canonical JSON, copy bands into
`EffectiveModelRate`, record base/band/aggregate selections at row-pricing time,
and bump usage/activity/session export schema constants from 3 to 4.

```bash
go test ./internal/export -count=1
```

- [ ] **Step 5: Commit the cost-engine checkpoint**

Use the mandatory commit skill and commit only export/cost changes.

______________________________________________________________________

### Task 3: Persist and consume bands in SQLite

**Files:**

- Modify: `internal/db/schema.sql`
- Modify: `internal/db/db.go`
- Modify: `internal/db/pricing.go`
- Modify: `internal/db/pricing_list.go`
- Modify: `internal/db/pricing_test.go`
- Modify: `internal/db/db_test.go`
- Modify: `internal/db/read_only_test.go`
- Modify: `internal/db/usage.go`
- Modify: `internal/db/usage_test.go`
- Modify: `internal/db/activityreport_test.go`
- Modify: `internal/db/session_stats_test.go`
- Modify: `internal/pricingrefresh/refresh.go`
- Modify: `internal/pricingrefresh/refresh_test.go`
- Modify: `cmd/agentsview/usage.go`
- Modify: `cmd/agentsview/usage_test.go`

**Interfaces:** Produces `db.PricingBand`, nested `ModelPricing.Bands`, and
SQLite `model_pricing_bands` keyed by model/threshold.

- [ ] **Step 1: Write failing persistence and observable tests**

Test schema columns, sorted get/list, complete-set replacement/removal,
band-only change detection, insert-missing not contaminating an existing flat
row, full-resync copy, refresh conversion, flat custom suppression, source
classification as `fetched` when base rates match fallback but bands differ,
read-only offline fallback above a threshold and read-only upgrade diagnostics,
exact daily/session/activity 200,001 cost, separate 150K rows staying
base-priced, an unbound 300K session aggregate staying base-priced, an
ordinal-bound 300K event using the band, mixed application counts, and session
cache actual/counterfactual costs.

```bash
go test -tags fts5 ./internal/db ./internal/pricingrefresh -run 'Test.*(PricingBand|ModelPricing.*Band)' -count=1
```

Expected: missing types/table and flat-cost assertions fail.

- [ ] **Step 2: Add the forward SQLite schema**

```sql
CREATE TABLE IF NOT EXISTS model_pricing_bands (
    model_pattern TEXT NOT NULL
        REFERENCES model_pricing(model_pattern) ON DELETE CASCADE,
    above_input_tokens INTEGER NOT NULL CHECK (above_input_tokens > 0),
    input_microdollars_per_mtok INTEGER NOT NULL,
    output_microdollars_per_mtok INTEGER NOT NULL,
    cache_creation_microdollars_per_mtok INTEGER NOT NULL,
    cache_read_microdollars_per_mtok INTEGER NOT NULL,
    updated_at TEXT NOT NULL
        DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (model_pattern, above_input_tokens)
);
```

Add it to read-only required schema; do not bump parser `dataVersion`. An older
archive opened read-only returns the existing typed
`SchemaUpgradeRequiredError`; a writable open installs the table without a
parser resync.

- [ ] **Step 3: Implement atomic persistence/load/copy**

Compare sorted band tuples in `FilterChangedModelPricing`. In existing pricing
transactions delete each changed model's bands and insert its complete set.
Insert bands only for genuinely missing parents. Copy parents then children in
one attached transaction. Load timestamps/bands into `export.ModelRates` and map
catalog bands in pricing refresh/fallback conversion. Custom overlays replace
the entire rate object with nil bands. Embedded/fetched classification compares
the complete sorted bands as well as the four base rates. Pass deterministic row
scope (`message` or non-nil `message_ordinal`) into cost selection and
application-count recording; force other computed usage events to base rates.
Every changed band set, including removal of the final band, also advances the
parent row timestamp so catalog version metadata never moves backward. Replace
the read-only `SetEffectivePricing` flattening path with band-bearing effective
rows, and deep-clone nested slices at resolver/cache boundaries.

- [ ] **Step 4: Verify SQLite GREEN**

```bash
go test -tags fts5 ./internal/db ./internal/pricingrefresh -count=1
```

- [ ] **Step 5: Write failing cache-savings test**

For one banded row split across uncached/cache-read/cache-create, assert literal
cost and savings from the same selected tuple.

```bash
go test -tags fts5 ./internal/db -run 'TestGetDailyUsage.*PricingBandSavings' -count=1
```

Expected: cost uses band; savings still uses base rates.

- [ ] **Step 6: Select rates for savings and verify**

Use `selected := rates.RatesForTokens(inputTok, cacheCrTok, cacheRdTok)` for
cache deltas, keeping reported-cost branches unchanged.

```bash
go test -tags fts5 ./internal/db -count=1
```

- [ ] **Step 7: Commit SQLite persistence**

Commit the forward table, complete-set writes, copying, refresh conversion,
parent timestamp advancement, and read-only effective-rate transport after their
focused tests pass.

- [ ] **Step 8: Commit SQLite usage behavior**

Commit request-scope selection, application counts, and savings after their
focused tests pass. Together these are the SQLite part of the single conceptual
catalog-schema change.

______________________________________________________________________

### Task 4: Persist and consume bands in PostgreSQL

**Files:**

- Modify: `internal/postgres/schema.go`
- Modify: `internal/postgres/schema_test.go`
- Modify: `internal/postgres/schema_pgtest_test.go`
- Modify: `internal/postgres/pricing.go`
- Modify: `internal/postgres/pricing_unit_test.go`
- Modify: `internal/postgres/pricing_pgtest_test.go`
- Modify: `internal/postgres/push_test.go`
- Modify: `internal/postgres/push_pgtest_test.go`
- Modify: `internal/postgres/usage.go`
- Modify: `internal/postgres/usage_pgtest_test.go`
- Modify: `internal/postgres/activityreport.go`
- Modify: `internal/postgres/activityreport_pgtest_test.go`

**Interfaces:** PostgreSQL table/sync/load semantics match SQLite.

- [ ] **Step 1: Write failing PG schema/sync/parity tests**

Test required push and read schema (including a missing band table and each
missing required band column), SQL arguments, pushed band persistence/removal,
custom suppression, fetched classification for a band-only fallback mismatch,
exact daily/session/activity 200,001 cost/savings, separate 150K requests,
unbound aggregate fallback, ordinal-bound request selection, and matching
application counts. Run both unit and real-PG tests before implementation:

```bash
go test ./internal/postgres -run 'Test.*(PricingBand|ModelPricing.*Band)' -count=1
make test-postgres
```

Expected: missing table/load/sync failures.

- [ ] **Step 2: Add PG schema and atomic sync**

Use the SQLite columns with `BIGINT`, a foreign key to `model_pricing`, positive
threshold check, text `updated_at`, and the same composite primary key. Add the
table to push compatibility checks and `CheckSchemaCompat`'s read probes so
`pg serve` rejects old/incomplete schemas cleanly. Load nested bands;
delete/reinsert complete changed sets in the base upsert transaction; map
timestamps/fallback bands; compare complete bands for embedded/fetched source
classification; keep custom rows flat; use selected rates for PG savings. Mirror
SQLite's request-scope predicate and application-count recording. Advance the
parent timestamp for every band-set replacement/removal and deep-clone cached
band slices.

- [ ] **Step 3: Verify PG GREEN**

```bash
go test ./internal/postgres -count=1
make test-postgres
```

- [ ] **Step 4: Commit PostgreSQL schema/sync parity**

Commit schema compatibility, persistence, copying, classification, and parent
timestamp advancement after the schema/sync tests pass.

- [ ] **Step 5: Commit PostgreSQL usage parity**

Commit request-scope pricing, application counts, and savings after real-PG
behavior tests pass.

______________________________________________________________________

### Task 5: Rebuild and populate DuckDB bands

**Files:**

- Modify: `internal/duckdb/schema.go`
- Modify: `internal/duckdb/schema_test.go`
- Modify: `internal/duckdb/push.go`
- Modify: `internal/duckdb/sync_test.go`
- Modify: `internal/duckdb/analytics_usage.go`
- Modify: `internal/duckdb/store_test.go`
- Modify: `internal/duckdb/activityreport_test.go`

**Interfaces:** Bumps `duckdb.SchemaVersion` 6 → 7 and mirrors normalized bands
without ALTER migrations.

- [ ] **Step 1: Write failing mirror tests**

Test schema version/table, full/incremental band persistence/removal, custom
suppression, fetched classification for a band-only fallback mismatch, and
daily/session/activity 200,001 costs/savings. Include unbound aggregate,
ordinal-bound request, and application-count parity cases.

```bash
go test ./internal/duckdb -run 'Test.*PricingBand' -count=1
```

Expected: missing mirror table and flat-cost failures.

- [ ] **Step 2: Implement create-only schema/sync/load**

Add `model_pricing_bands` to `mirrorTables`, replace changed sets in the current
pricing transaction, attach bands to loaded rates, and route direct DuckDB
session billing through `export.ModelRates.CostForTokens`. Use selected rates
for savings; compare complete bands for source classification; custom rates
replace bands. Mirror the same request-scope predicate and application-count
recording.

- [ ] **Step 3: Verify DuckDB GREEN and commit**

```bash
go test ./internal/duckdb -count=1
```

Use the mandatory commit skill and commit the mirror rebuild/sync behavior.

______________________________________________________________________

### Task 6: Version export fixtures and record evidence

**Files:**

- Rename: `testdata/golden/usage_daily_v3.json` → `usage_daily_v4.json`

- Rename: `testdata/golden/usage_daily_breakdown_v3.json` →
  `usage_daily_breakdown_v4.json`

- Rename: `testdata/golden/activity_report_v3.json` → `activity_report_v4.json`

- Rename: `testdata/golden/session_export_v3.json` → `session_export_v4.json`

- Rename: `testdata/golden/session_export_v3.ndjson` →
  `session_export_v4.ndjson`

- Modify: `cmd/agentsview/usage_test.go`

- Modify: `cmd/agentsview/activity_test.go`

- Modify: `cmd/agentsview/export_sessions_test.go`

- Modify: `cmd/agentsview/export.go`

- Modify: `docs/session-export.md`

- Modify: `docs/token-usage.md`

- Modify: `docs/activity.md`

- Modify: `docs/session-api.md`

- Modify: `docs/internal/session-format-sources.md`

- Create: `frontend/src/lib/api/generated/models/ExportPricingBand.ts`

- Create: `frontend/src/lib/api/generated/models/ExportPricingApplication.ts`

- Create: `frontend/src/lib/api/generated/models/ExportAppliedPricingBand.ts`

- Modify: `frontend/src/lib/api/generated/models/ExportEffectiveModelRate.ts`

- Modify: `frontend/src/lib/api/generated/index.ts`

- [ ] **Step 1: Update v4 fixtures/docs**

Update test filenames, `sampleDailyUsageJSON`, golden schema versions/digests,
and the session-export example. Document schema version 4, model `bands`, and
base/band/aggregate application counts and revised canonical digest row keys in
`docs/token-usage.md`; update the activity/session API examples and
shared-contract descriptions. Use the existing `-update` golden path, then:
Merge application counts across paginated session-export pages by summing base,
aggregate, and matching per-threshold counts; deep-clone band/application slices
in the merged document.

```bash
go test -tags fts5 ./cmd/agentsview -run 'Test(UsageDaily.*Golden|ActivityReportGolden|ExportSessions.*Golden)' -count=1
```

- [ ] **Step 2: Regenerate and check the OpenAPI client**

```bash
cd frontend
npm run generate:api
npm run check
```

Commit the generated band/application models plus updated effective-rate/index
files; do not hand-edit generated TypeScript.

- [ ] **Step 3: Record pinned pricing evidence**

Document LiteLLM commit `551e5d097c11f08fd2400a25a651b1844fcf89c2` and these
files:

```text
model_prices_and_context_window.json
litellm/litellm_core_utils/llm_cost_calc/utils.py
```

Record standard 200K/272K fields, strict-greater/highest-threshold selection,
and service-tier suffix separation. Cross-reference Claude/Codex token
normalization without claiming their transcripts contain pricing metadata.

- [ ] **Step 4: Commit fixtures/evidence**

Use the mandatory commit skill and commit the versioned contract and evidence.

______________________________________________________________________

### Task 7: Verify and finish

**Files:**

- Modify: this plan only to check completed steps.

- [ ] **Step 1: Format/check**

```bash
go fmt ./...
mdformat docs/superpowers/specs/2026-07-29-request-pricing-bands-design.md docs/superpowers/plans/2026-07-29-request-pricing-bands.md docs/internal/session-format-sources.md
git diff --check
```

If `mdformat` remains unavailable, report that exact limitation; hooks still run
their configured formatter before each commit.

- [ ] **Step 2: Focused/full verification**

```bash
go test -tags fts5 ./internal/pricing/... ./internal/export ./internal/db ./internal/postgres ./internal/duckdb ./internal/pricingrefresh -count=1
make test-postgres
go vet ./...
make test
```

- [ ] **Step 3: Audit spec coverage**

Check both key spellings, inherited/zero rates, exclusive/highest selection,
request-scope/aggregate fallback, applied selection counts, per-row aggregation,
custom suppression, reported cost authority, persistence/copy/sync,
provenance/digest, SQLite/PG parity, DuckDB rebuild, and all deliberate
exclusions.

- [ ] **Step 4: Commit any final integration edits and finish**

Use the mandatory commit skill, then invoke
`superpowers:finishing-a-development-branch`. Do not push, open a PR, change
branches, or merge without the corresponding user authorization.
