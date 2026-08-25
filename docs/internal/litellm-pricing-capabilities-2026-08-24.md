# LiteLLM pricing capabilities on 2026-08-24

This note checks LiteLLM's machine-readable pricing catalog and pricing code at
commit
[`947dbbf0298eecd3d226f765d4a4fb7ea3fd2551`](https://github.com/BerriAI/litellm/commit/947dbbf0298eecd3d226f765d4a4fb7ea3fd2551).
It separates upstream data from what Agentsview currently imports.

## Short answer

LiteLLM already supplies both `xai/grok-4.6` and `gpt-5.6-luna`, and it is much
more current than the Pydantic GenAI Prices data for Grok 4.6. The Grok record
landed in LiteLLM as a
[`day-0 pricing` change on 2026-08-13](https://github.com/BerriAI/litellm/commit/928dfab65c59b86317a8b4336734ed6177b2d9c8).

Agentsview also already imports LiteLLM's standard whole-request context bands.
The restored embedded snapshot contains:

- `gpt-5.6-luna` at $0.20/M input and $1.20/M output, then $0.40/M and
  $1.80/M above 272,000 input tokens.
- `xai/grok-4.6` at $2/M input and $6/M output, then $4/M and $12/M above
  200,000 input tokens.

The important distinction is that these are simultaneous context-dependent
rates. LiteLLM's JSON does not retain the old and new Luna prices with effective
dates. Its 2026-07-30 update
[`f1b781d`](https://github.com/BerriAI/litellm/commit/f1b781d06b6155df7c8979110ddc45938c3b81fb)
replaced the old $1/M input and $6/M output fields with $0.20/M and $1.20/M. The
old values remain in Git history, not in the current catalog as selectable price
records.

## What the upstream catalog can represent

| Capability                          | LiteLLM upstream                                                                                                                                                                                             | Evidence                                                                                                                                                                                                                                                                                                                                             |
| ----------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Current model coverage              | Yes. The current map has 3,174 model entries.                                                                                                                                                                | [Current cost map](https://github.com/BerriAI/litellm/blob/947dbbf0298eecd3d226f765d4a4fb7ea3fd2551/model_prices_and_context_window.json)                                                                                                                                                                                                            |
| Several rates for one model         | Yes. A model object can carry standard, cache, context-band, batch, flex, priority, and regional rates at once.                                                                                              | [Luna record, lines 26271-26332](https://github.com/BerriAI/litellm/blob/947dbbf0298eecd3d226f765d4a4fb7ea3fd2551/model_prices_and_context_window.json#L26271-L26332)                                                                                                                                                                                |
| Whole-request token tiers           | Yes. It has named `*_above_128k_tokens`, `*_above_200k_tokens`, `*_above_272k_tokens`, and other thresholds, plus general `tiered_pricing` range arrays. The current map has 21 `tiered_pricing` records.    | [Pricing field types, lines 193-288](https://github.com/BerriAI/litellm/blob/947dbbf0298eecd3d226f765d4a4fb7ea3fd2551/litellm/types/utils.py#L193-L288), [range selection, lines 28-55](https://github.com/BerriAI/litellm/blob/947dbbf0298eecd3d226f765d4a4fb7ea3fd2551/litellm/litellm_core_utils/llm_cost_calc/tiered_pricing.py#L28-L55)         |
| Batch and service tiers             | Yes. The map has batch, flex, and priority rates. LiteLLM's calculator selects a service-tier suffix and falls back to the standard rate when needed.                                                        | [Service-tier selection, lines 192-210](https://github.com/BerriAI/litellm/blob/947dbbf0298eecd3d226f765d4a4fb7ea3fd2551/litellm/litellm_core_utils/llm_cost_calc/utils.py#L192-L210), [batch calculation, lines 2125-2215](https://github.com/BerriAI/litellm/blob/947dbbf0298eecd3d226f765d4a4fb7ea3fd2551/litellm/cost_calculator.py#L2125-L2215) |
| Regional pricing                    | Yes, through EU/US processing multipliers, Vertex regional endpoint multipliers, provider-specific geo multipliers, and separate provider or region-qualified model keys.                                    | [Regional calculators, lines 729-801](https://github.com/BerriAI/litellm/blob/947dbbf0298eecd3d226f765d4a4fb7ea3fd2551/litellm/litellm_core_utils/llm_cost_calc/utils.py#L729-L801)                                                                                                                                                                  |
| Token and non-token billing units   | Yes. Fields cover input, output, cache read/write, audio, image, video, and reasoning tokens; characters; images and pixels; audio and video seconds; queries; pages; credits; sessions; calls; and GB-days. | [Model pricing fields, lines 193-306](https://github.com/BerriAI/litellm/blob/947dbbf0298eecd3d226f765d4a4fb7ea3fd2551/litellm/types/utils.py#L193-L306), [catalog sample, lines 2-40](https://github.com/BerriAI/litellm/blob/947dbbf0298eecd3d226f765d4a4fb7ea3fd2551/model_prices_and_context_window.json#L2-L40)                                 |
| Alias metadata                      | Supported by the loader, but unused in the current live JSON. The loader expands an `aliases` array into top-level keys. A full scan of the current map found zero entries with `aliases`.                   | [Alias expansion, lines 359-423](https://github.com/BerriAI/litellm/blob/947dbbf0298eecd3d226f765d4a4fb7ea3fd2551/litellm/litellm_core_utils/get_model_cost_map.py#L359-L423)                                                                                                                                                                        |
| Regex matching metadata             | Limited support. The JSON has an ordered `fallback_generalizations.rules` block with case-insensitive regexes. Exact entries win. The current rules provide routing and capability defaults, not prices.     | [Rule semantics, lines 1-39](https://github.com/BerriAI/litellm/blob/947dbbf0298eecd3d226f765d4a4fb7ea3fd2551/litellm/litellm_core_utils/fallback_generalizations.py#L1-L39), [current rules](https://github.com/BerriAI/litellm/blob/947dbbf0298eecd3d226f765d4a4fb7ea3fd2551/model_prices_and_context_window.json#L50211)                          |
| Effective-dated or historical rates | No. A scan of every current model field found no `effective_date`, `start_date`, `end_date`, or pricing-history field. The only date field is `deprecation_date`. Price changes replace fields in place.     | [Luna price-cut diff](https://github.com/BerriAI/litellm/commit/f1b781d06b6155df7c8979110ddc45938c3b81fb)                                                                                                                                                                                                                                            |
| Time-of-day conditions              | No. The current field inventory and calculator have no pricing time window or clock condition.                                                                                                               | [Current cost map](https://github.com/BerriAI/litellm/blob/947dbbf0298eecd3d226f765d4a4fb7ea3fd2551/model_prices_and_context_window.json)                                                                                                                                                                                                            |
| General conditional expressions     | No. Conditions are encoded as known field names and provider-specific calculator branches, not as a versioned condition language in the data.                                                                | [Generic cost calculator, lines 821-919](https://github.com/BerriAI/litellm/blob/947dbbf0298eecd3d226f765d4a4fb7ea3fd2551/litellm/litellm_core_utils/llm_cost_calc/utils.py#L821-L919)                                                                                                                                                               |

The negative findings above came from enumerating every key in the live JSON,
not from the abbreviated `sample_spec`. That scan found 3,176 top-level objects,
3,174 model records, 21 records with `tiered_pricing`, one rules block, no alias
arrays, and no effective-date field.

## Grok 4.6 and Luna coverage

LiteLLM's current direct xAI Grok 4.6 record has standard input, output, and
cache-read rates plus higher rates above 200,000 input tokens:

- [Current `xai/grok-4.6`, lines 43373-43392](https://github.com/BerriAI/litellm/blob/947dbbf0298eecd3d226f765d4a4fb7ea3fd2551/model_prices_and_context_window.json#L43373-L43392)
- [The commit that added it](https://github.com/BerriAI/litellm/commit/928dfab65c59b86317a8b4336734ed6177b2d9c8)
- [Current Bedrock-qualified Grok 4.6, lines 49002-49015](https://github.com/BerriAI/litellm/blob/947dbbf0298eecd3d226f765d4a4fb7ea3fd2551/model_prices_and_context_window.json#L49002-L49015)

The current OpenAI Luna record has standard and greater-than-272,000 rates for
input, output, cache creation, and cache reads. It also has batch, flex,
priority, and regional variants:

- [Current `gpt-5.6-luna`, lines 26271-26332](https://github.com/BerriAI/litellm/blob/947dbbf0298eecd3d226f765d4a4fb7ea3fd2551/model_prices_and_context_window.json#L26271-L26332)
- [Current Azure Luna, lines 6733-6782](https://github.com/BerriAI/litellm/blob/947dbbf0298eecd3d226f765d4a4fb7ea3fd2551/model_prices_and_context_window.json#L6733-L6782)
- [Current Bedrock Mantle Luna, lines 48652-48682](https://github.com/BerriAI/litellm/blob/947dbbf0298eecd3d226f765d4a4fb7ea3fd2551/model_prices_and_context_window.json#L48652-L48682)

Agentsview's pinned LiteLLM source commit,
[`418c7c6012d7c39a9d4a28c72cabe1995595ad2b`](https://github.com/BerriAI/litellm/commit/418c7c6012d7c39a9d4a28c72cabe1995595ad2b),
already contains both the
[`gpt-5.6-luna` record](https://github.com/BerriAI/litellm/blob/418c7c6012d7c39a9d4a28c72cabe1995595ad2b/model_prices_and_context_window.json#L25686)
and the
[`xai/grok-4.6` record](https://github.com/BerriAI/litellm/blob/418c7c6012d7c39a9d4a28c72cabe1995595ad2b/model_prices_and_context_window.json#L42714).

## What Agentsview currently keeps

Agentsview's current LiteLLM parser reduces each model to:

- standard input and output token rates;
- standard cache-creation and cache-read token rates;
- any standard `input_cost_per_token_above_<N>_tokens` or
  `input_cost_per_token_above_<N>k_tokens` band, with matching output and
  cache rates.

See
[`internal/pricing/catalog/litellm.go`](../../internal/pricing/catalog/litellm.go),
especially the five-field `ModelPricing` structure and `PricingBand` at lines
26-46, the four base fields parsed at lines 99-145, and band parsing at lines
149-219. Agentsview persists those complete bands and selects the highest band
whose threshold is below the request's total input tokens. The storage model is
in [`internal/db/pricing.go`](../../internal/db/pricing.go) at lines 14-32.

The restored embedded snapshot was generated from LiteLLM commit `418c7c...`.
Direct inspection of the snapshot confirms that Agentsview retained the Luna
greater-than-272,000 band and the Grok greater-than-200,000 band, not just their
flat rates. The pin is declared in
[`internal/pricing/cmd/litellm-snapshot/main.go`](../../internal/pricing/cmd/litellm-snapshot/main.go)
at line 44.

Agentsview currently drops the rest of LiteLLM's pricing metadata. That includes
batch, flex, priority, regional multipliers, general `tiered_pricing` arrays,
reasoning/audio/image/video units, alias arrays, fallback regex rules, source
URLs, and deprecation dates. This is a parser and storage limitation, not an
absence in LiteLLM.

## Consequence for date-dependent estimates

LiteLLM is a strong current-pricing and model-coverage source. It is already
good enough to cover Grok 4.6 and both standard Luna context bands in
Agentsview.

LiteLLM alone cannot answer, "What did this model cost at the event timestamp?"
The current JSON contains one current model object, and a refresh overwrites its
previous rates. Reconstructing old rates from LiteLLM would require Agentsview
to turn Git history or successive snapshots into its own effective-date history.
That history is not present as data that the normal LiteLLM pricing loader can
select.
