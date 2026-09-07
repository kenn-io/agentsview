# Copilot request accounting and pricing

Checked 2026-09-07 against GitHub documentation and its published CLI package.
The follow-up producer trace establishes the request boundary used by the parser
and pricing fixes below.

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
`8b8f67a38e893b61e6cef9c011a6fe22d8fdcd4d`. The initial inspection was static.
The follow-up executed native tracking methods with explicitly selected scratch
paths. It did not install the CLI, invoke a model, or use live session data.

The embedded schema gives `assistant_usage_events` an autoincrement `id`,
nullable `turn_index`, model, input/output/cache/reasoning counts,
`total_nano_aiu`, `request_multiplier`, `token_details_json`, and `created_at`.
It has no unique constraint on session plus turn. The insert SQL appends those
fields, and embedded reporting SQL sums token columns by session. Embedded
tracking strings refer to `assistant.usage` and `insertAssistantUsageEvent`. The
initial static inspection supported incremental accounting but left the exact
event-to-row mapping unresolved. The follow-up closed that gap:

1. In the same archive, `package/app.js` registers a session event listener in
   `dtr`. It passes each event to `handleTrackingEventForSession`. Its `ltr`
   serialization preserves `assistant.usage` data, agent ID, and timestamp.
1. Load the archive's `runtime.node` with Node.js. Construct an in-memory
   session using `sessionConstruct` with explicit scratch working and session
   paths, `sessionFsIsLocal: false`, `isLocalSession: false`, and empty update
   options. Do not apply its returned host actions.
1. Open `SessionStoreHandle` with an explicit scratch database path, call
   `upsertSession`, and initialize tracking with
   `sessionStoreTrackingInitSessionState` and `getMaxTurnIndex`.
1. Send three `assistant.usage` events to `handleTrackingEventForSession`, with
   input counts 150,000, 150,000, and 300,000, output count 10 each, model
   `gpt-5.4`, distinct timestamps, and zero cache counts. Flush tracking.
1. Query the database. It contains exactly three rows with those unchanged
   input/output counts, distinct IDs 1, 2, and 3, and `turn_index = 0` for all
   three. Close the scratch store.

This exercises GitHub's shipped writer, rather than a reimplementation or a
fixture insert. Combined with the documented per-call event semantics, it
establishes individual model-call rows for CLI 1.0.83. A shared turn index
cannot be used to merge those rows. The trace does not claim that every older
CLI release has an identical schema.

The SDK's `AssistantMessageData` independently declares `model` optional, with
the description "Model that produced this assistant message, if known." Its
optional `outputTokens` is the actual API output count. Agentsview accepts these
independently, but its store fallback previously discarded the count when
neither a message model nor a prior model-change event supplied a model. The
regression uses that supported event shape. It is not a claim that this
combination was observed in the inspected local transcripts.

## Consequences for the review finding

Agentsview's `internal/usagefacts/fact.go` determines request scope from either
`MessageOrdinal` or `SourceIsRequestScoped`. Its source helper explicitly
supports provider requests with no message. An ordinal is not mandatory. The
parser emits source `session-store`, which the helper previously did not
recognize. `internal/db/usage_rollup_build.go` selects input-size bands only for
facts marked request-scoped; other facts retain base rates.

The parser now retains uncovered output in session totals even without model
identity. It still requires a model to attach priced message usage, and still
excludes responses already covered by store rows.

The source helper now classifies `session-store` as request-scoped without
inventing a message ordinal. A focused pricing test supplies the dated GitHub
rates explicitly: one 300K-input call costs $1.50 for input, while two separate
150K-input calls total $0.75. The test isolates request classification from
catalog updates. The usage-cache format advances so existing cached facts are
rebuilt under the corrected classification.

This fix does not establish that every model-provider catalog rate matches
GitHub's rate for a particular account and date. Reported Copilot usage cost,
catalog estimates, premium requests, and invoice charges remain distinct.
