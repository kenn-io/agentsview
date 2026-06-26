package activity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	// Same logical usage from message source and usage_events source share
	// no claude IDs but share a dedup key -> must count once. A row outside
	// the range is dropped without claiming a key.
	usage := []UsageRow{
		{SessionID: "a", Model: "m1", Timestamp: "2026-06-16T10:00:00Z",
			OutputTokens: 100, Cost: 1.0, ClaudeMessageID: "x", ClaudeRequestID: "r"},
		{SessionID: "a", Model: "m1", Timestamp: "2026-06-16T10:00:00Z",
			OutputTokens: 100, Cost: 1.0, ClaudeMessageID: "x", ClaudeRequestID: "r"},
		{SessionID: "a", Model: "m1", Timestamp: "2026-06-15T23:00:00Z",
			OutputTokens: 999, Cost: 9.0, UsageDedupKey: "k-out"},
	}
	start := mustStart(t, "2026-06-16T00:00:00Z")
	end := mustStart(t, "2026-06-17T00:00:00Z")
	windows, err := BuildBuckets(start, end, p.Bucket, p.Loc)
	require.NoError(t, err)
	r := Report{Buckets: make([]Bucket, len(windows))}
	applyUsage(&r, p, windows, start, end, usage, nil)
	assert.Equal(t, 100, r.Totals.OutputTokens)
	assert.InDelta(t, 1.0, r.Totals.Cost, 1e-9)
	// A nil automated set classifies every session as interactive.
	assert.InDelta(t, 1.0, r.Totals.InteractiveCost, 1e-9)
	assert.InDelta(t, 0.0, r.Totals.AutomatedCost, 1e-9)
	// 10:00 UTC -> bucket 120 (10*12).
	assert.Equal(t, 100, r.Buckets[120].OutputTokens)
	assert.InDelta(t, 1.0, r.Buckets[120].Cost, 1e-9)
}

func TestApplyUsage_DedupBySourceUUIDFallback(t *testing.T) {
	p := baseParams(t, "2026-06-16", "UTC")
	usage := []UsageRow{
		{SessionID: "earlier", Model: "m1", Timestamp: "2026-06-16T10:00:00Z",
			OutputTokens: 500, Cost: 5.0, Agent: "claude",
			ClaudeMessageID: "dup-m", SourceUUID: "src-dup"},
		{SessionID: "later", Model: "m1", Timestamp: "2026-06-16T10:01:00Z",
			OutputTokens: 900, Cost: 9.0, Agent: "claude",
			ClaudeMessageID: "dup-m", SourceUUID: "src-dup"},
	}
	start := mustStart(t, "2026-06-16T00:00:00Z")
	end := mustStart(t, "2026-06-17T00:00:00Z")
	windows, err := BuildBuckets(start, end, p.Bucket, p.Loc)
	require.NoError(t, err)
	r := Report{Buckets: make([]Bucket, len(windows))}
	applyUsage(&r, p, windows, start, end, usage, nil)
	assert.Equal(t, 500, r.Totals.OutputTokens)
	assert.InDelta(t, 5.0, r.Totals.Cost, 1e-9)
	assert.Equal(t, 500, r.Buckets[120].OutputTokens)
	assert.InDelta(t, 5.0, r.Buckets[120].Cost, 1e-9)
}
