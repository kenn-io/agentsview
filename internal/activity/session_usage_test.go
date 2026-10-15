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
		name, source, agent, messageID, requestID, uuid, key string
		want                                                 UsageDedupToken
	}{
		{
			name:      "Claude pair wins",
			source:    "message",
			agent:     "claude",
			messageID: "m",
			requestID: "r",
			uuid:      "u",
			want:      UsageDedupToken{Kind: "claude", Value: "m:r"},
		},
		{
			name:      "incomplete pair falls back to message UUID",
			source:    "message",
			agent:     "claude",
			messageID: "m",
			uuid:      "u",
			want:      UsageDedupToken{Kind: "source", Value: "claude:u"},
		},
		{
			name:   "non-message source ignores source UUID",
			source: "session",
			agent:  "claude",
			uuid:   "u",
			key:    "k",
			want:   UsageDedupToken{Kind: "usage", Value: "k"},
		},
		{name: "no identity"},
		{name: "message UUID without agent has no identity", source: "message", uuid: "u"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity := UsageIdentity{
				UsageSource: tc.source, Agent: tc.agent,
				ClaudeMessageID: tc.messageID, ClaudeRequestID: tc.requestID,
				SourceUUID: tc.uuid, UsageDedupKey: tc.key,
			}
			got, ok := SessionUsageDedupTokenForRow(identity)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.want != UsageDedupToken{}, ok)
		})
	}
}

func TestSelectSessionUsageRows(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		rows                   []UsageRow
		wantIndexes            []int
		wantSearches           []int
		wantCost               *money.Money
		wantRaw, wantDiscarded map[string]int
		wantContributing       map[string]struct{}
	}{
		{
			name: "message UUID removes noncontributing duplicate",
			rows: []UsageRow{
				{SessionID: "root", UsageSource: "message", Agent: "codex", SourceUUID: "u"},
				{SessionID: "child", UsageSource: "message", Agent: "codex", SourceUUID: "u"},
			},
			wantIndexes: []int{0}, wantRaw: map[string]int{"root": 0, "child": 0},
			wantSearches:  []int{0},
			wantDiscarded: map[string]int{"child": 0}, wantContributing: map[string]struct{}{},
		},
		{
			name: "Claude snapshot forwards pricing and tracks contributing sessions",
			rows: []UsageRow{
				{
					SessionID:       "root",
					Timestamp:       "2026-06-16T10:00:00Z",
					InputTokens:     10,
					OutputTokens:    5,
					ClaudeMessageID: "m",
					ClaudeRequestID: "r",
				},
				{
					SessionID:         "child",
					WebSearchRequests: 2,
					Timestamp:         "2026-06-16T10:00:01Z",
					InputTokens:       100,
					OutputTokens:      20,
					ClaudeMessageID:   "m",
					ClaudeRequestID:   "r",
				},
			},
			wantIndexes:      []int{1},
			wantSearches:     []int{2},
			wantRaw:          map[string]int{"root": 5, "child": 20},
			wantDiscarded:    map[string]int{"root": 5, "child": 20},
			wantContributing: map[string]struct{}{"root": {}, "child": {}},
			wantCost:         &money.Money{Microdollars: 9},
		},
		{
			name:         "empty rows retain empty collections",
			wantSearches: []int{},
			wantIndexes:  []int{}, wantRaw: map[string]int{}, wantDiscarded: map[string]int{}, wantContributing: map[string]struct{}{},
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
				cost := money.Money{}
				if tc.rows[i].Contributes {
					cost.Microdollars = int64(searches + 7)
				}
				return UsagePrice{
					Cost: cost, CostSource: export.CostSourceComputed,
					Priced: true, Contributes: tc.rows[i].Contributes,
				}, nil
			})
			require.NoError(t, err)
			require.Len(t, result.Rows, len(tc.wantIndexes))
			require.NotNil(t, result.Rows)
			assert.Equal(t, tc.wantIndexes, pricedIndexes)
			assert.Equal(t, tc.wantSearches, pricedSearches)
			assert.Equal(t, tc.wantRaw, result.RawOutputTokensBySession)
			assert.Equal(t, tc.wantDiscarded, result.DeduplicatedOutputTokens)
			assert.Equal(t, tc.wantContributing, result.DiscardedContributingSessions)
			if tc.wantCost != nil {
				assert.Equal(t, *tc.wantCost, result.Rows[0].Cost)
				assert.Equal(t, 2, result.Rows[0].WebSearchRequests)
				assert.Equal(t, "root", result.Rows[0].SessionID)
				assert.Equal(t, "child", result.Rows[0].SourceSessionID)
				coverage, err := CanonicalSessionTokenCoverageContext(t.Context(), tc.rows)
				require.NoError(t, err)
				assert.Equal(t, coverage, result.CanonicalTokenCoverageBySession)
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
