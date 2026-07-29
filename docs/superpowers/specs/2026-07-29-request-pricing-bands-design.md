# Request Pricing Bands Design

## Goal

Preserve context-dependent token rates from the LiteLLM catalog and apply them
to each individual usage request before Agentsview aggregates costs. This fixes
long-context estimates for providers such as OpenAI and Anthropic without
hard-coding provider or model names into the pricing engine.

## Scope

This change covers LiteLLM's whole-request context bands encoded by standard
token-rate keys such as `input_cost_per_token_above_272k_tokens` and
`input_cost_per_token_above_200k_tokens`. It does not infer a threshold from a
model name or `max_input_tokens`, and it does not add Batch, Flex, Priority, or
regional processing estimates because stored session usage does not reliably
identify those processing variants.

LiteLLM also publishes a `tiered_pricing` array for some providers. Its entries
do not carry a provider-neutral indication of whether pricing is graduated or
selected once for the whole request. Agentsview will not interpret that array
until the upstream metadata makes that distinction deterministic. This avoids
silently applying the wrong charging model.

## Catalog Boundary

The LiteLLM adapter is the only code allowed to understand threshold-bearing
JSON field names. It will:

1. Recognize anchored standard-input keys of the form
   `input_cost_per_token_above_<N>k_tokens` or
   `input_cost_per_token_above_<N>_tokens`.
1. Convert `<N>` to an exact token count with checked integer arithmetic.
1. Read the matching output, cache-read, and cache-creation keys for the same
   threshold.
1. Materialize a complete rate tuple. A missing companion rate inherits the
   model's base rate, matching LiteLLM's cost calculator; an explicit zero
   remains zero.
1. Reject malformed, duplicate, or conflicting bands instead of guessing.

Service-tier-suffixed fields do not match the anchored standard key and remain
out of scope.

## Normalized Model

The pricing catalog and database model gain zero or more bands:

```go
type PricingBand struct {
    AboveInputTokens    int
    InputPerMTok        money.Money
    OutputPerMTok       money.Money
    CacheCreationPerMTok money.Money
    CacheReadPerMTok    money.Money
}
```

`AboveInputTokens` is an exclusive boundary. A request with exactly 272,000
input tokens uses the base rates; 272,001 uses the matching band. Bands are
sorted by threshold, and the highest satisfied threshold wins.

The request input used for selection is reconstructed from normalized usage:

```text
uncached input + cache-read input + cache-creation input
```

This works for Codex, whose inclusive upstream input is split by the parser, and
for Claude-style usage, whose input and cache categories are already separate.

## Persistence and Synchronization

SQLite and PostgreSQL gain a `model_pricing_bands` table keyed by
`(model_pattern, above_input_tokens)`. Every row stores a complete rate tuple
and an update timestamp. The table references `model_pricing` so a model and its
bands remain one catalog unit.

Catalog upserts replace the bands for each changed model in the same transaction
as its base rate. This makes removal of an upstream threshold as deterministic
as adding one. Full-resync archive copying and PostgreSQL push copy the band
rows alongside base pricing.

Fallback pricing snapshots retain bands, so offline estimates behave like fresh
LiteLLM pricing. User-configured custom pricing remains a single flat rate tuple
and replaces both the fetched base rate and its bands for that model; Agentsview
must not combine custom base rates with fetched tier rates.

## Cost Calculation

Band selection lives on the resolved model rates. All existing usage paths
already calculate cost one deduplicated usage row at a time, so they will call
the band-aware calculation before accumulating daily, project, model, session,
or activity totals.

Cache-savings calculations use the same selected band as request cost. Reported
provider costs remain authoritative and bypass computed pricing exactly as they
do today.

The pricing provenance block includes normalized bands and incorporates them
into its digest. That makes exports explain the applied threshold and ensures
catalog changes invalidate deterministic pricing metadata.

## Validation and Failure Behavior

- Thresholds must be positive and fit in `int`.
- Duplicate thresholds for a model are an ingestion error.
- Bands are emitted and persisted in ascending threshold order.
- A missing companion tier rate inherits the base component.
- An explicit zero tier rate is preserved.
- Models without bands retain current flat-rate behavior.
- Unknown threshold-like keys outside the supported standard token-rate shape
  remain ignored, matching the adapter's existing forward-compatible JSON
  behavior.

## Tests

Focused tests will protect observable behavior:

1. LiteLLM parsing converts 200K and 272K keys into literal normalized bands,
   including inherited and explicit-zero component rates.
1. Exactly-at-threshold requests use base rates and threshold-plus-one requests
   use the band for all token categories.
1. Two requests that are individually below the threshold remain base-priced
   even when their aggregate input exceeds it.
1. Daily usage, session usage, and activity calculations expose the same
   band-aware cost in SQLite.
1. PostgreSQL integration tests assert the same usage result and pricing-band
   persistence behavior.
1. Pricing export/digest tests show the applied bands without duplicating the
   implementation's calculations.

## Non-goals

- Inferring pricing thresholds from context-window capacity.
- Provider- or model-name allowlists for long-context rates.
- Running LiteLLM's Python calculator at runtime.
- Guessing semantics for LiteLLM's ambiguous `tiered_pricing` array.
- Adding processing-tier attribution when the source usage does not record it.
