# Authoritative microdollar money

## Problem

AgentsView currently represents monetary amounts and model-pricing rates as
`float64` values in Go, `REAL` values in SQLite, `DOUBLE PRECISION` values in
PostgreSQL and DuckDB, and JSON numbers in public responses. Floating-point
arithmetic makes equality, aggregation, fingerprints, and synchronization depend
on binary approximations of decimal dollar values.

Money needs one exact representation throughout the application. Existing
archives must migrate without losing sessions or requiring source transcripts to
still exist, and SQLite and PostgreSQL must continue to expose identical
behavior. DuckDB remains a disposable mirror and must rebuild rather than gain
an in-place compatibility path.

## Goals

- Represent every monetary value as signed 64-bit microdollars, where one US
  dollar equals 1,000,000 microdollars.

- Make the integer value authoritative in parsing, persistence, calculation,
  aggregation, comparison, sorting, fingerprints, synchronization, and public
  machine-readable output.

- Encode a public monetary value only as a money object containing an integer:

    ```json
    {"microdollars": 420000}
    ```

- Render ordinary dollar strings such as `$0.42` in human-facing CLI tables and
  UI labels.

- Migrate existing SQLite and PostgreSQL data forward transactionally without
  deleting or rebuilding the persistent session archive.

- Preserve observable behavior and query-shape parity between SQLite and
  PostgreSQL.

- Rebuild DuckDB mirrors through the existing schema-version mechanism.

- Reject invalid or overflowing monetary input without partially writing it.

## Non-goals

- Do not retain floating-point monetary fields in public or internal types.
- Do not emit a second dollar-valued representation beside microdollars.
- Do not retain old floating-point database columns, dual reads, dual writes,
  aliases, version-bridging reads, or permanent repair gates.
- Do not introduce a general currency framework. All tracked money remains USD.
- Do not use nanodollars, arbitrary-precision decimals, or arbitrary-precision
  integers.
- Do not convert unrelated floating-point measurements such as ratios,
  percentages, durations, scores, or percentiles.
- Do not reparse or recreate the SQLite session archive to perform the schema
  migration.

## Representation

### Money type

A focused internal money package will define the shared value type:

```go
type Money struct {
	Microdollars int64 `json:"microdollars"`
}
```

All fields whose value is denominated in dollars will use `Money` or `*Money`.
Pointers retain the existing distinction between an absent reported cost and an
authoritative zero cost. Existing `HasCost`-style fields continue to distinguish
an unavailable complete estimate from a real zero-valued estimate.

The package will own:

- checked addition and subtraction;
- comparison without conversion;
- exact parsing of decimal text at a declared source scale;
- conversion of integer cents and other declared source units;
- checked token-rate multiplication with wide integer intermediates;
- nearest-microdollar rounding; and
- ordinary dollar formatting for human presentation.

No method will expose a floating-point dollar value. UI and CLI formatters will
split the signed integer into whole dollars and fractional microdollars, then
apply the existing display precision rules without converting the amount to a
float.

### Range and precision

Signed `int64` microdollars cover approximately plus or minus $9.22 trillion.
Microdollars remain exactly representable as JavaScript numbers through
approximately $9.0 billion, comfortably above the operational range of a local
agent-cost archive. API schemas will describe the member as an `int64` integer,
and backend storage retains the full signed 64-bit range.

One microdollar is $0.000001, or one ten-thousandth of a cent. An imported
amount requiring finer precision is rounded to the nearest microdollar, with an
exact half rounded away from zero. Nonnegative source charges are validated as
nonnegative. Signed `Money` remains necessary for derived differences such as a
comparison delta.

### Pricing calculation

Model rates are `Money` values whose surrounding field names declare that the
amount applies per million tokens. For example:

```json
{
  "inputCostPerMTok": {"microdollars": 3000000}
}
```

For one independently priced usage row, the calculation will:

1. select the billable token counts using the existing reasoning-token and cache
   rules;
1. multiply every token category by its integer microdollars-per-million-token
   rate using an overflow-safe wide intermediate;
1. sum those unrounded products;
1. divide by 1,000,000 tokens; and
1. round the combined row result once to the nearest microdollar.

Rounding once per usage row avoids separately rounding input, output, cache
write, and cache-read components. Aggregate totals sum authoritative row costs
exactly. Reported costs remain authoritative over catalog pricing exactly as
they are today.

## Input boundaries

### Agent-reported values

JSON sources will preserve the original numeric token with `json.Number`, raw
JSON text, or an equivalent non-floating representation and convert it directly
to `Money`. Integer source units, including Grok cost ticks, will convert with
integer quotient and remainder arithmetic.

Some upstream formats already store their values as floating-point database
columns. AgentsView cannot recover decimal precision already discarded by an
upstream producer. Those boundary values will be checked for finiteness and
range, rounded immediately to `Money`, and never retained or aggregated as
floats inside AgentsView.

### Cursor admin usage

Cursor's `chargedCents` and `cursorTokenFee` values will be decoded from their
original decimal JSON tokens and converted directly from cents to microdollars.
Stored fields will be renamed to communicate their actual unit:

- `charged_microdollars`
- `cursor_token_fee_microdollars`

Cursor cost attribution will use `charged_microdollars` directly instead of
dividing cents by a floating-point constant in a query.

### Pricing catalogs

LiteLLM per-token decimal prices will be parsed from their JSON number text and
scaled directly into microdollars per million tokens. Embedded pricing snapshots
will contain integer microdollar rates. Pricing equality and canonical digests
will use those integers.

Custom model-pricing configuration will replace dollar-number settings with
integer settings whose names carry the unit:

```toml
[custom_model_pricing."example-model"]
input_microdollars_per_mtok = 3_000_000
output_microdollars_per_mtok = 15_000_000
cache_creation_microdollars_per_mtok = 3_750_000
cache_read_microdollars_per_mtok = 300_000
```

Old floating-point configuration keys will not be accepted through an alias or
fallback path.

## Persistence

### Fresh schemas

Fresh SQLite, PostgreSQL, and DuckDB schemas will use integer columns:

- `usage_events.cost_microdollars`, nullable;
- `cursor_usage_events.charged_microdollars`, not null;
- `cursor_usage_events.cursor_token_fee_microdollars`, not null; and
- `model_pricing.input_microdollars_per_mtok`, `output_microdollars_per_mtok`,
  `cache_creation_microdollars_per_mtok`, and
  `cache_read_microdollars_per_mtok`, all not null.

SQLite uses `INTEGER`; PostgreSQL and DuckDB use `BIGINT`.

### SQLite migration

One forward migration will detect the released floating-point schema and
transactionally rebuild the three affected tables. It will:

1. validate every legacy value for finiteness, nonnegative source-domain
   constraints, and scaled `int64` range;
1. create replacement tables with their final integer schemas;
1. copy every row while converting legacy values with nearest-microdollar
   rounding;
1. preserve primary keys, nullable cost semantics, foreign keys, and stable
   deduplication keys;
1. replace the old tables; and
1. recreate all indexes.

The migration commits only after all three tables and indexes are valid. A
failure rolls back the entire transaction. Fresh schemas already have the final
columns, and a completed migration has no legacy columns, making the migration
idempotent without a continuing compatibility read.

The migration will fail closed if it encounters an unexpected mixed schema
instead of guessing which representation is authoritative.

### PostgreSQL migration

PostgreSQL will run the equivalent forward migration inside a transaction. It
will preflight legacy values for finiteness, nonnegative source-domain
constraints, and scaled `BIGINT` range, then convert and rename the affected
columns. The final schema and every query will use the same units and semantics
as SQLite.

The migration runs before code that expects the new column names. It neither
keeps the old columns nor supports simultaneous old and new application
versions.

### DuckDB mirror

The DuckDB mirror schema version will advance from 3 to 4. Version 4 creates
only the integer money columns. A version mismatch triggers the established full
rebuild into a fresh validated file followed by atomic replacement. No DuckDB
`ALTER` migration or version-bridging query will be added.

### Synchronization and fingerprints

SQLite-to-PostgreSQL push, SQLite-to-DuckDB push, orphan preservation, session
export, and push fingerprints will transport and hash integer microdollars.
Fingerprints will never format money through a floating-point decimal string.
Backend comparisons and update predicates will compare integers directly.

## Public contracts

### Machine-readable output

Every machine-readable monetary field becomes a single `Money` object. Examples
include:

```json
{
  "cost": {"microdollars": 420000},
  "rollup_cost": {"microdollars": 1260000},
  "totalCost": {"microdollars": 1680000},
  "totalCostDelta": {"microdollars": -250000}
}
```

This is the only monetary JSON representation. Fields such as `cost_usd` and
floating-point forms of `cost`, `totalCost`, pricing rates, savings, or deltas
will be removed rather than emitted beside the authoritative object.

Naming follows each surface's established casing. The parent field supplies the
semantic meaning, while the object supplies the unit. Versioned usage, activity,
and session-export schemas will receive breaking-version increments. OpenAPI and
generated TypeScript types will define one reusable `Money` schema.

Dimensionless ratios and percentages remain JSON numbers. Token counts, session
counts, and other nonmonetary integers remain unchanged.

### Human-facing output

CLI tables, status lines, terminal summaries, and UI labels will render ordinary
dollars from `Money`, for example `$0.42`, `<$0.01`, or `$1,234.56` according to
the surface's existing display rules. They will not display the word
`microdollars` or raw scaled integers unless the user explicitly requests
machine-readable JSON.

Frontend calculations that sort, group, compare, or add costs will use the
integer `microdollars` member. Dollar conversion is confined to display
formatters. Ratios divide integer amounts only at the dimensionless result
boundary.

## Error handling

- Decimal parsing rejects malformed, empty, non-finite, and out-of-range input.
- Source charges and rates reject negative values.
- Checked arithmetic reports overflow instead of wrapping or saturating.
- Migration validation reports the affected table and row identity without
  including private session content.
- A migration error leaves the original schema and rows intact.
- API aggregation returns an error if an exact sum exceeds `int64`; it does not
  fall back to floating-point arithmetic.
- Presentation formatting handles the full signed `int64` range without taking
  an absolute value that overflows at `math.MinInt64`.

## Testing

Tests will protect behavior owned by AgentsView rather than the mechanics of Go,
SQLite, PostgreSQL, DuckDB, or JSON libraries.

Focused money tests will cover:

- exact decimal conversion at zero, positive, negative-derived, and exponent
  forms used by source data;
- values immediately below, at, and above a half-microdollar boundary;
- `int64` range boundaries and checked addition/subtraction;
- token-rate calculation that would overflow a naive 64-bit intermediate but
  produces a valid final result;
- combined-component rounding once per usage row; and
- dollar display formatting, including negative values and `math.MinInt64`.

SQLite migration tests will open a legacy fixture containing fractional reported
costs, Cursor cents, pricing rates, an explicit zero, and a null cost. They will
assert the exact migrated integers, preserved identifiers and row counts, final
column types, and unchanged deduplication behavior. Invalid or overflowing
legacy input will prove that the transaction leaves the original tables intact.

PostgreSQL integration tests will apply the migration to the equivalent released
schema and assert the same values and behavior. Fresh-schema tests will verify
that neither backend creates legacy money columns.

SQLite and PostgreSQL usage tests will use identical token and pricing fixtures
to protect cost, savings, filtering, ordering, rollups, and breakdown-sum
parity. DuckDB tests will verify the version-4 rebuild boundary, integer push
values, integer aggregation, and query parity with SQLite.

API, export, CLI JSON, and frontend tests will assert the sole
`{"microdollars": ...}` representation and exact integer values. Human CLI and
frontend tests will assert ordinary dollar rendering from those integers.
Realistic mutations such as an incorrect scale, component-wise rounding,
floating-point sorting, omitted cache cost, or stale legacy column will cause at
least one focused test to fail.

## Delivery constraints

The change is one atomic domain migration even though it touches parsers,
storage, services, APIs, CLI output, and the frontend. Shipping only one backend
or retaining floats in an intermediate layer would leave two competing monetary
authorities and is therefore outside the design.

Implementation will use test-first cycles. The database change will be the only
new forward schema migration in its pull request, and shipped migration history
will remain untouched.
