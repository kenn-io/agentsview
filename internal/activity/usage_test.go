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
	assert := assert.New(t)

	p := baseParams(t, "2026-06-16", "UTC")
	// Same logical usage from message source and usage_events source share
	// no claude IDs but share a dedup key -> must count once. A row outside
	// the range is dropped without claiming a key.
	usage := []UsageRow{
		{SessionID: "a", Model: "m1", Timestamp: "2026-06-16T10:00:00Z",
			InputTokens: 1000, OutputTokens: 100, Cost: money.MustParseDollars("1.0"), ClaudeMessageID: "x", ClaudeRequestID: "r"},
		{SessionID: "a", Model: "m1", Timestamp: "2026-06-16T10:00:00Z",
			InputTokens: 1000, OutputTokens: 100, Cost: money.MustParseDollars("1.0"), ClaudeMessageID: "x", ClaudeRequestID: "r"},
		{SessionID: "a", Model: "m1", Timestamp: "2026-06-15T23:00:00Z",
			InputTokens: 9999, OutputTokens: 999, Cost: money.MustParseDollars("9.0"), UsageDedupKey: "k-out"},
	}
	start := mustStart(t, "2026-06-16T00:00:00Z")
	end := mustStart(t, "2026-06-17T00:00:00Z")
	windows, err := BuildBuckets(start, end, p.Bucket, p.Loc)
	require.NoError(t, err)
	r := Report{Buckets: make([]Bucket, len(windows))}
	applyUsage(&r, p, windows, start, end, usage, nil)
	assert.Equal(100, r.Totals.OutputTokens)
	assert.Equal(money.MustParseDollars("1.0"), r.Totals.Cost)
	// A nil automated set classifies every session as interactive.
	assert.Equal(money.MustParseDollars("1.0"), r.Totals.InteractiveCost)
	assert.Equal(money.MustParseDollars("0.0"), r.Totals.AutomatedCost)
	// 10:00 UTC -> bucket 120 (10*12).
	assert.Equal(1000, r.Buckets[120].InputTokens)
	assert.Equal(100, r.Buckets[120].OutputTokens)
	assert.Equal(money.MustParseDollars("1.0"), r.Buckets[120].Cost)
}

func TestApplyUsage_PrefersCompleteClaudeSnapshotAcrossSessions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	p := baseParams(t, "2026-06-16", "UTC")
	usage := []UsageRow{
		{
			SessionID: "root", Model: "m1",
			Timestamp:    "2026-06-16T10:00:00Z",
			OutputTokens: 5, Cost: money.MustParseDollars("0.05"),
			ClaudeMessageID: "msg-stream", ClaudeRequestID: "req-stream",
		},
		{
			SessionID: "child", Model: "m1",
			Timestamp:    "2026-06-16T10:00:01Z",
			OutputTokens: 631, Cost: money.MustParseDollars("6.31"),
			ClaudeMessageID: "msg-stream", ClaudeRequestID: "req-stream",
		},
	}
	start := mustStart(t, "2026-06-16T00:00:00Z")
	end := mustStart(t, "2026-06-17T00:00:00Z")
	windows, err := BuildBuckets(start, end, p.Bucket, p.Loc)
	require.NoError(err)
	r := Report{Buckets: make([]Bucket, len(windows))}
	deduped := dedupUsage(start, end, p.EffectiveEnd, usage)
	require.Len(deduped, 1)
	assert.Equal("root", deduped[0].SessionID)
	assert.Equal(631, deduped[0].OutputTokens)

	applyUsage(&r, p, windows, start, end, usage, nil)

	assert.Equal(631, r.Totals.OutputTokens)
	assert.Equal(money.MustParseDollars("6.31"), r.Totals.Cost)
}

func TestDedupUsagePrefersLatestEqualOutputClaudeSnapshot(t *testing.T) {
	assert := assert.New(t)

	start := mustStart(t, "2026-06-16T00:00:00Z")
	end := mustStart(t, "2026-06-17T00:00:00Z")
	usage := []UsageRow{
		{
			SessionID: "root", Model: "m1",
			Timestamp:       "2026-06-16T10:00:00Z",
			InputTokens:     10,
			OutputTokens:    100,
			Cost:            money.MustParseDollars("1"),
			ClaudeMessageID: "msg-stream", ClaudeRequestID: "req-stream",
		},
		{
			SessionID: "child", Model: "m1",
			Timestamp:           "2026-06-16T10:01:00Z",
			InputTokens:         900,
			OutputTokens:        100,
			CacheCreationTokens: 200,
			CacheReadTokens:     300,
			WebSearchRequests:   2,
			Cost:                money.MustParseDollars("9"),
			ClaudeMessageID:     "msg-stream", ClaudeRequestID: "req-stream",
		},
	}

	deduped := dedupUsage(start, end, end, usage)
	require.Len(t, deduped, 1)
	assert.Equal("root", deduped[0].SessionID,
		"the earliest session retains attribution")
	assert.Equal(900, deduped[0].InputTokens)
	assert.Equal(100, deduped[0].OutputTokens)
	assert.Equal(200, deduped[0].CacheCreationTokens)
	assert.Equal(300, deduped[0].CacheReadTokens)
	assert.Equal(2, deduped[0].WebSearchRequests)
	assert.Equal(money.MustParseDollars("9"), deduped[0].Cost)
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

func TestClaudeSnapshotEquivalentInstantUsesSemanticTieBreakers(t *testing.T) {
	tests := []struct {
		name  string
		usage []UsageRow
		want  int
	}{
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
			assert := assert.New(t)

			mask, attribution, _ := ClaudeSnapshotSurvivorSelection(tt.usage)
			require.Len(t, mask, 2)
			assert.False(mask[1-tt.want])
			assert.True(mask[tt.want])
			assert.Equal(tt.usage[0].SessionID, attribution[tt.want])
		})
	}
}

func TestDedupUsagePreservesWebSearchesFromEarlierClaudeSnapshot(t *testing.T) {
	assert := assert.New(t)

	start := mustStart(t, "2026-06-16T00:00:00Z")
	end := mustStart(t, "2026-06-17T00:00:00Z")
	usage := []UsageRow{
		{
			SessionID: "root", Model: "m1",
			Timestamp:         "2026-06-16T10:00:00Z",
			OutputTokens:      100,
			WebSearchRequests: 2,
			ClaudeMessageID:   "msg-stream", ClaudeRequestID: "req-stream",
		},
		{
			SessionID: "child", Model: "m1",
			Timestamp:         "2026-06-16T10:01:00Z",
			OutputTokens:      200,
			WebSearchRequests: 0,
			ClaudeMessageID:   "msg-stream", ClaudeRequestID: "req-stream",
		},
	}

	deduped := dedupUsage(start, end, end, usage)
	require.Len(t, deduped, 1)
	assert.Equal("root", deduped[0].SessionID)
	assert.Equal(200, deduped[0].OutputTokens)
	assert.Equal(2, deduped[0].WebSearchRequests)
}

func TestApplyUsage_DedupBySourceUUIDFallback(t *testing.T) {
	assert := assert.New(t)

	p := baseParams(t, "2026-06-16", "UTC")
	usage := []UsageRow{
		{SessionID: "earlier", Model: "m1", Timestamp: "2026-06-16T10:00:00Z",
			OutputTokens: 500, Cost: money.MustParseDollars("5.0"), Agent: "claude",
			ClaudeMessageID: "dup-m", SourceUUID: "src-dup"},
		{SessionID: "later", Model: "m1", Timestamp: "2026-06-16T10:01:00Z",
			OutputTokens: 900, Cost: money.MustParseDollars("9.0"), Agent: "claude",
			ClaudeMessageID: "dup-m", SourceUUID: "src-dup"},
	}
	start := mustStart(t, "2026-06-16T00:00:00Z")
	end := mustStart(t, "2026-06-17T00:00:00Z")
	windows, err := BuildBuckets(start, end, p.Bucket, p.Loc)
	require.NoError(t, err)
	r := Report{Buckets: make([]Bucket, len(windows))}
	applyUsage(&r, p, windows, start, end, usage, nil)
	assert.Equal(500, r.Totals.OutputTokens)
	assert.Equal(money.MustParseDollars("5.0"), r.Totals.Cost)
	assert.Equal(500, r.Buckets[120].OutputTokens)
	assert.Equal(money.MustParseDollars("5.0"), r.Buckets[120].Cost)
}
