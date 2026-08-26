# GPT-5.6 Sol pricing change research on 2026-08-26

## Conclusion

OpenAI did change GPT-5.6 Sol API pricing on 2026-08-21. Its dated
[API changelog](https://developers.openai.com/api/docs/changelog.md) says Sol
"now costs $4 per million input tokens and $20 per million output tokens," with
input 20% lower and output 33% lower. The current
[model page](https://developers.openai.com/api/docs/models/gpt-5.6-sol.md)
repeats those rates and calls them promotional pricing available at least
through 2026-11-21.

The official changelog establishes the change date and the new prices. It does
not print the old prices in the same entry. The old $5 input and $30 output
rates are consistent with the stated reductions and are preserved in
[Pydantic GenAI Prices PR #592](https://github.com/pydantic/genai-prices/pull/592)
and its merged
[commit `5b18e535`](https://github.com/pydantic/genai-prices/commit/5b18e535c85503cc7c438cb542adb241d431964d).
That change keeps the old rates before 2026-08-21 and adds a dated record for
the new rates beginning on 2026-08-21.

## Standard rates per million tokens

| Usage                      | Before 2026-08-21 | From 2026-08-21 |
| -------------------------- | ----------------: | --------------: |
| Short-context input        |             $5.00 |           $4.00 |
| Short-context cached input |             $0.50 |           $0.40 |
| Short-context cache write  |             $6.25 |           $5.00 |
| Short-context output       |            $30.00 |          $20.00 |
| Long-context input         |            $10.00 |           $8.00 |
| Long-context cached input  |             $1.00 |           $0.80 |
| Long-context cache write   |            $12.50 |          $10.00 |
| Long-context output        |            $45.00 |          $30.00 |

OpenAI's current
[pricing table](https://developers.openai.com/api/docs/pricing.md) confirms the
right-hand column. The Pydantic commit records both columns and tests the
boundary at `2026-08-20T23:59:59Z` and `2026-08-21T00:00:00Z`.

## Pydantic provenance

Pydantic's automated
[price discrepancy issue #588](https://github.com/pydantic/genai-prices/issues/588)
detected that its $5/$30 Sol record no longer matched OpenAI's $4/$20 page on
2026-08-24. PR #592 then preserved the original record and added the 2026-08-21
start-date condition instead of overwriting the old prices. The Agentsview
snapshot at upstream commit `83a49e8` includes that merged history.

The earlier cost comparison is therefore repricing pre-2026-08-21 Sol usage at a
documented historical rate, not assigning Sol a change that only applied to Luna
or Terra.
