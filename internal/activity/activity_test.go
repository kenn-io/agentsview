package activity

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	require.NoError(t, err)
	return loc
}

// fixedNow is a far-future instant so test ranges are never "partial".
func fixedNow(t *testing.T) time.Time {
	t.Helper()
	n, err := time.Parse(time.RFC3339, "2030-01-01T00:00:00Z")
	require.NoError(t, err)
	return n
}

// baseParams resolves a one-day "day" Query for date/tz against a far-future
// now (so the day is complete) and copies it into aggregator Params.
func baseParams(t *testing.T, date, tz string) Params {
	t.Helper()
	q, err := ResolveQuery(QueryInput{Preset: "day", Date: date, Timezone: tz}, fixedNow(t))
	require.NoError(t, err)
	return paramsFromQuery(q)
}

// paramsFromQuery copies a resolved Query into the aggregator Params it feeds.
func paramsFromQuery(q Query) Params {
	return Params{
		RangeStart:    q.RangeStart,
		RangeEnd:      q.RangeEnd,
		Loc:           q.Loc,
		EffectiveEnd:  q.EffectiveEnd,
		Partial:       q.Partial,
		GapCapSeconds: q.GapCapSeconds,
		Bucket:        q.Bucket,
	}
}

func TestReportOmitsUnsetPricingMetadata(t *testing.T) {
	b, err := json.Marshal(Report{})
	require.NoError(t, err)

	assert.NotContains(t, string(b), `"pricing"`)
}

func TestReportEmitsEmptyProjectsMap(t *testing.T) {
	b, err := json.Marshal(Report{
		SchemaVersion: export.ActivityReportSchemaVersion,
		Projects:      map[string]export.ProjectMapEntry{},
	})
	require.NoError(t, err)

	assert.Contains(t, string(b), `"projects":{}`)
}

func TestAllocateUsageCostsDistributesSessionTotalByEstimatedCost(t *testing.T) {
	total := 0.03
	usage := []UsageRow{
		{SessionID: "s1", Model: "model-a", Cost: 10, Priced: true, Contributes: true},
		{SessionID: "s1", Model: "model-b", Cost: 20, SessionCost: &total, Priced: true, Contributes: true},
	}

	allocated := AllocateUsageCosts(usage)

	require.Len(t, allocated, 2)
	assert.InDelta(t, 0.01, allocated[0].Cost, 1e-12)
	assert.InDelta(t, 0.02, allocated[1].Cost, 1e-12)
	assert.Equal(t, export.CostSourceReported, allocated[0].CostSource)
	assert.Equal(t, export.CostSourceReported, allocated[1].CostSource)
	assert.Equal(t, total, allocated[0].Cost+allocated[1].Cost)
}

func TestAggregate_DayWindowUTC(t *testing.T) {
	r := Aggregate(baseParams(t, "2026-06-16", "UTC"), nil, nil, nil)
	assert.Equal(t, "2026-06-16T00:00:00Z", r.RangeStart)
	assert.Equal(t, "2026-06-17T00:00:00Z", r.RangeEnd)
	assert.Equal(t, "minute", r.BucketUnit)
	assert.Equal(t, 300, r.BucketSeconds)
	assert.Equal(t, 288, r.BucketCount)
	assert.False(t, r.Partial)
	assert.Equal(t, 288, r.ElapsedBucketCount)
	assert.Len(t, r.Buckets, 288)
	assert.Equal(t, "2026-06-16T00:00:00Z", r.Buckets[0].Start)
	assert.Equal(t, "2026-06-16T00:05:00Z", r.Buckets[0].End)
}

func TestAggregate_HourlyBucketRange(t *testing.T) {
	q, err := ResolveQuery(QueryInput{
		Preset: "custom", Timezone: "UTC",
		From: "2026-06-16T00:00:00Z", To: "2026-06-19T00:00:00Z", // 3 days -> hourly
	}, fixedNow(t))
	require.NoError(t, err)
	p := paramsFromQuery(q)
	act := []ActivityEvent{
		{SessionID: "a", Ordinal: 1, Timestamp: "2026-06-16T10:00:00Z", Role: "user"},
		{SessionID: "a", Ordinal: 2, Timestamp: "2026-06-16T10:30:00Z", Role: "assistant", Model: "m1"},
	}
	r := Aggregate(p, nil, act, nil)
	assert.Equal(t, "hour", r.BucketUnit)
	assert.Equal(t, 72, r.BucketCount, "3 days of hourly buckets")
	assert.Equal(t, "2026-06-16T10:00:00Z", r.Buckets[10].Start)
	assert.Equal(t, "2026-06-16T11:00:00Z", r.Buckets[10].End)
	// The 30-min gap caps to 5 min; that activity lands in the 10:00 bucket.
	assert.InDelta(t, 5.0, r.Buckets[10].AgentMinutes, 1e-9)
}

func TestAggregate_DailyCalendarBucketRange(t *testing.T) {
	q, err := ResolveQuery(QueryInput{Preset: "month", Date: "2026-06-10", Timezone: "UTC"}, fixedNow(t))
	require.NoError(t, err)
	p := paramsFromQuery(q)
	r := Aggregate(p, nil, nil, nil)
	assert.Equal(t, "day", r.BucketUnit)
	assert.Equal(t, 86400, r.BucketSeconds, "nominal day seconds")
	assert.Equal(t, 30, r.BucketCount, "June has 30 calendar-day buckets")
	assert.Equal(t, "2026-06-01T00:00:00Z", r.Buckets[0].Start)
	assert.Equal(t, "2026-06-02T00:00:00Z", r.Buckets[0].End)
}

func TestAggregate_ArbitraryRangeIntervalClip(t *testing.T) {
	// Range starts mid-day at 10:30, not midnight.
	q, err := ResolveQuery(QueryInput{
		Preset: "custom", Timezone: "UTC",
		From: "2026-06-16T10:30:00Z", To: "2026-06-16T12:00:00Z",
	}, fixedNow(t))
	require.NoError(t, err)
	p := paramsFromQuery(q)
	// Anchor at 10:28 (before range_start), successor at 10:40: interval
	// [10:28,10:33) clips to [10:30,10:33) = 3 min inside the range.
	act := []ActivityEvent{
		{SessionID: "a", Ordinal: 1, Timestamp: "2026-06-16T10:28:00Z", Role: "user"},
		{SessionID: "a", Ordinal: 2, Timestamp: "2026-06-16T10:40:00Z", Role: "assistant", Model: "m1"},
	}
	r := Aggregate(p, nil, act, nil)
	require.Len(t, r.Intervals, 1)
	assert.Equal(t, "2026-06-16T10:30:00Z", r.Intervals[0].Start, "clipped to range_start, not midnight")
	assert.Equal(t, "2026-06-16T10:33:00Z", r.Intervals[0].End)
	assert.InDelta(t, 3.0, r.Totals.AgentMinutes, 1e-9)
}

func TestAggregate_FutureRangeNoActivity(t *testing.T) {
	now, err := time.Parse(time.RFC3339, "2026-06-16T00:00:00Z")
	require.NoError(t, err)
	q, err := ResolveQuery(QueryInput{Preset: "day", Date: "2026-06-20", Timezone: "UTC"}, now)
	require.NoError(t, err)
	p := paramsFromQuery(q)
	r := Aggregate(p, nil, nil, nil)
	assert.True(t, r.Partial)
	assert.Equal(t, 0, r.ElapsedBucketCount, "fully future range elapses no buckets")
	assert.Equal(t, 288, r.BucketCount, "but the full day's buckets are still listed")
	assert.InDelta(t, 0.0, r.Totals.AgentMinutes, 1e-9)
}

func TestAggregate_DSTSpringForward23Hours(t *testing.T) {
	// America/New_York springs forward 2026-03-08 (23-hour local day).
	r := Aggregate(baseParams(t, "2026-03-08", "America/New_York"), nil, nil, nil)
	assert.Equal(t, 276, r.BucketCount) // 23h * 12
}

func TestAggregate_DSTFallBack25Hours(t *testing.T) {
	// America/New_York falls back 2026-11-01 (25-hour local day).
	r := Aggregate(baseParams(t, "2026-11-01", "America/New_York"), nil, nil, nil)
	assert.Equal(t, 300, r.BucketCount) // 25h * 12
}

func TestAggregate_SweepLineNonOverlapVsOverlap(t *testing.T) {
	p := baseParams(t, "2026-06-16", "UTC")
	// Two sessions each with two messages 1 min apart, in the SAME 5-min
	// bucket but never overlapping in time.
	act := []ActivityEvent{
		{SessionID: "a", Ordinal: 1, Timestamp: "2026-06-16T10:00:00Z", Role: "user"},
		{SessionID: "a", Ordinal: 2, Timestamp: "2026-06-16T10:01:00Z", Role: "assistant", Model: "m1"},
		{SessionID: "b", Ordinal: 1, Timestamp: "2026-06-16T10:03:00Z", Role: "user"},
		{SessionID: "b", Ordinal: 2, Timestamp: "2026-06-16T10:03:30Z", Role: "assistant", Model: "m1"},
	}
	r := Aggregate(p, nil, act, nil)
	assert.Equal(t, 1, r.Peak.Agents, "non-overlapping must peak at 1")

	// Now make them overlap: b starts inside a's interval.
	act[2].Timestamp = "2026-06-16T10:00:30Z"
	act[3].Timestamp = "2026-06-16T10:01:30Z"
	r = Aggregate(p, nil, act, nil)
	assert.Equal(t, 2, r.Peak.Agents, "overlapping must peak at 2")
}

func TestAggregate_AdjacentIntervalsOneSessionNotConcurrent(t *testing.T) {
	p := baseParams(t, "2026-06-16", "UTC")
	// A SINGLE session with three adjacent messages yields two abutting
	// half-open intervals [10:00,10:02) and [10:02,10:05). They share the
	// boundary 10:02 but must NOT be counted as overlapping there.
	act := []ActivityEvent{
		{SessionID: "a", Ordinal: 1, Timestamp: "2026-06-16T10:00:00Z", Role: "user"},
		{SessionID: "a", Ordinal: 2, Timestamp: "2026-06-16T10:02:00Z", Role: "assistant", Model: "m1"},
		{SessionID: "a", Ordinal: 3, Timestamp: "2026-06-16T10:05:00Z", Role: "assistant", Model: "m1"},
	}
	r := Aggregate(p, nil, act, nil)
	assert.Equal(t, 1, r.Peak.Agents, "abutting intervals from one session never overlap")
	for i, b := range r.Buckets {
		assert.LessOrEqualf(t, b.MaxAgents, 1, "bucket %d max_agents", i)
	}
}

func TestAggregate_PartialDayClipsUsage(t *testing.T) {
	loc := mustLoad(t, "UTC")
	start, err := time.Parse(time.RFC3339, "2026-06-16T00:00:00Z")
	require.NoError(t, err)
	end := start.AddDate(0, 0, 1)
	effEnd, err := time.Parse(time.RFC3339, "2026-06-16T12:00:00Z")
	require.NoError(t, err)
	p := Params{
		RangeStart: start, RangeEnd: end, Loc: loc,
		EffectiveEnd: effEnd, Partial: true,
		GapCapSeconds: 300, Bucket: BucketSpec{BucketMinute, 300},
	}
	usage := []UsageRow{
		{SessionID: "s1", Model: "m1", Timestamp: "2026-06-16T10:00:00Z",
			OutputTokens: 100, Cost: 1.0, ClaudeMessageID: "a", ClaudeRequestID: "x"},
		{SessionID: "s1", Model: "m1", Timestamp: "2026-06-16T14:00:00Z",
			OutputTokens: 200, Cost: 2.0, ClaudeMessageID: "b", ClaudeRequestID: "y"},
	}
	sessions := []SessionMeta{{SessionID: "s1", Project: "p", Agent: "claude"}}
	r := Aggregate(p, sessions, nil, usage)
	assert.True(t, r.Partial, "mid-day report must be partial")
	assert.Equal(t, 100, r.Totals.OutputTokens, "row at/after effEnd excluded from totals")
	assert.InDelta(t, 1.0, r.Totals.Cost, 1e-9)
	require.Len(t, r.BySession, 1)
	assert.Equal(t, 100, r.BySession[0].OutputTokens, "session row clipped to as_of")
	assert.InDelta(t, 1.0, r.BySession[0].Cost, 1e-9)
}

func TestAggregate_OverlapUnionVsSumAndPeakAt(t *testing.T) {
	p := baseParams(t, "2026-06-16", "UTC")
	// Two OVERLAPPING sessions on a full past day:
	//   a = [10:00, 10:03)  (3 min)
	//   b = [10:01, 10:05)  (4 min)
	// Active minutes are the UNION (10:00-10:05 = 5), agent-minutes are the
	// SUM (3+4 = 7). Asserting both proves union != sum here, so a regression
	// that dropped the `live > 0` guard in sweepLine (making active accumulate
	// the SUM) would be caught.
	act := []ActivityEvent{
		{SessionID: "a", Ordinal: 1, Timestamp: "2026-06-16T10:00:00Z", Role: "user"},
		{SessionID: "a", Ordinal: 2, Timestamp: "2026-06-16T10:03:00Z", Role: "assistant", Model: "m1"},
		{SessionID: "b", Ordinal: 1, Timestamp: "2026-06-16T10:01:00Z", Role: "user"},
		{SessionID: "b", Ordinal: 2, Timestamp: "2026-06-16T10:05:00Z", Role: "assistant", Model: "m1"},
	}
	r := Aggregate(p, nil, act, nil)
	assert.InDelta(t, 5.0, r.Totals.ActiveMinutes, 1e-9,
		"active minutes are the union 10:00-10:05, not the sum")
	assert.InDelta(t, 7.0, r.Totals.AgentMinutes, 1e-9,
		"agent minutes are the sum 3+4, proving union != sum")
	assert.Equal(t, 2, r.Peak.Agents, "both sessions live in [10:01,10:03)")
	require.NotNil(t, r.Peak.At, "peak instant must be reported")
	assert.Equal(t, "2026-06-16T10:01:00Z", *r.Peak.At,
		"peak first occurs when b opens at 10:01")
	// Full-day denominator: 1440 minutes minus the 5 active union minutes.
	assert.InDelta(t, 1435.0, r.Totals.IdleMinutes, 1e-9,
		"idle is the full-day 1440 minus active 5")
}

func TestAggregate_PartialDayClipsActivityAndBuckets(t *testing.T) {
	loc := mustLoad(t, "UTC")
	// "Today" with effEnd mid-day at 12:00 makes the report partial.
	start, err := time.Parse(time.RFC3339, "2026-06-16T00:00:00Z")
	require.NoError(t, err)
	end := start.AddDate(0, 0, 1)
	effEnd, err := time.Parse(time.RFC3339, "2026-06-16T12:00:00Z")
	require.NoError(t, err)
	p := Params{
		RangeStart: start, RangeEnd: end, Loc: loc,
		EffectiveEnd: effEnd, Partial: true,
		GapCapSeconds: 300, Bucket: BucketSpec{BucketMinute, 300},
	}
	// One session whose activity interval STRADDLES effEnd: messages at 11:58
	// and 12:10. The 12-minute gap caps to 5 min, so the natural interval is
	// [11:58, 12:03) -- it crosses 12:00. The clip to effEnd is the binding
	// constraint here (the cap alone would leave 5 min past 11:58), so only
	// 11:58->12:00 = 2 minutes are counted, proving the straddle is clipped.
	act := []ActivityEvent{
		{SessionID: "s1", Ordinal: 1, Timestamp: "2026-06-16T11:58:00Z", Role: "user"},
		{SessionID: "s1", Ordinal: 2, Timestamp: "2026-06-16T12:10:00Z", Role: "assistant", Model: "m1"},
	}
	sessions := []SessionMeta{{SessionID: "s1", Project: "p", Agent: "claude"}}
	r := Aggregate(p, sessions, act, nil)

	assert.True(t, r.Partial, "mid-day report must be partial")
	// All windows are emitted regardless of how much of the range has elapsed.
	assert.Equal(t, 288, r.BucketCount, "full local day has 288 five-minute buckets")
	assert.Len(t, r.Buckets, r.BucketCount,
		"buckets slice lists every window, not just the elapsed ones")
	assert.Less(t, r.ElapsedBucketCount, r.BucketCount,
		"a partial day elapses fewer buckets than the full day")
	assert.Equal(t, 144, r.ElapsedBucketCount, "12h elapsed yields 144 buckets")
	// Straddling interval clipped to effEnd: 11:58->12:00 = 2 minutes.
	assert.InDelta(t, 2.0, r.Totals.AgentMinutes, 1e-9,
		"interval clipped to effEnd, not the full capped span")
	assert.InDelta(t, 2.0, r.Totals.ActiveMinutes, 1e-9,
		"single clipped interval contributes 2 active minutes")
	require.Len(t, r.BySession, 1)
	require.NotNil(t, r.BySession[0].AgentMinutes)
	assert.InDelta(t, 2.0, *r.BySession[0].AgentMinutes, 1e-9,
		"per-session minutes also clipped to effEnd")
	// Idle is measured against the ELAPSED denominator (720 min), not the
	// full-day 1440: 720 - 2 = 718.
	assert.InDelta(t, 718.0, r.Totals.IdleMinutes, 1e-9,
		"partial idle uses the elapsed denominator, not full day")
}

func TestAggregate_GapCapAndActiveMinutes(t *testing.T) {
	p := baseParams(t, "2026-06-16", "UTC")
	// One session: 3 messages. Gap 1 -> 2 is 2 min, gap 2 -> 3 is 40 min
	// (capped to 5). Active = 2 + 5 = 7 minutes.
	act := []ActivityEvent{
		{SessionID: "a", Ordinal: 1, Timestamp: "2026-06-16T10:00:00Z", Role: "user"},
		{SessionID: "a", Ordinal: 2, Timestamp: "2026-06-16T10:02:00Z", Role: "assistant", Model: "m1"},
		{SessionID: "a", Ordinal: 3, Timestamp: "2026-06-16T10:42:00Z", Role: "assistant", Model: "m1"},
	}
	r := Aggregate(p, nil, act, nil)
	assert.InDelta(t, 7.0, r.Totals.AgentMinutes, 1e-9)
	assert.InDelta(t, 7.0, r.Totals.ActiveMinutes, 1e-9)
}

func TestAggregate_NonMonotonicGapIgnored(t *testing.T) {
	p := baseParams(t, "2026-06-16", "UTC")
	act := []ActivityEvent{
		{SessionID: "a", Ordinal: 1, Timestamp: "2026-06-16T10:05:00Z", Role: "user"},
		{SessionID: "a", Ordinal: 2, Timestamp: "2026-06-16T10:04:00Z", Role: "assistant", Model: "m1"},
	}
	r := Aggregate(p, nil, act, nil)
	assert.InDelta(t, 0.0, r.Totals.AgentMinutes, 1e-9)
}

func TestAggregate_MidnightClipWithFarSuccessor(t *testing.T) {
	p := baseParams(t, "2026-06-16", "UTC")
	// Anchor at 23:59 on the day, successor at 00:20 next day (gap capped to
	// 5 min). Interval [23:59, 00:04) clipped to the day end 00:00 leaves
	// [23:59, 00:00) = 1 minute in-range.
	act := []ActivityEvent{
		{SessionID: "a", Ordinal: 1, Timestamp: "2026-06-16T23:59:00Z", Role: "user"},
		{SessionID: "a", Ordinal: 2, Timestamp: "2026-06-17T00:20:00Z", Role: "assistant", Model: "m1"},
	}
	r := Aggregate(p, nil, act, nil)
	assert.InDelta(t, 1.0, r.Totals.AgentMinutes, 1e-9)
}

func TestAggregate_IntervalsExposedSortedAndContiguous(t *testing.T) {
	p := baseParams(t, "2026-06-16", "UTC")
	// Session "a" has THREE messages in the 10:00-10:05 slot, so buildIntervals
	// emits TWO contiguous consecutive-pair intervals there: [10:00,10:01) and
	// [10:01,10:02). The frontend must dedup these by session id. Session "b"
	// yields one interval [10:01,10:03).
	act := []ActivityEvent{
		{SessionID: "a", Ordinal: 1, Timestamp: "2026-06-16T10:00:00Z", Role: "user"},
		{SessionID: "a", Ordinal: 2, Timestamp: "2026-06-16T10:01:00Z", Role: "assistant", Model: "m1"},
		{SessionID: "a", Ordinal: 3, Timestamp: "2026-06-16T10:02:00Z", Role: "assistant", Model: "m1"},
		{SessionID: "b", Ordinal: 1, Timestamp: "2026-06-16T10:01:00Z", Role: "user"},
		{SessionID: "b", Ordinal: 2, Timestamp: "2026-06-16T10:03:00Z", Role: "assistant", Model: "m1"},
	}
	r := Aggregate(p, nil, act, nil)
	want := []ReportInterval{
		{SessionID: "a", Start: "2026-06-16T10:00:00Z", End: "2026-06-16T10:01:00Z"},
		{SessionID: "a", Start: "2026-06-16T10:01:00Z", End: "2026-06-16T10:02:00Z"},
		{SessionID: "b", Start: "2026-06-16T10:01:00Z", End: "2026-06-16T10:03:00Z"},
	}
	assert.Equal(t, want, r.Intervals,
		"intervals exposed, sorted by (start,end,session); a's two contiguous "+
			"intervals are both present so the frontend dedups by session id")
}

func TestAggregate_IntervalsClippedToEffEnd(t *testing.T) {
	loc := mustLoad(t, "UTC")
	start, err := time.Parse(time.RFC3339, "2026-06-16T00:00:00Z")
	require.NoError(t, err)
	end := start.AddDate(0, 0, 1)
	effEnd, err := time.Parse(time.RFC3339, "2026-06-16T12:00:00Z")
	require.NoError(t, err)
	p := Params{
		RangeStart: start, RangeEnd: end, Loc: loc,
		EffectiveEnd: effEnd, Partial: true,
		GapCapSeconds: 300, Bucket: BucketSpec{BucketMinute, 300},
	}
	// Pair [11:58,12:10): the 12-min gap caps to 5 min -> [11:58,12:03); the clip
	// to effEnd (12:00) is binding, so the exposed interval ends at 12:00.
	act := []ActivityEvent{
		{SessionID: "s1", Ordinal: 1, Timestamp: "2026-06-16T11:58:00Z", Role: "user"},
		{SessionID: "s1", Ordinal: 2, Timestamp: "2026-06-16T12:10:00Z", Role: "assistant", Model: "m1"},
	}
	r := Aggregate(p, nil, act, nil)
	require.True(t, r.Partial)
	require.Len(t, r.Intervals, 1)
	assert.Equal(t, "2026-06-16T11:58:00Z", r.Intervals[0].Start)
	assert.Equal(t, "2026-06-16T12:00:00Z", r.Intervals[0].End,
		"interval straddling effEnd is clipped to it")
}

func TestAggregate_OverlapExceedsPeakConcurrency(t *testing.T) {
	p := baseParams(t, "2026-06-16", "UTC")
	// Within the single 5-min slot [10:05,10:10): session a is active
	// [10:05,10:07) and session b [10:08,10:10). They never overlap in time, so
	// peak concurrency is 1 -- but TWO distinct sessions overlap the slot. This
	// is exactly why the popover's "N active" can exceed the bar's max_agents.
	act := []ActivityEvent{
		{SessionID: "a", Ordinal: 1, Timestamp: "2026-06-16T10:05:00Z", Role: "user"},
		{SessionID: "a", Ordinal: 2, Timestamp: "2026-06-16T10:07:00Z", Role: "assistant", Model: "m1"},
		{SessionID: "b", Ordinal: 1, Timestamp: "2026-06-16T10:08:00Z", Role: "user"},
		{SessionID: "b", Ordinal: 2, Timestamp: "2026-06-16T10:10:00Z", Role: "assistant", Model: "m1"},
	}
	r := Aggregate(p, nil, act, nil)
	assert.Equal(t, 1, r.Peak.Agents, "sessions never overlap in time -> peak concurrency 1")
	want := []ReportInterval{
		{SessionID: "a", Start: "2026-06-16T10:05:00Z", End: "2026-06-16T10:07:00Z"},
		{SessionID: "b", Start: "2026-06-16T10:08:00Z", End: "2026-06-16T10:10:00Z"},
	}
	assert.Equal(t, want, r.Intervals,
		"two distinct sessions overlap the slot though peak concurrency is 1")
}

func TestAggregate_IntervalsUseSecondResolutionForParity(t *testing.T) {
	p := baseParams(t, "2026-06-16", "UTC")
	// Two messages 0.5s apart yield a sub-second interval. Bounds are exposed at
	// second resolution (RFC3339) so they stay byte-identical across the
	// microsecond-resolution PostgreSQL/DuckDB mirrors; finer precision would let
	// the same session serialize differently per backend. The span therefore
	// collapses to a point (start == end), which the client places in the slot
	// containing the instant. See activeSessions.ts.
	act := []ActivityEvent{
		{SessionID: "a", Ordinal: 1, Timestamp: "2026-06-16T10:00:00.300Z", Role: "user"},
		{SessionID: "a", Ordinal: 2, Timestamp: "2026-06-16T10:00:00.800Z", Role: "assistant", Model: "m1"},
	}
	r := Aggregate(p, nil, act, nil)
	require.Len(t, r.Intervals, 1)
	assert.Equal(t, "2026-06-16T10:00:00Z", r.Intervals[0].Start)
	assert.Equal(t, "2026-06-16T10:00:00Z", r.Intervals[0].End,
		"sub-second bounds collapse to second resolution for cross-backend parity")
}

func TestAggregate_BucketPeakSplitAtTotalPeakInstant(t *testing.T) {
	p := baseParams(t, "2026-06-16", "UTC")
	// All activity falls in the single 5-min bucket [10:00,10:05). Two AUTOMATED
	// sessions are both live [10:00,10:01); two INTERACTIVE sessions are both
	// live [10:02,10:04). Each class independently peaks at 2, but at DIFFERENT
	// instants, so naively stacking the two independent peaks (2+2=4) would
	// overstate the true peak of 2. The split is taken at the instant the total
	// peak first occurs (10:00), where only the two automated sessions are live:
	// AutomatedAtPeak=2, InteractiveAtPeak=0, summing to MaxAgents=2.
	act := []ActivityEvent{
		{SessionID: "a1", Ordinal: 1, Timestamp: "2026-06-16T10:00:00Z", Role: "user"},
		{SessionID: "a1", Ordinal: 2, Timestamp: "2026-06-16T10:01:00Z", Role: "assistant", Model: "m1"},
		{SessionID: "a2", Ordinal: 1, Timestamp: "2026-06-16T10:00:00Z", Role: "user"},
		{SessionID: "a2", Ordinal: 2, Timestamp: "2026-06-16T10:01:00Z", Role: "assistant", Model: "m1"},
		{SessionID: "i1", Ordinal: 1, Timestamp: "2026-06-16T10:02:00Z", Role: "user"},
		{SessionID: "i1", Ordinal: 2, Timestamp: "2026-06-16T10:04:00Z", Role: "assistant", Model: "m1"},
		{SessionID: "i2", Ordinal: 1, Timestamp: "2026-06-16T10:02:00Z", Role: "user"},
		{SessionID: "i2", Ordinal: 2, Timestamp: "2026-06-16T10:04:00Z", Role: "assistant", Model: "m1"},
	}
	sessions := []SessionMeta{
		{SessionID: "a1", Project: "P", Agent: "claude", IsAutomated: true},
		{SessionID: "a2", Project: "P", Agent: "claude", IsAutomated: true},
		{SessionID: "i1", Project: "P", Agent: "claude", IsAutomated: false},
		{SessionID: "i2", Project: "P", Agent: "claude", IsAutomated: false},
	}
	r := Aggregate(p, sessions, act, nil)

	b := r.Buckets[120] // [10:00,10:05)
	assert.Equal(t, 2, b.MaxAgents, "true peak is 2, never the 2+2 independent stack")
	assert.Equal(t, 2, b.AutomatedAtPeak, "both automated sessions live at the peak instant")
	assert.Equal(t, 0, b.InteractiveAtPeak, "no interactive session live at the peak instant")

	// Invariant across every bucket: the split sums to the true peak.
	for i, bk := range r.Buckets {
		assert.Equalf(t, bk.MaxAgents, bk.AutomatedAtPeak+bk.InteractiveAtPeak,
			"bucket %d: automated+interactive at-peak must equal max_agents", i)
	}
}

func TestAggregate_BreakdownCostAndAutomatedSegments(t *testing.T) {
	loc := mustLoad(t, "UTC")
	start := mustStart(t, "2026-06-16T00:00:00Z")
	end := start.AddDate(0, 0, 1)
	p := Params{
		RangeStart: start, RangeEnd: end, Loc: loc,
		EffectiveEnd: end, Partial: false,
		GapCapSeconds: 300, Bucket: BucketSpec{BucketMinute, 300},
	}
	// ta: timed automated (2 min, cost 1). ti: timed interactive (3 min, cost 2).
	// ua: UNTIMED automated (no activity, cost 4). All project "P", model "m1".
	act := []ActivityEvent{
		{SessionID: "ta", Ordinal: 1, Timestamp: "2026-06-16T10:00:00Z", Role: "user"},
		{SessionID: "ta", Ordinal: 2, Timestamp: "2026-06-16T10:02:00Z", Role: "assistant", Model: "m1"},
		{SessionID: "ti", Ordinal: 1, Timestamp: "2026-06-16T10:00:00Z", Role: "user"},
		{SessionID: "ti", Ordinal: 2, Timestamp: "2026-06-16T10:03:00Z", Role: "assistant", Model: "m1"},
	}
	usage := []UsageRow{
		{SessionID: "ta", Model: "m1", Timestamp: "2026-06-16T10:00:00Z", OutputTokens: 10, Cost: 1.0, ClaudeMessageID: "ta", ClaudeRequestID: "r"},
		{SessionID: "ti", Model: "m1", Timestamp: "2026-06-16T10:00:00Z", OutputTokens: 20, Cost: 2.0, ClaudeMessageID: "ti", ClaudeRequestID: "r"},
		{SessionID: "ua", Model: "m1", Timestamp: "2026-06-16T10:00:00Z", OutputTokens: 40, Cost: 4.0, ClaudeMessageID: "ua", ClaudeRequestID: "r"},
	}
	sessions := []SessionMeta{
		{SessionID: "ta", Project: "P", Agent: "claude", IsAutomated: true},
		{SessionID: "ti", Project: "P", Agent: "claude", IsAutomated: false},
		{SessionID: "ua", Project: "P", Agent: "claude", IsAutomated: true},
	}
	r := Aggregate(p, sessions, act, usage)

	require.Len(t, r.ByProject, 1)
	proj := r.ByProject[0]
	assert.Equal(t, "P", proj.Key)
	assert.InDelta(t, 5.0, proj.AgentMinutes, 1e-9, "2+3 timed minutes")
	assert.InDelta(t, 7.0, proj.Cost, 1e-9, "1+2+4 includes the untimed session")
	assert.InDelta(t, 2.0, proj.AutomatedAgentMinutes, 1e-9)
	assert.InDelta(t, 3.0, proj.InteractiveAgentMinutes, 1e-9)
	assert.InDelta(t, 5.0, proj.AutomatedCost, 1e-9, "ta 1 + ua 4")
	assert.InDelta(t, 2.0, proj.InteractiveCost, 1e-9, "ti 2")
	assert.InDelta(t, proj.AgentMinutes,
		proj.AutomatedAgentMinutes+proj.InteractiveAgentMinutes, 1e-9)
	assert.InDelta(t, proj.Cost, proj.AutomatedCost+proj.InteractiveCost, 1e-9)
	assert.InDelta(t, r.Totals.Cost, proj.Cost, 1e-9,
		"cost breakdown sums to total cost; untimed cost is not dropped")

	assert.InDelta(t, 5.0, r.Totals.AgentMinutes, 1e-9)
	assert.InDelta(t, 2.0, r.Totals.AutomatedAgentMinutes, 1e-9)
	assert.InDelta(t, 3.0, r.Totals.InteractiveAgentMinutes, 1e-9)
	assert.InDelta(t, 5.0, r.Totals.AutomatedCost, 1e-9)
	assert.InDelta(t, 2.0, r.Totals.InteractiveCost, 1e-9)

	autoByID := map[string]bool{}
	for _, row := range r.BySession {
		autoByID[row.SessionID] = row.IsAutomated
	}
	assert.True(t, autoByID["ta"])
	assert.False(t, autoByID["ti"])
	assert.True(t, autoByID["ua"], "untimed automated session keeps its class")

	require.Len(t, r.ByModel, 1)
	assert.Equal(t, "m1", r.ByModel[0].Key)
	assert.InDelta(t, 5.0, r.ByModel[0].AgentMinutes, 1e-9)
	assert.InDelta(t, 7.0, r.ByModel[0].Cost, 1e-9)
	assert.InDelta(t, 5.0, r.ByModel[0].AutomatedCost, 1e-9)
	assert.InDelta(t, 2.0, r.ByModel[0].InteractiveCost, 1e-9)
}

// TestAggregate_UsageOnlySessionZeroCostKeepsPrimaryModel confirms a session
// whose only signal is zero-cost or unpriced usage still reports its known
// model as the primary. Model weight for usage-only sessions comes from cost,
// so a zero cost left primary_model blank while models listed the model,
// showing a known-model session with no model in the table.
func TestAggregate_UsageOnlySessionZeroCostKeepsPrimaryModel(t *testing.T) {
	loc := mustLoad(t, "UTC")
	start := mustStart(t, "2026-06-16T00:00:00Z")
	end := start.AddDate(0, 0, 1)
	p := Params{
		RangeStart: start, RangeEnd: end, Loc: loc,
		EffectiveEnd: end, Partial: false,
		GapCapSeconds: 300, Bucket: BucketSpec{BucketMinute, 300},
	}
	// One untimed session (no activity events) whose single usage row has a
	// known model but ZERO cost.
	usage := []UsageRow{
		{SessionID: "u", Model: "m1", Timestamp: "2026-06-16T10:00:00Z",
			OutputTokens: 0, Cost: 0, ClaudeMessageID: "u", ClaudeRequestID: "r"},
	}
	sessions := []SessionMeta{
		{SessionID: "u", Project: "P", Agent: "claude"},
	}
	r := Aggregate(p, sessions, nil, usage)

	require.Len(t, r.BySession, 1)
	row := r.BySession[0]
	assert.Equal(t, "m1", row.PrimaryModel,
		"zero-cost usage must still report its known model as primary")
	assert.Equal(t, []string{"m1"}, row.Models)
}

// TestAggregate_BreakdownCostDeterministicAcrossSessionOrder pins that the
// per-key cost rollup does not depend on the order sessions arrive in. The
// activityReportSessions queries impose no ORDER BY, so SQLite, PostgreSQL, and
// DuckDB can return the same sessions in different orders. addKey sums float64
// costs across sessions and float addition is not associative -- (0.1+0.2)+0.3
// rounds to a different last bit than (0.3+0.2)+0.1 -- so without a
// deterministic session order the three backends produced 1-ULP-different
// breakdown costs for identical data. Aggregate sorts sessions by ID, so any
// input order yields byte-identical breakdowns.
func TestAggregate_BreakdownCostDeterministicAcrossSessionOrder(t *testing.T) {
	loc := mustLoad(t, "UTC")
	start := mustStart(t, "2026-06-16T00:00:00Z")
	end := start.AddDate(0, 0, 1)
	p := Params{
		RangeStart: start, RangeEnd: end, Loc: loc,
		EffectiveEnd: end, Partial: false,
		GapCapSeconds: 300, Bucket: BucketSpec{BucketMinute, 300},
	}
	// Three usage-only sessions sharing one project, agent, and model so all
	// their costs roll into a single by-project/agent/model key. Costs
	// 0.1/0.2/0.3 are chosen because (0.1+0.2)+0.3 != (0.3+0.2)+0.1 in float64,
	// so reversing the session order shifts the rolled-up cost by one ULP unless
	// the order is normalized.
	usage := []UsageRow{
		{SessionID: "s1", Model: "m1", Timestamp: "2026-06-16T10:00:00Z",
			OutputTokens: 10, Cost: 0.1, ClaudeMessageID: "s1", ClaudeRequestID: "r"},
		{SessionID: "s2", Model: "m1", Timestamp: "2026-06-16T11:00:00Z",
			OutputTokens: 20, Cost: 0.2, ClaudeMessageID: "s2", ClaudeRequestID: "r"},
		{SessionID: "s3", Model: "m1", Timestamp: "2026-06-16T12:00:00Z",
			OutputTokens: 30, Cost: 0.3, ClaudeMessageID: "s3", ClaudeRequestID: "r"},
	}
	meta := func(id string) SessionMeta {
		return SessionMeta{SessionID: id, Project: "P", Agent: "claude"}
	}
	ascending := []SessionMeta{meta("s1"), meta("s2"), meta("s3")}
	descending := []SessionMeta{meta("s3"), meta("s2"), meta("s1")}

	rAsc := Aggregate(p, ascending, nil, usage)
	rDesc := Aggregate(p, descending, nil, usage)

	require.Len(t, rAsc.ByModel, 1)
	require.Len(t, rDesc.ByModel, 1)
	require.Len(t, rAsc.ByAgent, 1)
	require.Len(t, rAsc.ByProject, 1)
	// Exact float equality (not InDelta): byte-for-byte parity is the point.
	require.Equal(t, rAsc.ByModel[0].Cost, rDesc.ByModel[0].Cost,
		"by-model cost must not depend on session arrival order")
	require.Equal(t, rAsc.ByAgent[0].Cost, rDesc.ByAgent[0].Cost,
		"by-agent cost must not depend on session arrival order")
	require.Equal(t, rAsc.ByProject[0].Cost, rDesc.ByProject[0].Cost,
		"by-project cost must not depend on session arrival order")
}
