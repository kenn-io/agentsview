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
		ordinary, session                                    UsageDedupToken
	}{
		{
			name:      "Claude pair wins",
			source:    "event",
			agent:     "claude",
			messageID: "m",
			requestID: "r",
			uuid:      "u",
			key:       "k",
			ordinary:  UsageDedupToken{Kind: "claude", Value: "m:r"},
			session:   UsageDedupToken{Kind: "claude", Value: "m:r"},
		},
		{
			name:      "incomplete pair falls back to message UUID",
			source:    "message",
			agent:     "claude",
			messageID: "m",
			uuid:      "u",
			key:       "k",
			ordinary:  UsageDedupToken{Kind: "source", Value: "claude:u"},
			session:   UsageDedupToken{Kind: "source", Value: "claude:u"},
		},
		{
			name:     "event UUID falls back to stored session key",
			source:   "event",
			agent:    "codex",
			uuid:     "u",
			key:      "k",
			ordinary: UsageDedupToken{Kind: "source", Value: "codex:u"},
			session:  UsageDedupToken{Kind: "usage", Value: "k"},
		},
		{
			name:     "event UUID alone has no session identity",
			source:   "event",
			agent:    "codex",
			uuid:     "u",
			ordinary: UsageDedupToken{Kind: "source", Value: "codex:u"},
		},
		{
			name:     "UUID without agent falls back to stored key",
			source:   "message",
			uuid:     "u",
			key:      "k",
			ordinary: UsageDedupToken{Kind: "usage", Value: "k"},
			session:  UsageDedupToken{Kind: "usage", Value: "k"},
		},
		{name: "no identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity := UsageIdentity{
				UsageSource: tc.source, Agent: tc.agent,
				ClaudeMessageID: tc.messageID, ClaudeRequestID: tc.requestID,
				SourceUUID: tc.uuid, UsageDedupKey: tc.key,
			}
			ordinary, ok := UsageDedupTokenForRow(identity)
			assert.Equal(t, tc.ordinary, ordinary)
			assert.Equal(t, tc.ordinary != UsageDedupToken{}, ok)
			session, ok := SessionUsageDedupTokenForRow(identity)
			assert.Equal(t, tc.session, session)
			assert.Equal(t, tc.session != UsageDedupToken{}, ok)
		})
	}
}

func TestAddUsageDedupToken(t *testing.T) {
	seen := make(map[UsageDedupToken]struct{})
	token := UsageDedupToken{Kind: "source", Value: "agent:uuid"}
	assert.True(t, AddUsageDedupToken(seen, token))
	assert.False(t, AddUsageDedupToken(seen, token))
	assert.True(t, AddUsageDedupToken(seen, UsageDedupToken{
		Kind: "usage", Value: token.Value,
	}))
	assert.Len(t, seen, 2)
	assert.True(t, AddUsageDedupToken(make(map[UsageDedupToken]struct{}), token))
}

func TestSelectSessionUsageRows(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		rows                   []UsageRow
		wantIndexes            []int
		wantRaw, wantDiscarded map[string]int
		wantContributing       map[string]struct{}
	}{
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
			for i, row := range tc.rows {
				tc.rows[i].Contributes = row.OutputTokens > 0
			}
			pricedIndexes := []int{}
			result, err := SelectSessionUsageRows(t.Context(), tc.rows, func(i, searches int) (UsagePrice, error) {
				pricedIndexes = append(pricedIndexes, i)
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
			for _, row := range result.Rows {
				assert.Equal(t, export.CostSourceReported, row.CostSource)
				assert.Equal(t, &money.Money{Microdollars: 42}, row.SessionCost)
				assert.True(t, row.Priced)
				assert.True(t, row.Contributes)
			}
		})
	}
}

func TestSelectSessionUsageRowsPreservesSurvivorFields(t *testing.T) {
	rows := []UsageRow{
		{
			SessionID:         "root",
			Timestamp:         "2026-06-16T10:00:00Z",
			InputTokens:       10,
			OutputTokens:      5,
			WebSearchRequests: 3,
			ClaudeMessageID:   "m",
			ClaudeRequestID:   "r",
			Contributes:       true,
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
			Contributes:       true,
		},
	}
	var pricedIndexes, pricedSearches []int
	result, err := SelectSessionUsageRows(t.Context(), rows, func(i, searches int) (UsagePrice, error) {
		pricedIndexes = append(pricedIndexes, i)
		pricedSearches = append(pricedSearches, searches)
		return UsagePrice{Cost: money.Money{Microdollars: int64(searches + 7)}}, nil
	})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	assert.Equal(t, []int{1}, pricedIndexes)
	assert.Equal(t, []int{3}, pricedSearches)
	assert.Equal(t, "root", result.Rows[0].SessionID)
	assert.Equal(t, "child", result.Rows[0].SourceSessionID)
	assert.Equal(t, "model-a", result.Rows[0].Model)
	assert.Equal(t, rows[1].Timestamp, result.Rows[0].Timestamp)
	assert.Equal(t, 100, result.Rows[0].InputTokens)
	assert.Equal(t, 20, result.Rows[0].OutputTokens)
	assert.Equal(t, 3, result.Rows[0].WebSearchRequests)
	assert.Equal(t, money.Money{Microdollars: 10}, result.Rows[0].Cost)
	assert.NotNil(t, result.CanonicalTokenCoverageBySession)
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
