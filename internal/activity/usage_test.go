package activity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/money"
)

// mustStart parses an RFC3339 string used as the range-start anchor.
func mustStart(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return ts.UTC()
}

func TestAggregateUsageTotalsIncludeOnlySelectedSnapshot(t *testing.T) {
	p := baseParams(t, "2026-06-16", "UTC")
	usage := []UsageRow{
		{
			SessionID: "root", Model: "m1", Timestamp: "2026-06-16T10:00:00Z",
			InputTokens: 100, OutputTokens: 5, Cost: money.MustParseDollars("1.0"),
			ClaudeMessageID: "shared-message", ClaudeRequestID: "shared-request",
		},
		{
			SessionID: "child", Model: "m1", Timestamp: "2026-06-16T10:01:00Z",
			InputTokens: 1000, OutputTokens: 500, Cost: money.MustParseDollars("5.0"),
			ClaudeMessageID: "shared-message", ClaudeRequestID: "shared-request",
		},
	}
	sessions := []SessionMeta{{SessionID: "root"}, {SessionID: "child", IsAutomated: true}}
	r, err := Aggregate(p, sessions, nil, usage)
	require.NoError(t, err)
	assert.Equal(t, 500, r.Totals.OutputTokens)
	assert.Equal(t, money.MustParseDollars("5.0"), r.Totals.Cost)
	assert.Equal(t, "2026-06-16T10:00:00Z", r.Buckets[120].Start)
	assert.Equal(t, 1000, r.Buckets[120].InputTokens)
	assert.Equal(t, 500, r.Buckets[120].OutputTokens)
	assert.Equal(t, money.MustParseDollars("5.0"), r.Buckets[120].Cost)
	assert.Equal(t, money.MustParseDollars("5.0"), r.Totals.InteractiveCost)
	assert.Zero(t, r.Totals.AutomatedCost.Microdollars)
	bySession := make(map[string]SessionRow, len(r.BySession))
	for _, row := range r.BySession {
		bySession[row.SessionID] = row
	}
	require.Contains(t, bySession, "root")
	require.Contains(t, bySession, "child")
	assert.Equal(t, 500, bySession["root"].OutputTokens)
	assert.Equal(t, money.MustParseDollars("5.0"), bySession["root"].Cost)
	assert.Zero(t, bySession["child"].OutputTokens)
	assert.Zero(t, bySession["child"].Cost.Microdollars)
}

func TestCanonicalSessionTokenCoverageCreditsDuplicateCategories(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage []UsageRow
		want  SessionTokenCoverage
	}{
		{
			name: "claude snapshot",
			usage: []UsageRow{
				{
					SessionID: "root", Timestamp: "2026-06-16T10:00:00Z",
					InputTokens: 100, OutputTokens: 5,
					ClaudeMessageID: "msg-stream", ClaudeRequestID: "req-stream",
				},
				{
					SessionID: "child", Timestamp: "2026-06-16T10:01:00Z",
					InputTokens: 900, OutputTokens: 100,
					CacheCreationTokens: 200, CacheReadTokens: 300,
					ClaudeMessageID: "msg-stream", ClaudeRequestID: "req-stream",
				},
			},
			want: SessionTokenCoverage{OutputTokens: 100, PeakContextTokens: 1400},
		},
		{
			name: "generic duplicate",
			usage: []UsageRow{
				{
					SessionID: "root", InputTokens: 700, OutputTokens: 80,
					CacheReadTokens: 100, UsageDedupKey: "shared-usage",
				},
				{
					SessionID: "child", OutputTokens: 80,
					UsageDedupKey: "shared-usage",
				},
			},
			want: SessionTokenCoverage{OutputTokens: 80, PeakContextTokens: 800},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coverage, err := CanonicalSessionTokenCoverageContext(t.Context(), tc.usage)
			require.NoError(t, err)
			assert.Equal(t, tc.want, coverage["root"])
			assert.Equal(t, tc.want, coverage["child"])
		})
	}
}

func TestClaudeSnapshotPrefersLaterTimestampThenSemanticTieBreakers(t *testing.T) {
	tests := []struct {
		name  string
		usage []UsageRow
		want  int
	}{
		{
			name: "later instant",
			usage: []UsageRow{
				{
					SessionID: "root", Timestamp: "2026-06-16T10:00:00Z",
					OutputTokens: 100, ClaudeMessageID: "msg", ClaudeRequestID: "req",
				},
				{
					SessionID: "child", Timestamp: "2026-06-16T10:01:00Z",
					OutputTokens: 100, ClaudeMessageID: "msg", ClaudeRequestID: "req",
				},
			},
			want: 1,
		},
		{
			name: "session ID",
			usage: []UsageRow{
				{
					SessionID: "a-parent", Timestamp: "2026-06-16T10:00:00Z",
					OutputTokens: 100, ClaudeMessageID: "msg",
					ClaudeRequestID: "req",
				},
				{
					SessionID: "z-child", Timestamp: "2026-06-16T05:00:00-05:00",
					OutputTokens: 100, ClaudeMessageID: "msg",
					ClaudeRequestID: "req",
				},
			},
			want: 1,
		},
		{
			name: "message ordinal",
			usage: []UsageRow{
				{
					SessionID: "session", Timestamp: "2026-06-16T10:00:00Z",
					MessageOrdinal: 1, OutputTokens: 100,
					ClaudeMessageID: "msg", ClaudeRequestID: "req",
				},
				{
					SessionID: "session", Timestamp: "2026-06-16T05:00:00-05:00",
					MessageOrdinal: 2, OutputTokens: 100,
					ClaudeMessageID: "msg", ClaudeRequestID: "req",
				},
			},
			want: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mask, attribution, _ := ClaudeSnapshotSurvivorSelection(tt.usage)
			require.Len(t, mask, 2)
			assert.False(t, mask[1-tt.want])
			assert.True(t, mask[tt.want])
			assert.Equal(t, tt.usage[0].SessionID, attribution[tt.want])
		})
	}
}

// Two interleaved snapshot identities each take their winner, attribution
// and web-search count from three different rows, so a decision leaking
// across identities or rows changes the result.
func TestClaudeSnapshotSurvivorSelectionKeepsDecisionsPerIdentity(t *testing.T) {
	usage := []UsageRow{
		{
			SessionID: "b", Timestamp: "2026-06-16T10:00:02Z",
			OutputTokens: 10, ClaudeMessageID: "msg-a", ClaudeRequestID: "req",
		},
		{
			SessionID: "a", Timestamp: "2026-06-16T10:00:00Z",
			OutputTokens: 5, WebSearchRequests: 3,
			ClaudeMessageID: "msg-b", ClaudeRequestID: "req",
		},
		{
			SessionID: "c", Timestamp: "2026-06-16T10:00:01Z",
			OutputTokens: 50, WebSearchRequests: 1,
			ClaudeMessageID: "msg-a", ClaudeRequestID: "req",
		},
		{
			SessionID: "x", Timestamp: "2026-06-16T10:00:04Z",
			OutputTokens: 7, WebSearchRequests: 4,
			ClaudeMessageID: "msg-a",
		},
		{
			SessionID: "d", Timestamp: "2026-06-16T10:00:03Z",
			OutputTokens: 20, ClaudeMessageID: "msg-b", ClaudeRequestID: "req",
		},
		{
			SessionID: "e", Timestamp: "2026-06-16T09:59:59Z",
			OutputTokens: 1, WebSearchRequests: 2,
			ClaudeMessageID: "msg-a", ClaudeRequestID: "req",
		},
	}

	for _, tc := range []struct {
		name            string
		ranged          bool
		wantAttribution []string
		wantSearches    []int
	}{
		{"all rows", false, []string{"", "", "e", "x", "a", ""}, []int{0, 0, 2, 4, 3, 0}},
		{"ineligible row excluded", true, []string{"", "", "c", "x", "a", ""}, []int{0, 0, 1, 4, 3, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mask []bool
			var attribution []string
			var searches []int
			if tc.ranged {
				mask, attribution, searches = UsageSurvivorSelection(
					mustStart(t, "2026-06-16T10:00:00Z"),
					mustStart(t, "2026-06-17T00:00:00Z"),
					mustStart(t, "2026-06-17T00:00:00Z"), usage,
				)
			} else {
				mask, attribution, searches = ClaudeSnapshotSurvivorSelection(usage)
			}
			assert.Equal(t, []bool{false, false, true, true, true, false}, mask)
			assert.Equal(t, tc.wantAttribution, attribution)
			assert.Equal(t, tc.wantSearches, searches)
		})
	}
}

// Agent and source UUID deduplicate rows with an empty UsageSource after range filtering.
func TestUsageSurvivorSelectionFiltersRangeBeforeUngatedAgentSourceUUIDDedup(t *testing.T) {
	rows := []UsageRow{
		{SessionID: "out", Timestamp: "2026-06-15T23:00:00Z", OutputTokens: 1000, WebSearchRequests: 9},
		{SessionID: "a", Timestamp: "2026-06-16T10:00:00Z", OutputTokens: 10, WebSearchRequests: 2},
		{SessionID: "b", Timestamp: "2026-06-16T10:01:00Z", OutputTokens: 50, WebSearchRequests: 1},
	}
	for i := range rows {
		rows[i].Agent = "claude"
		rows[i].ClaudeMessageID = "msg"
		rows[i].SourceUUID = "source"
	}
	mask, attribution, searches := UsageSurvivorSelection(
		mustStart(t, "2026-06-16T00:00:00Z"),
		mustStart(t, "2026-06-17T00:00:00Z"),
		mustStart(t, "2026-06-17T00:00:00Z"), rows,
	)
	assert.Equal(t, []bool{false, true, false}, mask)
	assert.Equal(t, "a", attribution[1])
	assert.Equal(t, 2, searches[1])
}
