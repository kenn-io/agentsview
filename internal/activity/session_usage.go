package activity

import (
	"context"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

// UsagePrice holds the backend pricing result for a surviving usage row.
type UsagePrice struct {
	Cost        money.Money
	CostSource  export.CostSource
	SessionCost *money.Money
	Priced      bool
	Contributes bool
}

// SelectSessionUsageRows selects and prices survivors from sorted rows with matching contribution flags.
func SelectSessionUsageRows(ctx context.Context, rows []UsageRow, contributes []bool, price func(int, int) (UsagePrice, error)) (*SessionUsageRows, error) {
	coverage, err := CanonicalSessionTokenCoverageContext(ctx, rows)
	if err != nil {
		return nil, err
	}
	mask, attribution, searches, err := ClaudeSnapshotSurvivorSelectionContext(ctx, rows)
	if err != nil {
		return nil, err
	}
	result := &SessionUsageRows{
		Rows:                            make([]UsageRow, 0, len(rows)),
		RawOutputTokensBySession:        make(map[string]int),
		DeduplicatedOutputTokens:        make(map[string]int),
		DiscardedContributingSessions:   make(map[string]struct{}),
		CanonicalTokenCoverageBySession: coverage,
	}
	seen := make(map[UsageDedupToken]struct{})
	for i, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result.RawOutputTokensBySession[row.SessionID] += row.OutputTokens
		discard := func() {
			result.DeduplicatedOutputTokens[row.SessionID] += row.OutputTokens
			if contributes[i] {
				result.DiscardedContributingSessions[row.SessionID] = struct{}{}
			}
		}
		if !mask[i] {
			discard()
			continue
		}
		if attribution[i] != row.SessionID {
			discard()
		}
		if key, ok := SessionUsageDedupTokenForRow(row.UsageSource, row.Agent, row.ClaudeMessageID, row.ClaudeRequestID, row.SourceUUID, row.UsageDedupKey); ok && !AddUsageDedupToken(seen, key, struct{}{}) {
			discard()
			continue
		}
		pricing, err := price(i, searches[i])
		if err != nil {
			return nil, err
		}
		priced := row
		priced.Cost = pricing.Cost
		priced.CostSource = pricing.CostSource
		priced.SessionCost = pricing.SessionCost
		priced.Priced = pricing.Priced
		priced.Contributes = pricing.Contributes
		priced.SourceSessionID = row.SessionID
		priced.SessionID = attribution[i]
		priced.WebSearchRequests = searches[i]
		result.Rows = append(result.Rows, priced)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
