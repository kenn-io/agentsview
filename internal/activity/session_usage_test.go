package activity

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

func TestSelectSessionUsageRows(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		rows                   []UsageRow
		wantIndexes            []int
		wantRaw, wantDiscarded map[string]int
		wantContributing       map[string]struct{}
		wantSourceSessionID    string
		wantCost               money.Money
	}{
		{
			name: "complete snapshot keeps earliest attribution and maximum searches",
			rows: []UsageRow{
				{SessionID: "root", Timestamp: "2026-06-16T10:00:00Z", InputTokens: 10, OutputTokens: 5, WebSearchRequests: 3, ClaudeMessageID: "m", ClaudeRequestID: "r"},
				{SessionID: "child", Timestamp: "2026-06-16T10:00:01Z", InputTokens: 100, OutputTokens: 20, WebSearchRequests: 1, ClaudeMessageID: "m", ClaudeRequestID: "r"},
			},
			wantIndexes: []int{1}, wantRaw: map[string]int{"root": 5, "child": 20},
			wantDiscarded: map[string]int{"root": 5, "child": 20}, wantContributing: map[string]struct{}{"root": {}, "child": {}},
			wantSourceSessionID: "child", wantCost: money.Money{Microdollars: 10},
		},
		{
			name: "message UUID deduplicates while event UUID does not",
			rows: []UsageRow{
				{SessionID: "root", UsageSource: "message", Agent: "codex", SourceUUID: "u", OutputTokens: 10},
				{SessionID: "child", UsageSource: "message", Agent: "codex", SourceUUID: "u", OutputTokens: 10},
				{SessionID: "child", UsageSource: "event", Agent: "codex", SourceUUID: "u", OutputTokens: 2},
			},
			wantIndexes: []int{0, 2}, wantRaw: map[string]int{"root": 10, "child": 12},
			wantDiscarded: map[string]int{"child": 10}, wantContributing: map[string]struct{}{"child": {}},
		},
		{
			name:        "usage key removes noncontributing duplicate",
			rows:        []UsageRow{{SessionID: "root", UsageDedupKey: "k"}, {SessionID: "child", UsageDedupKey: "k"}},
			wantIndexes: []int{0}, wantRaw: map[string]int{"root": 0, "child": 0},
			wantDiscarded: map[string]int{"child": 0}, wantContributing: map[string]struct{}{},
		},
		{
			name:        "empty rows retain empty collections",
			wantIndexes: []int{}, wantRaw: map[string]int{}, wantDiscarded: map[string]int{}, wantContributing: map[string]struct{}{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contributes := make([]bool, len(tc.rows))
			for i, row := range tc.rows {
				contributes[i] = row.OutputTokens > 0
			}
			pricedIndexes := []int{}
			result, err := SelectSessionUsageRows(context.Background(), tc.rows, contributes, func(i, searches int) (UsageRow, error) {
				pricedIndexes = append(pricedIndexes, i)
				row := tc.rows[i]
				row.Cost = money.Money{Microdollars: int64(searches + 7)}
				row.SessionCost = &money.Money{Microdollars: 42}
				row.CostSource = export.CostSourceReported
				row.Priced, row.Contributes = true, true
				return row, nil
			})
			require.NoError(t, err)
			require.Len(t, result.Rows, len(tc.wantIndexes))
			require.NotNil(t, result.Rows)
			assert.Equal(t, tc.wantIndexes, pricedIndexes)
			assert.Equal(t, tc.wantRaw, result.RawOutputTokensBySession)
			assert.Equal(t, tc.wantDiscarded, result.DeduplicatedOutputTokens)
			assert.Equal(t, tc.wantContributing, result.DiscardedContributingSessions)
			for _, row := range result.Rows {
				assert.Equal(t, export.CostSourceReported, row.CostSource)
				assert.Equal(t, &money.Money{Microdollars: 42}, row.SessionCost)
				assert.True(t, row.Priced)
				assert.True(t, row.Contributes)
			}
			if tc.wantSourceSessionID != "" {
				assert.Equal(t, "root", result.Rows[0].SessionID)
				assert.Equal(t, tc.wantSourceSessionID, result.Rows[0].SourceSessionID)
				assert.Equal(t, 3, result.Rows[0].WebSearchRequests)
				assert.Equal(t, tc.wantCost, result.Rows[0].Cost)
				assert.NotNil(t, result.CanonicalTokenCoverageBySession)
			}
		})
	}
}

func TestSelectSessionUsageRowsErrors(t *testing.T) {
	priceErr := errors.New("pricing unavailable")
	for _, tc := range []struct {
		name                       string
		cancelBefore, cancelDuring bool
		want                       error
	}{
		{"pricing", false, false, priceErr}, {"already canceled", true, false, context.Canceled}, {"canceled by pricing", false, true, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelBefore {
				cancel()
			}
			calls := 0
			result, err := SelectSessionUsageRows(ctx, []UsageRow{{SessionID: "root"}}, []bool{false}, func(int, int) (UsageRow, error) {
				calls++
				if tc.cancelDuring {
					cancel()
					return UsageRow{}, nil
				}
				return UsageRow{}, priceErr
			})
			assert.ErrorIs(t, err, tc.want)
			assert.Nil(t, result)
			if tc.cancelBefore {
				assert.Zero(t, calls)
			} else {
				assert.Equal(t, 1, calls)
			}
		})
	}
}
