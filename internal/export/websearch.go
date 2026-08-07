// ABOUTME: the flat per-request fee Anthropic charges for server-side web
// ABOUTME: search, applied on top of the token cost of a usage row.
package export

import (
	"fmt"

	"go.kenn.io/agentsview/internal/money"
)

// WebSearchRequestMicrodollars is what one Anthropic server-side web search
// request costs: $10 per 1,000 requests, i.e. $0.01 (10,000 microdollars)
// each. Source: https://docs.claude.com/en/docs/agents-and-tools/tool-use/web-search-tool
//
// This is deliberately a constant rather than a `model_pricing` row. The fee
// is charged per request and not per token, so it has no place in the
// per-million-token catalog the LiteLLM sync populates, and it does not vary
// by model.
const WebSearchRequestMicrodollars int64 = 10_000

// webSearchRequestsPerMillionRate expresses the same price as a
// per-million-requests rate so the fee can be computed with the exact
// big.Int arithmetic money.CostPerMillion already uses for token rates.
var webSearchRequestsPerMillionRate = money.Money{
	Microdollars: WebSearchRequestMicrodollars * 1_000_000,
}

// WebSearchFee returns the fee for the given number of Anthropic server-side
// web search requests. A count of zero or less costs nothing.
func WebSearchFee(requests int) (money.Money, error) {
	if requests <= 0 {
		return money.Money{}, nil
	}
	fee, err := money.CostPerMillion([]money.RatedTokens{{
		Tokens: int64(requests),
		Rate:   webSearchRequestsPerMillionRate,
	}})
	if err != nil {
		return money.Money{}, fmt.Errorf(
			"pricing %d web search requests: %w", requests, err)
	}
	return fee, nil
}

// AddWebSearchFee returns cost plus the fee for requests server-side web
// searches. It is the single place every backend adds the fee, so SQLite,
// PostgreSQL, and DuckDB cannot drift apart on the price.
func AddWebSearchFee(cost money.Money, requests int) (money.Money, error) {
	if requests <= 0 {
		return cost, nil
	}
	fee, err := WebSearchFee(requests)
	if err != nil {
		return money.Money{}, err
	}
	total, err := money.Add(cost, fee)
	if err != nil {
		return money.Money{}, fmt.Errorf(
			"adding web search fee for %d requests: %w", requests, err)
	}
	return total, nil
}
