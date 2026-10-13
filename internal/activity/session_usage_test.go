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

func TestUsageDedupTokenIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, agent, messageID, requestID, uuid, key string
		ordinary                                     UsageDedupToken
	}{
		{
			name:      "Claude pair wins",
			agent:     "claude",
			messageID: "m",
			requestID: "r",
			uuid:      "u",
			key:       "k",
			ordinary:  UsageDedupToken{Kind: "claude", Value: "m:r"},
		},
		{
			name:      "incomplete pair falls back to message UUID",
			agent:     "claude",
			messageID: "m",
			uuid:      "u",
			key:       "k",
			ordinary:  UsageDedupToken{Kind: "source", Value: "claude:u"},
		},
		{
			name:     "UUID without agent falls back to stored key",
			uuid:     "u",
			key:      "k",
			ordinary: UsageDedupToken{Kind: "usage", Value: "k"},
		},
		{name: "no identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity := UsageIdentity{
				Agent:           tc.agent,
				ClaudeMessageID: tc.messageID, ClaudeRequestID: tc.requestID,
				SourceUUID: tc.uuid, UsageDedupKey: tc.key,
			}
			ordinary, ok := UsageDedupTokenForRow(identity)
			assert.Equal(t, tc.ordinary, ordinary)
			assert.Equal(t, tc.ordinary != UsageDedupToken{}, ok)
		})
	}
}

func TestSelectSessionUsageRows(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		rows                   []UsageRow
		wantIndexes            []int
		wantSearches           []int
		wantSurvivor           *UsageRow
		wantRaw, wantDiscarded map[string]int
		wantContributing       map[string]struct{}
	}{
		{
			name: "message UUID deduplicates across sessions",
			rows: []UsageRow{
				{SessionID: "root", UsageSource: "message", Agent: "codex", SourceUUID: "u", OutputTokens: 10},
				{SessionID: "child", UsageSource: "message", Agent: "codex", SourceUUID: "u", OutputTokens: 10},
			},
			wantIndexes: []int{0}, wantRaw: map[string]int{"root": 10, "child": 10},
			wantDiscarded: map[string]int{"child": 10}, wantContributing: map[string]struct{}{"child": {}},
		},
		{
			name:        "usage key removes noncontributing duplicate",
			rows:        []UsageRow{{SessionID: "root", UsageDedupKey: "k"}, {SessionID: "child", UsageDedupKey: "k"}},
			wantIndexes: []int{0}, wantRaw: map[string]int{"root": 0, "child": 0},
			wantDiscarded: map[string]int{"child": 0}, wantContributing: map[string]struct{}{},
		},
		{
			name: "Claude snapshot preserves survivor fields and attribution",
			rows: []UsageRow{
				{
					SessionID:         "root",
					Timestamp:         "2026-06-16T10:00:00Z",
					InputTokens:       10,
					OutputTokens:      5,
					WebSearchRequests: 3,
					ClaudeMessageID:   "m",
					ClaudeRequestID:   "r",
				},
				{
					SessionID:         "child",
					Model:             "model-a",
					Timestamp:         "2026-06-16T10:00:01Z",
					InputTokens:       100,
					OutputTokens:      20,
					WebSearchRequests: 1,
					ClaudeMessageID:   "m",
					ClaudeRequestID:   "r",
				},
			},
			wantIndexes: []int{1}, wantSearches: []int{3},
			wantRaw:          map[string]int{"root": 5, "child": 20},
			wantDiscarded:    map[string]int{"root": 5, "child": 20},
			wantContributing: map[string]struct{}{"root": {}, "child": {}},
			wantSurvivor: &UsageRow{
				SessionID: "root", SourceSessionID: "child", Model: "model-a",
				Timestamp: "2026-06-16T10:00:01Z", InputTokens: 100,
				OutputTokens: 20, WebSearchRequests: 3,
				Cost: money.Money{Microdollars: 10},
			},
		},
		{
			name:        "empty rows retain empty collections",
			wantIndexes: []int{}, wantRaw: map[string]int{}, wantDiscarded: map[string]int{}, wantContributing: map[string]struct{}{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i, row := range tc.rows {
				tc.rows[i].Contributes = row.OutputTokens > 0
			}
			pricedIndexes := []int{}
			pricedSearches := []int{}
			result, err := SelectSessionUsageRows(t.Context(), tc.rows, func(i, searches int) (UsagePrice, error) {
				pricedIndexes = append(pricedIndexes, i)
				pricedSearches = append(pricedSearches, searches)
				return UsagePrice{
					Cost:        money.Money{Microdollars: int64(searches + 7)},
					SessionCost: &money.Money{Microdollars: 42},
					CostSource:  export.CostSourceReported, Priced: true, Contributes: true,
				}, nil
			})
			require.NoError(t, err)
			require.Len(t, result.Rows, len(tc.wantIndexes))
			require.NotNil(t, result.Rows)
			assert.Equal(t, tc.wantIndexes, pricedIndexes)
			assert.Equal(t, tc.wantRaw, result.RawOutputTokensBySession)
			assert.Equal(t, tc.wantDiscarded, result.DeduplicatedOutputTokens)
			assert.Equal(t, tc.wantContributing, result.DiscardedContributingSessions)
			if tc.wantSurvivor != nil {
				assert.Equal(t, tc.wantSearches, pricedSearches)
				row := result.Rows[0]
				assert.Equal(t, tc.wantSurvivor.SessionID, row.SessionID)
				assert.Equal(t, tc.wantSurvivor.SourceSessionID, row.SourceSessionID)
				assert.Equal(t, tc.wantSurvivor.Model, row.Model)
				assert.Equal(t, tc.wantSurvivor.Timestamp, row.Timestamp)
				assert.Equal(t, tc.wantSurvivor.InputTokens, row.InputTokens)
				assert.Equal(t, tc.wantSurvivor.OutputTokens, row.OutputTokens)
				assert.Equal(t, tc.wantSurvivor.WebSearchRequests, row.WebSearchRequests)
				assert.Equal(t, tc.wantSurvivor.Cost, row.Cost)
			}
			for _, row := range result.Rows {
				assert.Equal(t, export.CostSourceReported, row.CostSource)
				assert.Equal(t, &money.Money{Microdollars: 42}, row.SessionCost)
				assert.True(t, row.Priced)
				assert.True(t, row.Contributes)
			}
		})
	}
}

func TestSelectSessionUsageRowsErrors(t *testing.T) {
	priceErr := errors.New("pricing unavailable")
	for _, tc := range []struct {
		name           string
		rows           []UsageRow
		cancelAfter    bool
		priceErr       error
		want           error
		wantPriceCalls int
	}{
		{name: "pricing", rows: []UsageRow{{SessionID: "root"}}, priceErr: priceErr, want: priceErr, wantPriceCalls: 1},
		{
			name:           "canceled between pricing calls",
			rows:           []UsageRow{{SessionID: "root"}, {SessionID: "child"}},
			cancelAfter:    true,
			want:           context.Canceled,
			wantPriceCalls: 1,
		},
		{
			name:           "canceled after last pricing call",
			rows:           []UsageRow{{SessionID: "root"}},
			cancelAfter:    true,
			want:           context.Canceled,
			wantPriceCalls: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			result, err := SelectSessionUsageRows(ctx, tc.rows, func(int, int) (UsagePrice, error) {
				calls++
				if tc.priceErr != nil {
					return UsagePrice{}, tc.priceErr
				}
				if tc.cancelAfter {
					cancel()
				}
				return UsagePrice{}, nil
			})
			require.ErrorIs(t, err, tc.want)
			assert.Nil(t, result)
			assert.Equal(t, tc.wantPriceCalls, calls)
		})
	}
}
