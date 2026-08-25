# Pricing source survey on 2026-08-25

This note compares machine-readable AI pricing sources for Agentsview. It uses
the default branch or live API observed on 2026-08-25 and pins repository
citations to the audited commit. The two coverage probes are `xai/grok-4.6` and
`gpt-5.6-luna` because they expose the practical difference between a current
catalog and a historically useful one.

## Bottom line

LiteLLM is still the best primary source for current pricing. It covers both
probe models, added Grok 4.6 on release day, and already carries context bands,
batch, flex, priority, regional, cache, search, and non-token rates. Agentsview
currently discards most of that structure. That is an ingestion limitation, not
an upstream data gap.

Portkey Models is the strongest licensed alternative for current prices. Its
JSON represents Luna's context bands, execution modes, regions, cache rates, and
additional units more explicitly than LiteLLM's flat field namespace. It also
added Grok 4.6 on release day. It does not retain effective-dated price history,
however, so replacing LiteLLM with Portkey would not solve the issue's
historical-pricing requirement.

Pydantic GenAI Prices has the most useful general conditional schema. It can
select prices by start date and UTC time window, use tier arrays and arbitrary
units, and match provider and model aliases. Its current feed preserves both
Luna price periods but still has no Grok 4.6. It is a good historical and
conditional layer, not the primary current catalog.

RoninForge AI Price Index is the most deliberate history-first project in the
survey. Every record has effective dates, validation dates, confidence, and a
first-party source. Its coverage is still small, and its current Luna history
incorrectly applies the reduced July 30 price from the model's July 9 launch. It
is useful as evidence and a future source, but it is not ready to be the sole
historical authority.

No one source has LiteLLM's current coverage and Pydantic's conditional history.
The evidence supports a layered design: use LiteLLM for current broad coverage,
preserve more of its upstream fields, and let effective-dated GenAI Prices
records override it when an event timestamp selects a historical rate.

## Comparison

| Source                    | Probe coverage on 2026-08-25 | Pricing structure                                                                                                                              | Effective history                     | License and currentness                                                                               | Provenance                                                                                                              |
| ------------------------- | ---------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------- | ----------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------- |
| LiteLLM                   | Both                         | Very broad token and non-token fields; named context, batch, flex, priority, and regional fields; general tier arrays                          | No. Updates replace current fields    | MIT outside `enterprise/`; 3,174 current model records; audited commit `947dbbf` from 2026-08-22      | Independent community catalog with provider citations                                                                   |
| Portkey Models            | Both                         | Explicit context tiers, regions, standard/batch/flex/priority modes, cache, and arbitrary additional units                                     | No. Price updates overwrite records   | MIT; repository and catalog updated 2026-08-25; claims 2,000+ models across 40+ providers             | Independent community catalog with contributor source-link policy                                                       |
| Pydantic GenAI Prices v2  | Luna, not Grok 4.6           | Ordered conditional prices, start dates, UTC time windows, tier arrays, arbitrary units, provider fallbacks, and rich matching                 | Yes, when curated                     | MIT; 1,477 models across 36 providers; audited commit `83a49e8` from 2026-08-25                       | Curated composite; LiteLLM, OpenRouter, Helicone, and other sources are discrepancy inputs                              |
| models.dev                | Both                         | Standard/cache/audio token rates, formal context tiers, and named experimental modes                                                           | No                                    | MIT; 7,285 provider-model entries across 199 providers; audited commit `a4ba8de` from 2026-08-25      | Independent first-party provider records plus provider API synchronization; its OpenRouter provider reflects OpenRouter |
| Langfuse                  | Luna, not Grok 4.6           | Ordered tiers with numeric usage conditions, attribute conditions, regex model matching, and arbitrary usage keys                              | No                                    | MIT Expat outside enterprise paths; 167 default records; audited commit `e491ab9` from 2026-08-25     | Independent curated defaults                                                                                            |
| OpenRouter                | Both                         | Current routed-model price fields, context overrides, batch-qualified model IDs, cache, web search, images, audio, and reasoning               | No                                    | Live API had 419 models; no open-data license accompanies the API or documentation                    | Independent live marketplace data; prices describe OpenRouter routes, not necessarily first-party list prices           |
| Helicone                  | Neither on audited `main`    | Threshold arrays, provider and model aliases, cache multipliers, requests, web search, and multimodal rates; legacy rows can have a date range | Partial legacy shape only             | Apache-2.0; audited `main` commit `67df07b` from 2026-07-21                                           | Mixed. First-party records are authored; OpenRouter fallback records are generated from OpenRouter                      |
| AgentOps tokencost        | Neither                      | LiteLLM's older flat per-model objects; consumer exposes prompt and completion estimates                                                       | No                                    | MIT; last catalog update was 2025-09-05                                                               | Direct LiteLLM repackaging                                                                                              |
| RoninForge AI Price Index | Both                         | Price records with variations, several units, effective intervals, validation date, confidence, and source                                     | Yes by schema; coverage is incomplete | Data CC BY 4.0, tooling MIT; 108 models across 11 providers; daily export at audited commit `2322493` | Independent first-party transcription; aggregators are detectors and cross-checks only                                  |

## LiteLLM

The audited
[`model_prices_and_context_window.json`](https://github.com/BerriAI/litellm/blob/947dbbf0298eecd3d226f765d4a4fb7ea3fd2551/model_prices_and_context_window.json)
contains 3,174 model objects. Its
[`gpt-5.6-luna` record](https://github.com/BerriAI/litellm/blob/947dbbf0298eecd3d226f765d4a4fb7ea3fd2551/model_prices_and_context_window.json#L26271)
has standard and greater-than-272K input, output, cache-write, and cache-read
prices. It also has batch, flex, priority, EU and US uplifts, and search-query
pricing. The
[`xai/grok-4.6` record](https://github.com/BerriAI/litellm/blob/947dbbf0298eecd3d226f765d4a4fb7ea3fd2551/model_prices_and_context_window.json#L43373)
has standard and greater-than-200K input, output, and cache-read prices.

Grok 4.6 arrived in a
[`day-0 catalog commit`](https://github.com/BerriAI/litellm/commit/928dfab65c59b86317a8b4336734ed6177b2d9c8).
The Luna reduction demonstrates the history limitation: commit
[`f1b781d`](https://github.com/BerriAI/litellm/commit/f1b781d06b6155df7c8979110ddc45938c3b81fb)
replaced the prior prices. The current JSON has no effective-start or
effective-end field. The old values remain in Git history, not in the normal
pricing document.

The catalog is MIT because it sits outside LiteLLM's enterprise directory; the
exact terms are in the audited
[`LICENSE`](https://github.com/BerriAI/litellm/blob/947dbbf0298eecd3d226f765d4a4fb7ea3fd2551/LICENSE).
The more detailed field inventory and the gap between upstream and Agentsview
are documented in
[`litellm-pricing-capabilities-2026-08-24.md`](litellm-pricing-capabilities-2026-08-24.md).

## Portkey Models

Portkey publishes provider JSON files and an unauthenticated API. Its
[`README`](https://github.com/Portkey-AI/models/blob/5518f1e3ed1e08a5dd5bab93c6a3b2052c5a7cfa/README.md)
documents cents-per-token units, token and cache fields, arbitrary
`additional_units`, and a separate batch configuration. The audited
[`gpt-5.6-luna` object](https://github.com/Portkey-AI/models/blob/5518f1e3ed1e08a5dd5bab93c6a3b2052c5a7cfa/pricing/openai.json)
goes further. Its `custom_pricing` tree has a 272K context map, default and
data-residency regions, and standard, batch, flex, and priority execution modes.
Each leaf retains input, output, cache, web-search, and file-search rates.

The audited
[`grok-4.6` object](https://github.com/Portkey-AI/models/blob/5518f1e3ed1e08a5dd5bab93c6a3b2052c5a7cfa/pricing/x-ai.json)
has current input, output, cache-read, web-search, X-search, code-execution,
attachment-search, and file-search rates. It lacks Grok's greater-than-200K
band, so the schema is capable but this record is incomplete compared with
LiteLLM, models.dev, OpenRouter, and RoninForge.

Portkey added Grok in
[`d559716`](https://github.com/Portkey-AI/models/commit/d5597164783c5cf6bee7f1f59f31d8dcc99d4d23)
on 2026-08-12. It updated Luna on July 30 in
[`6da7d21`](https://github.com/Portkey-AI/models/commit/6da7d21065244f23b3c2b4f33a5f45cb7e3f2d32).
That diff overwrote both the normal and date-stamped model objects. A dated
model ID identifies a provider model snapshot; it is not an effective-dated
price series. The repository is
[`MIT`](https://github.com/Portkey-AI/models/blob/5518f1e3ed1e08a5dd5bab93c6a3b2052c5a7cfa/LICENSE).

Portkey is attractive for Go embedding because the source is already plain
provider JSON. Its explicit nested condition dimensions are easier to preserve
than LiteLLM's growing set of suffix-named fields. It remains a second current
catalog, though, and would not remove the need for history data.

## Pydantic GenAI Prices

The live v2 feed at audited commit `83a49e8` has 36 providers and 1,477 models.
The
[`data.json`](https://github.com/pydantic/genai-prices/blob/83a49e8b386176a1e28e9d9aedeea5e2b4abc586/prices/new_data/v2/data.json)
contains OpenAI and AWS Luna records but no Grok 4.6. The OpenAI record has the
old $1/$6 short-context rates, then a `start_date: 2026-07-30` record with the
new $0.20/$1.20 rates. Both price sets include the 272K whole-request tiers and
cache prices.

The typed source contract supports ordered conditional prices, start-date and
timezone-aware daily windows, numeric tier arrays, arbitrary price units,
provider fallbacks, and exact, prefix, suffix, contains, regex, OR, and AND
matching. See
[`prices_types.py`](https://github.com/pydantic/genai-prices/blob/83a49e8b386176a1e28e9d9aedeea5e2b4abc586/prices/src/prices/prices_types.py)
and the generated
[`v2 JSON Schema`](https://github.com/pydantic/genai-prices/blob/83a49e8b386176a1e28e9d9aedeea5e2b4abc586/prices/new_data/v2/data.schema.json).

This is not an independent price census. The project's
[`pricing-data README`](https://github.com/pydantic/genai-prices/blob/83a49e8b386176a1e28e9d9aedeea5e2b4abc586/prices/README.md)
says it downloads Helicone, OpenRouter, LiteLLM, and Simon Willison's prices to
inject discrepancies for manual resolution. Its
[`LiteLLM importer`](https://github.com/pydantic/genai-prices/blob/83a49e8b386176a1e28e9d9aedeea5e2b4abc586/prices/src/prices/source_litellm.py)
and
[`OpenRouter importer`](https://github.com/pydantic/genai-prices/blob/83a49e8b386176a1e28e9d9aedeea5e2b4abc586/prices/src/prices/source_openrouter.py)
show what each comparison source contributes. The curated provider YAML is the
published authority, so it can add history the inputs lack, but update speed
depends on that review step. The repository is
[`MIT`](https://github.com/pydantic/genai-prices/blob/83a49e8b386176a1e28e9d9aedeea5e2b4abc586/LICENSE).

The versioned JSON is easy to embed unchanged in Go. Its missing Grok coverage
means lookup must fall through to a current catalog.

## models.dev

models.dev's live `api.json` had 7,285 provider-model entries across 199
providers. The first-party source TOMLs contain both probe models and cite the
provider pricing pages:

- [`providers/openai/models/gpt-5.6-luna.toml`](https://github.com/anomalyco/models.dev/blob/a4ba8deb3229fa1df7b0901da2b577dbeec798ee/providers/openai/models/gpt-5.6-luna.toml)
  has standard and 272K-tier costs plus an experimental fast mode.
- [`providers/xai/models/grok-4.6.toml`](https://github.com/anomalyco/models.dev/blob/a4ba8deb3229fa1df7b0901da2b577dbeec798ee/providers/xai/models/grok-4.6.toml)
  has standard and 200K-tier costs.

The normalized
[`Cost` schema](https://github.com/anomalyco/models.dev/blob/a4ba8deb3229fa1df7b0901da2b577dbeec798ee/packages/core/src/schema.ts)
covers input, output, reasoning, cache read and write, audio token prices, and
formal context tiers. Named experimental modes can attach a different cost and
provider request shape. It has no general condition language, effective price
dates, time windows, or non-token billing units. Provider and model IDs are
exact; build-time `base_model` inheritance is not runtime alias matching.

Grok landed in
[`2f03855`](https://github.com/anomalyco/models.dev/commit/2f038556752cc5c05febea83789e266903738491)
on release day, and Luna's cut landed in
[`9b6e58f`](https://github.com/anomalyco/models.dev/commit/9b6e58f1e296f12af4d06a04bb216dcf73baba5a)
on 2026-07-31. The project is
[`MIT`](https://github.com/anomalyco/models.dev/blob/a4ba8deb3229fa1df7b0901da2b577dbeec798ee/LICENSE).
The generated JSON is easy to embed in Go, but it is a current provider
capability catalog rather than historical pricing data.

## Langfuse

Langfuse's audited
[`default-model-prices.json`](https://github.com/langfuse/langfuse/blob/e491ab99300221380e7c650a2b5f9a15f176a3c2/worker/src/constants/default-model-prices.json)
has 167 records. It contains Luna with six ordered tiers: standard, fast, flex,
greater than 272K, fast plus greater than 272K, and flex plus greater than 272K.
It has no Grok 4.6.

The tier matcher evaluates numeric usage-detail regexes and exact attribute
values, ANDs conditions, sorts non-default tiers by ascending priority, and
selects the first match before falling back to the default. The implementation
is in
[`matcher.ts`](https://github.com/langfuse/langfuse/blob/e491ab99300221380e7c650a2b5f9a15f176a3c2/packages/shared/src/server/pricing-tiers/matcher.ts),
and the accepted condition shapes are in
[`validation.ts`](https://github.com/langfuse/langfuse/blob/e491ab99300221380e7c650a2b5f9a15f176a3c2/packages/shared/src/features/model-pricing/validation.ts).
The defaults currently use `service_tier` and `speed`; arbitrary exact metadata
keys could encode a region. There is no date or time comparison. `createdAt` and
`updatedAt` are record maintenance metadata, not selectors.

The default catalog is independent curated data and has an automated
[`model-price audit workflow`](https://github.com/langfuse/langfuse/blob/e491ab99300221380e7c650a2b5f9a15f176a3c2/.github/workflows/model-price-audit.yml).
It is outside Langfuse's enterprise paths, so the root
[`LICENSE`](https://github.com/langfuse/langfuse/blob/e491ab99300221380e7c650a2b5f9a15f176a3c2/LICENSE)
applies MIT Expat terms. The JSON is straightforward to embed, but the missing
Grok record and lack of history make it weaker than LiteLLM or Portkey for this
repository.

## OpenRouter

The unauthenticated live
[`GET /api/v1/models`](https://openrouter.ai/api/v1/models) returned 419 models.
It contains Grok 4.6 with $2/$6 standard rates, a 200K override to $4/$12,
cache-read overrides, and web-search pricing. It contains standard and `:batch`
Luna IDs, each with 272K overrides, cache rates, and web-search pricing. The
official
[`models API reference`](https://openrouter.ai/docs/api/api-reference/models/list-all-models-and-their-properties)
documents the model and price response.

Across the live response, pricing objects used prompt, completion, cache read
and write, one-hour cache write, internal reasoning, audio, image, and web
search fields. The `created` timestamp describes the model listing, not when a
price took effect. There is no effective price interval or historical price
array.

OpenRouter is a marketplace and router, so its values answer what an OpenRouter
route costs. They are not automatically the first-party provider's standard list
price. This is visible in the Luna case: OpenRouter has distinct standard and
batch IDs, while older consumers have mistaken the discounted route for the
standard price.

The live API is easy to parse in Go but unsuitable as the only embedded snapshot
without a redistribution decision. Neither the API reference nor the official
[`Terms of Service`](https://openrouter.ai/terms) attaches an open-data license
to the catalog. That is not proof that redistribution is forbidden; it means
this survey found no affirmative open license comparable with MIT or CC BY 4.0.

## Helicone

At audited `main` commit
[`67df07b`](https://github.com/Helicone/helicone/commit/67df07b8d807a960f2e53d9ec2a9c49513ca2379),
Helicone has neither target model. The xAI catalog stops before Grok 4.6 in
[`models.ts`](https://github.com/Helicone/helicone/blob/67df07b8d807a960f2e53d9ec2a9c49513ca2379/packages/cost/models/authors/xai/models.ts),
and the OpenAI catalog tree reaches GPT-5.4 but not 5.6.

The current registry's
[`ModelPricing`](https://github.com/Helicone/helicone/blob/67df07b8d807a960f2e53d9ec2a9c49513ca2379/packages/cost/models/types.ts)
supports threshold arrays, input and output rates, cache multipliers and
storage, thinking, request, web-search, and per-modality pricing. Endpoint
records add provider IDs, aliases, regions, locations, and priorities. These
TypeScript modules are less convenient for Go embedding than a checked-in JSON
artifact.

Helicone's older
[`ModelRow`](https://github.com/Helicone/helicone/blob/67df07b8d807a960f2e53d9ec2a9c49513ca2379/packages/cost/interfaces/Cost.ts)
allows a `dateRange`, and the
[`llm-costs` API route](https://github.com/Helicone/helicone/blob/67df07b8d807a960f2e53d9ec2a9c49513ca2379/bifrost/app/api/llm-costs/route.ts)
returns it. The newer registry type has no effective-date field, so history is a
partial legacy capability rather than a general contract.

The first-party author modules are independently authored. OpenRouter fallbacks
are not independent: the
[`OpenRouter endpoint guide`](https://github.com/Helicone/helicone/blob/67df07b8d807a960f2e53d9ec2a9c49513ca2379/packages/cost/models/authors/OPENROUTER_ENDPOINT_GUIDE.md)
and
[`buildPricesOpenRouter.py`](https://github.com/Helicone/helicone/blob/67df07b8d807a960f2e53d9ec2a9c49513ca2379/packages/buildPricesOpenRouter.py)
fetch OpenRouter's API directly. Helicone is
[`Apache-2.0`](https://github.com/Helicone/helicone/blob/67df07b8d807a960f2e53d9ec2a9c49513ca2379/LICENSE).

## AgentOps tokencost

tokencost is not an independent pricing source. Its
[`update_prices.py`](https://github.com/AgentOps-AI/tokencost/blob/e7f7c1928084af55bb4955102327982401ed8649/update_prices.py)
calls the package's LiteLLM refresh, compares that result with the checked-in
JSON, and writes the refreshed LiteLLM objects. A
[`daily workflow`](https://github.com/AgentOps-AI/tokencost/blob/e7f7c1928084af55bb4955102327982401ed8649/.github/workflows/update-prices.yml)
once automated that copy.

The audited
[`model_prices.json`](https://github.com/AgentOps-AI/tokencost/blob/e7f7c1928084af55bb4955102327982401ed8649/tokencost/model_prices.json)
has neither probe model. Its last catalog commit was 2025-09-05, almost a year
before the models in this survey. The project is
[`MIT`](https://github.com/AgentOps-AI/tokencost/blob/e7f7c1928084af55bb4955102327982401ed8649/LICENSE),
but embedding it would add a stale copy of a source Agentsview already uses.

## RoninForge AI Price Index

RoninForge explicitly prioritizes dated first-party evidence over breadth. Its
[`README`](https://github.com/RoninForge/ai-price-index/blob/23224934f8e890c7c738c0222395fce34ca135ef/README.md)
describes point-in-time lookups and a flagship-first rollout. The observed
`current.json` contained 327 price variations for 108 models across 11
providers. It includes both probe models and all their standard and context band
token rates.

The
[`price-record JSON Schema`](https://github.com/RoninForge/ai-price-index/blob/23224934f8e890c7c738c0222395fce34ca135ef/schema/price-record.schema.json)
requires provider, model, variation, unit, price, `effective_from`, validation
date, source, source kind, and confidence. It supports closed effective
intervals and input, output, cache, batch, context-tier, embedding, audio, and
image variations across token, image, minute, character, and request units. Its
[`methodology`](https://github.com/RoninForge/ai-price-index/blob/23224934f8e890c7c738c0222395fce34ca135ef/METHODOLOGY.md)
says aggregators only detect discrepancies; a first-party source must confirm
published values. Daily generated and signed export commits show active
maintenance.

There is a material Luna-history error in the audited data. The
[`OpenAI records`](https://github.com/RoninForge/ai-price-index/blob/23224934f8e890c7c738c0222395fce34ca135ef/data/records/openai.json)
apply the reduced $0.20/$1.20 rates from `effective_from: 2026-07-09` and have
no preceding $1/$6 interval. LiteLLM's price-cut diff, Portkey's July 30 diff,
models.dev's source comment, and Pydantic's conditional record all place the cut
on July 30. RoninForge's current rates are correct, but its advertised
point-in-time answer is wrong for Luna usage between July 9 and July 29.

The
[`xAI records`](https://github.com/RoninForge/ai-price-index/blob/23224934f8e890c7c738c0222395fce34ca135ef/data/records/xai.json)
correctly include Grok 4.6 standard and 200K-tier rates from 2026-08-12. The
data is
[`CC BY 4.0`](https://github.com/RoninForge/ai-price-index/blob/23224934f8e890c7c738c0222395fce34ca135ef/DATA-LICENSE.md),
which permits embedding with attribution. The project is worth monitoring and
could supply corroborating historical records, but its narrow coverage and the
Luna error rule it out as the sole source now.

## Recommendation for Agentsview

1. Keep LiteLLM as the required current catalog. Its coverage and update speed
   are proven on both target models.
1. Expand Agentsview's LiteLLM representation before adding another current
   catalog. Batch, flex, priority, regional, non-token, and general tier data
   already exist upstream.
1. Use Pydantic GenAI Prices as an optional first-pass historical and
   conditional layer. If it has a matching effective record, use it; otherwise
   fall through to current LiteLLM pricing. Preserve the upstream v2 JSON so
   new units and conditions do not require inventing another catalog format.
1. Treat Portkey as the best licensed current cross-check and possible future
   replacement input. It is fresh and structurally clean, but it does not add
   history and its Grok record currently omits the 200K band.
1. Do not add tokencost or Helicone as fallback catalogs. tokencost is a stale
   LiteLLM copy, and Helicone misses both probe models. OpenRouter is useful
   for route-specific gaps but has no effective history and no affirmative
   open catalog license. RoninForge is useful evidence, not yet an authority.

This arrangement avoids maintaining hand-authored prices in Agentsview. It also
keeps the two different problems separate: broad current coverage comes from
LiteLLM, while date-sensitive selection comes from the narrower source that
actually models dates.
