# Copilot Reported Cost Cutoff Design

## Goal

Use Copilot CLI's reported `totalNanoAiu` value as authoritative dollar cost
only for sessions created under GitHub's usage-based AI Credits pricing model,
without changing existing schemas or removing existing usage surfaces.

## Pricing Boundary

GitHub replaced premium-request billing with usage-based AI Credits billing on
June 1, 2026. GitHub's REST documentation describes sessions since that date as
using `ai_credits` and older sessions as using `premium_requests`.

AgentsView will therefore convert `totalNanoAiu` to USD only when the Copilot
session's effective start timestamp is on or after
`2026-06-01T00:00:00Z`. The session timestamp, rather than the shutdown event
timestamp, controls eligibility so a session spanning the boundary retains the
pricing model under which it was created.

For eligible sessions:

- `cost_usd` is `totalNanoAiu / 100_000_000_000`.
- `cost_source` is `copilot-reported`.
- The final cumulative shutdown value wins, including zero.

For older sessions, `totalNanoAiu` does not participate in dollar pricing.
Existing catalog-based model pricing remains in effect.

## Compatibility and Scope

The database schema, API schema, frontend types, UI, CLI output, localization,
and AI Credits behavior remain as they are on `main` wherever possible.
Specifically, the existing Copilot AI Credits summary card,
`copilotAICredits` totals field, session `ai_credits` field, and CLI AI Credits
line remain present.

The generic `cost_usd` and `cost_source` fields already exist on `main`; this
change only populates and consumes them differently for eligible Copilot
sessions. No Copilot-specific cost column, compatibility probe, dual-read path,
or usage-daily schema-version change is introduced.

Branch changes made solely to remove existing credit surfaces, add or remove a
draft Copilot credit schema, or preserve draft-schema data during resync are
out of scope and will be reverted. The parser data-version bump remains because
existing sessions created since June 1, 2026 must be reparsed to populate the
reported dollar cost.

## Data Flow

The Copilot parser records the first valid event timestamp as the session start.
When processing a shutdown event, it checks that start against the cutoff before
attaching reported cost to the final shutdown usage row. Downstream SQLite,
PostgreSQL, and DuckDB aggregation continue to treat `copilot-reported` as an
authoritative session total and otherwise retain catalog estimates.

Model-filtered reports continue using model-level catalog estimates because a
session-level reported total cannot be allocated reliably among models.

## Verification

Parser tests will cover sessions immediately before, exactly at, and after the
UTC cutoff, including a session that starts before the cutoff and shuts down
after it. Existing tests will verify that AI Credits surfaces and schemas match
`main`. Backend parity tests will continue to verify authoritative reported cost
for eligible sessions and catalog estimates for ineligible sessions.
