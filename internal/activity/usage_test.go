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

func TestApplyUsage_DedupAndDayFilter(t *testing.T) {
	p := baseParams(t, "2026-06-16", "UTC")
	usage := []UsageRow{
		{
			SessionID: "a", Model: "m1", Timestamp: "2026-06-16T10:00:00Z",
			InputTokens: 1000, OutputTokens: 500, Cost: money.MustParseDollars("5.0"), Agent: "claude", ClaudeMessageID: "dup-m", SourceUUID: "src-dup",
		},
		{
			SessionID: "child", Model: "m1", Timestamp: "2026-06-16T10:00:01Z",
			InputTokens: 1000, OutputTokens: 900, Cost: money.MustParseDollars("9.0"), Agent: "claude", ClaudeMessageID: "dup-m", SourceUUID: "src-dup",
		},
		{
			SessionID: "root", Model: "m1", Timestamp: "2026-06-16T10:02:00Z",
			InputTokens: 10, OutputTokens: 5, Cost: money.MustParseDollars("0.05"),
			ClaudeMessageID: "snapshot", ClaudeRequestID: "request", WebSearchRequests: 4,
		},
		{
			SessionID: "child", Model: "m1", Timestamp: "2026-06-16T10:03:00Z",
			InputTokens: 900, OutputTokens: 631, Cost: money.MustParseDollars("6.31"),
			ClaudeMessageID: "snapshot", ClaudeRequestID: "request", WebSearchRequests: 1,
		},
		{
			SessionID: "a", Model: "m1", Timestamp: "2026-06-15T23:00:00Z",
			InputTokens: 9999, OutputTokens: 999, Cost: money.MustParseDollars("9.0"), UsageDedupKey: "k-out",
		},
	}
	start := mustStart(t, "2026-06-16T00:00:00Z")
	end := mustStart(t, "2026-06-17T00:00:00Z")
	survivors := dedupUsage(start, end, end, usage)
	require.Len(t, survivors, 2)
	assert.Equal(t, usage[0], survivors[0], "the source UUID fallback keeps the first row")
	assert.Equal(t, "root", survivors[1].SessionID, "the complete snapshot retains the earliest session attribution")
	assert.Equal(t, 4, survivors[1].WebSearchRequests, "the complete snapshot retains the maximum search count")
	windows, err := BuildBuckets(start, end, p.Bucket, p.Loc)
	require.NoError(t, err)
	r := Report{Buckets: make([]Bucket, len(windows))}
	applyUsage(&r, p, windows, start, end, usage, nil)
	assert.Equal(t, 1131, r.Totals.OutputTokens)
	assert.Equal(t, money.MustParseDollars("11.31"), r.Totals.Cost)
	// A nil automated set classifies every session as interactive.
	assert.Equal(t, money.MustParseDollars("11.31"), r.Totals.InteractiveCost)
	assert.Equal(t, money.MustParseDollars("0.0"), r.Totals.AutomatedCost)
	// 10:00 UTC -> bucket 120 (10*12).
	assert.Equal(t, 1900, r.Buckets[120].InputTokens)
	assert.Equal(t, 1131, r.Buckets[120].OutputTokens)
	assert.Equal(t, money.MustParseDollars("11.31"), r.Buckets[120].Cost)
}

func TestCanonicalSessionTokenCoverageCreditsEquivalentSnapshotCategories(t *testing.T) {
	usage := []UsageRow{
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
	}

	coverage, err := CanonicalSessionTokenCoverageContext(
		t.Context(), usage)
	require.NoError(t, err)

	want := SessionTokenCoverage{OutputTokens: 100, PeakContextTokens: 1400}
	assert.Equal(t, want, coverage["root"])
	assert.Equal(t, want, coverage["child"])
}

func TestCanonicalSessionTokenCoverageCreditsGenericDuplicateCategories(t *testing.T) {
	usage := []UsageRow{
		{
			SessionID: "root", InputTokens: 700, OutputTokens: 80,
			CacheReadTokens: 100, UsageDedupKey: "shared-usage",
		},
		{
			SessionID: "child", OutputTokens: 80,
			UsageDedupKey: "shared-usage",
		},
	}

	coverage, err := CanonicalSessionTokenCoverageContext(
		t.Context(), usage)
	require.NoError(t, err)

	want := SessionTokenCoverage{OutputTokens: 80, PeakContextTokens: 800}
	assert.Equal(t, want, coverage["root"])
	assert.Equal(t, want, coverage["child"])
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
			mask, _, _ := ClaudeSnapshotSurvivorSelection(tt.usage)
			require.Len(t, mask, 2)
			assert.False(t, mask[1-tt.want])
			assert.True(t, mask[tt.want])
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

	t.Run("all rows", func(t *testing.T) {
		mask, attribution, webSearchRequests := ClaudeSnapshotSurvivorSelection(usage)
		assert.Equal(t, []bool{false, false, true, true, true, false}, mask)
		assert.Equal(t, []string{"", "", "e", "x", "a", ""}, attribution)
		assert.Equal(t, []int{0, 0, 2, 4, 3, 0}, webSearchRequests)
	})

	t.Run("ineligible row excluded", func(t *testing.T) {
		mask, attribution, webSearchRequests := UsageSurvivorSelection(
			mustStart(t, "2026-06-16T10:00:00Z"),
			mustStart(t, "2026-06-17T00:00:00Z"),
			mustStart(t, "2026-06-17T00:00:00Z"),
			usage,
		)
		assert.Equal(t, []bool{false, false, true, true, true, false}, mask)
		assert.Equal(t, []string{"", "", "c", "x", "a", ""}, attribution)
		assert.Equal(t, []int{0, 0, 1, 4, 3, 0}, webSearchRequests)
	})
}
