# Copilot request accounting and pricing

Checked 2026-09-07 against GitHub documentation and its published CLI package.
This investigation does not change parser or pricing behavior.

## What GitHub documents

[Models and pricing for GitHub Copilot](https://docs.github.com/en/copilot/reference/copilot-billing/models-and-pricing)
says each token is priced by model and converted into AI credits, with one AI
credit equal to $0.01 USD. GitHub itself publishes input-size bands:

| GPT-5.4 input size  | Input per million | Cached input per million | Output per million |
| ------------------- | ----------------- | ------------------------ | ------------------ |
| At most 272K tokens | $2.50             | $0.25                    | $15.00             |
| Above 272K tokens   | $5.00             | $0.50                    | $22.50             |

For example, 300,000 uncached input tokens in one model call cost $1.50 at
these published rates. Applying the base input rate gives $0.75. Output is
additional. These are usage values before plan allowances and discounts. The
prices are a dated example, not constants to copy into the application.

The official
[SDK usage and billing guide](https://docs.github.com/en/copilot/how-tos/copilot-sdk/features/usage-and-billing)
states that `assistant.usage` fires once per model API call in a turn, including
sub-agent calls. Its counts describe that single call.
`session.usage.getMetrics` instead returns accumulated session totals. The guide
describes `assistant.usage.cost` as a premium-request multiplier, not dollars.
It recommends runtime `models.list` billing data for estimates and says prices
must not be hard-coded.

The pinned
[SDK event definitions](https://github.com/github/copilot-sdk/blob/1644e74578db3637bc7527951bac227aabbc0584/nodejs/src/generated/session-events.ts)
also define per-call input, output, cache-read and cache-write counts,
`copilotUsage.totalNanoAiu`, `isAuto`, and `isByok`. Those fields do not by
themselves establish how older CLI versions persisted database records. The SDK
guide cautions that the nano-AI-unit conversion must be verified against billing
documentation. This research does not independently validate Agentsview's
monetary conversion or its June 1 date gate.

[Usage-based billing for individuals](https://docs.github.com/en/copilot/concepts/billing/usage-based-billing-for-individuals)
documents a 10% model-cost discount for paid users selecting Auto, including in
Copilot CLI. Included credits are consumed before additional billed usage. A
token-dollar estimate therefore does not establish an invoice increment.

The
[legacy premium-request rules](https://docs.github.com/en/copilot/reference/copilot-billing/request-based-billing-legacy/copilot-requests)
apply to existing annual Pro and Pro+ plans that remain on that scheme after
June 1, 2026. Each CLI prompt consumes a premium request times the model
multiplier; autonomous tool calls do not each consume a premium request. A model
API call and a premium billing request are different units.

## What the session store establishes

[GitHub's session-data documentation](https://docs.github.com/en/copilot/concepts/agents/copilot-cli/chronicle)
describes the SQLite store as an incrementally populated subset of session
files. It does not specify the `assistant_usage_events` schema or guarantee one
row per model API call.

Static inspection of the official
[`@github/copilot-darwin-arm64` 1.0.83 distribution](https://registry.npmjs.org/@github/copilot-darwin-arm64/-/copilot-darwin-arm64-1.0.83.tgz)
found the schema and SQL in `package/prebuilds/darwin-arm64/runtime.node`. The
archive SHA-1 matched the npm metadata:
`8b8f67a38e893b61e6cef9c011a6fe22d8fdcd4d`. No package code was installed or
executed.

The embedded schema gives `assistant_usage_events` an autoincrement `id`,
nullable `turn_index`, model, input/output/cache/reasoning counts,
`total_nano_aiu`, `request_multiplier`, `token_details_json`, and `created_at`.
It has no unique constraint on session plus turn. The insert SQL appends those
fields, and embedded reporting SQL sums token columns by session. Embedded
tracking strings refer to `assistant.usage` and `insertAssistantUsageEvent`.
These observations support incremental usage accounting. Strings in compiled
code do not prove that exactly one SDK usage event creates exactly one row, or
exclude batching or transformation before insertion. A producer trace or
readable implementation of that mapping remains necessary to settle that link.

To reproduce the static check, download the versioned archive, verify its SHA-1,
and inspect that member for `CREATE TABLE IF NOT EXISTS assistant_usage_events`,
`INSERT INTO assistant_usage_events`, and the session-level `SUM` queries. Do
not install or run the package for this check.

## Consequences for the review finding

Agentsview's `internal/usagefacts/fact.go` determines request scope from either
`MessageOrdinal` or `SourceIsRequestScoped`. Its source helper explicitly
supports provider requests with no message. An ordinal is not mandatory. The
parser emits source `session-store`, which the helper does not recognize.
`internal/db/usage_rollup_build.go` selects input-size bands only for facts
marked request-scoped; other facts retain base rates.

That classification mechanism is established by the current implementation.
GitHub's own prices establish that request size can matter financially. The
exact database-row-to-call mapping remains incompletely verified, so this
research does not fully confirm the review finding or authorize a production
pricing change. It also does not establish that every model-provider catalog
rate matches GitHub's rate for a particular account and date. Reported Copilot
usage cost, catalog estimates, premium requests, and invoice charges must stay
distinct. No production fix or regression test was added in this investigation.
