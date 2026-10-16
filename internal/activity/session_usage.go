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

// SelectSessionUsageRows selects and prices survivors from sorted usage rows.
func SelectSessionUsageRows(
	ctx context.Context, rows []UsageRow,
	price func(int, int) (UsagePrice, error),
) (*SessionUsageRows, error) {
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
			if row.Contributes {
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
		if !KeepSessionUsageRow(seen, row.UsageIdentity()) {
			discard()
			continue
		}
		pricing, err := price(i, searches[i])
		if err != nil {
			return nil, err
		}
		result.Rows = append(result.Rows, UsageRow{
			SessionID:           attribution[i],
			SourceSessionID:     row.SessionID,
			Model:               row.Model,
			ProviderID:          row.ProviderID,
			Timestamp:           row.Timestamp,
			MessageOrdinal:      row.MessageOrdinal,
			UsageSource:         row.UsageSource,
			InputTokens:         row.InputTokens,
			OutputTokens:        row.OutputTokens,
			CacheCreationTokens: row.CacheCreationTokens,
			CacheReadTokens:     row.CacheReadTokens,
			WebSearchRequests:   searches[i],
			Cost:                pricing.Cost,
			CostSource:          pricing.CostSource,
			SessionCost:         pricing.SessionCost,
			Priced:              pricing.Priced,
			Contributes:         pricing.Contributes,
			Agent:               row.Agent,
			ClaudeMessageID:     row.ClaudeMessageID,
			ClaudeRequestID:     row.ClaudeRequestID,
			SourceUUID:          row.SourceUUID,
			UsageDedupKey:       row.UsageDedupKey,
		})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
